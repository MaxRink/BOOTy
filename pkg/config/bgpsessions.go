package config

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frrconf"
)

// BGPSessionConfig is one BGP session or peer-group of the native config,
// the same model as an frr.conf neighbor. A session inherits every unset
// field from its peer-group; its own value wins (FRR
// tests/bgpd/test_peer_attr.c). Unset timers use the "defaults" profile.
type BGPSessionConfig struct {
	// Name is the peer address, the interface name of an unnumbered
	// session, or the peer-group name.
	Name string `yaml:"name" json:"name"`
	// Interface makes the session unnumbered on the interface Name.
	Interface bool `yaml:"interface,omitempty" json:"interface,omitempty"`
	// VRF binds the session to a VRF, like "router bgp N vrf NAME".
	// Empty or "default" is the default VRF.
	VRF string `yaml:"vrf,omitempty" json:"vrf,omitempty"`
	// PeerGroup is the peer-group of a session.
	PeerGroup string `yaml:"peerGroup,omitempty" json:"peerGroup,omitempty"`
	// RemoteAS is a number, "internal" or "external". JSON and YAML take
	// the number with or without quotes.
	RemoteAS RemoteAS `yaml:"remoteAS,omitempty" json:"remoteAS,omitempty"`
	// LocalAS is "local-as N"; LocalASMode must then be
	// "no-prepend-replace-as" (the default), the only mode BOOTy has.
	LocalAS     uint32 `yaml:"localAS,omitempty" json:"localAS,omitempty"`
	LocalASMode string `yaml:"localASMode,omitempty" json:"localASMode,omitempty"`
	// Keepalive, Hold and ConnectRetry are in seconds; unset uses the
	// peer-group, then the defaults profile.
	Keepalive    *uint32 `yaml:"keepalive,omitempty" json:"keepalive,omitempty"`
	Hold         *uint32 `yaml:"hold,omitempty" json:"hold,omitempty"`
	ConnectRetry *uint32 `yaml:"connectRetry,omitempty" json:"connectRetry,omitempty"`
	// Families are the activated address families: ipv4-unicast,
	// ipv6-unicast, l2vpn-evpn. Families of the peer-group add to them.
	Families []string `yaml:"families,omitempty" json:"families,omitempty"`
	// AllowASIn accepts paths with the own AS up to this many times in each
	// of Families, like "neighbor X allowas-in N" (1-10; FRR's default
	// without N is 3). 0 is unset.
	AllowASIn int `yaml:"allowASIn,omitempty" json:"allowASIn,omitempty"`
	// RouteMaps are the import (in) and export (out) route-maps per address
	// family, like "neighbor X route-map M in|out" in that family. They name
	// network.bgp.routeMaps or frr.conf route-maps.
	RouteMaps []network.SessionRouteMaps `yaml:"routeMaps,omitempty" json:"routeMaps,omitempty"`
	// BFD and BFDProfile request a BFD session ("neighbor X bfd [profile P]").
	BFD        bool   `yaml:"bfd,omitempty" json:"bfd,omitempty"`
	BFDProfile string `yaml:"bfdProfile,omitempty" json:"bfdProfile,omitempty"`
}

// RemoteAS is a remote-as value: a number, "internal" or "external".
type RemoteAS string

// UnmarshalJSON accepts a JSON number as well as a string.
func (r *RemoteAS) UnmarshalJSON(b []byte) error {
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*r = RemoteAS(n.String())
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("remoteAS must be a number or a string: %w", err)
	}
	*r = RemoteAS(s)
	return nil
}

// UnmarshalYAML accepts an unquoted number as well as a string.
func (r *RemoteAS) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: remoteAS must be a number or a string", n.Line)
	}
	*r = RemoteAS(n.Value)
	return nil
}

// BFDProfileConfig is a BFD profile, like the frr.conf "bfd / profile
// NAME" block. Unset values use the FRR defaults (300 ms, 300 ms, 3).
type BFDProfileConfig struct {
	Name string `yaml:"name" json:"name"`
	// TransmitInterval and ReceiveInterval are in milliseconds, 10-60000.
	TransmitInterval *uint32 `yaml:"transmitInterval,omitempty" json:"transmitInterval,omitempty"`
	ReceiveInterval  *uint32 `yaml:"receiveInterval,omitempty" json:"receiveInterval,omitempty"`
	// DetectMultiplier is 2-255.
	DetectMultiplier *uint32 `yaml:"detectMultiplier,omitempty" json:"detectMultiplier,omitempty"`
}

// localASModeReplace is the only local-as mode: "no-prepend replace-as".
const localASModeReplace = "no-prepend-replace-as"

var sessionFamilies = map[string][2]string{
	frrconf.FamilyIPv4Unicast: {"ipv4", "unicast"},
	frrconf.FamilyIPv6Unicast: {"ipv6", "unicast"},
	frrconf.FamilyL2VPNEVPN:   {"l2vpn", "evpn"},
}

