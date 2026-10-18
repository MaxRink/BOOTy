//go:build e2e_prodtpl

// EVPN Type-5 part of the production template lab (#630 slice d, C16,
// C18). dcgw01 is the gateway of the provision L3VNI 3000. It sends the
// IPv6 Type-5 default and the prefix of the IPv6 CAPRF mock. The servers
// originate their lo /128 and the provisioning /64 ("network" lines with
// "advertise ipv6 unicast"). Every test checks BOOTy against the shadow FRR
// node with the same template.
package integration

import (
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
	prodtplCAPRF6     = "clab-booty-prodtpl-lab-caprf6"
	prodtplGatewayMAC = "02:54:c6:33:64:01" // dcgw01 br3000, the L3VNI RMAC
	prodtplV6Prefix   = "2001:db8:40a:5481::/64"
)

// prodtplV6 holds the per server values of the L3VNI.
type prodtplV6 struct {
	lo   string // lo /128 address
	rmac string // br.provision MAC
}

var prodtplV6Of = map[string]prodtplV6{
	"booty":  {"2001:db8:40a:5481::17", "02:54:c0:00:02:11"},
	"shadow": {"2001:db8:40a:5481::18", "02:54:c0:00:02:12"},
	"native": {"2001:db8:40a:5481::19", "02:54:c0:00:02:13"},
}

func prodtplContainer(node prodtplNode) string {
	switch node.name {
	case "shadow":
		return prodtplShadow
	case "native":
		return prodtplNative
	case "native-vrf":
		return prodtplNativeVRF
	}
	return prodtplBooty
}

// prodtplL3VNIState is the kernel state of the L3VNI on a server, with
// the values of the other servers replaced by "other-...". It holds the
// routes over an IPv4-mapped gateway, the blackhole routes, the neighbor
// entries of br.provision, the FDB entries of vx.3000 and the lo address.
func prodtplL3VNIState(ctx context.Context, node prodtplNode) ([]string, error) {
	c := prodtplContainer(node)
	var pairs []string
	for _, other := range prodtplServers {
		if other.name != node.name {
			pairs = append(pairs, other.underlay, "other-vtep",
				prodtplV6Of[other.name].lo, "other-lo",
				prodtplV6Of[other.name].rmac, "other-rmac")
		}
	}
	norm := strings.NewReplacer(pairs...)
	var state []string
	var routes []struct {
		Type     string   `json:"type"`
		Dst      string   `json:"dst"`
		Gateway  string   `json:"gateway"`
		Dev      string   `json:"dev"`
		Protocol string   `json:"protocol"`
		Metric   int      `json:"metric"`
		Flags    []string `json:"flags"`
		Nexthops []struct {
			Gateway string   `json:"gateway"`
			Dev     string   `json:"dev"`
			Flags   []string `json:"flags"`
		} `json:"nexthops"`
	}
	if err := prodtplJSON(ctx, c, &routes, "ip", "-j", "-6", "route", "show", "table", "main"); err != nil {
		return nil, err
	}
	for _, r := range routes {
		if len(r.Nexthops) == 1 {
			// zebra uses a nexthop object; a single next hop is the same route.
			r.Gateway, r.Dev, r.Flags = r.Nexthops[0].Gateway, r.Nexthops[0].Dev, r.Nexthops[0].Flags
		}
		switch {
		case r.Type == "blackhole":
			state = append(state, fmt.Sprintf("blackhole %s proto %s metric %d", r.Dst, r.Protocol, r.Metric))
		case strings.HasPrefix(r.Gateway, "::ffff:"):
			state = append(state, fmt.Sprintf("route %s via %s dev %s %v proto %s metric %d", r.Dst, r.Gateway, r.Dev, r.Flags, r.Protocol, r.Metric))
		}
	}
	var neighs []struct {
		Dst    string   `json:"dst"`
		LLAddr string   `json:"lladdr"`
		State  []string `json:"state"`
	}
	if err := prodtplJSON(ctx, c, &neighs, "ip", "-j", "-6", "neigh", "show", "dev", "br.provision"); err != nil {
		return nil, err
	}
	for _, n := range neighs {
		state = append(state, fmt.Sprintf("neigh %s lladdr %s %v", n.Dst, n.LLAddr, n.State))
	}
	var fdb []struct {
		MAC   string `json:"mac"`
		Dst   string `json:"dst"`
		State string `json:"state"`
	}
	if err := prodtplJSON(ctx, c, &fdb, "bridge", "-j", "fdb", "show", "dev", "vx.3000"); err != nil {
		return nil, err
	}
	for _, f := range fdb {
		if f.Dst != "" {
			state = append(state, fmt.Sprintf("fdb %s dst %s", f.MAC, f.Dst))
		}
	}
	var addrs []struct {
		AddrInfo []struct {
			Local     string `json:"local"`
			PrefixLen int    `json:"prefixlen"`
			Scope     string `json:"scope"`
		} `json:"addr_info"`
	}
	if err := prodtplJSON(ctx, c, &addrs, "ip", "-j", "-6", "addr", "show", "dev", "lo"); err != nil {
		return nil, err
	}
	for _, a := range addrs {
		for _, i := range a.AddrInfo {
			if i.Scope == "global" {
				state = append(state, fmt.Sprintf("lo %s/%d", i.Local, i.PrefixLen))
			}
		}
	}
	for i := range state {
		state[i] = norm.Replace(state[i])
	}
	slices.Sort(state)
	return state, nil
}

