//go:build e2e_prodtpl

// BFD part of the production template lab (#630 section 3.6, C19). The
// leaf runs single-hop BFD to every uplink, the route reflectors run
// multihop BFD to every server. BOOTy (template profile "fabric"), the
// native server (BFD_TRANSMIT_MS/BFD_RECEIVE_MS) and the shadow FRR node
// must look the same from the fabric.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// bfdView is the part of a FRR "show bfd peers json" entry that must be
// the same for every server.
type bfdView struct {
	Status       string
	Multihop     bool
	RemoteTx     float64
	RemoteRx     float64
	RemoteDetect float64
}

// prodtplBFDPeers returns the BFD peers of a FRR node by interface (single
// hop) or by peer address (multihop).
func prodtplBFDPeers(ctx context.Context, container string) (map[string]bfdView, error) {
	out, err := prodtplExec(ctx, container, "vtysh", "-c", "show bfd peers json")
	if err != nil {
		return nil, err
	}
	var peers []map[string]any
	// vtysh can print warnings before the JSON document.
	if i := strings.Index(out, "\n["); i >= 0 {
		out = out[i+1:]
	} else if !strings.HasPrefix(out, "[") {
		return nil, fmt.Errorf("%s: no JSON list in %q", container, out)
	}
	if err := json.Unmarshal([]byte(out), &peers); err != nil {
		return nil, fmt.Errorf("decode %s bfd peers: %w", container, err)
	}
	views := map[string]bfdView{}
	for _, p := range peers {
		v := bfdView{}
		v.Status, _ = p["status"].(string)
		v.Multihop, _ = p["multihop"].(bool)
		v.RemoteTx, _ = p["remote-transmit-interval"].(float64)
		v.RemoteRx, _ = p["remote-receive-interval"].(float64)
		v.RemoteDetect, _ = p["remote-detect-multiplier"].(float64)
		key, _ := p["interface"].(string)
		if v.Multihop || key == "" {
			key, _ = p["peer"].(string)
		}
		views[key] = v
	}
	return views, nil
}

// At the start, the route reflectors re-key their BFD session to a server
// when the BGP session comes up ("update-source lo",
// bgp_peer_bfd_update_source), so the server's new BFD session goes Down
// in that second. FRR (the shadow node) keeps the BGP session, like bgpd's
// last_event > peer->uptime; BOOTy must too: it must not have reset an RR
// session for BFD by now (this is the first test of the lab). The RR drop
// counters are only logged: an RR also resets a session itself when the
// lab setup activates its address families after the session came up
// ("No AFI/SAFI activated for peer"), which is not BOOTy's doing.
func TestProdTplRRSessionsNotResetAtStart(t *testing.T) {
	want := sessionView{State: "Established", RemoteAS: prodtplASN, LocalAS: prodtplASN,
		HoldMsecs: 90000, KeepMsecs: 30000, Families: []string{"l2VpnEvpn"}}
	for _, rr := range []string{prodtplDCGW1, prodtplDCGW2} {
		for _, node := range prodtplServers {
			waitView(t, rr, node.underlay, want)
		}
	}
	time.Sleep(5 * time.Second) // a reset follows within a second
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, node := range prodtplServers {
		for _, rr := range []string{prodtplDCGW1, prodtplDCGW2} {
			if n, err := prodtplNeighbor(ctx, rr, node.underlay); err == nil {
				t.Logf("%s: %s session dropped %v times (%v)", rr, node.name, n["connectionsDropped"], n["lastResetDueTo"])
			}
		}
		if node.name == "shadow" {
			continue
		}
		out, err := exec.CommandContext(ctx, "docker", "logs", prodtplContainer(node)).CombinedOutput()
		if err != nil {
			t.Fatalf("%s logs: %v", node.name, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "BFD down, resetting the BGP session") && strings.Contains(line, "peer=198.51.100.") {
				t.Errorf("%s reset an RR session at the start: %s", node.name, line)
			}
		}
	}
}

