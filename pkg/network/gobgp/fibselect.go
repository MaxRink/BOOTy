package gobgp

import (
	"cmp"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/osrg/gobgp/v4/pkg/apiutil"
	"github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

// fibRoute is a kernel route that the FIB writer installs: the best path
// and the paths of equal cost (ECMP), like zebra does for bgpd. Table is
// the routing table of the sessions the paths came from.
type fibRoute struct {
	Table    int
	Prefix   netip.Prefix
	Nexthops []fibNexthop
}

// fibNexthop is one next hop. Dev is set for link-local next hops, which
// are only valid on their interface.
type fibNexthop struct {
	Addr netip.Addr
	Dev  string
}

// pathInfo holds the attributes that decide multipath and the next hop.
type pathInfo struct {
	localPref uint32
	asPath    []uint32
	asLen     int // AS_SET counts as one (RFC 4271 9.1.2.2)
	leftAS    uint32
	origin    uint8
	med       uint32
	peerAS    uint32
	internal  bool
	neighbor  string
	table     int
	best      bool
	nexthop   fibNexthop
}

// fibDest is one destination of a ListPath result.
type fibDest struct {
	prefix bgp.NLRI
	paths  []*apiutil.Path
}

// fibSelector holds what route selection needs to know about the
// sessions. localAS returns the effective local AS of the session of a
// neighbor (its local-as, else the router AS), table its routing table
// and peerDev the interface of an unnumbered peer.
type fibSelector struct {
	relax   bool
	localAS func(neighbor string) uint32
	table   func(neighbor string) int
	peerDev func(neighbor string) string
}

// selectRoutes returns the kernel routes for the destinations of one
// unicast table. Paths of sessions in different VRFs go to different
// tables, like FRR keeps one RIB per "router bgp N vrf X". In each table
// it takes the best path and every other path of equal cost under the FRR
// multipath rule (see equalCost). Paths that BOOTy originated itself are
// skipped. A path that can not be installed is skipped and reported in the
// error; the other routes are still returned.
func (s *fibSelector) selectRoutes(dests []fibDest) ([]fibRoute, error) {
	var out []fibRoute
	var errs []error
	for _, d := range dests {
		rs, err := s.selectDestination(d)
		if err != nil {
			errs = append(errs, err)
		}
		out = append(out, rs...)
	}
	return out, errors.Join(errs...)
}

// selectDestination returns the routes of one destination, one per table.
func (s *fibSelector) selectDestination(d fibDest) ([]fibRoute, error) {
	prefix, err := netip.ParsePrefix(d.prefix.String())
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", d.prefix, err)
	}
	byTable, err := s.pathsByTable(d, prefix)
	tables := make([]int, 0, len(byTable))
	for t := range byTable {
		tables = append(tables, t)
	}
	slices.Sort(tables)
	var out []fibRoute
	for _, t := range tables {
		paths := byTable[t]
		// GoBGP marks one best path over all VRFs; a table without it
		// gets its own best path with the FRR decision order.
		best := paths[0]
		for _, p := range paths[1:] {
			if p.best || !best.best && better(p, best) {
				best = p
			}
		}
		r := fibRoute{Table: t, Prefix: prefix.Masked()}
		for _, c := range paths {
			if (c == best || equalCost(best, c, s.relax)) && !slices.Contains(r.Nexthops, c.nexthop) {
				r.Nexthops = append(r.Nexthops, c.nexthop)
			}
		}
		slices.SortFunc(r.Nexthops, func(a, b fibNexthop) int {
			return cmp.Or(a.Addr.Compare(b.Addr), strings.Compare(a.Dev, b.Dev))
		})
		out = append(out, r)
	}
	return out, err
}

// pathsByTable groups the learned paths of a destination by the table of
// their session. A destination with a path of BOOTy itself (its own
// prefix) gets no kernel route, like a local route wins in FRR.
func (s *fibSelector) pathsByTable(d fibDest, prefix netip.Prefix) (map[int][]*pathInfo, error) {
	byTable := map[int][]*pathInfo{}
	var errs []error
	for _, p := range d.paths {
		if p.Filtered || p.Withdrawal {
			continue
		}
		if !learned(p) {
			return map[int][]*pathInfo{}, nil
		}
		info, err := s.describePath(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("destination %s: %w", prefix, err))
			continue
		}
		byTable[info.table] = append(byTable[info.table], info)
	}
	return byTable, errors.Join(errs...)
}

