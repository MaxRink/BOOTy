package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/telekom/BOOTy/pkg/network/frrconf"
)

// bm4xNativeSessions is the bm4x two-uplink template's BGP part in the
// native form.
const bm4xNativeSessions = `
asn: 4210000017
defaults: traditional
peerGroups:
  - name: dcgw
    vrf: Vrf_underlay
    remoteAS: internal
    bfd: true
    bfdProfile: fabric
    keepalive: 30
    hold: 90
    connectRetry: 5
    families: [l2vpn-evpn]
  - name: leaf
    vrf: Vrf_underlay
    remoteAS: "65500"
    localAS: 65501
    localASMode: no-prepend-replace-as
    bfd: true
    bfdProfile: fabric
    keepalive: 30
    hold: 90
    families: [ipv4-unicast]
sessions:
  - {name: ens1f0np0, interface: true, vrf: Vrf_underlay, peerGroup: leaf}
  - {name: ens1f1np1, interface: true, vrf: Vrf_underlay, peerGroup: leaf}
  - {name: 198.51.100.1, vrf: Vrf_underlay, peerGroup: dcgw}
  - {name: 198.51.100.2, vrf: Vrf_underlay, peerGroup: dcgw}
`

// The bm4x template sessions from frr.conf and from the native config are
// the same.
func TestNativeSessionsEqualFRRConf(t *testing.T) {
	data, err := os.ReadFile("../network/frrconf/testdata/bm4x/two-uplinks/frr.conf")
	if err != nil {
		t.Fatal(err)
	}
	model, err := frrconf.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	fromFRR, err := model.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	var bgp BGPConfig
	if err := yaml.Unmarshal([]byte(bm4xNativeSessions), &bgp); err != nil {
		t.Fatal(err)
	}
	native, err := bgp.ResolveSessions()
	if err != nil {
		t.Fatal(err)
	}
	for i := range fromFRR {
		fromFRR[i].Line = 0
	}
	if !reflect.DeepEqual(native, fromFRR) {
		t.Fatalf("native sessions differ from frr.conf:\n native  %+v\n frr.conf %+v", native, fromFRR)
	}
}

// Statements BOOTy does not support fail in both forms: a local-as mode
// other than no-prepend replace-as, local-as equal to remote-as, a member
// remote-as next to a peer-group remote-as.
func TestNativeSessionsRejectLikeFRRConf(t *testing.T) {
	for _, tt := range []struct {
		name, frr, native, want string
	}{
		{
			"local-as mode",
			"router bgp 65000\n neighbor 192.0.2.1 remote-as 65001\n neighbor 192.0.2.1 local-as 65002 no-prepend\n",
			"asn: 65000\nsessions:\n  - {name: 192.0.2.1, remoteAS: \"65001\", localAS: 65002, localASMode: no-prepend, families: [ipv4-unicast]}\n",
			"supported",
		},
		{
			"local-as is the router AS",
			"router bgp 65000\n neighbor 192.0.2.1 remote-as 65001\n neighbor 192.0.2.1 local-as 65000 no-prepend replace-as\n",
			"asn: 65000\nsessions:\n  - {name: 192.0.2.1, remoteAS: \"65001\", localAS: 65000, families: [ipv4-unicast]}\n",
			"local-as 65000 is the router AS",
		},
		{
			"member remote-as",
			"router bgp 65000\n neighbor g peer-group\n neighbor g remote-as 65001\n neighbor 192.0.2.1 peer-group g\n neighbor 192.0.2.1 remote-as 65002\n",
			"asn: 65000\npeerGroups:\n  - {name: g, remoteAS: \"65001\"}\nsessions:\n  - {name: 192.0.2.1, peerGroup: g, remoteAS: \"65002\", families: [ipv4-unicast]}\n",
			"has its own remote-as",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, frrErr := sessionsOf(tt.frr)
			var bgp BGPConfig
			if err := yaml.Unmarshal([]byte(tt.native), &bgp); err != nil {
				t.Fatal(err)
			}
			_, nativeErr := bgp.ResolveSessions()
			for name, err := range map[string]error{"frr.conf": frrErr, "native": nativeErr} {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Errorf("%s: error %v, want %q", name, err, tt.want)
				}
			}
		})
	}
}

func sessionsOf(conf string) ([]frrconf.Session, error) {
	model, err := frrconf.Parse([]byte(conf))
	if err != nil {
		return nil, err
	}
	return model.Sessions()
}

