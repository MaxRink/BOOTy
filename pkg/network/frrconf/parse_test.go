package frrconf

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// testdata/bm4x holds the bm4x-operator provisioning templates
// (cit/t-caas/bm4x-operator b90274c,
// internal/controller/infra/redfish/provcfg/templates) rendered with
// documentation addresses (RFC 5737, RFC 3849). testdata/caprf holds the
// CAPRF e2e variants from test/e2e/lib.sh generate_evpn_provisioner_config.
func goldenInputs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("testdata/*/*/frr.conf")
	if err != nil || len(files) == 0 {
		t.Fatalf("no golden inputs: %v", err)
	}
	return files
}

func TestParseGolden(t *testing.T) {
	for _, in := range goldenInputs(t) {
		t.Run(filepath.Dir(in), func(t *testing.T) {
			data, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			got, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join(filepath.Dir(in), "frr.golden.json")
			if *update {
				if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden file (run with -update): %v", err)
			}
			if strings.TrimSpace(string(got)) != strings.TrimSpace(string(want)) {
				t.Fatalf("model differs from %s (run with -update and review the diff):\n%s", golden, got)
			}
		})
	}
}

// Every statement of every production template fixture must matter:
// removing any single line changes the model or makes the config invalid.
// A line whose removal changes nothing would be a statement that BOOTy
// silently drops.
func TestParseMutationRemoveLine(t *testing.T) {
	files, err := filepath.Glob("testdata/bm4x/*/frr.conf")
	if err != nil || len(files) == 0 {
		t.Fatalf("no bm4x fixtures (%v)", err)
	}
	for _, file := range files {
		t.Run(filepath.Base(filepath.Dir(file)), func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			base, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				fields := strings.Fields(line)
				if len(fields) == 0 || strings.HasPrefix(fields[0], "!") || ignoredWhenRemoved(fields) {
					continue
				}
				mutated := strings.Join(append(append([]string{}, lines[:i]...), lines[i+1:]...), "\n")
				cfg, err := Parse([]byte(mutated))
				if err == nil && reflect.DeepEqual(stripLines(cfg), stripLines(base)) {
					t.Errorf("removing line %d %q changes nothing", i+1, strings.TrimSpace(line))
				}
			}
		})
	}
}

// ignoredWhenRemoved lists statements whose only effect is on FRR logging
// or on the block structure, so removing them does not change the model.
func ignoredWhenRemoved(f []string) bool {
	switch {
	case f[0] == "log", is(f, "service", "integrated-vtysh-config"), is(f, "exit"), is(f, "exit-address-family"):
		return true
	case len(f) == 3 && f[0] == "neighbor" && f[2] == "bfd":
		// The template also has "neighbor X bfd profile P", which enables
		// BFD as well (doc/user/bfd.rst, "neighbor <A.B.C.D|X:X::X:X|WORD>
		// bfd profile BFDPROF").
		return true
	}
	return false
}

// stripLines removes line numbers, which change with every removed line.
func stripLines(cfg *Config) *Config {
	data, _ := json.Marshal(cfg)
	var out Config
	_ = json.Unmarshal(data, &out)
	clearLines(reflect.ValueOf(&out).Elem())
	return &out
}

func clearLines(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).Name == "Line" {
				v.Field(i).SetInt(0)
				continue
			}
			clearLines(v.Field(i))
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			clearLines(v.Index(i))
		}
	case reflect.Pointer:
		if !v.IsNil() {
			clearLines(v.Elem())
		}
	}
}

// Statements outside the supported set fail with their line number, also
// statements FRR knows (FRR would log an error and go on).
func TestParseRejectsUnsupportedStatements(t *testing.T) {
	data, err := os.ReadFile("testdata/bm4x/two-uplinks/frr.conf")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	afterAF := 0
	for i, l := range lines {
		if strings.TrimSpace(l) == "address-family ipv4 unicast" {
			afterAF = i + 1
		}
	}
	for _, stmt := range []string{
		"neighbor leaf password secret",
		"neighbor leaf ebgp-multihop 2",
		"neighbor leaf local-as 65501 replace-as",
		"neighbor leaf local-as 65501 dual-as",
		"neighbor unknown activate",
		"bgp graceful-restart",
		"set metric 10",
		"ip route 192.0.2.0/24 192.0.2.1",
		"ipv6 route 192.0.2.0/24 blackhole",
		"echo-mode",
		"route-target import 65169",
		"frr defaults cloud",
		"address-family ipv4 multicast",
		"garbage",
	} {
		t.Run(stmt, func(t *testing.T) {
			mutated := append(append(append([]string{}, lines[:afterAF]...), "  "+stmt), lines[afterAF:]...)
			_, err := Parse([]byte(strings.Join(mutated, "\n")))
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Parse() = %v, want a ParseError", err)
			}
			if pe.Line != afterAF+1 {
				t.Fatalf("error line = %d, want %d (%v)", pe.Line, afterAF+1, err)
			}
		})
	}
}

