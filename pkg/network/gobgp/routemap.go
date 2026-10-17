//go:build linux

package gobgp

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	apipb "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"

	"github.com/telekom/BOOTy/pkg/network"
)

// policyPeer is a BGP peer with the route-maps and local-as that the
// policies must cover.
type policyPeer struct {
	name      string     // GoBGP neighbor address
	addr      netip.Addr // may carry a zone
	routeMaps []network.SessionRouteMaps
	localAS   uint32
	allowASIn int
	// requirePolicy is "bgp ebgp-requires-policy" for an eBGP peer;
	// families are its activated address families.
	requirePolicy bool
	families      []string
}

// policyReject is one family and direction of a peer whose routes
// "bgp ebgp-requires-policy" rejects.
type policyReject struct{ peer, dir, fam string }

// Names of the GoBGP policies BOOTy installs as the global import and
// export policies.
const (
	importPolicy = "booty-import"
	exportPolicy = "booty-export"
)

// bgpPolicy is the translation of FRR route-maps into GoBGP defined sets
// and statements. GoBGP applies one global import and one global export
// policy to all peers, so every statement starts with a neighbor set of
// the peers that use the route-map in that address family. FRR semantics
// (doc/user/routemap.rst): the entries run in sequence order, a matching
// permit entry applies its sets and stops, a matching deny entry rejects,
// and a route that no entry matches is rejected (implicit deny).
type bgpPolicy struct {
	sets     []*apipb.DefinedSet
	im, ex   []*apipb.Statement
	rejected []policyReject
}

func buildPolicy(peers []policyPeer, maps []network.RouteMap, lists []network.CommunityList) (*bgpPolicy, error) {
	p := &bgpPolicy{}
	p.localASChecks(peers)
	type key struct{ dir, rm, fam string }
	members := map[key][]string{}
	var order []key
	for _, peer := range peers {
		for _, rm := range peer.routeMaps {
			for dir, name := range map[string]string{"in": rm.In, "out": rm.Out} {
				if name == "" {
					continue
				}
				k := key{dir, name, rm.Family}
				if _, ok := members[k]; !ok {
					order = append(order, k)
				}
				members[k] = append(members[k], hostPrefix(peer.addr))
			}
		}
	}
	slices.SortFunc(order, func(a, b key) int {
		return strings.Compare(a.dir+a.rm+a.fam, b.dir+b.rm+b.fam)
	})
	p.requirePolicy(peers)
	for _, k := range order {
		i := slices.IndexFunc(maps, func(m network.RouteMap) bool { return m.Name == k.rm })
		if i < 0 {
			return nil, fmt.Errorf("route-map %q is not defined", k.rm)
		}
		nset := fmt.Sprintf("rm-%s-%s-%s", k.dir, k.rm, k.fam)
		p.sets = append(p.sets, &apipb.DefinedSet{DefinedType: apipb.DefinedType_NEIGHBOR, Name: nset, List: members[k]})
		stmts, err := p.routeMapStatements(&maps[i], nset, k.fam, lists)
		if err != nil {
			return nil, err
		}
		if k.dir == "in" {
			p.im = append(p.im, stmts...)
		} else {
			p.ex = append(p.ex, stmts...)
		}
	}
	return p, nil
}

// routeMapStatements turns one route-map into statements for the peers of
// the neighbor set nset in the family fam.
func (p *bgpPolicy) routeMapStatements(rm *network.RouteMap, nset, fam string, lists []network.CommunityList) ([]*apipb.Statement, error) {
	entries := slices.Clone(rm.Entries)
	slices.SortStableFunc(entries, func(a, b network.RouteMapEntry) int { return a.Seq - b.Seq })
	family := familyOf(fam)
	base := func() *apipb.Conditions {
		return &apipb.Conditions{
			NeighborSet: &apipb.MatchSet{Type: apipb.MatchSet_ANY, Name: nset},
			AfiSafiIn:   []*apipb.Family{family},
		}
	}
	var out []*apipb.Statement
	for _, e := range entries {
		actions := entryActions(&e)
		name := fmt.Sprintf("%s-%d", nset, e.Seq)
		if e.MatchCommunity == "" {
			out = append(out, &apipb.Statement{Name: name, Conditions: base(), Actions: actions})
			continue
		}
		sets, err := p.communitySets(e.MatchCommunity, lists)
		if err != nil {
			return nil, fmt.Errorf("route-map %s %d: %w", rm.Name, e.Seq, err)
		}
		for j, set := range sets {
			c := base()
			c.CommunitySet = &apipb.MatchSet{Type: apipb.MatchSet_ALL, Name: set}
			out = append(out, &apipb.Statement{Name: fmt.Sprintf("%s-%d", name, j), Conditions: c, Actions: actions})
		}
	}
	// Implicit deny at the end of a route-map.
	out = append(out, &apipb.Statement{Name: nset + "-deny", Conditions: base(), Actions: &apipb.Actions{RouteAction: apipb.RouteAction_REJECT}})
	return out, nil
}

