//go:build e2e_prodtpl

// Production template lab (#630 slice b, topology-production-templates.clab.yml).
//
// BOOTy runs the rendered bm4x-operator templates. A shadow FRR node runs
// the same template for another host. Every test checks BOOTy against the
// expected FRR result and against the shadow node, as seen from the leaf
// and from the route reflectors (differential check, #630 section 4.3).
//
// The route reflectors are reachable only over routes learned from the
// leaf, so the RR sessions also prove the FIB writer (C17).
//
// Prerequisites:
//
//	make clab-prodtpl-up
package integration

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	prodtplLeaf   = "clab-booty-prodtpl-lab-leaf01"
	prodtplDCGW1  = "clab-booty-prodtpl-lab-dcgw01"
	prodtplDCGW2  = "clab-booty-prodtpl-lab-dcgw02"
	prodtplBooty  = "clab-booty-prodtpl-lab-booty"
	prodtplShadow = "clab-booty-prodtpl-lab-shadow"
	prodtplNative = "clab-booty-prodtpl-lab-native"
	// prodtplNativeVRF has VRF_NAME but no BGP_NEIGHBOR_VRF.
	prodtplNativeVRF = "clab-booty-prodtpl-lab-native-vrf"

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
	// prodtplNativeNode is BOOTy with the L3VNI values from vars keys.
	prodtplNativeNode = prodtplNode{"native", "192.0.2.19", []string{"eth7", "eth8"}}
	prodtplServers    = []prodtplNode{prodtplShadowNode, prodtplBootyNode, prodtplNativeNode}
	// prodtplNativeVRFNode has only the leaf sessions, so it is not in
	// prodtplServers.
	prodtplNativeVRFNode = prodtplNode{"native-vrf", "192.0.2.20", []string{"eth9", "eth10"}}
)

// prodtplOthers returns the underlay addresses of the other servers.
func prodtplOthers(node prodtplNode) []string {
	var others []string
	for _, n := range append(slices.Clone(prodtplServers), prodtplNativeVRFNode) {
		if n.name != node.name {
			others = append(others, n.underlay)
		}
	}
	return others
}

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

