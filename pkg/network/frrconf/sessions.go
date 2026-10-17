package frrconf

import (
	"fmt"
)

// Address families of a session, in the order FRR shows them.
const (
	FamilyIPv4Unicast = "ipv4-unicast"
	FamilyIPv6Unicast = "ipv6-unicast"
	FamilyL2VPNEVPN   = "l2vpn-evpn"
)

// Session is one BGP peer with every attribute resolved: the peer value,
// else its peer-group value, else the default of the "frr defaults"
// profile (FRR tests/bgpd/test_peer_attr.c: a peer override wins over the
// group).
type Session struct {
	// Name is the peer address or, with Interface, the interface name.
	Name      string
	Interface bool
	// VRF is the VRF of the "router bgp" block; "" is the default VRF.
	VRF string
	// Internal is true for iBGP ("remote-as internal" or the own ASN).
	Internal bool
	// RemoteASN is the peer AS. 0 with Internal false is
	// "remote-as external": any AS other than the own one.
	RemoteASN uint32
	// LocalAS is "local-as N no-prepend replace-as", nil when unset.
	LocalAS *LocalAS
	// KeepaliveTime, HoldTime and ConnectRetry are in seconds.
	KeepaliveTime uint32
	HoldTime      uint32
	ConnectRetry  uint32
	// ConnectRetrySet is true when the peer or its group sets
	// "timers connect"; else ConnectRetry is the profile default.
	ConnectRetrySet bool
	// Families are the activated address families.
	Families []string
	// AllowASIn is the "allowas-in" count of the activated address
	// families (the largest one), 0 when unset.
	AllowASIn int
	// FastExternalFailover is "bgp fast-external-failover" of the router:
	// on unless "no bgp fast-external-failover" (the FRR default).
	FastExternalFailover bool
	// BFD and BFDProfile request a BFD session.
	BFD        bool
	BFDProfile string
	// BFD timers from the profile, with the FRR defaults for unset
	// values and for an unknown profile (doc/user/bfd.rst).
	BFDTransmitMS uint32
	BFDReceiveMS  uint32
	BFDDetectMult uint32
	Line          int
}

// FRR BFD defaults (bfdd/bfd.h BFD_DEFDESIREDMINTX,
// BFD_DEFREQUIREDMINRX, BFD_DEFDETECTMULT).
const (
	DefaultBFDIntervalMS = 300
	DefaultBFDDetectMult = 3
)

// BFDTimers returns the timers of a BFD profile. Like FRR, an unknown
// profile gives the defaults (bfd_profiles_topo1).
func (c *Config) BFDTimers(profile string) (tx, rx, mult uint32) {
	tx, rx, mult = DefaultBFDIntervalMS, DefaultBFDIntervalMS, DefaultBFDDetectMult
	for i := range c.BFDProfiles {
		p := &c.BFDProfiles[i]
		if p.Name != profile {
			continue
		}
		if p.TransmitInterval != nil {
			tx = *p.TransmitInterval
		}
		if p.ReceiveInterval != nil {
			rx = *p.ReceiveInterval
		}
		if p.DetectMultiplier != nil {
			mult = *p.DetectMultiplier
		}
	}
	return tx, rx, mult
}

// ProfileTimers returns keepalive, hold and connect-retry defaults of an
// "frr defaults" profile in current FRR (bgpd/bgp_vty.c, bgpd/bgpd.h
// BGP_DEFAULT_CONNECT_RETRY; FRR 8.2.2 had a traditional connect retry of 120).
// Without the line the daemon profile applies. The legacy ramdisk daemons
// file sets frr_profile="datacenter", so that is the default here.
func ProfileTimers(p Profile) (keepalive, hold, connect uint32) {
	if p == ProfileTraditional {
		return 60, 180, 30
	}
	return 3, 9, 10
}

// Sessions resolves every peer of every "router bgp" block. Peer-groups
// are not sessions; their members are.
func (c *Config) Sessions() ([]Session, error) {
	var out []Session
	for i := range c.Routers {
		r := &c.Routers[i]
		for j := range r.Neighbors {
			n := &r.Neighbors[j]
			if n.PeerGroup {
				continue
			}
			s, err := c.session(r, n)
			if err != nil {
				return nil, fmt.Errorf("line %d: neighbor %s: %w", n.Line, n.Name, err)
			}
			out = append(out, s)
		}
	}
	return out, nil
}