func entryActions(e *network.RouteMapEntry) *apipb.Actions {
	if e.Deny {
		return &apipb.Actions{RouteAction: apipb.RouteAction_REJECT}
	}
	a := &apipb.Actions{RouteAction: apipb.RouteAction_ACCEPT}
	if e.SetLocalPreference != nil {
		a.LocalPref = &apipb.LocalPrefAction{Value: *e.SetLocalPreference}
	}
	if len(e.SetCommunities) > 0 {
		typ := apipb.CommunityAction_REPLACE
		if e.SetCommunityAdditive {
			typ = apipb.CommunityAction_ADD
		}
		a.Community = &apipb.CommunityAction{Type: typ, Communities: e.SetCommunities}
	}
	return a
}

// communitySets returns one community set per entry of a standard
// community-list; a route matches the list when it carries all
// communities of one entry. Deny entries are not implemented.
func (p *bgpPolicy) communitySets(name string, lists []network.CommunityList) ([]string, error) {
	i := slices.IndexFunc(lists, func(l network.CommunityList) bool { return l.Name == name })
	if i < 0 {
		return nil, fmt.Errorf("community-list %q is not defined", name)
	}
	var names []string
	for j, e := range lists[i].Entries {
		if e.Deny {
			return nil, fmt.Errorf("community-list %q: deny entries are not implemented", name)
		}
		set := fmt.Sprintf("cl-%s-%d", name, j)
		if !slices.ContainsFunc(p.sets, func(s *apipb.DefinedSet) bool { return s.Name == set }) {
			p.sets = append(p.sets, &apipb.DefinedSet{DefinedType: apipb.DefinedType_COMMUNITY, Name: set, List: e.Communities})
		}
		names = append(names, set)
	}
	return names, nil
}

// requirePolicy rejects, like FRR "bgp ebgp-requires-policy"
// (bgpd/bgp_route.c, bgp_inbound_policy_exists and
// bgp_outbound_policy_exists), the routes of an eBGP peer in each family
// and direction that has no route-map. Route-maps are the only policy
// BOOTy has.
func (p *bgpPolicy) requirePolicy(peers []policyPeer) {
	type key struct{ dir, fam string }
	members := map[key][]string{}
	for _, peer := range peers {
		if !peer.requirePolicy {
			continue
		}
		for _, fam := range peer.families {
			i := slices.IndexFunc(peer.routeMaps, func(rm network.SessionRouteMaps) bool { return rm.Family == fam })
			for _, dir := range []string{"in", "out"} {
				if i >= 0 && (dir == "in" && peer.routeMaps[i].In != "" || dir == "out" && peer.routeMaps[i].Out != "") {
					continue
				}
				k := key{dir, fam}
				members[k] = append(members[k], hostPrefix(peer.addr))
				p.rejected = append(p.rejected, policyReject{peer.name, dir, fam})
			}
		}
	}
	keys := make([]key, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int { return strings.Compare(a.dir+a.fam, b.dir+b.fam) })
	for _, k := range keys {
		name := fmt.Sprintf("ebgp-requires-policy-%s-%s", k.dir, k.fam)
		p.sets = append(p.sets, &apipb.DefinedSet{DefinedType: apipb.DefinedType_NEIGHBOR, Name: name, List: members[k]})
		st := &apipb.Statement{
			Name: name,
			Conditions: &apipb.Conditions{
				NeighborSet: &apipb.MatchSet{Type: apipb.MatchSet_ANY, Name: name},
				AfiSafiIn:   []*apipb.Family{familyOf(k.fam)},
			},
			Actions: &apipb.Actions{RouteAction: apipb.RouteAction_REJECT},
		}
		if k.dir == "in" {
			p.im = append(p.im, st)
		} else {
			p.ex = append(p.ex, st)
		}
	}
}