// FRR ignores indentation and retries a statement in the parent nodes
// (lib/command.c, command_config_read_one_line).
func TestParseFRRNodeRules(t *testing.T) {
	cfg, err := Parse([]byte(`
router bgp 65000 vrf Vrf_underlay
neighbor leaf peer-group
 address-family ipv4 unicast
  network 192.0.2.1/32
 neighbor leaf remote-as 65500
neighbor eth0 interface peer-group leaf
address-family l2vpn evpn
advertise-all-vni
route-map RM permit 10
set local-preference 90
route-map RM deny 5
vrf red
 vni 100 prefix-routes-only
exit-vrf
vni 200
`))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	r := cfg.Routers[0]
	if r.VRF != "Vrf_underlay" || len(r.Neighbors) != 2 || r.Neighbors[0].RemoteAS == nil || r.Neighbors[1].Group != "leaf" {
		t.Fatalf("router = %+v", r)
	}
	if len(r.AddressFamilies) != 2 || r.AddressFamilies[0].Networks[0] != "192.0.2.1/32" || !r.AddressFamilies[1].AdvertiseAllVNI {
		t.Fatalf("address families = %+v", r.AddressFamilies)
	}
	if e := cfg.RouteMaps[0].Entries; len(e) != 2 || e[0].Seq != 5 || e[1].SetLocalPreference == nil {
		t.Fatalf("route-map entries = %+v, want sorted by sequence", e)
	}
	want := []VRFVNI{{VRF: "red", VNI: 100, PrefixRoutesOnly: true, Line: 14}, {VNI: 200, Line: 16}}
	if !reflect.DeepEqual(cfg.VRFVNIs, want) {
		t.Fatalf("VRFVNIs = %+v, want %+v", cfg.VRFVNIs, want)
	}
}

// Statements without effect on routing are accepted in strict mode and do
// not change the model; "#" lines are comments like "!" lines.
func TestParseNoOpStatements(t *testing.T) {
	base := "router bgp 65000\n neighbor leaf peer-group\n neighbor leaf remote-as external\n"
	want, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		" neighbor leaf description uplink to leaf01",
		" bgp log-neighbor-changes",
		" no bgp log-neighbor-changes",
		"debug bgp updates",
		"log timestamp precision 6",
		"# a comment",
		"   #indented comment with router bgp 1",
	} {
		t.Run(stmt, func(t *testing.T) {
			got, err := Parse([]byte(base + stmt + "\n"))
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			got.Routers[0].Line, want.Routers[0].Line = 0, 0
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("model changed:\n got  %+v\n want %+v", got, want)
			}
		})
	}
	if _, err := Parse([]byte(base + " neighbor other description x\n")); err == nil {
		t.Fatal("description of an undefined neighbor parsed")
	}
}

// A static route inside a vrf block is a route of the VRF table, which
// BOOTy does not install: it is rejected, not moved to the main table.
func TestParseRejectsVRFStaticRoute(t *testing.T) {
	cfg, err := Parse([]byte("vrf red\n ip route 192.0.2.0/24 blackhole\nexit-vrf\n"))
	if err == nil || !strings.Contains(err.Error(), "static routes inside vrf red") {
		t.Fatalf("Parse() = %+v, %v, want the vrf static route error", cfg, err)
	}
}