func prodtplJSON(ctx context.Context, container string, v any, args ...string) error {
	out, err := prodtplExec(ctx, container, args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		out = "[]"
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		return fmt.Errorf("decode %s %v: %w: %s", container, args, err, out)
	}
	return nil
}

// Like zebra (FRR 10.x bgp_evpn_rt5 test_router_check_ip): IPv6 routes via
// ::ffff:<VTEP> onlink on the L3VNI bridge, a permanent neighbor entry with
// the RMAC for that address and an FDB entry RMAC -> VTEP. The local
// blackhole /64 wins over the /64 of the other server, and the lo /128 is
// in place (C16, C18). BOOTy, the shadow node and the native server must
// be equal.
func TestProdTplType5IPv6Install(t *testing.T) {
	prodtplWaitL3VNI(t)
}

// prodtplL3VNIWant is the L3VNI state of a server in the steady state.
func prodtplL3VNIWant(node prodtplNode) []string {
	gw := "::ffff:198.51.100.1"
	const bgp = " [onlink] proto bgp metric 20"
	want := []string{
		"blackhole " + prodtplV6Prefix + " proto 196 metric 20",
		"fdb " + prodtplGatewayMAC + " dst 198.51.100.1",
		"fdb other-rmac dst other-vtep",
		"fdb other-rmac dst other-vtep",
		"neigh " + gw + " lladdr " + prodtplGatewayMAC + " [NOARP]",
		"neigh ::ffff:other-vtep lladdr other-rmac [NOARP]",
		"neigh ::ffff:other-vtep lladdr other-rmac [NOARP]",
		"route 2001:db8:c0::/64 via " + gw + " dev br.provision" + bgp,
		"route default via " + gw + " dev br.provision" + bgp,
		"route other-lo via ::ffff:other-vtep dev br.provision" + bgp,
		"route other-lo via ::ffff:other-vtep dev br.provision" + bgp,
		"lo " + prodtplV6Of[node.name].lo + "/128",
	}
	slices.Sort(want)
	return want
}

// prodtplWaitL3VNI waits for the steady L3VNI state on every server and
// checks that BOOTy and the native server equal the shadow node.
func prodtplWaitL3VNI(t *testing.T) {
	t.Helper()
	states := map[string][]string{}
	for _, node := range prodtplServers {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		want := prodtplL3VNIWant(node)
		var got []string
		var err error
		for {
			got, err = prodtplL3VNIState(ctx, node)
			if err == nil && reflect.DeepEqual(got, want) {
				break
			}
			select {
			case <-ctx.Done():
				cancel()
				t.Fatalf("%s L3VNI state (%v):\n  got  %q\n  want %q", node.name, err, got, want)
			case <-time.After(2 * time.Second):
			}
		}
		cancel()
		states[node.name] = slices.DeleteFunc(got, func(s string) bool { return strings.HasPrefix(s, "lo ") })
	}
	for _, name := range []string{"booty", "native"} {
		if !reflect.DeepEqual(states[name], states["shadow"]) {
			t.Fatalf("%s and shadow differ:\n  %s %q\n  shadow %q", name, name, states[name], states["shadow"])
		}
	}
}