// learned reports whether a path came from a peer. Paths that BOOTy
// added itself have no neighbor address.
func learned(p *apiutil.Path) bool {
	return p.PeerAddress.IsValid() && !p.PeerAddress.IsUnspecified()
}

// equalCost is the multipath rule of FRR 10.3 (bgpd/bgp_route.c
// bgp_path_info_cmp with bgpd/bgp_mpath.c): same local preference, AS path
// length, origin and peer type, and the same MED when the paths come from
// the same leftmost AS (no "always-compare-med"). Without "bgp bestpath
// as-path multipath-relax", eBGP paths must come from the same neighbor
// AS and iBGP paths must have the same AS path.
func equalCost(a, b *pathInfo, multipathRelax bool) bool {
	if a.localPref != b.localPref || a.asLen != b.asLen || a.origin != b.origin || a.internal != b.internal {
		return false
	}
	if a.leftAS == b.leftAS && a.med != b.med {
		return false
	}
	switch {
	case multipathRelax:
		return true
	case a.internal:
		return slices.Equal(a.asPath, b.asPath)
	default:
		return a.peerAS == b.peerAS
	}
}

// better reports whether a wins over b in the FRR decision order, as far
// as BOOTy has the attributes: local preference, AS path length, origin,
// MED (same leftmost AS only), eBGP over iBGP, then the lower neighbor
// address as a stable tie break.
func better(a, b *pathInfo) bool {
	switch {
	case a.localPref != b.localPref:
		return a.localPref > b.localPref
	case a.asLen != b.asLen:
		return a.asLen < b.asLen
	case a.origin != b.origin:
		return a.origin < b.origin
	case a.leftAS == b.leftAS && a.med != b.med:
		return a.med < b.med
	case a.internal != b.internal:
		return !a.internal
	}
	return a.neighbor < b.neighbor
}

// read fills the multipath attributes and returns the next hop and the
// link-local next hop.
func (info *pathInfo) read(attrs []bgp.PathAttributeInterface) (nh, ll netip.Addr) {
	for _, a := range attrs {
		switch a := a.(type) {
		case *bgp.PathAttributeLocalPref:
			info.localPref = a.Value
		case *bgp.PathAttributeOrigin:
			info.origin = a.Value
		case *bgp.PathAttributeMultiExitDisc:
			info.med = a.Value
		case *bgp.PathAttributeAsPath:
			for _, seg := range a.Value {
				info.asPath = append(info.asPath, seg.GetAS()...)
				info.asLen += seg.ASLen()
			}
		case *bgp.PathAttributeNextHop:
			nh = a.Value
		case *bgp.PathAttributeMpReachNLRI:
			nh = a.Nexthop
			ll = a.LinkLocalNexthop
		}
	}
	if len(info.asPath) > 0 {
		info.leftAS = info.asPath[0]
	}
	return nh, ll
}

func (s *fibSelector) describePath(p *apiutil.Path) (*pathInfo, error) {
	neighbor := p.PeerAddress.String()
	info := &pathInfo{
		localPref: 100,
		peerAS:    p.PeerASN,
		neighbor:  neighbor,
		best:      p.Best,
		table:     s.table(neighbor),
	}
	// iBGP is a peer in the session's effective local AS (FRR 10.3
	// peer_calc_sort uses the local-as).
	info.internal = info.peerAS == s.localAS(neighbor)
	nh, ll := info.read(p.Attrs)
	// A link-local next hop is preferred, like zebra does for a directly
	// connected peer that sends a global and a link-local address.
	if ll.IsValid() && ll.IsLinkLocalUnicast() {
		nh = ll
	}
	nh = nh.Unmap()
	if !nh.IsValid() || nh.IsUnspecified() {
		return nil, fmt.Errorf("path from %s has no next hop", neighbor)
	}
	info.nexthop = fibNexthop{Addr: nh}
	if nh.IsLinkLocalUnicast() {
		info.nexthop.Dev = s.peerDev(neighbor)
		if info.nexthop.Dev == "" {
			return nil, fmt.Errorf("link-local next hop %s from %s has no interface", nh, neighbor)
		}
	}
	return info, nil
}