// localASChecks rejects paths that carry the local-as more often than
// allowas-in permits. FRR checks both the router AS and the local-as of
// "local-as N no-prepend replace-as" (bgpd/bgp_route.c, aspath_loop_check
// on bgp->as and on change_local_as). GoBGP 3.37, which BOOTy uses, checks
// only the router AS: peer.go passes peerInfo.LocalAS, which NewPeerInfo
// sets to the global AS. GoBGP master checks only the per-peer local AS
// (conf.Config.LocalAs) instead; after an upgrade to it the router AS check
// would be the one to add here.
func (p *bgpPolicy) localASChecks(peers []policyPeer) {
	type key struct {
		as    uint32
		count int
	}
	groups := map[key][]string{}
	for _, peer := range peers {
		if peer.localAS != 0 {
			k := key{peer.localAS, peer.allowASIn}
			groups[k] = append(groups[k], hostPrefix(peer.addr))
		}
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int { return cmp.Or(cmp.Compare(a.as, b.as), cmp.Compare(a.count, b.count)) })
	for _, k := range keys {
		name := fmt.Sprintf("local-as-%d-%d", k.as, k.count)
		p.sets = append(p.sets,
			&apipb.DefinedSet{DefinedType: apipb.DefinedType_NEIGHBOR, Name: name, List: groups[k]},
			&apipb.DefinedSet{DefinedType: apipb.DefinedType_AS_PATH, Name: name, List: []string{asOccurrences(k.as, k.count+1)}})
		p.im = append(p.im, &apipb.Statement{
			Name: name,
			Conditions: &apipb.Conditions{
				NeighborSet: &apipb.MatchSet{Type: apipb.MatchSet_ANY, Name: name},
				AsPathSet:   &apipb.MatchSet{Type: apipb.MatchSet_ANY, Name: name},
			},
			Actions: &apipb.Actions{RouteAction: apipb.RouteAction_REJECT},
		})
	}
}

// asOccurrences is a regular expression for an AS path string ("65500
// 65501 65501") with at least n occurrences of as. It must not contain
// "_", which GoBGP replaces.
func asOccurrences(as uint32, n int) string {
	a := strconv.FormatUint(uint64(as), 10)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = a
	}
	return "^(.* )?" + strings.Join(parts, "( .* | )") + "( .*)?$"
}

func hostPrefix(a netip.Addr) string {
	return netip.PrefixFrom(a.WithZone(""), a.BitLen()).String()
}

func familyOf(fam string) *apipb.Family {
	switch fam {
	case "ipv6-unicast":
		return &apipb.Family{Afi: apipb.Family_AFI_IP6, Safi: apipb.Family_SAFI_UNICAST}
	case "l2vpn-evpn":
		return &apipb.Family{Afi: apipb.Family_AFI_L2VPN, Safi: apipb.Family_SAFI_EVPN}
	}
	return &apipb.Family{Afi: apipb.Family_AFI_IP, Safi: apipb.Family_SAFI_UNICAST}
}

// install adds the defined sets and the two policies and assigns them as
// the global import and export policies. Routes without a statement are
// accepted, like peers without a route-map in FRR.
func (p *bgpPolicy) install(ctx context.Context, s *server.BgpServer) error {
	for _, set := range p.sets {
		if err := s.AddDefinedSet(ctx, &apipb.AddDefinedSetRequest{DefinedSet: set}); err != nil {
			return fmt.Errorf("add defined set %s: %w", set.Name, err)
		}
	}
	for _, pol := range []struct {
		name  string
		dir   apipb.PolicyDirection
		stmts []*apipb.Statement
	}{{importPolicy, apipb.PolicyDirection_IMPORT, p.im}, {exportPolicy, apipb.PolicyDirection_EXPORT, p.ex}} {
		if len(pol.stmts) == 0 {
			continue
		}
		if err := s.AddPolicy(ctx, &apipb.AddPolicyRequest{Policy: &apipb.Policy{Name: pol.name, Statements: pol.stmts}}); err != nil {
			return fmt.Errorf("add policy %s: %w", pol.name, err)
		}
		if err := s.AddPolicyAssignment(ctx, &apipb.AddPolicyAssignmentRequest{Assignment: &apipb.PolicyAssignment{
			Name:          "global",
			Direction:     pol.dir,
			Policies:      []*apipb.Policy{{Name: pol.name}},
			DefaultAction: apipb.RouteAction_ACCEPT,
		}}); err != nil {
			return fmt.Errorf("assign policy %s: %w", pol.name, err)
		}
	}
	return nil
}