// Mirrors FRR bfd_profiles_topo1 test_bfd_profile_values: the fabric sees
// the "fabric" profile (150/150/3) of every server, single-hop on the
// uplinks at the leaf and multihop at the route reflectors.
func TestProdTplBFDSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
	defer cancel()
	single := bfdView{Status: "up", RemoteTx: 150, RemoteRx: 150, RemoteDetect: 3}
	multi := single
	multi.Multihop = true
	want := map[string]map[string]bfdView{prodtplLeaf: {}, prodtplDCGW1: {}, prodtplDCGW2: {}}
	for _, node := range prodtplServers {
		for _, port := range node.leafPorts {
			want[prodtplLeaf][port] = single
		}
		want[prodtplDCGW1][node.underlay] = multi
		want[prodtplDCGW2][node.underlay] = multi
	}
	for _, c := range []string{prodtplLeaf, prodtplDCGW1, prodtplDCGW2} {
		var got map[string]bfdView
		var err error
		for {
			got, err = prodtplBFDPeers(ctx, c)
			if err == nil && reflect.DeepEqual(pick(got, want[c]), want[c]) {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("%s BFD peers (%v):\n  got  %+v\n  want %+v", c, err, got, want[c])
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func pick(got, want map[string]bfdView) map[string]bfdView {
	out := map[string]bfdView{}
	for k := range want {
		if v, ok := got[k]; ok {
			out[k] = v
		}
	}
	return out
}

// maxRestore bounds the time from a working link to ECMP again. FRR
// connects 1 s after BFD down (BGP_INIT_START_TIMER); the GoBGP default
// idle hold time after a reset is 30 s. BOOTy takes 3.6 to 4.7 s in CI; the
// bound leaves room for slow CI runners (one run took 10.02 s, with a BFD
// session that went down again right after Up, fixed in #703).
const maxRestore = 15 * time.Second

// maxSession bounds the time from the carrier back to the leaf session
// Established again.
const maxSession = 5 * time.Second

// waitRoutesWithin waits like waitRoutes, but at most maxWait, and returns
// how long it took.
func waitRoutesWithin(t *testing.T, node prodtplNode, want map[string][]string, maxWait time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		got, err := prodtplRoutes(ctx, node)
		cancel()
		if err == nil && reflect.DeepEqual(got, want) {
			return time.Since(start)
		}
		if time.Since(start) > maxWait {
			t.Fatalf("%s: routes %v (%v) after %s, want %v within %s", node.name, got, err, time.Since(start), want, maxWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Mirrors FRR bfd_topo1 test_bfd_fast_convergence: the leaf stops sending
// on one uplink while the carrier stays up. BFD (3 x 300 ms at the server)
// ends the BGP session long before the 90 s hold time, so the next hop
// leaves the VRF table. The link coming back restores ECMP within
// maxRestore, as on the shadow node.
func TestProdTplBFDFailover(t *testing.T) {
	const maxFailover = 2500 * time.Millisecond // 900 ms detection + docker exec latency
	for _, node := range prodtplServers {
		ctx, cancel := context.WithTimeout(context.Background(), prodtplTimeout)
		port := node.leafPorts[0]
		waitRoutes(t, node, prodtplRoutesVia(prodtplUplinks...))
		start := time.Now()
		if out, err := prodtplExec(ctx, prodtplLeaf, "tc", "qdisc", "add", "dev", port, "root", "netem", "loss", "100%"); err != nil {
			cancel()
			t.Fatalf("leaf %s loss: %v\n%s", port, err, out)
		}
		want := prodtplRoutesVia(prodtplUplinks[1:]...)
		for {
			got, err := prodtplRoutes(ctx, node)
			if err == nil && reflect.DeepEqual(got, want) {
				break
			}
			if time.Since(start) > maxFailover {
				_, _ = prodtplExec(context.Background(), prodtplLeaf, "tc", "qdisc", "del", "dev", port, "root")
				cancel()
				t.Fatalf("%s: routes %v (%v) %s after the leaf stopped sending, want %v within %s", node.name, got, err, time.Since(start), want, maxFailover)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("%s: next hop removed %s after the leaf stopped sending", node.name, time.Since(start).Round(10*time.Millisecond))
		if out, err := prodtplExec(ctx, prodtplLeaf, "tc", "qdisc", "del", "dev", port, "root"); err != nil {
			cancel()
			t.Fatalf("leaf %s restore: %v\n%s", port, err, out)
		}
		cancel()
		d := waitRoutesWithin(t, node, prodtplRoutesVia(prodtplUplinks...), maxRestore)
		t.Logf("%s: ECMP back %s after the leaf sends again", node.name, d.Round(10*time.Millisecond))
	}
}

// A switch port that fails for longer than the BFD detection time: carrier
// loss, then the BFD timeout, then the link back. The leaf sees the session
// Established again within maxSession, like the shadow node (2 to 3 s: the
// leaf first waits for DAD of its link-local address), and the routes are
// back within maxRestore. The shadow's kernel route shows ECMP again at
// once because zebra keeps the nexthop group of the flapped link while its
// BGP session is still down, so its route time is not compared.
func TestProdTplCarrierLossPastBFD(t *testing.T) {
	for _, node := range prodtplServers {
		t.Run(node.name, func(t *testing.T) {
			port := node.leafPorts[0]
			link := func(state string) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if out, err := prodtplExec(ctx, prodtplLeaf, "ip", "link", "set", port, state); err != nil {
					t.Fatalf("leaf %s %s: %v\n%s", port, state, err, out)
				}
			}
			waitRoutes(t, node, prodtplRoutesVia(prodtplUplinks...))
			link("down")
			t.Cleanup(func() { link("up") })
			waitRoutesWithin(t, node, prodtplRoutesVia(prodtplUplinks[1:]...), 5*time.Second)
			time.Sleep(3 * time.Second) // past the 900 ms BFD detection time
			link("up")
			start := time.Now()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				n, err := prodtplNeighbor(ctx, prodtplLeaf, port)
				cancel()
				if err == nil && n["bgpState"] == "Established" {
					break
				}
				if time.Since(start) > maxSession {
					t.Fatalf("%s: leaf session on %s %v (%v) %s after the carrier came back, want Established within %s",
						node.name, port, n["bgpState"], err, time.Since(start), maxSession)
				}
				time.Sleep(50 * time.Millisecond)
			}
			est := time.Since(start)
			d := waitRoutesWithin(t, node, prodtplRoutesVia(prodtplUplinks...), maxRestore)
			t.Logf("%s: session Established %s, ECMP back %s after the carrier came back", node.name,
				est.Round(10*time.Millisecond), (est + d).Round(10*time.Millisecond))
		})
	}
}