// The error names the node the parser is in, also after a statement that
// popped to a parent node.
func TestParseErrorNamesTheNode(t *testing.T) {
	for _, tt := range []struct{ conf, want string }{
		{"router bgp 65000\n address-family l2vpn evpn\n  frobnicate 8\n", "(in router bgp 65000 > address-family l2vpn evpn)"},
		{"router bgp 65000\n address-family ipv4 unicast\n exit-address-family\n maximum-paths 8\n", "(in router bgp 65000)"},
		{"route-map RM permit 10\n set metric 5\n", "(in route-map RM permit 10)"},
		{"bgp graceful-restart\n", "(in the top level)"},
		{"router bgp 65000\n neighbor 192.0.2.1 remote-as 99999999999\n", "invalid remote-as \"99999999999\" (in router bgp 65000)"},
	} {
		_, err := Parse([]byte(tt.conf))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Parse(%q) = %v, want %q", tt.conf, err, tt.want)
		}
	}
}

// Lenient mode (FRR_CONFIG_STRICT=false) reports every statement it skips
// with its line and keeps the node, so the next statements still apply.
func TestParseLenient(t *testing.T) {
	var skipped []int
	cfg, err := ParseWith([]byte("router bgp 65000\n address-family ipv4 unicast\n  maximum-paths 8\n  network 192.0.2.0/24\nip prefix-list PL seq 5 permit 192.0.2.0/24\n"),
		Options{Lenient: true, Warn: func(e *ParseError) { skipped = append(skipped, e.Line) }})
	if err != nil {
		t.Fatalf("ParseWith() = %v", err)
	}
	if !reflect.DeepEqual(skipped, []int{3, 5}) {
		t.Fatalf("skipped lines = %v, want [3 5]", skipped)
	}
	if nets := cfg.Routers[0].AddressFamilies[0].Networks; !reflect.DeepEqual(nets, []string{"192.0.2.0/24"}) {
		t.Fatalf("networks = %v: the statement after a skipped one must stay in the address-family", nets)
	}
}

// Lenient mode skips only unsupported statements. An invalid value and an
// unsupported statement that opens a block stay errors: skipping a block
// opener would apply its body to the enclosing node.
func TestParseLenientLimits(t *testing.T) {
	base := "router bgp 65000\n neighbor leaf peer-group\n neighbor leaf remote-as external\n address-family ipv4 unicast\n"
	for _, stmt := range []string{
		" neighbor 192.0.2.1 remote-as 65oo1",
		" bgp router-id 300.1.1.1",
		" network 192.0.2.0/33",
		"router ospf",
		"router bgp 65000 view foo",
		"interface eth0",
		" address-family ipv4 multicast",
		"vrf red blue",
		" neighbor swp2 interface peer-group UNDEF",
	} {
		t.Run(stmt, func(t *testing.T) {
			_, err := ParseWith([]byte(base+stmt+"\n"), Options{Lenient: true})
			var pe *ParseError
			if !errors.As(err, &pe) || pe.Line != 5 {
				t.Fatalf("ParseWith(lenient) = %v, want a ParseError on line 5", err)
			}
		})
	}
}

// A rejected "neighbor IFNAME interface ..." leaves no neighbor behind, and
// a statement that failed in a parent node leaves the parser in its node.
func TestParseLenientKeepsState(t *testing.T) {
	cfg, err := ParseWith([]byte(`router bgp 65000
 neighbor swp1 interface v6only
 address-family ipv4 unicast
  ip route 192.0.2.0/24 192.0.2.1
  network 198.51.100.0/24
`), Options{Lenient: true})
	if err != nil {
		t.Fatalf("ParseWith() = %v", err)
	}
	r := cfg.Routers[0]
	if len(r.Neighbors) != 0 {
		t.Fatalf("neighbors = %+v, want none from the rejected interface statement", r.Neighbors)
	}
	if len(cfg.StaticRoutes) != 0 || !reflect.DeepEqual(r.AddressFamilies[0].Networks, []string{"198.51.100.0/24"}) {
		t.Fatalf("static routes %+v, networks %v", cfg.StaticRoutes, r.AddressFamilies[0].Networks)
	}
}