// installPolicies installs the route-maps and the local-as check of the
// peers, then soft resets the peers so that routes received before the
// policies existed are filtered too.
func (u *UnderlayTier) installPolicies(ctx context.Context) error {
	peers := u.policyPeers()
	p, err := buildPolicy(peers, u.cfg.RouteMaps, u.cfg.CommunityLists)
	if err != nil {
		return err
	}
	if len(p.im) == 0 && len(p.ex) == 0 {
		return nil
	}
	if err := p.install(ctx, u.bgp); err != nil {
		return err
	}
	for _, peer := range peers {
		_ = u.bgp.ResetPeer(ctx, &apipb.ResetPeerRequest{Address: peer.name, Soft: true, Direction: apipb.ResetPeerRequest_BOTH})
	}
	for _, r := range p.rejected {
		u.log.Warn("eBGP peer has no route-map, routes rejected like FRR bgp ebgp-requires-policy",
			"peer", r.peer, "family", r.fam, "direction", r.dir)
	}
	u.log.Info("installed BGP route policies", "import_statements", len(p.im), "export_statements", len(p.ex))
	return nil
}

// policyPeers lists the peers with their route-maps: the frr.conf sessions,
// or the native peers with BGP_ROUTE_MAP_IN/OUT in every family.
func (u *UnderlayTier) policyPeers() []policyPeer {
	var out []policyPeer
	add := func(peer policyPeer) {
		a, err := netip.ParseAddr(peer.name)
		if err == nil {
			peer.addr = a
			out = append(out, peer)
		}
	}
	if len(u.cfg.Sessions) > 0 {
		for i := range u.cfg.Sessions {
			s := &u.cfg.Sessions[i]
			name := s.Name
			if s.Interface {
				name = u.ifacePeers[s.Name]
			}
			add(policyPeer{
				name: name, routeMaps: s.RouteMaps, localAS: s.LocalASN, allowASIn: s.AllowASIn,
				requirePolicy: s.EBGPRequiresPolicy && !s.Internal, families: sessionFamilyNames(s),
			})
		}
		return out
	}
	var maps []network.SessionRouteMaps
	if u.cfg.RouteMapIn != "" || u.cfg.RouteMapOut != "" {
		for _, f := range []string{"ipv4-unicast", "ipv6-unicast", "l2vpn-evpn"} {
			maps = append(maps, network.SessionRouteMaps{Family: f, In: u.cfg.RouteMapIn, Out: u.cfg.RouteMapOut})
		}
	}
	all := []string{"ipv4-unicast", "ipv6-unicast", "l2vpn-evpn"}
	for _, name := range u.ifacePeers {
		add(policyPeer{
			name: name, routeMaps: maps, localAS: u.cfg.LocalAS, allowASIn: int(u.cfg.AllowASIn),
			requirePolicy: u.cfg.EBGPRequiresPolicy, families: all,
		})
	}
	ebgp := u.cfg.RemoteASN != 0 && u.cfg.RemoteASN != u.cfg.ASN
	for _, name := range u.cfg.NeighborAddrs {
		add(policyPeer{name: name, routeMaps: maps, allowASIn: int(u.cfg.AllowASIn), requirePolicy: u.cfg.EBGPRequiresPolicy && ebgp, families: all})
	}
	return out
}

func sessionFamilyNames(s *network.BGPSession) []string {
	var out []string
	for f, on := range map[string]bool{"ipv4-unicast": s.IPv4Unicast, "ipv6-unicast": s.IPv6Unicast, "l2vpn-evpn": s.L2VPNEVPN} {
		if on {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return out
}