// Config.Validate applies the session rules, including the timers that
// GoBGP can not run: 0 disables keepalives in FRR.
func TestValidateNativeSessions(t *testing.T) {
	for _, tt := range []struct{ yaml, want string }{
		{"sessions:\n  - {name: 192.0.2.1, remoteAS: external, families: [ipv4-unicast]}\n", ""},
		{"sessions:\n  - {name: 192.0.2.1, families: [ipv4-unicast]}\n", "no remote-as"},
		{"sessions:\n  - {name: 192.0.2.1, remoteAS: external, keepalive: 0, hold: 0, families: [ipv4-unicast]}\n", "0 is not supported"},
		{"sessions:\n  - {name: 192.0.2.1, remoteAS: external}\n", "no address family"},
		{"sessions:\n  - {name: 192.0.2.1, remoteAS: external, families: [vpnv4]}\n", "family \"vpnv4\""},
		{"defaults: cloud\nsessions:\n  - {name: 192.0.2.1, remoteAS: external, families: [ipv4-unicast]}\n", "traditional or datacenter"},
		{"sessions:\n  - {name: 192.0.2.1, peerGroup: nope, families: [ipv4-unicast]}\n", "peer-group nope is not defined"},
		{"peerGroups:\n  - {name: g, remoteAS: external}\n", "apply only with network.bgp.sessions"},
		{"defaults: traditional\n", "apply only with network.bgp.sessions"},
		{"defaults: cloud\n", "traditional or datacenter"},
		{"sessions:\n  - {name: leaf0, remoteAS: external, families: [ipv4-unicast]}\n", "not an IP address"},
		{"sessions:\n  - {name: eth0, interface: true, remoteAS: external, families: [ipv4-unicast]}\n", ""},
		{"peerGroups:\n  - {name: g, interface: true, remoteAS: external}\nsessions:\n  - {name: eth0, interface: true, peerGroup: g, families: [ipv4-unicast]}\n", "session fields"},
		{"peerGroups:\n  - {name: g, peerGroup: h, remoteAS: external}\nsessions:\n  - {name: 192.0.2.1, peerGroup: g, families: [ipv4-unicast]}\n", "session fields"},
		{"peerGroups:\n  - {name: g, vrf: Vrf_a, remoteAS: external}\nsessions:\n  - {name: 192.0.2.1, vrf: Vrf_b, peerGroup: g, families: [ipv4-unicast]}\n", "differs from the VRF Vrf_a"},
		{"sessions:\n  - {name: 192.0.2.1, remoteAS: external, families: [ipv4-unicast]}\n  - {name: 192.0.2.1, remoteAS: internal, families: [ipv4-unicast]}\n", "defined twice"},
		{"sessions:\n  - {name: 192.0.2.1, remoteAS: external, families: [ipv4-unicast]}\n  - {name: 192.0.2.1, vrf: Vrf_b, remoteAS: external, families: [ipv4-unicast]}\n", ""},
		{"sessions:\n  - {name: 192.0.2.1, remoteAS: 65001, families: [ipv4-unicast]}\n", ""},
	} {
		c := &Config{}
		if err := yaml.Unmarshal([]byte(tt.yaml), &c.Network.BGP); err != nil {
			t.Fatal(err)
		}
		err := c.Validate()
		if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("%q: Validate = %v, want %q", tt.yaml, err, tt.want)
		}
	}
}

// remoteAS takes a number with or without quotes, in YAML and JSON.
func TestRemoteASNumberOrString(t *testing.T) {
	var y BGPSessionConfig
	if err := yaml.Unmarshal([]byte("{name: a, remoteAS: 65001}"), &y); err != nil || y.RemoteAS != "65001" {
		t.Fatalf("YAML number: %q %v", y.RemoteAS, err)
	}
	for _, in := range []string{`{"name":"a","remoteAS":65001}`, `{"name":"a","remoteAS":"65001"}`} {
		var j BGPSessionConfig
		if err := json.Unmarshal([]byte(in), &j); err != nil || j.RemoteAS != "65001" {
			t.Fatalf("JSON %s: %q %v", in, j.RemoteAS, err)
		}
	}
	var bad BGPSessionConfig
	if err := json.Unmarshal([]byte(`{"name":"a","remoteAS":[1]}`), &bad); err == nil {
		t.Fatal("JSON list remoteAS parsed")
	}
	if err := yaml.Unmarshal([]byte("{name: a, remoteAS: [1]}"), &bad); err == nil {
		t.Fatal("YAML list remoteAS parsed")
	}
}