// A skipped statement moves the parser to the node where FRR applies it
// (lib/command.c node fallback): "neighbor X capability extended-nexthop"
// is a router-node command, so FRR leaves the address-family, and the
// following "neighbor leaf activate" and "advertise-all-vni" do not land in
// the l2vpn evpn address-family either.
func TestParseLenientFollowsFRRNode(t *testing.T) {
	var warned []string
	cfg, err := ParseWith([]byte(`router bgp 65000
 neighbor leaf peer-group
 neighbor leaf remote-as external
 address-family l2vpn evpn
  neighbor leaf capability extended-nexthop
  neighbor leaf activate
  advertise-all-vni
`), Options{Lenient: true, Warn: func(e *ParseError) { warned = append(warned, fmt.Sprintf("%d %v", e.Line, e.Err)) }})
	if err != nil {
		t.Fatalf("ParseWith() = %v", err)
	}
	af := cfg.Routers[0].AddressFamilies[0]
	if len(af.Neighbors) != 0 || af.AdvertiseAllVNI {
		t.Fatalf("l2vpn evpn = %+v, want nothing applied after the router-node statement", af)
	}
	if len(warned) != 3 || !strings.Contains(warned[0], "(in router bgp 65000)") {
		t.Fatalf("warnings = %q, want lines 5-7 skipped, the first in the router node", warned)
	}
}

// A statement that FRR does not know in any node of the stack leaves the
// parser where it is, and every following statement of the same block is
// applied with a warning, because FRR may put it elsewhere. A statement of
// a parent node ends the block.
func TestParseLenientWarnsAfterUnknownStatement(t *testing.T) {
	var warned []int
	_, err := ParseWith([]byte(`router bgp 65000
 address-family ipv4 unicast
  frobnicate now
  network 192.0.2.0/24
  redistribute connected
 exit-address-family
 bgp router-id 192.0.2.1
`), Options{Lenient: true, Warn: func(e *ParseError) { warned = append(warned, e.Line) }})
	if err != nil {
		t.Fatalf("ParseWith() = %v", err)
	}
	if !reflect.DeepEqual(warned, []int{3, 4, 5, 6}) {
		t.Fatalf("warned lines = %v, want [3 4 5 6] (the skipped one and the rest of its block)", warned)
	}
}

// BFD "peer" opens a node in FRR: it stays an error in lenient mode, so its
// body cannot change the profile it would otherwise land in.
func TestParseLenientBFDPeer(t *testing.T) {
	for _, opener := range []string{"peer 192.0.2.1", "bmp targets t1", "l2vpn x type vpls"} {
		_, err := ParseWith([]byte("bfd\n profile fabric\n  transmit-interval 150\n "+opener+"\n  receive-interval 50\n"), Options{Lenient: true})
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Line != 4 {
			t.Errorf("%q: ParseWith(lenient) = %v, want an error on line 4", opener, err)
		}
	}
}

// Invalid input changes nothing: "set community additive" alone does not
// set additive, and "neighbor ADDR remote-as N extra" creates no peer.
func TestParseRejectedStatementsLeaveNoState(t *testing.T) {
	cfg, err := ParseWith([]byte("route-map RM permit 10\n set community additive\nrouter bgp 65000\n neighbor 192.0.2.1 remote-as 65001 extra\n"), Options{Lenient: true, Warn: func(*ParseError) {}})
	if err != nil {
		t.Fatalf("ParseWith() = %v", err)
	}
	if cfg.RouteMaps[0].Entries[0].SetCommunityAdditive {
		t.Error("set community additive without communities set additive")
	}
	if len(cfg.Routers[0].Neighbors) != 0 {
		t.Errorf("neighbors = %+v, want no peer from the rejected statement", cfg.Routers[0].Neighbors)
	}
}

// Without a Warn function, lenient mode still logs every skipped statement.
func TestParseLenientDefaultWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if _, err := ParseWith([]byte("router bgp 65000\n maximum-paths 8\n"), Options{Lenient: true}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "line=2") || !strings.Contains(out, "maximum-paths 8") {
		t.Fatalf("log = %q, want the skipped statement with its line", out)
	}
}

