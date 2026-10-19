package netplan

import (
	"bytes"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frrconf"
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
 neighbor fabric peer-group
 neighbor fabric remote-as external
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
			name: "top-level vni after a block closed by bang",
			conf: "router bgp 65100\n address-family l2vpn evpn\n  advertise-all-vni\n exit-address-family\n!\nvni 1000\n",
			want: []uint32{1000},
		},
		{
			name: "duplicate vni statements",
			conf: "vni 1000\nvni 1000\n",
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

// The bm4x-operator production template writes block lines without
// indentation. FRR reads them, so the parser must find every uplink, the
// local-as and the router-id there too.
func TestParseFRRConfigBytes_UnindentedBlockLines(t *testing.T) {
	cfg := `router bgp 4210000017 vrf Vrf_underlay
bgp router-id 192.0.2.17
neighbor leaf peer-group
neighbor leaf remote-as 65500
neighbor leaf local-as 65501 no-prepend replace-as
neighbor ens1f0np0 interface peer-group leaf
neighbor ens1f1np1 interface peer-group leaf
neighbor 192.0.2.1 remote-as 65500
!
address-family l2vpn evpn
advertise-all-vni
exit-address-family
`
	params, err := ParseFRRConfigBytes([]byte(cfg))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes() = %v", err)
	}
	if params.RouterID != "192.0.2.17" || params.LocalASN != 65501 {
		t.Errorf("RouterID = %q, LocalASN = %d, want 192.0.2.17 and 65501", params.RouterID, params.LocalASN)
	}
	if want := []string{"ens1f0np0", "ens1f1np1"}; !slices.Equal(params.UnnumberedPeers, want) {
		t.Errorf("UnnumberedPeers = %v, want %v", params.UnnumberedPeers, want)
	}
	if want := []string{"192.0.2.1"}; !slices.Equal(params.NumberedPeers, want) {
		t.Errorf("NumberedPeers = %v, want %v", params.NumberedPeers, want)
	}
	if !params.EVPN || !params.AdvertiseAllVNI {
		t.Errorf("EVPN = %v, AdvertiseAllVNI = %v, want both true", params.EVPN, params.AdvertiseAllVNI)
	}
}

// Statements BOOTy does not implement fail with their line number instead
// of being ignored.
func TestParseFRRConfigBytes_RejectsUnsupported(t *testing.T) {
	for _, conf := range []string{
		"interface vxlan100\n vni 100\nexit\n",
		"vni 1000\nvni abc\n",
		"vni 99999999999\n",
	} {
		if _, err := ParseFRRConfigBytes([]byte(conf)); err == nil {
			t.Errorf("ParseFRRConfigBytes(%q) = nil error, want an error", conf)
		}
	}
}

