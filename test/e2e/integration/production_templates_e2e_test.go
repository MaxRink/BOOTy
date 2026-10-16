//go:build e2e_prodtpl

// Production template lab (#630 slice b, topology-production-templates.clab.yml).
//
// BOOTy runs the rendered bm4x-operator templates. A shadow FRR node runs
// the same template for another host. Every test checks BOOTy against the
// expected FRR result and against the shadow node, as seen from the leaf
// and from the route reflectors (differential check, #630 section 4.3).
//
// BOOTy has no FIB writer before slice c, so the route reflectors are not
// reachable from its VRF on their own. ensureRRRoutes installs the routes
// that zebra installs on the shadow node.
//
// Prerequisites:
//
//	make clab-prodtpl-up
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	prodtplLeaf   = "clab-booty-prodtpl-lab-leaf01"
	prodtplDCGW1  = "clab-booty-prodtpl-lab-dcgw01"
	prodtplDCGW2  = "clab-booty-prodtpl-lab-dcgw02"
	prodtplBooty  = "clab-booty-prodtpl-lab-booty"
	prodtplShadow = "clab-booty-prodtpl-lab-shadow"

	prodtplASN     = 4210000017
	prodtplLocalAS = 65501

	prodtplTimeout = 3 * time.Minute
)

// prodtplNode is one server as the fabric sees it.
type prodtplNode struct {
	name      string
	underlay  string   // router-id and RR neighbor address
	leafPorts []string // leaf interfaces of its uplinks
}

var (
	prodtplBootyNode  = prodtplNode{"booty", "192.0.2.17", []string{"eth1", "eth2"}}
	prodtplShadowNode = prodtplNode{"shadow", "192.0.2.18", []string{"eth3", "eth4"}}
)

func prodtplExec(ctx context.Context, container string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", container}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker exec %s %s: %w: %s", container, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// jsonPart drops vtysh warnings ("% Can't open ... vtysh.conf") around
// the JSON document.
func jsonPart(out string) string {
	i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if i < 0 || j < i {
		return out
	}
	return out[i : j+1]
}

// prodtplNeighbor returns the FRR "show bgp neighbors X json" entry.
func prodtplNeighbor(ctx context.Context, container, neighbor string) (map[string]any, error) {
	out, err := prodtplExec(ctx, container, "vtysh", "-c", "show bgp neighbors "+neighbor+" json")
	if err != nil {
		return nil, err
	}
	var all map[string]map[string]any
	if err := json.Unmarshal([]byte(jsonPart(out)), &all); err != nil {
		return nil, fmt.Errorf("decode %s neighbor %s: %w", container, neighbor, err)
	}
	n, ok := all[neighbor]
	if !ok {
		return nil, fmt.Errorf("%s has no neighbor %s", container, neighbor)
	}
	return n, nil
}

// sessionView is the normalized part of a session that must be the same
// for BOOTy and for FRR with the same template.
type sessionView struct {
	State     string
	RemoteAS  float64
	LocalAS   float64
	HoldMsecs float64
	KeepMsecs float64
	Families  []string // negotiated in both directions
}

func viewOf(n map[string]any) sessionView {
	v := sessionView{}
	v.State, _ = n["bgpState"].(string)
	v.RemoteAS, _ = n["remoteAs"].(float64)
	v.LocalAS, _ = n["localAs"].(float64)
	v.HoldMsecs, _ = n["bgpTimerHoldTimeMsecs"].(float64)
	v.KeepMsecs, _ = n["bgpTimerKeepAliveIntervalMsecs"].(float64)
	caps, _ := n["neighborCapabilities"].(map[string]any)
	mp, _ := caps["multiprotocolExtensions"].(map[string]any)
	for fam, st := range mp {
		if m, _ := st.(map[string]any); m["advertisedAndReceived"] == true {
			v.Families = append(v.Families, fam)
		}
	}
	slices.Sort(v.Families)
	return v
}

// waitView polls until the session reaches want or the timeout expires.
func waitView(t *testing.T, container, neighbor string, want sessionView) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	var last string
	for {
		n, err := prodtplNeighbor(ctx, container, neighbor)
		if err == nil {
			got := viewOf(n)
			if reflect.DeepEqual(got, want) {
				return
			}
			last = fmt.Sprintf("%+v", got)
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s neighbor %s: got %s, want %+v", container, neighbor, last, want)
		case <-time.After(2 * time.Second):
		}
	}
}

var rrRoutesOnce sync.Once

