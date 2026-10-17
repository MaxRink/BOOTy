//go:build e2e_prodtpl

// Long outages and session drops per fault in the production template
// lab, compared with the shadow FRR node (kindmetal acceptance round 3).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// prodtplDrops returns FRR's connectionsEstablished and connectionsDropped
// of a neighbor.
func prodtplDrops(ctx context.Context, container, neighbor string) (established, dropped int, err error) {
	n, err := prodtplNeighbor(ctx, container, neighbor)
	if err != nil {
		return 0, 0, err
	}
	e, _ := n["connectionsEstablished"].(float64)
	d, _ := n["connectionsDropped"].(float64)
	return int(e), int(d), nil
}

// prodtplWaitEstablished waits until the leaf sees the session on port
// Established and returns the time it took.
func prodtplWaitEstablished(ctx context.Context, port string) (time.Duration, error) {
	start := time.Now()
	for {
		n, err := prodtplNeighbor(ctx, prodtplLeaf, port)
		if err == nil && n["bgpState"] == "Established" {
			return time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("leaf session on %s not Established after %s: %v", port, time.Since(start), err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func prodtplLeafMAC(ctx context.Context, port string) (string, error) {
	out, err := prodtplExec(ctx, prodtplLeaf, "ip", "-j", "link", "show", port)
	if err != nil {
		return "", err
	}
	var links []struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal([]byte(out), &links); err != nil || len(links) != 1 {
		return "", fmt.Errorf("decode link %s: %v", port, err)
	}
	return links[0].Address, nil
}

// prodtplLeafPort takes a leaf port down, optionally sets its MAC (so
// that its EUI-64 link-local address changes when it comes up), waits and
// brings it up again.
func prodtplLeafPort(ctx context.Context, port, mac string, outage time.Duration) error {
	steps := [][]string{{"ip", "link", "set", port, "down"}}
	if mac != "" {
		steps = append(steps, []string{"ip", "link", "set", port, "address", mac})
	}
	for _, s := range steps {
		if out, err := prodtplExec(ctx, prodtplLeaf, s...); err != nil {
			return fmt.Errorf("%v: %w: %s", s, err, out)
		}
	}
	time.Sleep(outage)
	if out, err := prodtplExec(ctx, prodtplLeaf, "ip", "link", "set", port, "up"); err != nil {
		return fmt.Errorf("leaf %s up: %w: %s", port, err, out)
	}
	return nil
}

// A leaf port is down for longer than the 90 s hold time and comes back
// with another link-local address (a restarted switch or port with a new
// MAC). FRR peers with "neighbor PORT interface" and takes the new
// address from the RAs; BOOTy must move its GoBGP peer to it. Before,
// GoBGP rejected the leaf's connections from the new address ("Can't
// find configuration for a new passive connection") and the session never
// came back (kindmetal: single-leaf fixture, 60 s outage). Every server
// must be Established within max(shadow + 5 s, 10 s). The old MAC is put
// back the same way, with a short outage.
func TestProdTplLongOutageNewLinkLocal(t *testing.T) {
	const outage = 95 * time.Second
	var (
		mu    sync.Mutex
		times = map[string]time.Duration{}
		errs  []error
		wg    sync.WaitGroup
	)
	for i, node := range prodtplServers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), outage+prodtplTimeout)
			defer cancel()
			port := node.leafPorts[0]
			fail := func(err error) {
				mu.Lock()
				defer mu.Unlock()
				errs = append(errs, fmt.Errorf("%s: %w", node.name, err))
			}
			mac, err := prodtplLeafMAC(ctx, port)
			if err != nil {
				fail(err)
				return
			}
			if err := prodtplLeafPort(ctx, port, fmt.Sprintf("02:00:00:00:0b:%02x", i), outage); err != nil {
				fail(err)
				return
			}
			d, err := prodtplWaitEstablished(ctx, port)
			if err != nil {
				fail(err)
				return
			}
			mu.Lock()
			times[node.name] = d
			mu.Unlock()
			if err := prodtplLeafPort(ctx, port, mac, 3*time.Second); err != nil {
				fail(err)
				return
			}
			if _, err := prodtplWaitEstablished(ctx, port); err != nil {
				fail(fmt.Errorf("after the old MAC came back: %w", err))
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		t.Error(err)
	}
	limit := max(times["shadow"]+5*time.Second, 10*time.Second)
	for _, node := range prodtplServers {
		d, ok := times[node.name]
		if !ok {
			continue // reported above
		}
		t.Logf("%s: Established %s after the port came back with a new link-local address", node.name, d.Round(10*time.Millisecond))
		if d > limit {
			t.Errorf("%s: Established after %s, want within %s (shadow %s)", node.name, times[node.name], limit, times["shadow"])
		}
	}
	for _, node := range prodtplServers {
		waitRoutes(t, node, prodtplRoutesVia(prodtplUplinks...))
	}
}

// One carrier fault past the BFD detection time drops the leaf session
// exactly once, like FRR, and the dcgw01 session as often as on the shadow
// node (never with two uplinks, once with one). Before, BOOTy's BFD sent
// its next packet after the 1 s slow start interval although it advertised
// 150 ms at Up, so the leaf's bfdd timed out about 450 ms after Up and
// reset the new session a second time (kindmetal: dropped=2, legacy 1).
func TestProdTplOneDropPerFault(t *testing.T) {
	var shadow [4]int
	for _, node := range prodtplServers {
		t.Run(node.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
			defer cancel()
			port := node.leafPorts[0]
			waitRoutes(t, node, prodtplRoutesVia(prodtplUplinks...))
			// The dcgw01 session is up again before the counters are read.
			waitView(t, prodtplDCGW1, node.underlay, sessionView{State: "Established", RemoteAS: prodtplASN, LocalAS: prodtplASN,
				HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"l2VpnEvpn"}})
			counts := func() [4]int {
				le, ld, err1 := prodtplDrops(ctx, prodtplLeaf, port)
				re, rd, err2 := prodtplDrops(ctx, prodtplDCGW1, node.underlay)
				if err1 != nil || err2 != nil {
					t.Fatalf("counters: %v %v", err1, err2)
				}
				return [4]int{le, ld, re, rd}
			}
			before := counts()
			if err := prodtplLeafPort(ctx, port, "", 3*time.Second); err != nil {
				t.Fatal(err)
			}
			if _, err := prodtplWaitEstablished(ctx, port); err != nil {
				t.Fatal(err)
			}
			waitRoutes(t, node, prodtplRoutesVia(prodtplUplinks...))
			time.Sleep(10 * time.Second) // a second drop came within 1 s
			after := counts()
			got := [4]int{after[0] - before[0], after[1] - before[1], after[2] - before[2], after[3] - before[3]}
			t.Logf("%s: leaf established/dropped, dcgw01 established/dropped changed by %v", node.name, got)
			if node.name == "shadow" {
				shadow = got
			}
			if got[0] != 1 || got[1] != 1 || got != shadow {
				t.Errorf("%s: counters changed by %v, want leaf 1/1 and the shadow node's %v", node.name, got, shadow)
			}
		})
	}
}