// FRR_CONFIG_STRICT=false logs each skipped statement with the file and
// the line, and keeps the rest of the configuration.
func TestParseFRRConfigBytesWithLenientWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	params, err := ParseFRRConfigBytesWith([]byte("router bgp 65000\n maximum-paths 8\n neighbor eth0 interface remote-as external\n"), "/deploy/frr.conf", false)
	if err != nil {
		t.Fatalf("ParseFRRConfigBytesWith() = %v", err)
	}
	if params.ASN != 65000 || len(params.UnnumberedPeers) != 1 {
		t.Fatalf("params = %+v, want ASN 65000 and the eth0 peer", params)
	}
	out := buf.String()
	for _, want := range []string{"FRR_CONFIG_STRICT=false", "file=/deploy/frr.conf", "line=2", `"maximum-paths 8"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if _, err := ParseFRRConfigBytesWith([]byte("router bgp 65000\n maximum-paths 8\n"), "/deploy/frr.conf", true); err == nil {
		t.Error("strict mode accepted maximum-paths")
	}
}

// Statements GoBGP mode parses but does not apply yet are logged, one line
// per statement, not dropped silently.
func TestParseFRRConfigWarnsUnapplied(t *testing.T) {
	data, err := os.ReadFile("../frrconf/testdata/bm4x/two-uplinks/frr.conf")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, err := ParseFRRConfigBytes(data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	warned := func(stmt string) bool {
		return strings.Contains(out, `statement="`+stmt+`"`) || strings.Contains(out, "statement="+stmt+" ")
	}
	for _, stmt := range []string{"soft-reconfiguration inbound", "[no] bgp suppress-duplicates"} {
		if !warned(stmt) {
			t.Errorf("no warning for %q in:\n%s", stmt, out)
		}
	}
	// GoBGP mode applies these.
	for _, stmt := range []string{"allowas-in", "bgp bestpath as-path multipath-relax", "route-map", "[no] bgp ebgp-requires-policy"} {
		if warned(stmt) {
			t.Errorf("warning for the applied %q in:\n%s", stmt, out)
		}
	}
}

// The rd and route-targets of a vni block are parsed only and warned.
func TestParseFRRConfigWarnsVNIRD(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, err := ParseFRRConfigBytes([]byte("router bgp 65000\n address-family l2vpn evpn\n  vni 100\n   rd 192.0.2.1:100\n  exit-vni\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "vni rd/route-target") {
		t.Errorf("no warning for the vni rd: %s", buf.String())
	}
}

// Without route-target lines FRR uses the auto route target
// (AS & 0xFFFF):VNI; for the 4-byte ASN 4210000017 that is 32913:3000.
func TestL3VNIAutoRouteTarget4ByteASN(t *testing.T) {
	model := &frrconf.Config{
		VRFVNIs: []frrconf.VRFVNI{{VNI: 3000}},
		Routers: []frrconf.Router{{ASN: 4210000017, VRF: "default"}},
	}
	imp, exp := l3vniRouteTargets(model, 3000)
	if strings.Join(imp, " ") != "32913:3000" || strings.Join(exp, " ") != "32913:3000" {
		t.Fatalf("auto route targets = %v / %v, want 32913:3000", imp, exp)
	}
}

// "enable-resolve-overlay-index" turns on the Type-5 gateway IP resolution.
func TestParseFRRConfigBytes_ResolveOverlayIndex(t *testing.T) {
	conf := "router bgp 65101\n address-family l2vpn evpn\n  advertise-all-vni\n  enable-resolve-overlay-index\n exit-address-family\nexit\n"
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatalf("ParseFRRConfigBytes: %v", err)
	}
	if !params.ResolveOverlayIndex {
		t.Fatal("ResolveOverlayIndex = false, want true")
	}
	cfg := &network.Config{}
	applyEVPNMode(params, cfg)
	if !cfg.EVPNResolveOverlayIndex {
		t.Fatal("EVPNResolveOverlayIndex = false after applyEVPNMode")
	}
}

// The L3VNI RD follows FRR: the VRF's "rd", else its "router bgp"
// instance id, given in frr.conf order from 1 (the bm4x template:
// Vrf_underlay 1, default 2, so <router-id>:2 like the shadow node). A
// VRF without "router bgp" gets the id after the configured instances.
func TestL3VNIRD(t *testing.T) {
	evpn := func(rd string) []frrconf.AddressFamily {
		return []frrconf.AddressFamily{{AFI: "l2vpn", SAFI: "evpn", RD: rd}}
	}
	for _, tt := range []struct {
		name             string
		model            frrconf.Config
		rd               string
		index, instances int
	}{
		{"template", frrconf.Config{VRFVNIs: []frrconf.VRFVNI{{VNI: 3000}}, Routers: []frrconf.Router{{VRF: "Vrf_underlay"}, {VRF: "default"}}}, "", 2, 2},
		{"default first", frrconf.Config{VRFVNIs: []frrconf.VRFVNI{{VNI: 3000}}, Routers: []frrconf.Router{{}, {VRF: "Vrf_underlay"}}}, "", 1, 2},
		{"repeated block", frrconf.Config{VRFVNIs: []frrconf.VRFVNI{{VNI: 3000}}, Routers: []frrconf.Router{{VRF: "Vrf_underlay"}, {VRF: "Vrf_underlay"}, {}}}, "", 2, 2},
		{"explicit rd", frrconf.Config{VRFVNIs: []frrconf.VRFVNI{{VNI: 3000, VRF: "Vrf_p"}}, Routers: []frrconf.Router{{}, {VRF: "Vrf_p", AddressFamilies: evpn("65000:9")}}}, "65000:9", 2, 2},
		{"no router for the VRF", frrconf.Config{VRFVNIs: []frrconf.VRFVNI{{VNI: 3000, VRF: "Vrf_p"}}, Routers: []frrconf.Router{{}}}, "", 2, 2},
		{"L2VNI", frrconf.Config{Routers: []frrconf.Router{{VRF: "Vrf_underlay"}, {}}}, "", 2, 2},
		{"L2VNI without a default VRF instance", frrconf.Config{Routers: []frrconf.Router{{VRF: "Vrf_underlay"}}}, "", 1, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rd, index, instances := l3vniRD(&tt.model, 3000)
			if rd != tt.rd || index != tt.index || instances != tt.instances {
				t.Fatalf("l3vniRD = %q, %d, %d; want %q, %d, %d", rd, index, instances, tt.rd, tt.index, tt.instances)
			}
		})
	}
}

// applyEVPNMode sets the RD ids also in L2 mode: the provision VNI is an
// L2VNI, BOOTy's Type-5 route uses the default VRF instance's id and the
// L2VNI the id after the instances.
func TestApplyEVPNModeL2RD(t *testing.T) {
	frr := &FRRParams{AdvertiseAllVNI: true, Model: &frrconf.Config{
		Routers: []frrconf.Router{{VRF: "Vrf_underlay"}, {}},
	}}
	cfg := &network.Config{ProvisionVNI: 100}
	applyEVPNMode(frr, cfg)
	if !cfg.EVPNL2Enabled || cfg.EVPNType5Only || cfg.EVPNRDIndex != 2 || cfg.BGPInstances != 2 {
		t.Fatalf("L2 mode: L2 %v Type-5 only %v RD index %d instances %d; want true false 2 2",
			cfg.EVPNL2Enabled, cfg.EVPNType5Only, cfg.EVPNRDIndex, cfg.BGPInstances)
	}
}

// The rd of the L3VNI VRF in frr.conf becomes the EVPN RD, unchanged for
// a 4-byte ASN, with the VRF's instance id.
func TestApplyFRRParamsL3VNIRD(t *testing.T) {
	const conf = `vrf provision
 vni 3000
exit-vrf
router bgp 4210000017
 neighbor 192.0.2.1 remote-as external
 address-family l2vpn evpn
  neighbor 192.0.2.1 activate
  advertise-all-vni
 exit-address-family
exit
router bgp 4210000017 vrf provision
 address-family l2vpn evpn
  rd 4210000017:9
  advertise ipv4 unicast
 exit-address-family
exit
`
	params, err := ParseFRRConfigBytes([]byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &network.Config{ProvisionVNI: 3000}
	applyFRRParams(params, cfg)
	if !cfg.EVPNType5Only || cfg.EVPNRD != "4210000017:9" || cfg.EVPNRDIndex != 2 || cfg.BGPInstances != 2 {
		t.Fatalf("Type-5 only %v RD %q index %d instances %d; want true 4210000017:9 2 2",
			cfg.EVPNType5Only, cfg.EVPNRD, cfg.EVPNRDIndex, cfg.BGPInstances)
	}
}