// The servers originate the "network" lines of the template as IPv6
// Type-5 routes with their VTEP and RMAC (advertise ipv6 unicast, import
// check satisfied by the lo address and the blackhole route). dcgw01
// installs them like the routes of the shadow node.
func TestProdTplType5Origination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	for _, node := range prodtplServers {
		gw := "::ffff:" + node.underlay
		v6 := prodtplV6Of[node.name]
		want := []string{
			"neigh " + gw + " lladdr " + v6.rmac,
			"route " + prodtplV6Prefix + " via " + gw,
			"route " + v6.lo + " via " + gw,
		}
		var got []string
		var err error
		for {
			got, err = prodtplGatewayView(ctx, gw)
			if err == nil && reflect.DeepEqual(got, want) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s: dcgw01 VRF provision (%v):\n  got  %q\n  want %q", node.name, err, got, want)
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// prodtplGatewayView returns the dcgw01 routes of VRF provision with a
// next hop gw and the neighbor entry of gw.
func prodtplGatewayView(ctx context.Context, gw string) ([]string, error) {
	var routes []struct {
		Dst      string `json:"dst"`
		Gateway  string `json:"gateway"`
		Nexthops []struct {
			Gateway string `json:"gateway"`
		} `json:"nexthops"`
	}
	if err := prodtplJSON(ctx, prodtplDCGW1, &routes, "ip", "-j", "-6", "route", "show", "table", "100"); err != nil {
		return nil, err
	}
	var view []string
	for _, r := range routes {
		gws := []string{r.Gateway}
		for _, nh := range r.Nexthops {
			gws = append(gws, nh.Gateway)
		}
		if slices.Contains(gws, gw) {
			view = append(view, "route "+r.Dst+" via "+gw)
		}
	}
	var neighs []struct {
		Dst    string `json:"dst"`
		LLAddr string `json:"lladdr"`
	}
	if err := prodtplJSON(ctx, prodtplDCGW1, &neighs, "ip", "-j", "-6", "neigh", "show", "dev", "br3000"); err != nil {
		return nil, err
	}
	for _, n := range neighs {
		if n.Dst == gw {
			view = append(view, "neigh "+gw+" lladdr "+n.LLAddr)
		}
	}
	slices.Sort(view)
	return view, nil
}

// The servers reach the IPv6 CAPRF mock over the Type-5 default, from
// their lo /128 (#630 section 4.3).
func TestProdTplType5CAPRFOverIPv6(t *testing.T) {
	for _, node := range prodtplServers {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		var out string
		var err error
		for {
			out, err = prodtplExec(ctx, prodtplContainer(node), "wget", "-q", "-O", "-", "-T", "3", "http://[2001:db8:c0::10]/health")
			if err == nil && out == "ok" {
				break
			}
			select {
			case <-ctx.Done():
				cancel()
				t.Fatalf("%s: GET caprf6 /health = %q, %v", node.name, out, err)
			case <-time.After(2 * time.Second):
			}
		}
		logs, err := exec.CommandContext(ctx, "docker", "logs", prodtplCAPRF6).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("caprf6 logs: %v", err)
		}
		if lo := prodtplV6Of[node.name].lo; !strings.Contains(string(logs), lo+` "GET /health`) {
			t.Fatalf("%s: caprf6 has no request from %s:\n%s", node.name, lo, logs)
		}
	}
}

// A Type-5 route for a prefix that a server has connected in the L3VNI
// VRF (here the management subnet on eth0, in the default VRF like the
// L3VNI) does not win over the connected route, like zebra. dcgw01
// originates the management subnet as an IPv4 Type-5 route. On every
// server the kernel must still send the subnet to eth0 (ip route get);
// BOOTy installs its Type-5 route next to it with metric 20 (protocol
// bgp), which takes over only if the address goes away. Before, BOOTy
// installed IPv4 Type-5 routes with metric 0, which replaced the
// connected route (and with it the path to the CAPRF mock).
func TestProdTplType5ConnectedPrefixKept(t *testing.T) {
	const mgmt = "172.31.250.0/24"
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	conf := func(ctx context.Context, no string) {
		t.Helper()
		args := []string{"vtysh", "-c", "configure terminal",
			"-c", no + "ip route " + mgmt + " blackhole vrf provision",
			"-c", "router bgp 4210000017 vrf provision",
			"-c", "address-family ipv4 unicast", "-c", no + "network " + mgmt, "-c", "exit-address-family",
			"-c", "address-family l2vpn evpn", "-c", no + "advertise ipv4 unicast"}
		if out, err := prodtplExec(ctx, prodtplDCGW1, args...); err != nil {
			t.Fatalf("dcgw01 %snetwork %s: %v\n%s", no, mgmt, err, out)
		}
	}
	conf(ctx, "")
	t.Cleanup(func() {
		// The test's context is canceled by then.
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		conf(cctx, "no ")
	})
	// The route reaches the servers: the shadow node has it in its EVPN table.
	for {
		out, err := prodtplExec(ctx, prodtplShadow, "vtysh", "-c", "show bgp l2vpn evpn route type prefix")
		if err == nil && strings.Contains(out, "[172.31.250.0]") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the Type-5 route for %s did not reach the shadow node: %v\n%s", mgmt, err, out)
		case <-time.After(time.Second):
		}
	}
	time.Sleep(5 * time.Second) // BOOTy imports it within a second
	for _, node := range prodtplServers {
		c := prodtplContainer(node)
		out, err := prodtplExec(ctx, c, "ip", "-j", "route", "get", "172.31.250.10")
		if err != nil {
			t.Fatalf("%s route get: %v", node.name, err)
		}
		var got []struct {
			Dev string `json:"dev"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || got[0].Dev != "eth0" {
			t.Errorf("%s: ip route get 172.31.250.10 = %s (%v), want the connected route on eth0", node.name, out, err)
		}
		if node.name == "shadow" {
			continue // zebra keeps the BGP route out of the kernel
		}
		out, err = prodtplExec(ctx, c, "ip", "-j", "route", "show", "table", "main", mgmt, "proto", "bgp")
		if err != nil {
			t.Fatalf("%s routes: %v", node.name, err)
		}
		var bgp []struct {
			Metric int `json:"metric"`
		}
		if err := json.Unmarshal([]byte(out), &bgp); err != nil || len(bgp) != 1 || bgp[0].Metric != 20 {
			t.Errorf("%s: Type-5 routes for %s = %s (%v), want one with metric 20", node.name, mgmt, out, err)
		}
	}
}