// HasSessions reports whether the native config defines BGP sessions.
func (b *BGPConfig) HasSessions() bool { return len(b.Sessions) > 0 }

// ResolveSessions builds the FRR model of the native sessions and
// peer-groups and resolves them with the frr.conf rules, so both inputs
// give the same sessions.
func (b *BGPConfig) ResolveSessions() ([]frrconf.Session, error) {
	model, err := b.frrModel()
	if err != nil {
		return nil, err
	}
	sessions, err := model.Sessions()
	if err != nil {
		return nil, fmt.Errorf("network.bgp.sessions: %w", err)
	}
	return sessions, nil
}

func (b *BGPConfig) frrModel() (*frrconf.Config, error) {
	model, err := b.emptyModel()
	if err != nil {
		return nil, err
	}
	profiles, err := b.bfdProfiles()
	if err != nil {
		return nil, err
	}
	model.BFDProfiles = profiles
	vrfName := func(vrf string) string { return cmp.Or(vrf, "default") }
	routers := map[string]*frrconf.Router{}
	var vrfs []string
	seen := map[[2]string]bool{}
	router := func(vrf string) *frrconf.Router {
		vrf = vrfName(vrf)
		if r, ok := routers[vrf]; ok {
			return r
		}
		r := &frrconf.Router{ASN: b.ASN, VRF: vrf, DefaultIPv4Unicast: new(false), FastExternalFailover: b.FastExternalFailover, EBGPRequiresPolicy: b.EBGPRequiresPolicy}
		routers[vrf] = r
		vrfs = append(vrfs, vrf)
		return r
	}
	for i := range b.Sessions {
		s := &b.Sessions[i]
		key := [2]string{vrfName(s.VRF), s.Name}
		if seen[key] {
			return nil, fmt.Errorf("network.bgp.sessions %s: defined twice in VRF %s", s.Name, key[0])
		}
		seen[key] = true
		g, err := b.sessionGroup(s, key[0], vrfName)
		if err != nil {
			return nil, err
		}
		r := router(s.VRF)
		if g >= 0 && slices.IndexFunc(r.Neighbors, func(n frrconf.Neighbor) bool { return n.Name == s.PeerGroup }) < 0 {
			if err := addNeighbor(r, &b.PeerGroups[g], true); err != nil {
				return nil, err
			}
		}
		if err := addNeighbor(r, s, false); err != nil {
			return nil, err
		}
	}
	for _, vrf := range vrfs {
		model.Routers = append(model.Routers, *routers[vrf])
	}
	return model, nil
}

// emptyModel returns the model with the defaults profile, after checking
// the peer-groups.
func (b *BGPConfig) emptyModel() (*frrconf.Config, error) {
	model := &frrconf.Config{Defaults: frrconf.ProfileDatacenter}
	switch b.Defaults {
	case "", string(frrconf.ProfileDatacenter):
	case string(frrconf.ProfileTraditional):
		model.Defaults = frrconf.ProfileTraditional
	default:
		return nil, fmt.Errorf("network.bgp.defaults %q must be traditional or datacenter", b.Defaults)
	}
	for i := range b.PeerGroups {
		if g := &b.PeerGroups[i]; g.Interface || g.PeerGroup != "" {
			return nil, fmt.Errorf("network.bgp.peerGroups %s: interface and peerGroup are session fields, not peer-group fields", g.Name)
		}
	}
	return model, nil
}

// sessionGroup returns the index of the peer-group of s, -1 without one,
// checked against the session's VRF.
func (b *BGPConfig) sessionGroup(s *BGPSessionConfig, vrf string, vrfName func(string) string) (int, error) {
	if s.PeerGroup == "" {
		return -1, nil
	}
	i := slices.IndexFunc(b.PeerGroups, func(g BGPSessionConfig) bool { return g.Name == s.PeerGroup })
	if i < 0 {
		return -1, fmt.Errorf("network.bgp.sessions %s: peer-group %s is not defined", s.Name, s.PeerGroup)
	}
	if gv := b.PeerGroups[i].VRF; gv != "" && vrfName(gv) != vrf {
		return -1, fmt.Errorf("network.bgp.sessions %s: VRF %s differs from the VRF %s of peer-group %s", s.Name, vrf, vrfName(gv), s.PeerGroup)
	}
	return i, nil
}

// BFDModel returns the native BFD profiles in the frr.conf model, checked
// with the frr.conf ranges.
func (b *BGPConfig) BFDModel() (*frrconf.Config, error) {
	profiles, err := b.bfdProfiles()
	if err != nil {
		return nil, err
	}
	return &frrconf.Config{BFDProfiles: profiles}, nil
}

