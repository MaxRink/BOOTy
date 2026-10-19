package netplan

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frrconf"
)

// FRRParams holds parameters extracted from an FRR configuration file.
type FRRParams struct {
	ASN             uint32
	LocalASN        uint32 // local-as override (BM4X uses per-neighbor local-as).
	RouterID        string
	UnnumberedPeers []string // Interface names from "neighbor X interface" lines.
	NumberedPeers   []string // IPv4 addresses from "neighbor X remote-as" lines.
	NumberedPeersV6 []string // IPv6 addresses from "neighbor X remote-as" lines.
	EVPN            bool     // True if "address-family l2vpn evpn" found.
	AdvertiseAllVNI bool     // True if "advertise-all-vni" found.
	// ResolveOverlayIndex is true if "enable-resolve-overlay-index" found.
	ResolveOverlayIndex bool
	// L3VNIs lists VNIs mapped to a VRF ("vni N" at top level for the default
	// VRF or inside a "vrf" block). FRR advertises only Type-5 routes for them;
	// advertise-all-vni produces Type-2/Type-3 routes for L2VNIs only.
	L3VNIs []uint32
	// Sessions are the resolved BGP peers. Each keeps its own remote-as,
	// local-as, timers and address families.
	Sessions []network.BGPSession
	// MultipathRelax is set when a router has "bgp bestpath as-path
	// multipath-relax".
	MultipathRelax bool
	// BlackholeRoutes are the "ip route" and "ipv6 route" blackhole prefixes.
	BlackholeRoutes []string
	// Model is the full parsed configuration.
	Model *frrconf.Config
}

// ParseFRRConfig extracts networking parameters from an FRR configuration file.
func ParseFRRConfig(path string) (*FRRParams, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read frr config %s: %w", path, err)
	}
	return ParseFRRConfigBytes(data)
}

// ParseFRRConfigBytes parses FRR config content with frrconf.Parse and
// derives the parameters the network stack uses today. A statement BOOTy
// does not implement is an error.
func ParseFRRConfigBytes(data []byte) (*FRRParams, error) {
	return ParseFRRConfigBytesWith(data, "frr.conf", true)
}

// ParseFRRConfigBytesWith is ParseFRRConfigBytes. With strict false
// (FRR_CONFIG_STRICT=false) a statement BOOTy does not implement is logged
// with the file name and its line and skipped.
func ParseFRRConfigBytesWith(data []byte, name string, strict bool) (*FRRParams, error) {
	model, err := frrconf.ParseWith(data, frrconf.Options{Lenient: !strict, Warn: func(e *frrconf.ParseError) {
		slog.Warn("frr.conf statement skipped (FRR_CONFIG_STRICT=false)", "file", name, "line", e.Line, "statement", e.Text, "error", e.Err)
	}})
	if err != nil {
		return nil, err //nolint:wrapcheck // the ParseError names the line; the caller names the file
	}
	params := &FRRParams{Model: model}
	for i := range model.Routers {
		r := &model.Routers[i]
		if params.ASN == 0 {
			params.ASN = r.ASN
		}
		if params.RouterID == "" {
			params.RouterID = r.RouterID
		}
		addPeers(params, r)
		params.MultipathRelax = params.MultipathRelax || r.MultipathRelax
		for j := range r.AddressFamilies {
			af := &r.AddressFamilies[j]
			if af.SAFI == "evpn" {
				params.EVPN = true
				params.AdvertiseAllVNI = params.AdvertiseAllVNI || af.AdvertiseAllVNI
				params.ResolveOverlayIndex = params.ResolveOverlayIndex || af.ResolveOverlayIndex
			}
		}
	}
	sessions, err := model.Sessions()
	if err != nil {
		return nil, fmt.Errorf("frr config: %w", err)
	}
	params.Sessions = BGPSessions(sessions)
	for _, r := range model.StaticRoutes {
		params.BlackholeRoutes = append(params.BlackholeRoutes, r.Prefix)
	}
	warnUnapplied(model)
	for _, v := range model.VRFVNIs {
		if !slices.Contains(params.L3VNIs, v.VNI) {
			params.L3VNIs = append(params.L3VNIs, v.VNI)
		}
	}
	return params, nil
}

// addPeers collects interface peers, numbered peers with their own
// remote-as, and the first local-as, in config order.
func addPeers(params *FRRParams, r *frrconf.Router) {
	for i := range r.Neighbors {
		n := &r.Neighbors[i]
		if params.LocalASN == 0 && n.LocalAS != nil {
			params.LocalASN = n.LocalAS.ASN
		}
		switch {
		case n.PeerGroup:
		case n.Interface:
			if !slices.Contains(params.UnnumberedPeers, n.Name) {
				params.UnnumberedPeers = append(params.UnnumberedPeers, n.Name)
			}
		case n.RemoteAS != nil:
			addNumberedPeer(params, n.Name)
		}
	}
}