// The config path of the old single-leaf CAPRF fixture (lab variant
// caprf-old: scalar BGP_PEER_MODE=unnumbered, the main table, one uplink,
// a leaf link-local set by hand). The leaf port is down for 60 s and comes
// back with another MAC and the hand-set fe80::100 again, like the
// kindmetal fixture after a leaf outage; there GoBGP logged "Can't find
// configuration for a new passive connection Key=fe80::100%eth1" every
// 120 s and the session never came back, the legacy ramdisk recovered in 5
// to 6 s. Runs only in that variant (it is not a CI variant).
func TestProdTplCAPRFOldOutage(t *testing.T) {
	if prodtplVariant != "caprf-old" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second+prodtplTimeout)
	defer cancel()
	port := prodtplNativeNode.leafPorts[0]
	handSet := func() {
		if out, err := prodtplExec(ctx, prodtplLeaf, "sh", "-c", "ip -6 addr add fe80::100/64 dev "+port+" 2>/dev/null || true"); err != nil {
			t.Fatalf("hand-set fe80::100: %v: %s", err, out)
		}
	}
	handSet()
	if _, err := prodtplWaitEstablished(ctx, port); err != nil {
		t.Fatal(err)
	}
	if err := prodtplLeafPort(ctx, port, "02:00:00:00:0c:01", 60*time.Second); err != nil {
		t.Fatal(err)
	}
	handSet()
	d, err := prodtplWaitEstablished(ctx, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native (caprf-old path): Established %s after the 60 s outage", d.Round(10*time.Millisecond))
	if d > 10*time.Second {
		t.Errorf("Established after %s, want within 10 s (legacy ramdisk 5 to 6 s)", d)
	}
}
