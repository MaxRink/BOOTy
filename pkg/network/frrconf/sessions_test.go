package frrconf

import (
	"reflect"
	"strings"
	"testing"
)

func mustSessions(t *testing.T, conf string) []Session {
	t.Helper()
	cfg, err := Parse([]byte(conf))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	s, err := cfg.Sessions()
	if err != nil {
		t.Fatalf("Sessions() = %v", err)
	}
	return s
}

// The production template: two leaf uplinks and two route reflectors in
// peer-groups (FRR tests/bgpd/test_peer_attr.c: members inherit the group
// attributes).
func TestSessionsPeerGroupInheritance(t *testing.T) {
	got := mustSessions(t, `frr defaults traditional
router bgp 4210000017 vrf Vrf_underlay
no bgp default ipv4-unicast
neighbor dcgw peer-group
neighbor dcgw remote-as internal
neighbor dcgw timers 30 90
neighbor dcgw timers connect 5
neighbor leaf peer-group
neighbor leaf remote-as 65500
neighbor leaf local-as 65501 no-prepend replace-as
neighbor leaf bfd profile fabric
neighbor leaf timers 30 90
neighbor ens1f0np0 interface peer-group leaf
neighbor ens1f1np1 interface peer-group leaf
neighbor 198.51.100.1 peer-group dcgw
neighbor 198.51.100.2 peer-group dcgw
neighbor 198.51.100.2 timers 10 30
!
address-family ipv4 unicast
  neighbor leaf activate
exit-address-family
address-family l2vpn evpn
  neighbor dcgw activate
exit-address-family
`)
	leaf := func(name string, line int) Session {
		return Session{Name: name, Interface: true, VRF: "Vrf_underlay", RemoteASN: 65500,
			LocalAS:       &LocalAS{ASN: 65501, NoPrepend: true, ReplaceAS: true},
			KeepaliveTime: 30, HoldTime: 90, ConnectRetry: 30,
			Families: []string{FamilyIPv4Unicast}, BFD: true, BFDProfile: "fabric", FastExternalFailover: true,
			BFDTransmitMS: 300, BFDReceiveMS: 300, BFDDetectMult: 3, EBGPRequiresPolicy: true, Line: line}
	}
	dcgw := func(name string, keepalive, hold uint32, line int) Session {
		return Session{Name: name, VRF: "Vrf_underlay", Internal: true, RemoteASN: 4210000017,
			KeepaliveTime: keepalive, HoldTime: hold, ConnectRetry: 5, ConnectRetrySet: true,
			Families: []string{FamilyL2VPNEVPN}, FastExternalFailover: true, EBGPRequiresPolicy: true, Line: line}
	}
	want := []Session{
		leaf("ens1f0np0", 13), leaf("ens1f1np1", 14),
		dcgw("198.51.100.1", 30, 90, 15), dcgw("198.51.100.2", 10, 30, 16),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sessions() =\n%+v\nwant\n%+v", got, want)
	}
}

// Without explicit timers the "frr defaults" profile applies; without the
// line the legacy daemons profile (datacenter) applies. IPv4 unicast is
// active unless "no bgp default ipv4-unicast" is set.
func TestSessionsProfileDefaults(t *testing.T) {
	tests := []struct {
		profile             string
		keepalive, hold, cr uint32
	}{
		{"frr defaults traditional\n", 60, 180, 30},
		{"frr defaults datacenter\n", 3, 9, 10},
		{"", 3, 9, 10},
	}
	for _, tt := range tests {
		got := mustSessions(t, tt.profile+"router bgp 65101 vrf default\n neighbor 192.0.2.1 remote-as external\n")
		s := got[0]
		if s.KeepaliveTime != tt.keepalive || s.HoldTime != tt.hold || s.ConnectRetry != tt.cr {
			t.Errorf("%q: timers = %d/%d/%d, want %d/%d/%d", tt.profile, s.KeepaliveTime, s.HoldTime, s.ConnectRetry, tt.keepalive, tt.hold, tt.cr)
		}
		if s.VRF != "" || s.Internal || s.RemoteASN != 0 {
			t.Errorf("%q: VRF %q internal %v remote %d, want default VRF, external", tt.profile, s.VRF, s.Internal, s.RemoteASN)
		}
		if !reflect.DeepEqual(s.Families, []string{FamilyIPv4Unicast}) {
			t.Errorf("%q: families = %v, want ipv4 unicast by default", tt.profile, s.Families)
		}
	}
}