// The l2vpn evpn address-family (BGP_EVPN_NODE) has fewer commands than the
// unicast ones: maximum-paths, redistribute, send-community and the like
// move FRR to the router node, so a following advertise-all-vni is not an
// EVPN statement any more.
func TestParseLenientEVPNNode(t *testing.T) {
	for _, stmt := range []string{
		"bgp graceful-restart", "redistribute connected", "maximum-paths 8",
		"aggregate-address 192.0.2.0/24", "neighbor leaf send-community",
		"neighbor leaf prefix-list PL in",
	} {
		t.Run(stmt, func(t *testing.T) {
			cfg, err := ParseWith([]byte("router bgp 65000\n neighbor leaf peer-group\n neighbor leaf remote-as external\n address-family l2vpn evpn\n  "+stmt+"\n  advertise-all-vni\n"), Options{Lenient: true, Warn: func(*ParseError) {}})
			if err != nil {
				t.Fatalf("ParseWith() = %v", err)
			}
			if cfg.Routers[0].AddressFamilies[0].AdvertiseAllVNI {
				t.Fatal("advertise-all-vni applied to l2vpn evpn after FRR left the address-family")
			}
		})
	}
}

// A skipped statement that FRR knows in the same node keeps the warnings
// of an earlier unknown statement of that block going.
func TestParseLenientUnsureSurvivesKnownSkip(t *testing.T) {
	var warned []int
	_, err := ParseWith([]byte("router bgp 65000\n address-family ipv4 unicast\n  frobnicate now\n  maximum-paths 8\n  network 192.0.2.0/24\n"),
		Options{Lenient: true, Warn: func(e *ParseError) { warned = append(warned, e.Line) }})
	if err != nil {
		t.Fatalf("ParseWith() = %v", err)
	}
	if !reflect.DeepEqual(warned, []int{3, 4, 5}) {
		t.Fatalf("warned lines = %v, want [3 4 5]", warned)
	}
}

// "no" in front of a block opener is still a block opener.
func TestParseLenientNoOpener(t *testing.T) {
	for _, stmt := range []string{"no router ospf", "no vrf red", "no bfd"} {
		_, err := ParseWith([]byte("router bgp 65000\n"+stmt+"\n"), Options{Lenient: true, Warn: func(*ParseError) {}})
		var pe *ParseError
		if !errors.As(err, &pe) || pe.Line != 2 {
			t.Errorf("%q: ParseWith(lenient) = %v, want an error on line 2", stmt, err)
		}
	}
}

// vrf-policy opens a node (vnc), so it stays an error in lenient mode.
func TestParseLenientVRFPolicyOpener(t *testing.T) {
	_, err := ParseWith([]byte("router bgp 65000\n vrf-policy red\n  rd 65000:1\n"), Options{Lenient: true, Warn: func(*ParseError) {}})
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 2 {
		t.Fatalf("ParseWith(lenient) = %v, want an error on line 2", err)
	}
}

// next-hop-self, advertise-svi-ip and advertise-pip are commands of the
// l2vpn evpn address-family in FRR 8.2.2, so skipping them keeps the
// parser there.
func TestParseLenientEVPNNodeKnown(t *testing.T) {
	for _, stmt := range []string{"neighbor leaf next-hop-self", "advertise-svi-ip", "advertise-pip ip 192.0.2.1"} {
		cfg, err := ParseWith([]byte("router bgp 65000\n neighbor leaf peer-group\n neighbor leaf remote-as external\n address-family l2vpn evpn\n  "+stmt+"\n  advertise-all-vni\n"), Options{Lenient: true, Warn: func(*ParseError) {}})
		if err != nil {
			t.Fatalf("%q: ParseWith() = %v", stmt, err)
		}
		if !cfg.Routers[0].AddressFamilies[0].AdvertiseAllVNI {
			t.Errorf("%q: advertise-all-vni not applied to l2vpn evpn", stmt)
		}
	}
}

// In the current FRR (10.3), maximum-prefix and soo are neighbor commands
// of the l2vpn evpn address-family too (FRR 8.2.2 had them only in the
// unicast families), so skipping them keeps the parser there.
func TestParseLenientEVPNNodeCurrentFRR(t *testing.T) {
	for _, stmt := range []string{"neighbor leaf maximum-prefix 100", "neighbor leaf soo 65000:1", "mac-vrf soo 65000:1"} {
		var warns []string
		cfg, err := ParseWith([]byte("router bgp 65000\n neighbor leaf peer-group\n neighbor leaf remote-as external\n address-family l2vpn evpn\n  "+stmt+"\n  advertise-all-vni\n"),
			Options{Lenient: true, Warn: func(e *ParseError) { warns = append(warns, e.Error()) }})
		if err != nil {
			t.Fatalf("%q: ParseWith() = %v", stmt, err)
		}
		if !cfg.Routers[0].AddressFamilies[0].AdvertiseAllVNI {
			t.Errorf("%q: advertise-all-vni not applied to l2vpn evpn", stmt)
		}
		// One warning for the skipped statement, none for the next one.
		if len(warns) != 1 || !strings.Contains(warns[0], stmt) {
			t.Errorf("%q: warnings %q, want exactly one for it", stmt, warns)
		}
	}
}