func (c *Config) session(r *Router, n *Neighbor) (Session, error) {
	var g *Neighbor
	if n.Group != "" {
		g = findNeighbor(r, n.Group)
	}
	s := Session{Name: n.Name, Interface: n.Interface, Line: n.Line}
	if r.VRF != "default" {
		s.VRF = r.VRF
	}

	la := n.LocalAS
	if la == nil && g != nil {
		la = g.LocalAS
	}
	if la != nil {
		v := *la
		s.LocalAS = &v
	}
	if err := setRemoteAS(&s, n, g, r.ASN); err != nil {
		return s, err
	}
	if err := checkLocalAS(&s, r.ASN); err != nil {
		return s, err
	}

	keepalive, hold, connect := ProfileTimers(c.Defaults)
	s.KeepaliveTime = pick(n.KeepaliveTime, g, func(x *Neighbor) *uint32 { return x.KeepaliveTime }, keepalive)
	s.HoldTime = pick(n.HoldTime, g, func(x *Neighbor) *uint32 { return x.HoldTime }, hold)
	s.ConnectRetry = pick(n.ConnectRetry, g, func(x *Neighbor) *uint32 { return x.ConnectRetry }, connect)
	s.ConnectRetrySet = n.ConnectRetry != nil || g != nil && g.ConnectRetry != nil

	s.BFD, s.BFDProfile = n.BFD, n.BFDProfile
	if g != nil {
		s.BFD = s.BFD || g.BFD
		if s.BFDProfile == "" {
			s.BFDProfile = g.BFDProfile
		}
	}
	if s.BFD {
		s.BFDTransmitMS, s.BFDReceiveMS, s.BFDDetectMult = c.BFDTimers(s.BFDProfile)
	}

	s.Families = families(r, n)
	s.AllowASIn = allowASIn(r, n)
	s.FastExternalFailover = r.FastExternalFailover == nil || *r.FastExternalFailover
	return s, nil
}

// setRemoteAS resolves the remote-as: "internal" and the own ASN are iBGP,
// "external" is any other AS.
func setRemoteAS(s *Session, n, g *Neighbor, ownASN uint32) error {
	ras := n.RemoteAS
	if ras != nil && g != nil && g.RemoteAS != nil && *ras != *g.RemoteAS {
		// FRR rejects a member remote-as that differs from the
		// peer-group's (BGP_ERR_PEER_GROUP_MEMBER,
		// BGP_ERR_PEER_GROUP_AS_MISMATCH). The same value is harmless.
		return fmt.Errorf("remote-as on a member of peer-group %s, which has its own remote-as", g.Name)
	}
	if ras == nil && g != nil {
		ras = g.RemoteAS
	}
	if ras == nil {
		return fmt.Errorf("no remote-as")
	}
	// The session is iBGP when the peer AS is the effective local AS: the
	// local-as when set, else the router AS (FRR 10.3 peer_calc_sort with
	// change_local_as; GoBGP compares PeerAsn and LocalAsn the same way).
	local := ownASN
	if s.LocalAS != nil {
		local = s.LocalAS.ASN
	}
	switch ras.Kind {
	case RemoteASInternal:
		if s.LocalAS != nil {
			// FRR 10.3 accepts this as an asymmetric iBGP session: its OPEN
			// carries the local-as, but it expects the router AS from the
			// peer. GoBGP has one AS per side and can not do that.
			return fmt.Errorf("local-as %d with remote-as internal is not supported: FRR sends the local-as but expects the router AS from the peer, which GoBGP can not do; use remote-as %d for iBGP in AS %d", local, local, local)
		}
		s.Internal, s.RemoteASN = true, local
	case RemoteASExternal:
	default:
		s.RemoteASN = ras.Number
		s.Internal = ras.Number == local
	}
	return nil
}

// checkLocalAS accepts only "local-as N no-prepend replace-as" on eBGP
// sessions. That is the only local-as mode GoBGP has (design C6). FRR
// allows local-as only for eBGP peers.
func checkLocalAS(s *Session, ownASN uint32) error {
	la := s.LocalAS
	if la == nil {
		return nil
	}
	if !la.NoPrepend || !la.ReplaceAS {
		return fmt.Errorf("local-as %d needs no-prepend replace-as; other local-as modes are not supported", la.ASN)
	}
	if la.ASN == ownASN {
		// FRR: BGP_ERR_CANNOT_HAVE_LOCAL_AS_SAME_AS.
		return fmt.Errorf("local-as %d is the router AS", la.ASN)
	}
	return nil
}

func pick(own *uint32, g *Neighbor, get func(*Neighbor) *uint32, def uint32) uint32 {
	if own != nil {
		return *own
	}
	if g != nil {
		if v := get(g); v != nil {
			return *v
		}
	}
	return def
}

// families returns the address families activated for the peer or its
// group. IPv4 unicast is active for every peer unless the router has
// "no bgp default ipv4-unicast" (FRR bgp_default_afi_safi topotest).
func families(r *Router, n *Neighbor) []string {
	active := map[string]bool{FamilyIPv4Unicast: r.DefaultIPv4Unicast == nil || *r.DefaultIPv4Unicast}
	for i := range r.AddressFamilies {
		af := &r.AddressFamilies[i]
		for _, an := range af.Neighbors {
			if an.Activate && (an.Name == n.Name || n.Group != "" && an.Name == n.Group) {
				active[familyName(af)] = true
			}
		}
	}
	var out []string
	for _, f := range []string{FamilyIPv4Unicast, FamilyIPv6Unicast, FamilyL2VPNEVPN} {
		if active[f] {
			out = append(out, f)
		}
	}
	return out
}

// allowASIn returns the largest "allowas-in" count set for the peer or
// its group in an address family. GoBGP has one count per peer.
func allowASIn(r *Router, n *Neighbor) int {
	count := 0
	for i := range r.AddressFamilies {
		for _, an := range r.AddressFamilies[i].Neighbors {
			if an.Name == n.Name || n.Group != "" && an.Name == n.Group {
				count = max(count, an.AllowASIn)
			}
		}
	}
	return count
}

func familyName(af *AddressFamily) string {
	if af.AFI == "l2vpn" {
		return FamilyL2VPNEVPN
	}
	return af.AFI + "-" + af.SAFI
}
