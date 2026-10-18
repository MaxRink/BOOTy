//go:build linux

package gobgp

import (
	"encoding/binary"
	"fmt"
	"net"
	"slices"
	"strings"

	apipb "github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// This file is the receive side of EVPN multihoming that FRR does as a
// VTEP without a local Ethernet segment (bgpd/bgp_evpn_mh.c, remote ES and
// ES-EVI; zebra/zebra_evpn_mh.c, the MAC nexthop group of an ES):
//
//   - A remote VTEP is active on an ES when BOOTy has its EAD-per-ES route
//     (Ethernet Tag MAX-ET) and its EAD-per-EVI route for the ES
//     (bgp_evpn_es_evi_vtep_re_eval_active, with the default
//     enable_ead_evi_rx).
//   - A MAC whose best Type-2 route has a non-reserved ESI is installed
//     through a kernel FDB nexthop group of the active VTEPs of the ES
//     (aliasing, RFC 7432 section 14.1.2). While the ES has no active VTEP
//     the MAC is not installed (zebra_evpn_rem_mac_install returns when
//     the NHG is not active; RFC 7432 section 9.2.2).
//   - The withdraw of an EAD route removes its VTEP from the group; the
//     MACs of the ES follow at once without their Type-2 withdraws (mass
//     withdraw, RFC 7432 section 17.3). The last VTEP gone uninstalls them
//     (zebra_evpn_nhg_update, zebra_evpn_nhg_mac_update).

// maxET is the Ethernet Tag of an EAD-per-ES route (RFC 7432 section 8.2.1).
const maxET = 0xFFFFFFFF

// nhaFDB is NHA_FDB of linux/nexthop.h (a flag attribute), which x/sys does
// not define.
const nhaFDB = 0xb

// nhIDBase starts the nexthop IDs BOOTy allocates, far from IDs that a
// routing daemon would take from 1.
const nhIDBase = 0x0B007000

// fdbNexthop is a kernel FDB nexthop: one VTEP, or a group of nexthop IDs.
type fdbNexthop struct {
	id    uint32
	vtep  net.IP
	group []uint32
}

// type1Path is one live EAD route: the ES, its VTEP and whether it is the
// EAD-per-ES route.
type type1Path struct {
	esi          string
	vtep         string
	perES        bool
	singleActive bool // Single-Active flag of the ESI Label community (per-ES route)
}

// esGroup is the installed FDB nexthop group of an ES and its VTEPs.
type esGroup struct {
	id    uint32
	vteps []string
}

// esiKey names a non-reserved ESI, "" for the reserved values 0 and MAX-ESI
// (RFC 7432 section 5), which have no ES.
func esiKey(esi *apipb.EthernetSegmentIdentifier) string {
	v := esi.GetValue()
	zero := esi.GetType() == 0 && !slices.ContainsFunc(v, func(b byte) bool { return b != 0 })
	maxESI := esi.GetType() == 0xff && len(v) == 9 && !slices.ContainsFunc(v, func(b byte) bool { return b != 0xff })
	if zero || maxESI {
		return ""
	}
	return fmt.Sprintf("%02x:%x", esi.GetType(), v)
}

// type1Key is the route key of an EAD route: RD, ESI and Ethernet Tag.
func type1Key(route *apipb.EVPNEthernetAutoDiscoveryRoute) string {
	return fmt.Sprintf("%s|%s|%d", rdKey(route.GetRd()), esiKey(route.GetEsi()), route.GetEthernetTag())
}

// handleType1Route records or removes an EAD route and updates the FDB
// nexthop group of its ES and the MACs that use it.
func (o *OverlayTier) handleType1Route(route *apipb.EVPNEthernetAutoDiscoveryRoute, vtep string, singleActive, withdraw bool) {
	esi := esiKey(route.GetEsi())
	if esi == "" {
		return
	}
	if !withdraw && (net.ParseIP(vtep).To4() == nil || vtep == o.cfg.RouterID) {
		return
	}
	key := type1Key(route)
	o.type2Mu.Lock()
	defer o.type2Mu.Unlock()
	if o.type2Stopped {
		return
	}
	if withdraw {
		delete(o.type1Paths, key)
	} else {
		if o.type1Paths == nil {
			o.type1Paths = map[string]type1Path{}
		}
		perES := route.GetEthernetTag() == maxET
		o.type1Paths[key] = type1Path{esi: esi, vtep: vtep, perES: perES, singleActive: perES && singleActive}
	}
	o.syncES(esi)
	o.syncESMACs(esi) // the Single-Active flag may have changed
}

// esiSingleActive reports the Single-Active flag of the ESI Label extended
// community of a path (RFC 7432 section 7.5).
func esiSingleActive(path *apipb.Path) bool {
	for _, attr := range path.GetPattrs() {
		for _, c := range attr.GetExtendedCommunities().GetCommunities() {
			if l := c.GetEsiLabel(); l != nil {
				return l.GetIsSingleActive()
			}
		}
	}
	return false
}

// esSingleActive reports a single-active ES: an EAD-per-ES route of it has
// the Single-Active flag (RFC 7432 section 14.1.1: one is enough). The
// caller holds type2Mu.
func (o *OverlayTier) esSingleActive(esi string) bool {
	for _, p := range o.type1Paths {
		if p.esi == esi && p.singleActive {
			return true
		}
	}
	return false
}

// activeVTEPs returns the VTEPs with both EAD routes for the ES, sorted.
// The caller holds type2Mu.
func (o *OverlayTier) activeVTEPs(esi string) []string {
	// ponytail: scans every EAD route per update; index by ESI if a host
	// ever sees thousands of them.
	perES, perEVI := map[string]bool{}, map[string]bool{}
	for _, p := range o.type1Paths {
		if p.esi != esi {
			continue
		}
		if p.perES {
			perES[p.vtep] = true
		} else {
			perEVI[p.vtep] = true
		}
	}
	var out []string
	for v := range perES {
		if perEVI[v] {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// syncES makes the kernel nexthop group of an ES match its active VTEPs
// and updates the MACs of the ES. The caller holds type2Mu.
func (o *OverlayTier) syncES(esi string) {
	vteps := o.activeVTEPs(esi)
	old, had := o.esGroups[esi]
	if (had && slices.Equal(old.vteps, vteps)) || (!had && len(vteps) == 0) {
		return
	}
	if len(vteps) == 0 {
		o.dropESGroup(esi, old, had)
		o.log.Info("ES has no active VTEP, its MACs are not installed", "esi", esi)
		return
	}
	members := make([]uint32, 0, len(vteps))
	for _, v := range vteps {
		id, ok := o.vtepNexthopID(v)
		if !ok {
			o.dropESGroup(esi, old, had) // the MACs fall back to their VTEP
			return
		}
		members = append(members, id)
	}
	g := esGroup{id: old.id, vteps: vteps}
	if !had {
		g.id = o.nextNexthopID()
	}
	if err := o.netlinkOps.NexthopReplace(fdbNexthop{id: g.id, group: members}); err != nil {
		o.log.Warn("failed to install ES nexthop group, its MACs use their VTEP", "esi", esi, "vteps", vteps, "error", err)
		o.dropESGroup(esi, old, had)
		return
	}
	if o.esGroups == nil {
		o.esGroups = map[string]esGroup{}
	}
	o.esGroups[esi] = g
	o.dropUnusedVTEPNexthops()
	o.log.Info("ES nexthop group", "esi", esi, "id", g.id, "vteps", vteps)
	o.syncESMACs(esi)
}

// dropESGroup forgets the group of an ES, moves its MACs off it (to their
// VTEP while the ES is active, else out of the FDB) and deletes it. The
// caller holds type2Mu.
func (o *OverlayTier) dropESGroup(esi string, old esGroup, had bool) {
	delete(o.esGroups, esi)
	o.syncESMACs(esi) // before the group goes: the kernel keeps no entry on a deleted group
	if had {
		o.deleteNexthop(old.id)
	}
	o.dropUnusedVTEPNexthops()
}

// deleteNexthop deletes a kernel nexthop and keeps its ID for a retry when
// the delete fails. The caller holds type2Mu.
func (o *OverlayTier) deleteNexthop(id uint32) {
	if err := o.netlinkOps.NexthopDel(id); err != nil {
		o.log.Warn("failed to delete nexthop, retried later", "id", id, "error", err)
		o.staleNexthops = append(o.staleNexthops, id)
	}
}

// syncESMACs updates the FDB entries of the MACs whose best route has the
// ESI. The caller holds type2Mu.
func (o *OverlayTier) syncESMACs(esi string) {
	for macStr := range o.type2Paths {
		if best := o.bestType2Path(macStr); best != nil && best.esi == esi {
			if mac, err := net.ParseMAC(macStr); err == nil {
				o.syncType2FDB(mac)
			}
		}
	}
}

// vtepNexthopID returns the FDB nexthop of a VTEP, created on first use.
// The caller holds type2Mu.
func (o *OverlayTier) vtepNexthopID(vtep string) (uint32, bool) {
	if id, ok := o.vtepNexthops[vtep]; ok {
		return id, true
	}
	id := o.nextNexthopID()
	if err := o.netlinkOps.NexthopReplace(fdbNexthop{id: id, vtep: net.ParseIP(vtep)}); err != nil {
		o.log.Warn("failed to install VTEP nexthop", "vtep", vtep, "error", err)
		return 0, false
	}
	if o.vtepNexthops == nil {
		o.vtepNexthops = map[string]uint32{}
	}
	o.vtepNexthops[vtep] = id
	return id, true
}

// dropUnusedVTEPNexthops retries the failed nexthop deletes, then deletes
// the VTEP nexthops that no group uses. The caller holds type2Mu.
func (o *OverlayTier) dropUnusedVTEPNexthops() {
	stale := o.staleNexthops
	o.staleNexthops = nil
	for _, id := range stale {
		o.deleteNexthop(id)
	}
	for vtep, id := range o.vtepNexthops {
		used := false
		for _, g := range o.esGroups {
			used = used || slices.Contains(g.vteps, vtep)
		}
		if used {
			continue
		}
		delete(o.vtepNexthops, vtep)
		o.deleteNexthop(id)
	}
}

// nextNexthopID allocates a kernel nexthop ID. The caller holds type2Mu.
func (o *OverlayTier) nextNexthopID() uint32 {
	o.lastNexthopID++
	return nhIDBase + o.lastNexthopID
}

// removeESState deletes the nexthop groups, then the VTEP nexthops and the
// nexthops of earlier failed deletes, at Teardown, after the FDB entries
// that use them. Nexthops are global kernel objects: they outlive the VXLAN
// device. The caller holds type2Mu.
func (o *OverlayTier) removeESState() {
	ids := make([]uint32, 0, len(o.esGroups)+len(o.vtepNexthops)+len(o.staleNexthops))
	for _, g := range o.esGroups {
		ids = append(ids, g.id)
	}
	for _, id := range o.vtepNexthops {
		ids = append(ids, id)
	}
	ids = append(ids, o.staleNexthops...)
	for _, id := range ids {
		if err := o.netlinkOps.NexthopDel(id); err != nil {
			o.log.Warn("failed to delete nexthop at teardown", "id", id, "error", err)
		}
	}
	o.type1Paths, o.esGroups, o.vtepNexthops, o.staleNexthops = nil, nil, nil, nil
}

// nhGroupKey is the installed-FDB marker of a MAC that uses a group.
func nhGroupKey(id uint32) string { return fmt.Sprintf("nhg:%d", id) }

func parseNhGroupKey(s string) (uint32, bool) {
	var id uint32
	if !strings.HasPrefix(s, "nhg:") {
		return 0, false
	}
	_, err := fmt.Sscanf(s, "nhg:%d", &id)
	return id, err == nil
}

// The netlink package has no nexthop objects, and its Neigh struct has no
// field for the NDA_NH_ID attribute of FDB entries (the attribute constant
// exists), so these requests are built here, like `ip nexthop add id N
// via VTEP fdb`, `ip nexthop add id G group N1/N2 fdb` and `bridge fdb
// replace MAC dev vxlan nhid G self`.

type nhMsg unix.Nhmsg

func (m *nhMsg) Len() int { return unix.SizeofNhmsg }

func (m *nhMsg) Serialize() []byte {
	b := []byte{m.Family, m.Scope, m.Protocol, m.Resvd, 0, 0, 0, 0}
	binary.NativeEndian.PutUint32(b[4:], m.Flags)
	return b
}

func (netlinkOverlayOps) NexthopReplace(nh fdbNexthop) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWNEXTHOP, unix.NLM_F_CREATE|unix.NLM_F_REPLACE|unix.NLM_F_ACK)
	msg := &nhMsg{Family: unix.AF_UNSPEC, Protocol: unix.RTPROT_BGP}
	if nh.vtep != nil {
		msg.Family = unix.AF_INET
	}
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.NHA_ID, nl.Uint32Attr(nh.id)))
	if nh.vtep != nil {
		req.AddData(nl.NewRtAttr(unix.NHA_GATEWAY, nh.vtep.To4()))
	} else {
		grp := make([]byte, 0, len(nh.group)*unix.SizeofNexthopGrp)
		for _, id := range nh.group {
			grp = binary.NativeEndian.AppendUint32(grp, id)
			grp = append(grp, 0, 0, 0, 0) // weight 0 means 1; reserved
		}
		req.AddData(nl.NewRtAttr(unix.NHA_GROUP, grp))
	}
	req.AddData(nl.NewRtAttr(nhaFDB, nil))
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("nexthop %d: %w", nh.id, err)
	}
	return nil
}

