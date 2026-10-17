//go:build e2e_prodtpl

// Fault injection in the production template lab (#630 section 4.3).
package integration

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"
)

// bgpd of dcgw01 crashes and watchfrr restarts it (mirrors FRR
// bgp_evpn_rt5 and bgp_gr topotests that kill bgpd). dcgw01 is a route
// reflector and the gateway of the L3VNI: every server must withdraw its
// Type-5 routes, gateway neighbor entry and FDB entry, keep the routes of
// the other servers (reflected by dcgw02), and install everything again
// when dcgw01 is back, like the shadow node: each server withdraws within
// max(shadow + 2 s, 5 s) and recovers within shadow + 10 s.
func TestProdTplRRRestart(t *testing.T) {
	gwRoute := "route default via ::ffff:198.51.100.1 dev br.provision [onlink] proto bgp metric 20"
	otherRoute := "route other-lo via ::ffff:other-vtep dev br.provision [onlink] proto bgp metric 20"
	prodtplWaitL3VNI(t)

	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	if out, err := prodtplExec(ctx, prodtplDCGW1, "pkill", "-9", "bgpd"); err != nil {
		t.Fatalf("kill bgpd on dcgw01: %v: %s", err, out)
	}
	start := time.Now()
	// The outage lasts until dcgw01 peers with the leaf again (8 to 70 s
	// in the lab), so all servers are polled together.
	withdrawn := map[string]time.Duration{}
	for len(withdrawn) < len(prodtplServers) {
		for _, node := range prodtplServers {
			if _, ok := withdrawn[node.name]; ok {
				continue
			}
			got, err := prodtplL3VNIState(ctx, node)
			if err != nil || slices.Contains(got, gwRoute) {
				continue
			}
			if slices.Contains(got, "neigh ::ffff:198.51.100.1 lladdr "+prodtplGatewayMAC+" [NOARP]") ||
				slices.Contains(got, "fdb "+prodtplGatewayMAC+" dst 198.51.100.1") {
				t.Errorf("%s withdrew the dcgw01 routes but kept the gateway entries: %q", node.name, got)
			}
			// The other servers' routes come through dcgw02.
			others := 0
			for _, s := range got {
				if s == otherRoute {
					others++
				}
			}
			if others != 2 {
				t.Errorf("%s lost the routes of the other servers during the outage: %q", node.name, got)
			}
			withdrawn[node.name] = time.Since(start)
			t.Logf("%s: dcgw01 routes withdrawn %v after the crash", node.name, withdrawn[node.name].Round(10*time.Millisecond))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("dcgw01 routes not withdrawn on every server: %v", withdrawn)
		case <-time.After(200 * time.Millisecond):
		}
	}

	maxWithdraw := max(withdrawn["shadow"]+2*time.Second, 5*time.Second)
	for name, d := range withdrawn {
		if d > maxWithdraw {
			t.Errorf("%s withdrew the dcgw01 routes %v after the crash, want within %v (shadow %v)", name, d, maxWithdraw, withdrawn["shadow"])
		}
	}

	// Recovery: all servers are polled together until their L3VNI state is
	// complete again.
	recovered := map[string]time.Duration{}
	for len(recovered) < len(prodtplServers) {
		for _, node := range prodtplServers {
			if _, ok := recovered[node.name]; ok {
				continue
			}
			if got, err := prodtplL3VNIState(ctx, node); err == nil && reflect.DeepEqual(got, prodtplL3VNIWant(node)) {
				recovered[node.name] = time.Since(start)
				t.Logf("%s: recovered %v after the crash", node.name, recovered[node.name].Round(10*time.Millisecond))
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("not every server recovered: %v", recovered)
		case <-time.After(200 * time.Millisecond):
		}
	}
	for _, name := range []string{"booty", "native"} {
		if d := recovered[name]; d > recovered["shadow"]+10*time.Second {
			t.Errorf("%s recovered %v after the crash, want within shadow + 10 s (shadow %v)", name, d, recovered["shadow"])
		}
	}
	want := sessionView{State: "Established", RemoteAS: prodtplASN, LocalAS: prodtplASN,
		HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"l2VpnEvpn"}}
	for _, node := range prodtplServers {
		waitView(t, prodtplDCGW1, node.underlay, want)
	}
	prodtplWaitL3VNI(t)
}