// The current FRR neighbor attributes of a unicast address-family are
// skipped there; the next statement stays in the address-family.
func TestParseLenientUnicastAFCurrentFRR(t *testing.T) {
	for _, stmt := range []string{"neighbor leaf soo 65000:1", "neighbor leaf dampening", "neighbor leaf addpath-rx-paths-limit 4", "sid vpn export auto"} {
		var warns []string
		cfg, err := ParseWith([]byte("router bgp 65000\n neighbor leaf peer-group\n neighbor leaf remote-as external\n address-family ipv4 unicast\n  "+stmt+"\n  network 192.0.2.0/24\n"),
			Options{Lenient: true, Warn: func(e *ParseError) { warns = append(warns, e.Error()) }})
		if err != nil {
			t.Fatalf("%q: ParseWith() = %v", stmt, err)
		}
		if af := cfg.Routers[0].AddressFamilies[0]; len(af.Networks) != 1 {
			t.Errorf("%q: network not applied to ipv4 unicast: %+v", stmt, af)
		}
		if len(warns) != 1 || !strings.Contains(warns[0], "address-family ipv4 unicast") {
			t.Errorf("%q: warnings %q, want exactly one in the address-family", stmt, warns)
		}
	}
}

// "sid" is not an l2vpn evpn command: FRR runs it in the router node and
// stays there, so a following EVPN statement fails, like in FRR.
func TestParseLenientSIDInEVPNMovesToRouter(t *testing.T) {
	var warns []string
	cfg, err := ParseWith([]byte("router bgp 65000\n address-family l2vpn evpn\n  sid vpn per-vrf export auto\n  advertise-all-vni\n"),
		Options{Lenient: true, Warn: func(e *ParseError) { warns = append(warns, e.Error()) }})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routers[0].AddressFamilies[0].AdvertiseAllVNI {
		t.Error("advertise-all-vni applied to l2vpn evpn after sid moved to the router node")
	}
	if len(warns) != 2 || !strings.Contains(warns[0], "(in router bgp 65000)") || !strings.Contains(warns[1], "advertise-all-vni") {
		t.Errorf("warnings %q, want sid in the router node, then advertise-all-vni", warns)
	}
}

// advertise-default-gw in a vni block belongs to that VNI; exit-vni then
// leaves the block.
func TestParseVNIAdvertiseDefaultGW(t *testing.T) {
	cfg, err := Parse([]byte("router bgp 65000\n address-family l2vpn evpn\n  vni 100\n   advertise-default-gw\n  exit-vni\n  advertise-all-vni\n"))
	if err != nil {
		t.Fatal(err)
	}
	af := cfg.Routers[0].AddressFamilies[0]
	if !af.L2VNIs[0].AdvertiseDefaultGW || af.AdvertiseDefaultGW || !af.AdvertiseAllVNI {
		t.Errorf("af = %+v, want advertise-default-gw on VNI 100 only", af)
	}
}

// "sbfd" (current FRR) is skipped in the bfd node; the profile after it
// still belongs to bfd.
func TestParseLenientSBFD(t *testing.T) {
	var warns int
	cfg, err := ParseWith([]byte("bfd\n sbfd reflector source-address 2001:db8::1 discriminator 1\n profile fabric\n  transmit-interval 150\n"),
		Options{Lenient: true, Warn: func(*ParseError) { warns++ }})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.BFDProfiles) != 1 || *cfg.BFDProfiles[0].TransmitInterval != 150 || warns != 1 {
		t.Errorf("profiles %+v, %d warnings, want the profile and one warning", cfg.BFDProfiles, warns)
	}
}