func TestSessionsRejects(t *testing.T) {
	tests := []struct{ name, conf, want string }{
		{"no remote-as", "router bgp 65101\n neighbor g peer-group\n neighbor 192.0.2.1 peer-group g\n", "no remote-as"},
		{"plain local-as", "router bgp 65101\n neighbor 192.0.2.1 remote-as 65500\n neighbor 192.0.2.1 local-as 65501\n", "no-prepend replace-as"},
		{"no-prepend only", "router bgp 65101\n neighbor 192.0.2.1 remote-as 65500\n neighbor 192.0.2.1 local-as 65501 no-prepend\n", "no-prepend replace-as"},
		{"ibgp local-as", "router bgp 65101\n neighbor 192.0.2.1 remote-as internal\n neighbor 192.0.2.1 local-as 65501 no-prepend replace-as\n", "with remote-as internal"},
		{"local-as router AS", "router bgp 65101\n neighbor 192.0.2.1 remote-as 65500\n neighbor 192.0.2.1 local-as 65101 no-prepend replace-as\n", "is the router AS"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.conf))
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if _, err := cfg.Sessions(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Sessions() = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

// remote-as N with local-as N is iBGP with AS N, like FRR 10.3
// (peer_calc_sort uses the local-as) and GoBGP; a remote-as equal to the
// router AS is eBGP from the local-as.
func TestSessionsLocalASDecidesSort(t *testing.T) {
	s := mustSessions(t, "router bgp 65101\n neighbor 192.0.2.1 remote-as 65501\n neighbor 192.0.2.1 local-as 65501 no-prepend replace-as\n")[0]
	if !s.Internal || s.RemoteASN != 65501 || s.LocalAS.ASN != 65501 {
		t.Errorf("session = %+v, want iBGP in AS 65501", s)
	}
	s = mustSessions(t, "router bgp 65101\n neighbor 192.0.2.1 remote-as 65101\n neighbor 192.0.2.1 local-as 65501 no-prepend replace-as\n")[0]
	if s.Internal || s.RemoteASN != 65101 {
		t.Errorf("session = %+v, want eBGP to 65101 from local-as 65501", s)
	}
}

// A numbered remote-as equal to the router AS is iBGP, like FRR.
func TestSessionsNumberedOwnASIsInternal(t *testing.T) {
	s := mustSessions(t, "router bgp 65101\n neighbor 192.0.2.1 remote-as 65101\n")[0]
	if !s.Internal || s.RemoteASN != 65101 {
		t.Fatalf("session = %+v, want iBGP with 65101", s)
	}
}

// "allowas-in" on the group reaches the members; without a count FRR
// uses 3 (FRR bgp_as_allow_in topotest).
func TestSessionsAllowASIn(t *testing.T) {
	got := mustSessions(t, `router bgp 65101
 neighbor leaf peer-group
 neighbor leaf remote-as 65500
 neighbor eth1 interface peer-group leaf
 neighbor 192.0.2.1 remote-as 65500
 address-family ipv4 unicast
  neighbor leaf allowas-in
  neighbor 192.0.2.1 allowas-in 5
 exit-address-family
`)
	if got[0].AllowASIn != 3 || got[1].AllowASIn != 5 {
		t.Fatalf("AllowASIn = %d, %d, want 3 and 5", got[0].AllowASIn, got[1].AllowASIn)
	}
}

// Mirrors FRR bfd_profiles_topo1 test_bfd_profile_values: the profile
// values apply, unset values and unknown profiles give the defaults.
func TestSessionsBFDProfileTimers(t *testing.T) {
	cfg, err := Parse([]byte(`router bgp 65001
 neighbor a peer-group
 neighbor a remote-as external
 neighbor a bfd profile fast
 neighbor eth1 interface peer-group a
 neighbor eth2 interface remote-as external
 neighbor eth2 bfd profile half
 neighbor eth3 interface remote-as external
 neighbor eth3 bfd profile missing
 neighbor eth4 interface remote-as external
 neighbor eth4 bfd
 neighbor eth5 interface remote-as external
exit
bfd
 profile fast
  transmit-interval 150
  receive-interval 200
  detect-multiplier 5
 exit
 profile half
  receive-interval 100
 exit
exit
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	sessions, err := cfg.Sessions()
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := map[string][4]uint32{ // BFD on, tx, rx, multiplier
		"eth1": {1, 150, 200, 5},
		"eth2": {1, 300, 100, 3},
		"eth3": {1, 300, 300, 3},
		"eth4": {1, 300, 300, 3},
		"eth5": {0, 0, 0, 0},
	}
	for _, s := range sessions {
		on := uint32(0)
		if s.BFD {
			on = 1
		}
		if got := [4]uint32{on, s.BFDTransmitMS, s.BFDReceiveMS, s.BFDDetectMult}; got != want[s.Name] {
			t.Errorf("%s: BFD %v, want %v", s.Name, got, want[s.Name])
		}
	}
}

// "bgp ebgp-requires-policy" is on unless the router says "no", like
// current FRR.
// "bgp ebgp-requires-policy" follows "frr defaults" (FRR 10.3
// BGP_EBGP_REQUIRES_POLICY): on for traditional, off for datacenter and
// without the line (the legacy daemons profile); an explicit line wins.
func TestSessionsEBGPRequiresPolicy(t *testing.T) {
	const peer = "router bgp 65101\n neighbor 192.0.2.1 remote-as 65500\n"
	for _, tt := range []struct {
		conf string
		want bool
	}{
		{"frr defaults traditional\n" + peer, true},
		{"frr defaults datacenter\n" + peer, false},
		{peer, false},
		{"frr defaults traditional\nrouter bgp 65101\n no bgp ebgp-requires-policy\n neighbor 192.0.2.1 remote-as 65500\n", false},
		{"frr defaults datacenter\nrouter bgp 65101\n bgp ebgp-requires-policy\n neighbor 192.0.2.1 remote-as 65500\n", true},
	} {
		if s := mustSessions(t, tt.conf)[0]; s.EBGPRequiresPolicy != tt.want {
			t.Errorf("%q: ebgp-requires-policy %v, want %v", tt.conf, s.EBGPRequiresPolicy, tt.want)
		}
	}
}

// A route-map of the peer wins over the one of its peer-group in the same
// direction; the other direction still comes from the group.
func TestSessionsPeerRouteMapOverridesGroup(t *testing.T) {
	s := mustSessions(t, `router bgp 65101
no bgp default ipv4-unicast
neighbor leaf peer-group
neighbor leaf remote-as 65500
neighbor 192.0.2.1 peer-group leaf
address-family ipv4 unicast
  neighbor leaf activate
  neighbor leaf route-map GROUP-IN in
  neighbor leaf route-map GROUP-OUT out
  neighbor 192.0.2.1 route-map PEER-IN in
exit-address-family
`)[0]
	want := []SessionRouteMaps{{Family: FamilyIPv4Unicast, In: "PEER-IN", Out: "GROUP-OUT"}}
	if !reflect.DeepEqual(s.RouteMaps, want) {
		t.Fatalf("route-maps = %+v, want %+v", s.RouteMaps, want)
	}
}