func (b *BGPConfig) bfdProfiles() ([]frrconf.BFDProfile, error) {
	out := make([]frrconf.BFDProfile, 0, len(b.BFDProfiles))
	for _, p := range b.BFDProfiles {
		for _, iv := range []struct {
			name string
			v    *uint32
		}{{"transmit-interval", p.TransmitInterval}, {"receive-interval", p.ReceiveInterval}} {
			if iv.v != nil && (*iv.v < 10 || *iv.v > 60000) {
				return nil, fmt.Errorf("network.bgp.bfdProfiles %s: %s %d out of range 10-60000", p.Name, iv.name, *iv.v)
			}
		}
		if m := p.DetectMultiplier; m != nil && (*m < 2 || *m > 255) {
			return nil, fmt.Errorf("network.bgp.bfdProfiles %s: detect-multiplier %d out of range 2-255", p.Name, *m)
		}
		out = append(out, frrconf.BFDProfile{Name: p.Name, TransmitInterval: p.TransmitInterval, ReceiveInterval: p.ReceiveInterval, DetectMultiplier: p.DetectMultiplier})
	}
	return out, nil
}

// addNeighbor adds a session or peer-group with its address families.
func addNeighbor(r *frrconf.Router, s *BGPSessionConfig, group bool) error {
	what := "network.bgp.sessions " + s.Name
	if group {
		what = "network.bgp.peerGroups " + s.Name
	}
	n := frrconf.Neighbor{
		Name: s.Name, PeerGroup: group, Interface: s.Interface && !group, Group: s.PeerGroup,
		KeepaliveTime: s.Keepalive, HoldTime: s.Hold, ConnectRetry: s.ConnectRetry,
		BFD: s.BFD, BFDProfile: s.BFDProfile,
	}
	if s.RemoteAS != "" {
		ras, err := parseRemoteAS(string(s.RemoteAS))
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		n.RemoteAS = &ras
	}
	if s.LocalAS != 0 {
		if s.LocalASMode != "" && s.LocalASMode != localASModeReplace {
			return fmt.Errorf("%s: localASMode %q: only %s is supported", what, s.LocalASMode, localASModeReplace)
		}
		n.LocalAS = &frrconf.LocalAS{ASN: s.LocalAS, NoPrepend: true, ReplaceAS: true}
	}
	if s.AllowASIn < 0 || s.AllowASIn > 10 {
		return fmt.Errorf("%s: invalid allowas-in count %d, must be 1-10", what, s.AllowASIn)
	}
	r.Neighbors = append(r.Neighbors, n)
	for _, f := range s.Families {
		afi, ok := sessionFamilies[f]
		if !ok {
			return fmt.Errorf("%s: family %q must be ipv4-unicast, ipv6-unicast or l2vpn-evpn", what, f)
		}
		an := afNeighbor(r, afi, s.Name)
		an.Activate, an.AllowASIn = true, s.AllowASIn
	}
	for _, rm := range s.RouteMaps {
		af, ok := sessionFamilies[rm.Family]
		if !ok {
			return fmt.Errorf("%s: route-map family %q must be ipv4-unicast, ipv6-unicast or l2vpn-evpn", what, rm.Family)
		}
		an := afNeighbor(r, af, s.Name)
		an.RouteMapIn, an.RouteMapOut = rm.In, rm.Out
	}
	return nil
}

// afNeighbor returns the neighbor entry of name in the address family,
// adding the family and the entry when they are missing.
func afNeighbor(r *frrconf.Router, afi [2]string, name string) *frrconf.AFNeighbor {
	i := slices.IndexFunc(r.AddressFamilies, func(af frrconf.AddressFamily) bool { return af.AFI == afi[0] && af.SAFI == afi[1] })
	if i < 0 {
		r.AddressFamilies = append(r.AddressFamilies, frrconf.AddressFamily{AFI: afi[0], SAFI: afi[1]})
		i = len(r.AddressFamilies) - 1
	}
	af := &r.AddressFamilies[i]
	j := slices.IndexFunc(af.Neighbors, func(n frrconf.AFNeighbor) bool { return n.Name == name })
	if j < 0 {
		af.Neighbors = append(af.Neighbors, frrconf.AFNeighbor{Name: name})
		j = len(af.Neighbors) - 1
	}
	return &af.Neighbors[j]
}

func parseRemoteAS(v string) (frrconf.RemoteAS, error) {
	switch strings.ToLower(v) {
	case "internal":
		return frrconf.RemoteAS{Kind: frrconf.RemoteASInternal}, nil
	case "external":
		return frrconf.RemoteAS{Kind: frrconf.RemoteASExternal}, nil
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return frrconf.RemoteAS{}, fmt.Errorf("remoteAS %q must be a number, internal or external", v)
	}
	return frrconf.RemoteAS{Kind: frrconf.RemoteASNumber, Number: uint32(n)}, nil
}
