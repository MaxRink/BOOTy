//go:build linux

package gobgp

import (
	"context"
	"errors"
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
