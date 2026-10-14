package netplan

import (
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"

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
	// RemoteASN is the remote-as of the IPv4 numbered peers when it is not
	// the router ASN. All of them must use the same remote-as, because the
	// network config has one remote ASN for numbered peers.
	RemoteASN       uint32
	EVPN            bool // True if "address-family l2vpn evpn" found.
	AdvertiseAllVNI bool // True if "advertise-all-vni" found.
	// L3VNIs lists VNIs mapped to a VRF ("vni N" at top level for the default
	// VRF or inside a "vrf" block). FRR advertises only Type-5 routes for them;
	// advertise-all-vni produces Type-2/Type-3 routes for L2VNIs only.
	L3VNIs       []uint32
	numberedSeen bool
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
		if err := addPeers(params, r); err != nil {
			return nil, err
		}
		for j := range r.AddressFamilies {
			af := &r.AddressFamilies[j]
			if af.SAFI == "evpn" {
				params.EVPN = true
				params.AdvertiseAllVNI = params.AdvertiseAllVNI || af.AdvertiseAllVNI
			}
		}
	}
	for _, v := range model.VRFVNIs {
		if !slices.Contains(params.L3VNIs, v.VNI) {
			params.L3VNIs = append(params.L3VNIs, v.VNI)
		}
	}
	return params, nil
}

// addPeers collects interface peers, numbered peers with their own
// remote-as, and the first local-as, in config order.
func addPeers(params *FRRParams, r *frrconf.Router) error {
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
			if addNumberedPeer(params, n.Name) {
				if err := setRemoteASN(params, n, r.ASN); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// setRemoteASN records the remote-as of an IPv4 numbered peer. The network
// config has a single remote ASN for numbered peers, so peers with
// different remote-as values or with "remote-as external" are an error
// instead of sessions with a wrong ASN. iBGP peers leave it at 0.
func setRemoteASN(params *FRRParams, n *frrconf.Neighbor, routerASN uint32) error {
	asn := routerASN
	switch n.RemoteAS.Kind {
	case frrconf.RemoteASExternal:
		return fmt.Errorf("neighbor %s remote-as external: only a number or internal is supported", n.Name)
	case frrconf.RemoteASNumber:
		asn = n.RemoteAS.Number
	}
	if asn == routerASN {
		asn = 0
	}
	if params.numberedSeen && asn != params.RemoteASN {
		return fmt.Errorf("numbered neighbors use different remote-as values")
	}
	params.numberedSeen, params.RemoteASN = true, asn
	return nil
}

// addNumberedPeer records a numbered peer and reports whether it is an
// IPv4 peer that BOOTy dials.
func addNumberedPeer(params *FRRParams, name string) bool {
	addr, err := netip.ParseAddr(name)
	if err != nil {
		return false
	}
	list, skip := &params.NumberedPeers, []string{"169.254.", "127."}
	if !addr.Is4() {
		list, skip = &params.NumberedPeersV6, []string{"fd00:7:caa5:", "fe80:"}
	}
	for _, p := range skip {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	if !slices.Contains(*list, name) {
		*list = append(*list, name)
	}
	return addr.Is4()
}