func addNumberedPeer(params *FRRParams, name string) {
	addr, err := netip.ParseAddr(name)
	if err != nil {
		return
	}
	list, skip := &params.NumberedPeers, []string{"169.254.", "127."}
	if !addr.Is4() {
		list, skip = &params.NumberedPeersV6, []string{"fd00:7:caa5:", "fe80:"}
	}
	for _, p := range skip {
		if strings.HasPrefix(name, p) {
			return
		}
	}
	if !slices.Contains(*list, name) {
		*list = append(*list, name)
	}
}

// BGPSessions converts resolved FRR sessions (from frr.conf or from the
// native network.bgp.sessions) into the network stack sessions.
func BGPSessions(sessions []frrconf.Session) []network.BGPSession {
	out := make([]network.BGPSession, 0, len(sessions))
	for i := range sessions {
		out = append(out, toBGPSession(&sessions[i]))
	}
	return out
}

func toBGPSession(s *frrconf.Session) network.BGPSession {
	out := network.BGPSession{
		Name:                 s.Name,
		Interface:            s.Interface,
		VRF:                  s.VRF,
		Internal:             s.Internal,
		RemoteASN:            s.RemoteASN,
		KeepaliveTime:        s.KeepaliveTime,
		HoldTime:             s.HoldTime,
		ConnectRetry:         s.ConnectRetry,
		ConnectRetrySet:      s.ConnectRetrySet,
		IPv4Unicast:          slices.Contains(s.Families, frrconf.FamilyIPv4Unicast),
		IPv6Unicast:          slices.Contains(s.Families, frrconf.FamilyIPv6Unicast),
		L2VPNEVPN:            slices.Contains(s.Families, frrconf.FamilyL2VPNEVPN),
		AllowASIn:            s.AllowASIn,
		FastExternalFailover: s.FastExternalFailover,
		BFD:                  s.BFD,
		BFDProfile:           s.BFDProfile,
		BFDTransmitMS:        s.BFDTransmitMS,
		BFDReceiveMS:         s.BFDReceiveMS,
		BFDDetectMult:        s.BFDDetectMult,

		EBGPRequiresPolicy: s.EBGPRequiresPolicy,
	}
	if s.LocalAS != nil {
		out.LocalASN = s.LocalAS.ASN
	}
	for _, rm := range s.RouteMaps {
		out.RouteMaps = append(out.RouteMaps, network.SessionRouteMaps{Family: rm.Family, In: rm.In, Out: rm.Out})
	}
	return out
}

// warnUnapplied logs each parsed statement that GoBGP mode does not apply
// yet, so it is not dropped silently.
func warnUnapplied(model *frrconf.Config) {
	warn := func(line int, stmt string, args ...any) {
		slog.Warn("frr.conf statement parsed but not applied yet", append([]any{"line", line, "statement", stmt}, args...)...)
	}
	for i := range model.Routers {
		r := &model.Routers[i]
		if r.SuppressDuplicates != nil {
			warn(r.Line, "[no] bgp suppress-duplicates", "router", r.ASN)
		}
		for j := range r.AddressFamilies {
			af := &r.AddressFamilies[j]
			for _, n := range af.Neighbors {
				if n.SoftReconfigInbound {
					warn(af.Line, "soft-reconfiguration inbound", "neighbor", n.Name, "family", af.AFI+" "+af.SAFI)
				}
			}
			// BOOTy creates no L2VNI devices of its own; their rd and
			// route-targets are only checked.
			for _, v := range af.L2VNIs {
				if v.RD != "" || len(v.RouteTargetImport)+len(v.RouteTargetExport) > 0 {
					warn(v.Line, "vni rd/route-target", "vni", v.VNI, "rd", v.RD)
				}
			}
		}
	}
}

// policy converts the route-maps and community-lists of the model.
func policy(model *frrconf.Config) ([]network.RouteMap, []network.CommunityList) {
	rms := make([]network.RouteMap, 0, len(model.RouteMaps))
	for _, rm := range model.RouteMaps {
		out := network.RouteMap{Name: rm.Name}
		for _, e := range rm.Entries {
			out.Entries = append(out.Entries, network.RouteMapEntry{
				Seq:                  e.Seq,
				Deny:                 e.Action == frrconf.Deny,
				MatchCommunity:       e.MatchCommunity,
				SetLocalPreference:   e.SetLocalPreference,
				SetCommunities:       e.SetCommunities,
				SetCommunityAdditive: e.SetCommunityAdditive,
			})
		}
		rms = append(rms, out)
	}
	cls := make([]network.CommunityList, 0, len(model.CommunityLists))
	for _, cl := range model.CommunityLists {
		out := network.CommunityList{Name: cl.Name}
		for _, e := range cl.Entries {
			out.Entries = append(out.Entries, network.CommunityListEntry{Deny: e.Action == frrconf.Deny, Communities: e.Communities})
		}
		cls = append(cls, out)
	}
	return rms, cls
}

