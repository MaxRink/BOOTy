//go:build e2e_prodtpl

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// prodtplCapKeys are the capabilities of "show bgp neighbors json" that
// BOOTy must send like FRR. Known deviations of GoBGP are left out:
// enhancedRouteRefresh and pathsLimit (not implemented),
// the softwareVersion string, the Restart State (R) bit, and the family tuples of
// long-lived graceful restart, which GoBGP leaves out in helper mode (FRR
// sends them with stale time 0) (docs/networking.md, BGP capabilities).
var prodtplCapKeys = []string{
	"4byteAs", "addPath", "extendedMessage", "extendedNexthop", "gracefulRestart", "gracefulRestartRemoteTimerMsecs",
	"longLivedGracefulRestart", "multiprotocolExtensions", "routeRefresh",
}

// prodtplCaps returns the compared capabilities that a FRR node received
// from a neighbor, the received hostname and the remote graceful restart
// mode.
func prodtplCaps(ctx context.Context, container, neighbor string) (map[string]any, error) {
	out, err := prodtplExec(ctx, container, "vtysh", "-c", "show bgp vrf all neighbors "+neighbor+" json")
	if err != nil {
		return nil, err
	}
	var all map[string]any
	if err := json.Unmarshal([]byte(jsonPart(out)), &all); err != nil {
		return nil, fmt.Errorf("decode %s neighbor %s: %w", container, neighbor, err)
	}
	n := findNeighbor(all, neighbor)
	if n == nil {
		return nil, fmt.Errorf("%s has no neighbor %s", container, neighbor)
	}
	caps, _ := n["neighborCapabilities"].(map[string]any)
	view := map[string]any{}
	for _, k := range prodtplCapKeys {
		view[k] = caps[k]
	}

	host, _ := caps["hostName"].(map[string]any)
	view["rcvHostName"] = host["rcvHostName"]
	gr, _ := n["gracefulRestartInfo"].(map[string]any)
	view["remoteGrMode"] = gr["remoteGrMode"]
	// The N bit (RFC 8538). The R bit is a known deviation: FRR sets it
	// for the first restart time after bgpd starts.
	view["nBit"] = gr["nBit"]
	view["state"] = n["bgpState"]
	return view, nil
}

// findNeighbor finds the neighbor entry in the per-VRF output.
func findNeighbor(v any, neighbor string) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if n, ok := m[neighbor].(map[string]any); ok {
		return n
	}
	for _, c := range m {
		if n := findNeighbor(c, neighbor); n != nil {
			return n
		}
	}
	return nil
}

// The leaf (eBGP) and dcgw01 (iBGP) receive the same OPEN capabilities
// from BOOTy as from the shadow FRR node: graceful restart with restart
// time 120 and the family tuples (helper) and the N bit of "frr version
// 8.2.2" (off), long-lived graceful restart,
// add-path receive, route refresh, 4-octet AS and the hostname.
func TestProdTplOpenCapabilities(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	type view struct{ container, neighbor string }
	views := func(n prodtplNode) []view {
		return []view{{prodtplLeaf, n.leafPorts[0]}, {prodtplDCGW1, n.underlay}}
	}
	want := map[view]map[string]any{}
	for _, v := range views(prodtplShadowNode) {
		var got map[string]any
		var err error
		for got, err = prodtplCaps(ctx, v.container, v.neighbor); err != nil || got["state"] != "Established"; got, err = prodtplCaps(ctx, v.container, v.neighbor) {
			select {
			case <-ctx.Done():
				t.Fatalf("shadow at %s: %v %v", v.container, got, err)
			case <-time.After(2 * time.Second):
			}
		}
		want[v] = got
	}
	for _, node := range prodtplServers {
		if node.name == "shadow" {
			continue
		}
		for i, v := range views(node) {
			w := map[string]any{}
			for k, val := range want[views(prodtplShadowNode)[i]] {
				w[k] = val
			}
			w["rcvHostName"] = node.name
			var got map[string]any
			var err error
			for {
				got, err = prodtplCaps(ctx, v.container, v.neighbor)
				if err == nil && reflect.DeepEqual(got, w) {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("%s at %s: got %v (%v), want %v", node.name, v.container, got, err, w)
				case <-time.After(2 * time.Second):
				}
			}
		}
	}
}
