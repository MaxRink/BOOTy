//go:build linux

package gobgp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestWaitForNICsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	u := NewUnderlayTier(&Config{})
	_, err := u.waitForNICs(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForNICs error = %v, want context.Canceled", err)
	}
}

func TestWaitForConfiguredNICsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	u := NewUnderlayTier(&Config{Interfaces: []string{"eth-test0"}})
	_, err := u.waitForNamedNICs(ctx, u.cfg.Interfaces)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForNamedNICs error = %v, want context.Canceled", err)
	}
}

func TestDiscoverLinkLocalPeerContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := discoverLinkLocalPeer(ctx, "eth-test0")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("discoverLinkLocalPeer error = %v, want context.Canceled", err)
	}
}

func TestTeardownStopsPeriodicRA(t *testing.T) {
	u := NewUnderlayTier(&Config{})
	u.startPeriodicRA()
	if err := u.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown() error = %v", err)
	}

	select {
	case <-u.raDone:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic RA goroutine still running after Teardown")
	}
}

// The router lifetime of the periodic RAs is zebra's default for the
// bgpd unnumbered RA interval of 10 s: 30 s.
func TestRouterAdvertisementLifetimeLikeZebra(t *testing.T) {
	if raLifetime != 30 {
		t.Fatalf("RA router lifetime %d s, want 30 s like zebra", raLifetime)
	}
	if b := routerAdvertisement(net.HardwareAddr{2, 0, 0, 0, 0, 1}, raLifetime); b[6] != 0 || b[7] != 30 {
		t.Fatalf("RA lifetime bytes %x, want 001e", b[6:8])
	}
}

// TestRouterAdvertisementPacket checks the RA of RFC 4861 §4.2 that BOOTy
// sends on the unnumbered NICs.
func TestRouterAdvertisementPacket(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x54, 0, 0, 0, 1}
	for _, lifetime := range []uint16{raLifetime, 0} {
		b := routerAdvertisement(mac, lifetime)
		want := []byte{134, 0, 0, 0, 64, 0, byte(lifetime >> 8), byte(lifetime), 0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 0x02, 0x54, 0, 0, 0, 1}
		if !bytes.Equal(b, want) {
			t.Errorf("lifetime %d: RA = %x, want %x", lifetime, b, want)
		}
	}
}

// TestPeriodicRAEndsWithZeroLifetime checks RFC 4861 §6.2.5: when the RAs
// stop, every NIC gets a final RA with Router Lifetime 0.
func TestPeriodicRAEndsWithZeroLifetime(t *testing.T) {
	var mu sync.Mutex
	sent := map[string][]uint16{}
	orig := raSend
	t.Cleanup(func() { raSend = orig })
	raSend = func(iface string, lifetime uint16) bool {
		if lifetime == 0 {
			// A slow final RA: Teardown must still wait for it.
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		sent[iface] = append(sent[iface], lifetime)
		mu.Unlock()
		return true
	}
	u := NewUnderlayTier(&Config{})
	u.nics = []string{"eth0", "eth1"}
	u.startPeriodicRA()
	// No wait here: Teardown itself waits until the final RA is sent.
	if err := u.Teardown(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, nic := range u.nics {
		got := sent[nic]
		if len(got) < 2 || got[0] != raLifetime || got[len(got)-1] != 0 {
			t.Errorf("%s: lifetimes %v, want %d first and 0 last", nic, got, raLifetime)
		}
	}
}
