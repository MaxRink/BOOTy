package netplan

import (
	"slices"
	"testing"
)

func TestParseFRRConfigBytes_Basic(t *testing.T) {
	t.Helper()
	conf := `
frr defaults datacenter
!
router bgp 65101
 bgp router-id 192.168.4.11
 no bgp default ipv4-unicast
 neighbor fabric peer-group
 neighbor fabric remote-as external
 neighbor eth0 interface peer-group fabric
 !
 address-family ipv4 unicast
  neighbor fabric activate
  redistribute connected
 exit-address-family
 !
 address-family l2vpn evpn
  neighbor fabric activate
  advertise-all-vni
 exit-address-family
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}

	if params.ASN != 65101 {
		t.Errorf("ASN = %d, want 65101", params.ASN)
	}
	if params.RouterID != "192.168.4.11" {
		t.Errorf("RouterID = %q, want 192.168.4.11", params.RouterID)
	}
	if len(params.UnnumberedPeers) != 1 || params.UnnumberedPeers[0] != "eth0" {
		t.Errorf("UnnumberedPeers = %v, want [eth0]", params.UnnumberedPeers)
	}
	if !params.EVPN {
		t.Error("EVPN should be true")
	}
	if !params.AdvertiseAllVNI {
		t.Error("AdvertiseAllVNI should be true")
	}
}

func TestParseFRRConfigBytes_MultipleInterfaces(t *testing.T) {
	t.Helper()
	conf := `
router bgp 65100
 bgp router-id 10.0.0.1
 neighbor eth1 interface remote-as external
 neighbor eth2 interface remote-as external
 neighbor eth3 interface remote-as external
 !
 address-family l2vpn evpn
  advertise-all-vni
 exit-address-family
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}

	if params.ASN != 65100 {
		t.Errorf("ASN = %d, want 65100", params.ASN)
	}
	if len(params.UnnumberedPeers) != 3 {
		t.Errorf("UnnumberedPeers count = %d, want 3", len(params.UnnumberedPeers))
	}
}

func TestParseFRRConfigBytes_NumberedPeers(t *testing.T) {
	t.Helper()
	conf := `
router bgp 65200
 bgp router-id 10.0.0.2
 neighbor 10.0.0.1 remote-as 65100
 neighbor 10.0.0.3 remote-as 65100
 !
 address-family ipv4 unicast
  neighbor 10.0.0.1 activate
 exit-address-family
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}

	if len(params.NumberedPeers) != 2 {
		t.Errorf("NumberedPeers = %v, want 2 entries", params.NumberedPeers)
	}
	if params.EVPN {
		t.Error("EVPN should be false — no l2vpn evpn in config")
	}
}

func TestParseFRRConfigBytes_NoEVPN(t *testing.T) {
	t.Helper()
	conf := `
router bgp 65001
 bgp router-id 10.0.0.5
 neighbor eth0 interface remote-as external
 !
 address-family ipv4 unicast
  neighbor eth0 activate
 exit-address-family
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}

	if params.EVPN {
		t.Error("EVPN should be false")
	}
	if params.AdvertiseAllVNI {
		t.Error("AdvertiseAllVNI should be false")
	}
}

func TestParseFRRConfigBytes_PureType5EVPN(t *testing.T) {
	t.Helper()
	conf := `
router bgp 65100
 bgp router-id 192.168.4.10
 neighbor hbn interface remote-as external
 !
 address-family ipv4 unicast
  network 192.168.4.10/32
  neighbor hbn activate
 exit-address-family
 !
 address-family l2vpn evpn
  neighbor hbn activate
  advertise ipv4 unicast
  route-target import 65000:1000
  route-target export 65000:1000
 exit-address-family
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}

	if !params.EVPN {
		t.Error("EVPN should be true for Type-5 l2vpn evpn config")
	}
	if params.AdvertiseAllVNI {
		t.Error("AdvertiseAllVNI should be false for pure Type-5 config")
	}
	if len(params.UnnumberedPeers) != 1 || params.UnnumberedPeers[0] != "hbn" {
		t.Errorf("UnnumberedPeers = %v, want [hbn]", params.UnnumberedPeers)
	}
}

func TestParseFRRConfigBytes_SkipsLinkLocal(t *testing.T) {
	t.Helper()
	conf := `
router bgp 65101
 bgp router-id 10.0.0.1
 neighbor 169.254.100.100 remote-as 65170
 neighbor 10.0.0.2 remote-as 65100
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}

	// 169.254.x.x should be filtered out.
	if len(params.NumberedPeers) != 1 {
		t.Errorf("NumberedPeers = %v, want [10.0.0.2]", params.NumberedPeers)
	}
	if len(params.NumberedPeers) > 0 && params.NumberedPeers[0] != "10.0.0.2" {
		t.Errorf("NumberedPeers[0] = %q, want 10.0.0.2", params.NumberedPeers[0])
	}
}