// ensureRRRoutes installs the leaf-learned RR routes in the BOOTy VRF
// table, as zebra does on the shadow node. Stand-in for the slice c FIB
// writer.
func ensureRRRoutes(t *testing.T) {
	t.Helper()
	rrRoutesOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		defer cancel()
		script := `for i in $(seq 1 60); do ip link show Vrf_underlay >/dev/null 2>&1 && break; sleep 2; done
for n in 1 2; do ip route replace 198.51.100.$n/32 vrf Vrf_underlay src 192.0.2.17 \
  nexthop via inet6 fe80::1 dev ens1f0np0 nexthop via inet6 fe80::1 dev ens1f1np1; done`
		if out, err := prodtplExec(ctx, prodtplBooty, "sh", "-c", script); err != nil {
			t.Fatalf("install RR routes: %v\n%s", err, out)
		}
	})
}

// Both uplinks peer with the leaf (C5), with local-as 65501 replacing the
// real ASN (C6), the template timers 30/90 (C7) and only ipv4 unicast
// (C8: "no bgp default ipv4-unicast", only "neighbor leaf activate").
func TestProdTplLeafSessions(t *testing.T) {
	want := sessionView{State: "Established", RemoteAS: prodtplLocalAS, LocalAS: 65500,
		HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"ipv4Unicast"}}
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplBootyNode} {
		for _, port := range node.leafPorts {
			t.Run(node.name+"-"+port, func(t *testing.T) {
				waitView(t, prodtplLeaf, port, want)
			})
		}
	}
}

// local-as no-prepend replace-as: the leaf sees only 65501 on the server
// loopback, never the real ASN (FRR bgp_local_asn topotest).
func TestProdTplLocalASReplaceASPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	paths := func(prefix string) ([]string, error) {
		out, err := prodtplExec(ctx, prodtplLeaf, "vtysh", "-c", "show bgp ipv4 unicast "+prefix+" json")
		if err != nil {
			return nil, err
		}
		var r struct {
			Paths []struct {
				ASPath struct {
					String string `json:"string"`
				} `json:"aspath"`
			} `json:"paths"`
		}
		if err := json.Unmarshal([]byte(jsonPart(out)), &r); err != nil {
			return nil, fmt.Errorf("decode %s: %w", prefix, err)
		}
		var s []string
		for _, p := range r.Paths {
			s = append(s, p.ASPath.String)
		}
		return s, nil
	}
	want := []string{"65501", "65501"} // one path per uplink
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplBootyNode} {
		for {
			got, err := paths(node.underlay + "/32")
			if err == nil && reflect.DeepEqual(got, want) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s: leaf AS paths for %s = %v (%v), want %v", node.name, node.underlay, got, err, want)
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// Both route reflectors: iBGP from "remote-as internal" (C4), sessions in
// Vrf_underlay (C3), timers from the dcgw peer-group, l2vpn evpn only.
func TestProdTplRRSessions(t *testing.T) {
	ensureRRRoutes(t)
	want := sessionView{State: "Established", RemoteAS: prodtplASN, LocalAS: prodtplASN,
		HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"l2VpnEvpn"}}
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplBootyNode} {
		for _, rr := range []string{prodtplDCGW1, prodtplDCGW2} {
			t.Run(node.name+"-"+rr, func(t *testing.T) {
				waitView(t, rr, node.underlay, want)
			})
		}
	}
}

// dcgw02 is passive, so its sessions exist only because the servers
// connect out of Vrf_underlay. Its local port is the BGP port.
func TestProdTplRRSessionVRFOutbound(t *testing.T) {
	ensureRRRoutes(t)
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplBootyNode} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		n, err := prodtplNeighbor(ctx, prodtplDCGW2, node.underlay)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if n["bgpState"] != "Established" || n["portLocal"] != float64(179) {
			t.Errorf("%s: dcgw02 state %v local port %v, want Established on 179", node.name, n["bgpState"], n["portLocal"])
		}
	}
}

// With outbound BGP connects from BOOTy dropped, dcgw01 must open the
// session into Vrf_underlay and the BOOTy listener must accept it.
func TestProdTplRRSessionVRFInbound(t *testing.T) {
	ensureRRRoutes(t)
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	rule := []string{"OUTPUT", "-p", "tcp", "--syn", "-d", "198.51.100.1", "--dport", "179", "-j", "DROP"}
	if out, err := prodtplExec(ctx, prodtplBooty, append([]string{"iptables", "-I"}, rule...)...); err != nil {
		t.Fatalf("block outbound connects: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = prodtplExec(context.Background(), prodtplBooty, append([]string{"iptables", "-D"}, rule...)...)
	})
	if out, err := prodtplExec(ctx, prodtplDCGW1, "vtysh", "-c", "clear bgp 192.0.2.17"); err != nil {
		t.Fatalf("reset session: %v\n%s", err, out)
	}
	for {
		n, err := prodtplNeighbor(ctx, prodtplDCGW1, prodtplBootyNode.underlay)
		if err == nil && n["bgpState"] == "Established" && n["portForeign"] == float64(179) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("dcgw01 -> BOOTy session not opened inbound: %v %v", n, err)
		case <-time.After(2 * time.Second):
		}
	}
}