func (netlinkOverlayOps) NexthopDel(id uint32) error {
	req := nl.NewNetlinkRequest(unix.RTM_DELNEXTHOP, unix.NLM_F_ACK)
	req.AddData(&nhMsg{Family: unix.AF_UNSPEC})
	req.AddData(nl.NewRtAttr(unix.NHA_ID, nl.Uint32Attr(id)))
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("delete nexthop %d: %w", id, err)
	}
	return nil
}

func (netlinkOverlayOps) FDBNhidSet(linkIndex int, mac net.HardwareAddr, nhid uint32) error {
	req := nl.NewNetlinkRequest(unix.RTM_NEWNEIGH, unix.NLM_F_CREATE|unix.NLM_F_REPLACE|unix.NLM_F_ACK)
	req.AddData(&netlink.Ndmsg{
		Family: unix.AF_BRIDGE,
		Index:  uint32(linkIndex), //nolint:gosec // an interface index
		State:  netlink.NUD_PERMANENT,
		Flags:  netlink.NTF_SELF,
	})
	req.AddData(nl.NewRtAttr(netlink.NDA_LLADDR, mac))
	req.AddData(nl.NewRtAttr(netlink.NDA_NH_ID, nl.Uint32Attr(nhid)))
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("fdb %s nhid %d: %w", mac, nhid, err)
	}
	return nil
}

func (netlinkOverlayOps) FDBDel(linkIndex int, mac net.HardwareAddr) error {
	req := nl.NewNetlinkRequest(unix.RTM_DELNEIGH, unix.NLM_F_ACK)
	req.AddData(&netlink.Ndmsg{
		Family: unix.AF_BRIDGE,
		Index:  uint32(linkIndex), //nolint:gosec // an interface index
		Flags:  netlink.NTF_SELF,
	})
	req.AddData(nl.NewRtAttr(netlink.NDA_LLADDR, mac))
	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("delete fdb %s: %w", mac, err)
	}
	return nil
}
