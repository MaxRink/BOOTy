//go:build e2e_prodtpl

// Route-maps of the production template (#630 C13, C14), made visible by
// an eBGP test peer (AS 65510) on a third uplink of BOOTy and of the
// shadow FRR node. The template applies TAG-FABRIC-IN to the leaf and
// DENY-TAG-FABRIC-OUT to the leaf and the test peer.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	prodtplTester       = "clab-booty-prodtpl-lab-tester"
	prodtplTesterPrefix = "198.18.0.0/24"
)

// prodtplTesterPorts are the tester interfaces to BOOTy and the shadow.
var prodtplTesterPorts = map[string]string{"booty": "eth1", "shadow": "eth2"}

// prodtplReceived returns the prefixes the tester received from a port,
// before its own policy (soft-reconfiguration inbound).
func prodtplReceived(ctx context.Context, port string) ([]string, error) {
	out, err := prodtplExec(ctx, prodtplTester, "vtysh", "-c", "show bgp ipv4 unicast neighbors "+port+" received-routes json")
	if err != nil {
		return nil, err
	}
	var r struct {
		ReceivedRoutes map[string]any `json:"receivedRoutes"`
	}
	if err := json.Unmarshal([]byte(jsonPart(out)), &r); err != nil {
		return nil, fmt.Errorf("decode received routes of %s: %w", port, err)
	}
	var prefixes []string
	for p := range r.ReceivedRoutes {
		// FRR also sends the best path back to the peer it came from (the
		// receiver drops it, its AS is in the path); GoBGP does not. The
		// tester's own prefix is not part of the comparison.
		if p != prodtplTesterPrefix {
			prefixes = append(prefixes, p)
		}
	}
	slices.Sort(prefixes)
	return prefixes, nil
}

// prodtplTesterRoute returns the next hop devices of 198.18.0.0/24 in the
// underlay VRF of a server.
func prodtplTesterRoute(ctx context.Context, node prodtplNode) ([]string, error) {
	args := []string{"ip", "-j", "route", "show", "table", "10", prodtplTesterPrefix}
	if node.name == "shadow" {
		args = []string{"ip", "-j", "route", "show", "vrf", "Vrf_underlay", prodtplTesterPrefix}
	}
	out, err := prodtplExec(ctx, prodtplContainer(node), args...)
	if err != nil {
		return nil, err
	}
	var routes []struct {
		Dev      string `json:"dev"`
		Nexthops []struct {
			Dev string `json:"dev"`
		} `json:"nexthops"`
	}
	if strings.TrimSpace(out) == "" {
		out = "[]"
	}
	if err := json.Unmarshal([]byte(out), &routes); err != nil {
		return nil, fmt.Errorf("decode %s route: %w", node.name, err)
	}
	var devs []string
	for _, r := range routes {
		if r.Dev != "" {
			devs = append(devs, r.Dev)
		}
		for _, nh := range r.Nexthops {
			devs = append(devs, nh.Dev)
		}
	}
	slices.Sort(devs)
	return devs, nil
}

// DENY-TAG-FABRIC-OUT (mirrors FRR bgp_route_map topo2
// test_add_and_remove_community_list_referenced_by_rmap_p0): the routes
// learned from the leaf carry 65169:200 from TAG-FABRIC-IN, so the
// servers send only their own loopback to the test peer.
func TestProdTplRouteMapDenyTaggedOut(t *testing.T) {
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplBootyNode} {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		want := []string{node.underlay + "/32"}
		var got []string
		var err error
		for {
			got, err = prodtplReceived(ctx, prodtplTesterPorts[node.name])
			if err == nil && reflect.DeepEqual(got, want) {
				break
			}
			select {
			case <-ctx.Done():
				cancel()
				t.Fatalf("%s: the tester received %v (%v), want %v", node.name, got, err, want)
			case <-time.After(2 * time.Second):
			}
		}
		cancel()
	}
}

// TAG-FABRIC-IN (mirrors FRR bgp_route_map topo2
// test_set_localpref_weight_to_ebgp_and_med_to_ibgp_peers_p0): the leaf
// path to 198.18.0.0/24 gets local-pref 90, so the longer path from the
// test peer (local-pref 100) wins and is the only next hop.
func TestProdTplRouteMapTagIn(t *testing.T) {
	want := []string{"ens1f2np2"}
	for _, node := range []prodtplNode{prodtplShadowNode, prodtplBootyNode} {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		var got []string
		var err error
		for {
			got, err = prodtplTesterRoute(ctx, node)
			if err == nil && reflect.DeepEqual(got, want) {
				break
			}
			select {
			case <-ctx.Done():
				cancel()
				t.Fatalf("%s: %s via %v (%v), want %v", node.name, prodtplTesterPrefix, got, err, want)
			case <-time.After(2 * time.Second):
			}
		}
		cancel()
	}
}

// "bgp ebgp-requires-policy" stays on in the default VRF router of both
// servers (only Vrf_underlay has "no bgp ebgp-requires-policy"). The
// tester session there comes up, but like FRR, BOOTy neither accepts the
// tester prefix nor sends routes to the tester.
func TestProdTplEBGPRequiresPolicy(t *testing.T) {
	for _, tt := range []struct {
		node prodtplNode
		port string
	}{{prodtplShadowNode, "eth4"}, {prodtplBootyNode, "eth3"}} {
		t.Run(tt.node.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
			defer cancel()
			for {
				n, err := prodtplNeighbor(ctx, prodtplTester, tt.port)
				if err == nil && n["bgpState"] == "Established" {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("tester session on %s not established: %v %v", tt.port, n["bgpState"], err)
				case <-time.After(2 * time.Second):
				}
			}
			// Updates go out within a few seconds of the session.
			time.Sleep(15 * time.Second)
			got, err := prodtplReceived(ctx, tt.port)
			if err != nil || len(got) != 0 {
				t.Errorf("tester received %v (%v) from %s, want nothing", got, err, tt.node.name)
			}
			out, err := prodtplExec(ctx, prodtplContainer(tt.node), "ip", "route", "show", prodtplTesterPrefix)
			if err != nil || strings.Contains(out, "ens1f3np3") {
				t.Errorf("%s main table has %q (%v), want no route over ens1f3np3", tt.node.name, out, err)
			}
		})
	}
}