// FRR 10 "remote-as auto" is not supported.
func TestParseRemoteASAuto(t *testing.T) {
	if _, err := Parse([]byte("router bgp 65000\n neighbor 192.0.2.1 remote-as auto\n")); err == nil || !strings.Contains(err.Error(), "remote-as auto is not supported") {
		t.Fatalf("Parse() = %v, want the remote-as auto error", err)
	}
}

// multipath-relax as-set and remote-as auto are errors also in lenient
// mode: skipping them would change the routes silently.
func TestParseLenientPlainErrors(t *testing.T) {
	for _, conf := range []string{
		"router bgp 65000\n bgp bestpath as-path multipath-relax as-set\n",
		"router bgp 65000\n neighbor 192.0.2.1 remote-as auto\n",
	} {
		if _, err := ParseWith([]byte(conf), Options{Lenient: true, Warn: func(*ParseError) {}}); err == nil {
			t.Errorf("%q: lenient parse accepted it", conf)
		}
	}
}

// "[no] bgp fast-external-failover" sets the router flag; sessions follow it.
func TestParseFastExternalFailover(t *testing.T) {
	cfg, err := Parse([]byte("router bgp 65000\n no bgp fast-external-failover\n neighbor 192.0.2.1 remote-as 65001\n"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := cfg.Sessions()
	if err != nil || cfg.Routers[0].FastExternalFailover == nil || *cfg.Routers[0].FastExternalFailover || s[0].FastExternalFailover {
		t.Fatalf("router %v sessions %+v (%v), want off", cfg.Routers[0].FastExternalFailover, s, err)
	}
}

// "rd" in the l2vpn evpn address family of a VRF sets the RD of its EVPN
// routes; it must be ASN:NN or IPv4:NN.
func TestParseVRFRD(t *testing.T) {
	conf := "router bgp 65000 vrf Vrf_x\n address-family l2vpn evpn\n  rd 192.0.2.17:9\n exit-address-family\n"
	m, err := Parse([]byte(conf))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Routers[0].AddressFamilies[0].RD; got != "192.0.2.17:9" {
		t.Fatalf("RD = %q", got)
	}
	if _, err := Parse([]byte(strings.Replace(conf, "192.0.2.17:9", "nope", 1))); err == nil {
		t.Fatal("rd nope accepted")
	}
	if _, err := Parse([]byte("router bgp 65000\n address-family ipv4 unicast\n  rd 65000:1\n")); err == nil {
		t.Fatal("rd outside l2vpn evpn accepted")
	}
}

// An rd keeps the NN range of its RD type, like FRR: 16 bits for IPv4 and
// a 4-byte ASN, 32 bits for a 2-byte ASN. The fleet ASN 4210000017 stays
// unchanged.
func TestParseRDRange(t *testing.T) {
	vrf := func(rd string) string {
		return "router bgp 4210000017 vrf provision\n address-family l2vpn evpn\n  rd " + rd + "\n exit-address-family\n"
	}
	l2vni := func(rd string) string {
		return "router bgp 4210000017\n address-family l2vpn evpn\n  vni 100\n   rd " + rd + "\n  exit-vni\n exit-address-family\n"
	}
	for _, rd := range []string{"4210000017:9", "65000:4294967295", "192.0.2.17:65535"} {
		cfg, err := Parse([]byte(vrf(rd)))
		if err != nil {
			t.Fatalf("vrf rd %s: %v", rd, err)
		}
		if got := cfg.Routers[0].AddressFamilies[0].RD; got != rd {
			t.Errorf("vrf rd = %q, want %q", got, rd)
		}
		cfg, err = Parse([]byte(l2vni(rd)))
		if err != nil {
			t.Fatalf("vni rd %s: %v", rd, err)
		}
		if got := cfg.Routers[0].AddressFamilies[0].L2VNIs[0].RD; got != rd {
			t.Errorf("vni rd = %q, want %q", got, rd)
		}
	}
	for _, rd := range []string{"4210000017:70000", "1.2.3.4:70000", "65000:5000000000", "4294967296:1"} {
		if _, err := Parse([]byte(vrf(rd))); err == nil {
			t.Errorf("vrf rd %s accepted, want an error", rd)
		}
		if _, err := Parse([]byte(l2vni(rd))); err == nil {
			t.Errorf("vni rd %s accepted, want an error", rd)
		}
	}
}
