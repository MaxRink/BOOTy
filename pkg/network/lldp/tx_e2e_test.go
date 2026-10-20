//go:build linux_e2e

package lldp

import (
	"context"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

// The sender follows the kernel carrier of a veth through netlink like
// networkd 249: 4 fast frames after the carrier gain, none while the peer
// is down, and 4 fast frames again when it is up. Needs root.
func TestEmitFollowsVethCarrier(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("needs root")
	}
	la := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "lldptxa"}, PeerName: "lldptxb"}
	_ = netlink.LinkDel(la)
	if err := netlink.LinkAdd(la); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(la) })
	a, _ := netlink.LinkByName("lldptxa")
	b, _ := netlink.LinkByName("lldptxb")
	if err := netlink.LinkSetUp(a); err != nil {
		t.Fatal(err)
	}
	testState(t, 0)
	var mu sync.Mutex
	var frames []time.Time
	orig := sendFrame
	t.Cleanup(func() { sendFrame = orig })
	sendFrame = func(iface string, info *Info, build func(string, net.HardwareAddr, *Info) ([]byte, error)) ([]byte, error) {
		f, err := orig(iface, info, build)
		if err == nil {
			mu.Lock()
			frames = append(frames, time.Now())
			mu.Unlock()
		}
		return f, err
	}
	count := func(since time.Time) (n int, first time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range frames {
			if !f.Before(since) {
				if n == 0 {
					first = f.Sub(since)
				}
				n++
			}
		}
		return n, first
	}
	ctx, cancel := context.WithCancel(context.Background())
	wait := Emit(ctx, []string{"lldptxa"}, &Info{MachineID: "m", SystemName: "host1"}, slog.Default())
	t.Cleanup(func() { cancel(); wait() })

	time.Sleep(2 * time.Second) // the peer is down: no carrier
	if n, _ := count(time.Time{}); n != 0 {
		t.Fatalf("%d frames without carrier", n)
	}
	gain := func(what string) {
		t.Helper()
		if err := netlink.LinkSetUp(b); err != nil {
			t.Fatal(err)
		}
		up := time.Now()
		time.Sleep(6 * time.Second)
		// 4 frames by 4.8 s, the fifth not before 33 s; the first 0.8 to
		// 1.2 s after the netlink update, which may come up to 0.3 s after
		// the link set.
		n, first := count(up)
		mu.Lock()
		t.Logf("%s: frames at %v after the link set", what, offsets(frames, up))
		mu.Unlock()
		if n != txFastInit || first < 800*time.Millisecond || first > 1500*time.Millisecond {
			t.Fatalf("%s: %d frames, the first after %v; want 4, the first after 0.8 to 1.5 s", what, n, first)
		}
	}
	gain("first carrier")
	if err := netlink.LinkSetDown(b); err != nil {
		t.Fatal(err)
	}
	down := time.Now()
	time.Sleep(3 * time.Second)
	if n, _ := count(down); n != 0 {
		t.Fatalf("%d frames after the carrier loss", n)
	}
	gain("carrier back")
}

func offsets(at []time.Time, since time.Time) []time.Duration {
	var d []time.Duration
	for _, a := range at {
		if !a.Before(since) {
			d = append(d, a.Sub(since).Round(time.Millisecond))
		}
	}
	return d
}