// l3vniRouteTargets returns the "route-target import|export" of the VRF
// that maps to vni. Without them FRR uses the auto route target
// (AS & 0xFFFF):VNI (bgpd/bgp_evpn.c, form_auto_rt).
func l3vniRouteTargets(model *frrconf.Config, vni uint32) (imp, exp []string) {
	i := slices.IndexFunc(model.VRFVNIs, func(v frrconf.VRFVNI) bool { return v.VNI == vni })
	if i < 0 {
		return nil, nil
	}
	vrf := model.VRFVNIs[i].VRF
	var asn uint32
	for j := range model.Routers {
		r := &model.Routers[j]
		if routerVRF(r.VRF) != vrf {
			continue
		}
		asn = r.ASN
		for k := range r.AddressFamilies {
			if af := &r.AddressFamilies[k]; af.SAFI == "evpn" {
				imp = append(imp, af.RouteTargetImport...)
				exp = append(exp, af.RouteTargetExport...)
			}
		}
	}
	if asn == 0 && len(model.Routers) > 0 {
		asn = model.Routers[0].ASN
	}
	auto := fmt.Sprintf("%d:%d", asn&0xFFFF, vni)
	if len(imp) == 0 {
		imp = []string{auto}
	}
	if len(exp) == 0 {
		exp = []string{auto}
	}
	return imp, exp
}

// l3vniRD returns the RD of the L3VNI vni's EVPN routes: the "rd" of its
// VRF, else FRR's automatic "<router-id>:<n>" (bgp_evpn.c
// bgp_evpn_derive_auto_rd_for_vrf), where n is the VRF instance's
// vrf_rd_id. bgpd gives each "router bgp" instance the lowest free id
// from 1 when it is created, in frr.conf order (bgpd.c bgp_create); a VRF
// without a "router bgp" gets an instance when its L3VNI appears, after
// the configured ones. It returns that n and the number of instances.
// For a VNI that is not an L3VNI, n is the id of the default VRF instance.
func l3vniRD(model *frrconf.Config, vni uint32) (rd string, index, instances int) {
	vrf, ok := "", false
	for _, v := range model.VRFVNIs {
		if v.VNI == vni {
			vrf, ok = v.VRF, true
		}
	}
	var seen []string
	for i := range model.Routers {
		r := &model.Routers[i]
		v := routerVRF(r.VRF)
		if !slices.Contains(seen, v) {
			seen = append(seen, v)
		}
		if !ok || v != vrf {
			continue
		}
		index = slices.Index(seen, v) + 1
		for k := range r.AddressFamilies {
			if af := &r.AddressFamilies[k]; af.SAFI == "evpn" && af.RD != "" {
				rd = af.RD
			}
		}
	}
	instances = len(seen)
	switch {
	case ok && index == 0:
		instances++
		index = instances
	case !ok:
		// An L2VNI: BOOTy's Type-5 route of the provision address is in
		// the default VRF.
		index = max(slices.Index(seen, "")+1, 1)
	}
	return rd, index, instances
}

// type5Networks returns the "network" prefixes that FRR originates as EVPN
// Type-5 routes for the L3VNI vni: the networks of the VRF that maps to vni,
// for each address family that its "advertise ipv4|ipv6 unicast" selects
// (doc/user/bgp.rst, "advertise <afi> unicast").
func type5Networks(model *frrconf.Config, vni uint32) []string {
	if model == nil {
		return nil
	}
	i := slices.IndexFunc(model.VRFVNIs, func(v frrconf.VRFVNI) bool { return v.VNI == vni })
	if i < 0 {
		return nil
	}
	vrf := model.VRFVNIs[i].VRF
	var out []string
	for j := range model.Routers {
		r := &model.Routers[j]
		if routerVRF(r.VRF) != vrf {
			continue
		}
		for _, afi := range advertisedAFIs(r) {
			for k := range r.AddressFamilies {
				if af := &r.AddressFamilies[k]; af.AFI == afi && af.SAFI == "unicast" {
					out = append(out, af.Networks...)
				}
			}
		}
	}
	return out
}

// routerVRF maps "router bgp N" and "router bgp N vrf default" to "".
func routerVRF(vrf string) string {
	if vrf == "default" {
		return ""
	}
	return vrf
}

func advertisedAFIs(r *frrconf.Router) []string {
	var afis []string
	for i := range r.AddressFamilies {
		af := &r.AddressFamilies[i]
		if af.SAFI != "evpn" {
			continue
		}
		if af.AdvertiseIPv4Unicast {
			afis = append(afis, "ipv4")
		}
		if af.AdvertiseIPv6Unicast {
			afis = append(afis, "ipv6")
		}
	}
	return afis
}