// Both uplinks peer with the leaf (C5), with local-as 65501 replacing the
// real ASN (C6), the template timers 30/90 (C7) and only ipv4 unicast
// (C8: "no bgp default ipv4-unicast", only "neighbor leaf activate").
func TestProdTplLeafSessions(t *testing.T) {
	want := sessionView{State: "Established", RemoteAS: prodtplLocalAS, LocalAS: 65500,
		HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"ipv4Unicast"}}
	for _, node := range prodtplServers {
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
	for _, node := range prodtplServers {
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
	want := sessionView{State: "Established", RemoteAS: prodtplASN, LocalAS: prodtplASN,
		HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"l2VpnEvpn"}}
	for _, node := range prodtplServers {
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
	for _, node := range prodtplServers {
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

// prodtplRoutes returns the BGP routes of the underlay VRF as prefix ->
// sorted next hop devices. The loopbacks of the other servers are renamed to
// "other-server", so BOOTy and the shadow node compare equal.
func prodtplRoutes(ctx context.Context, node prodtplNode) (map[string][]string, error) {
	container, args := prodtplContainer(node), []string{"ip", "-j", "route", "show", "table", "10", "proto", "bgp"}
	if node.name == "shadow" {
		args = []string{"ip", "-j", "route", "show", "vrf", "Vrf_underlay", "proto", "bgp"}
	}
	others := prodtplOthers(node)
	out, err := prodtplExec(ctx, container, args...)
	if err != nil {
		return nil, err
	}
	type hop struct {
		Dev     string `json:"dev"`
		Gateway string `json:"gateway"`
		Via     struct {
			Host string `json:"host"`
		} `json:"via"`
	}
	var routes []struct {
		Dst    string `json:"dst"`
		Metric int    `json:"metric"`
		hop
		Nexthops []hop `json:"nexthops"`
	}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return nil, fmt.Errorf("decode routes of %s: %w", node.name, err)
	}
	leaf, err := prodtplLeafLinkLocals(ctx)
	if err != nil {
		return nil, err
	}
	// Each next hop as "device via gateway metric N"; the leaf link-local
	// addresses differ per link, so they become "leaf".
	nexthop := func(h hop, metric int) string {
		gw := cmp.Or(h.Gateway, h.Via.Host)
		if leaf[gw] {
			gw = "leaf"
		}
		return fmt.Sprintf("%s via %s metric %d", h.Dev, gw, metric)
	}
	got := map[string][]string{}
	for _, r := range routes {
		dst := r.Dst
		if slices.Contains(others, dst) {
			dst = "other-server"
		}
		hops := []string{}
		if r.Dev != "" {
			hops = append(hops, nexthop(r.hop, r.Metric))
		}
		for _, nh := range r.Nexthops {
			hops = append(hops, nexthop(nh, r.Metric))
		}
		slices.Sort(hops)
		got[dst] = hops
	}
	return got, nil
}

// prodtplLeafLinkLocals returns the IPv6 link-local addresses of the leaf.
func prodtplLeafLinkLocals(ctx context.Context) (map[string]bool, error) {
	out, err := prodtplExec(ctx, prodtplLeaf, "ip", "-j", "-6", "addr", "show", "scope", "link")
	if err != nil {
		return nil, err
	}
	var links []struct {
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return nil, fmt.Errorf("decode leaf addresses: %w", err)
	}
	lls := map[string]bool{}
	for _, l := range links {
		for _, a := range l.AddrInfo {
			lls[a.Local] = true
		}
	}
	return lls, nil
}

func waitRoutes(t *testing.T, node prodtplNode, want map[string][]string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	for {
		got, err := prodtplRoutes(ctx, node)
		if err == nil && reflect.DeepEqual(got, want) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: BGP routes in Vrf_underlay = %v (%v), want %v", node.name, got, err, want)
		case <-time.After(2 * time.Second):
		}
	}
}

// prodtplRoutesVia is the BGP table of a server: the RR loopbacks, the
// other servers and the leaf route with the local-as 3 times (allowed by
// allowas-in 3). The leaf route with the local-as 4 times is rejected.
func prodtplRoutesVia(devs ...string) map[string][]string {
	hops := make([]string, 0, len(devs))
	for _, d := range devs {
		// Like zebra: the leaf link-local gateway (RFC 5549) and metric 20.
		hops = append(hops, d+" via leaf metric 20")
	}
	return map[string][]string{"198.51.100.1": hops, "198.51.100.2": hops, "other-server": hops, "203.0.113.128/25": hops}
}

// The learned routes are in the VRF table with ECMP over both uplinks,
// like zebra installs them on the shadow node (C17, C10).
func TestProdTplFIBRoutes(t *testing.T) {
	want := prodtplRoutesVia("ens1f0np0", "ens1f1np1")
	for _, node := range prodtplServers {
		waitRoutes(t, node, want)
	}
}

// A withdrawn uplink session removes its next hop; the session coming
// back adds it again.
func TestProdTplFIBWithdraw(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	leafShutdown := func(port string, on bool) {
		stmt := "neighbor " + port + " shutdown"
		if !on {
			stmt = "no " + stmt
		}
		if out, err := prodtplExec(ctx, prodtplLeaf, "vtysh", "-c", "configure terminal", "-c", "router bgp 65500", "-c", stmt); err != nil {
			t.Fatalf("leaf %s: %v\n%s", stmt, err, out)
		}
	}
	for _, node := range prodtplServers {
		port := node.leafPorts[0]
		leafShutdown(port, true)
		waitRoutes(t, node, prodtplRoutesVia("ens1f1np1"))
		leafShutdown(port, false)
		waitRoutes(t, node, prodtplRoutesVia("ens1f0np0", "ens1f1np1"))
	}
}

// Losing the carrier of one uplink ends its session at once (FRR
// fast-external-failover), so its next hop leaves the VRF table within
// about a second instead of after the 90 s hold time. The carrier coming
// back restores ECMP.
func TestProdTplUplinkCarrierLoss(t *testing.T) {
	const maxFailover = 2 * time.Second // includes docker exec latency
	for _, node := range prodtplServers {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		port := node.leafPorts[0]
		start := time.Now()
		if out, err := prodtplExec(ctx, prodtplLeaf, "ip", "link", "set", port, "down"); err != nil {
			cancel()
			t.Fatalf("leaf %s down: %v\n%s", port, err, out)
		}
		want := prodtplRoutesVia("ens1f1np1")
		for {
			got, err := prodtplRoutes(ctx, node)
			if err == nil && reflect.DeepEqual(got, want) {
				break
			}
			if time.Since(start) > maxFailover {
				cancel()
				t.Fatalf("%s: routes %v (%v) %s after carrier loss, want %v within %s", node.name, got, err, time.Since(start), want, maxFailover)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("%s: next hop removed %s after carrier loss", node.name, time.Since(start).Round(10*time.Millisecond))
		if out, err := prodtplExec(ctx, prodtplLeaf, "ip", "link", "set", port, "up"); err != nil {
			cancel()
			t.Fatalf("leaf %s up: %v\n%s", port, err, out)
		}
		cancel()
		waitRoutes(t, node, prodtplRoutesVia("ens1f0np0", "ens1f1np1"))
	}
}

// With VRF_NAME and no BGP_NEIGHBOR_VRF, the routes of the peers on the
// Vrf_underlay NICs go into the Vrf_underlay table, because FRR puts an
// interface peer of a VRF into the "router bgp" of that VRF (as on the
// shadow node). The main table gets no BGP routes.
func TestProdTplNativeVRFNameFIB(t *testing.T) {
	node := prodtplNativeVRFNode
	session := sessionView{State: "Established", RemoteAS: prodtplLocalAS, LocalAS: 65500,
		HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"ipv4Unicast"}}
	for _, port := range node.leafPorts {
		waitView(t, prodtplLeaf, port, session)
	}
	want := prodtplRoutesVia("ens1f0np0", "ens1f1np1")
	waitRoutes(t, prodtplShadowNode, want)
	waitRoutes(t, node, want)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := prodtplExec(ctx, prodtplNativeVRF, "ip", "route", "show", "table", "main", "proto", "bgp")
	if err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("%s: BGP routes in the main table: %q (%v), want none", node.name, out, err)
	}
}

// The leaf can open the sessions to the unnumbered peers in Vrf_underlay.
// With the outbound BGP connects of the server dropped, a session cleared
// on the leaf comes back with the leaf as client: on the shadow node (FRR
// listens per VRF) and on native-vrf (tcp_l3mdev_accept for VRF_NAME).
func TestProdTplLeafSessionVRFInbound(t *testing.T) {
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplNativeVRFNode} {
		t.Run(node.name, func(t *testing.T) { prodtplLeafInbound(t, node) })
	}
}