func TestParseFRRConfigBytes_DuplicateNeighbors(t *testing.T) {
	t.Helper()
	// Same interface appears multiple times (e.g. in different address families).
	conf := `
router bgp 65100
 neighbor eth0 interface remote-as external
 neighbor eth0 interface peer-group fabric
 !
 address-family l2vpn evpn
  neighbor eth0 activate
  advertise-all-vni
 exit-address-family
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}

	// Should deduplicate.
	if len(params.UnnumberedPeers) != 1 {
		t.Errorf("UnnumberedPeers = %v, want [eth0] (deduplicated)", params.UnnumberedPeers)
	}
}

func TestParseFRRConfigBytes_Empty(t *testing.T) {
	t.Helper()
	params, err := ParseFRRConfigBytes([]byte(""))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}
	if params.ASN != 0 {
		t.Errorf("ASN = %d, want 0", params.ASN)
	}
}

func TestExtractL3VNIs(t *testing.T) {
	tests := []struct {
		name string
		conf string
		want []uint32
	}{
		{
			name: "default VRF L3VNI as generated by CAPRF",
			conf: "frr version 9.1\nfrr defaults datacenter\n!\nvni 1000\n!\nrouter bgp 65100\n address-family l2vpn evpn\n  advertise-all-vni\n  advertise ipv4 unicast\n exit-address-family\nexit\n",
			want: []uint32{1000},
		},
		{
			name: "VRF block L3VNI",
			conf: "vrf cluster\n vni 4000\nexit-vrf\n!\nrouter bgp 65100\nexit\n",
			want: []uint32{4000},
		},
		{
			name: "L2VNI block under router bgp is not an L3VNI",
			conf: "router bgp 65100\n address-family l2vpn evpn\n  advertise-all-vni\n  vni 100\n   rd 1:100\n  exit-vni\n exit-address-family\nexit\n",
			want: nil,
		},
		{
			name: "VRF block L3VNI with prefix-routes-only",
			conf: "vrf cluster\n vni 4000 prefix-routes-only\nexit-vrf\n",
			want: []uint32{4000},
		},
		{
			name: "unindented L2VNI under router bgp is not an L3VNI",
			conf: "router bgp 65100\naddress-family l2vpn evpn\nadvertise-all-vni\nvni 100\nexit-vni\nexit-address-family\nexit\n",
			want: nil,
		},
		{
			name: "vni under a non-VRF block is not an L3VNI",
			conf: "interface vxlan100\n vni 100\nexit\n",
			want: nil,
		},
		{
			name: "top-level vni after a block closed by bang",
			conf: "router bgp 65100\n address-family l2vpn evpn\n  advertise-all-vni\n exit-address-family\n!\nvni 1000\n",
			want: []uint32{1000},
		},
		{
			name: "duplicate and invalid vni statements",
			conf: "vni 1000\nvni 1000\nvni abc\nvni 99999999999\n",
			want: []uint32{1000},
		},
		{
			name: "no vni statements",
			conf: "router bgp 65100\nexit\n",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params, err := ParseFRRConfigBytes([]byte(tt.conf))
			if err != nil {
				t.Fatalf("ParseFRRConfigBytes: %v", err)
			}
			if !slices.Equal(params.L3VNIs, tt.want) {
				t.Fatalf("L3VNIs = %v, want %v", params.L3VNIs, tt.want)
			}
		})
	}
}