func prodtplLeafInbound(t *testing.T, node prodtplNode) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	// The FRR image has no ip6tables; the BOOTy test image runs it in the
	// network namespace of the node.
	ip6tables := func(ctx context.Context, op string) (string, error) {
		args := []string{"run", "--rm", "--net", "container:" + prodtplContainer(node), "--cap-add", "NET_ADMIN",
			"--entrypoint", "ip6tables", "booty-gobgp-test:latest",
			op, "OUTPUT", "-p", "tcp", "--syn", "--dport", "179", "-j", "DROP"}
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return string(out), err
	}
	if out, err := ip6tables(ctx, "-I"); err != nil {
		t.Fatalf("block outbound connects: %v\n%s", err, out)
	}
	defer func() { _, _ = ip6tables(context.Background(), "-D") }()
	port := node.leafPorts[0]
	if out, err := prodtplExec(ctx, prodtplLeaf, "vtysh", "-c", "clear bgp "+port); err != nil {
		t.Fatalf("reset session: %v\n%s", err, out)
	}
	for {
		n, err := prodtplNeighbor(ctx, prodtplLeaf, port)
		if err == nil && n["bgpState"] == "Established" && n["portForeign"] == float64(179) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("leaf -> %s session on %s not opened by the leaf: %v %v", node.name, port, n, err)
		case <-time.After(2 * time.Second):
		}
	}
}

// With outbound BGP connects from BOOTy dropped, dcgw01 must open the
// session into Vrf_underlay and the BOOTy listener must accept it. Checked
// for the frr.conf sessions (booty) and the native numbered peers (native).
func TestProdTplRRSessionVRFInbound(t *testing.T) {
	for _, node := range []prodtplNode{prodtplBootyNode, prodtplNativeNode} {
		t.Run(node.name, func(t *testing.T) { prodtplInbound(t, node) })
	}
}

func prodtplInbound(t *testing.T, node prodtplNode) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	c := prodtplContainer(node)
	rule := []string{"OUTPUT", "-p", "tcp", "--syn", "-d", "198.51.100.1", "--dport", "179", "-j", "DROP"}
	if out, err := prodtplExec(ctx, c, append([]string{"iptables", "-I"}, rule...)...); err != nil {
		t.Fatalf("block outbound connects: %v\n%s", err, out)
	}
	defer func() {
		_, _ = prodtplExec(context.Background(), c, append([]string{"iptables", "-D"}, rule...)...)
	}()
	if out, err := prodtplExec(ctx, prodtplDCGW1, "vtysh", "-c", "clear bgp "+node.underlay); err != nil {
		t.Fatalf("reset session: %v\n%s", err, out)
	}
	for {
		n, err := prodtplNeighbor(ctx, prodtplDCGW1, node.underlay)
		if err == nil && n["bgpState"] == "Established" && n["portForeign"] == float64(179) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("dcgw01 -> %s session not opened inbound: %v %v", node.name, n, err)
		case <-time.After(2 * time.Second):
		}
	}
}

// Mirrors FRR bgp_as_allow_in test_bgp_allowas_in_p0 for the local-as of
// "local-as 65501 no-prepend replace-as": the leaf sends 203.0.113.0/24
// with 65501 four times and 203.0.113.128/25 with it three times. FRR
// rejects the first (more than allowas-in 3) and keeps the second; BOOTy
// must do the same (#630 C6, C11).
func TestProdTplLocalASLoopCheck(t *testing.T) {
	for _, node := range prodtplServers {
		waitRoutes(t, node, prodtplRoutesVia("ens1f0np0", "ens1f1np1"))
	}
}
