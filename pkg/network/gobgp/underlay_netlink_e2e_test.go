//go:build linux && linux_e2e

package gobgp

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/telekom/BOOTy/pkg/network"
)

// Setup starts the peer restarter and Teardown stops it (root, in a
// network namespace; numbered mode needs no NICs).
func TestUnderlaySetupStartsRestarter(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("test requires root (run with sudo)")
	}
	enterTestNetns(t)
	u := NewUnderlayTier(&Config{
		ASN: 65000, RouterID: "192.0.2.17", PeerMode: network.PeerModeNumbered,
		NeighborAddrs: []string{"192.0.2.1"}, RemoteASN: 65001, ListenPort: -1,
	})
	u.log = slog.Default()
	ctx := context.Background()
	if err := u.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if u.stopRestart == nil {
		_ = u.Teardown(ctx)
		t.Fatal("Setup did not start the peer restarter")
	}
	if err := u.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if u.stopRestart != nil {
		t.Fatal("Teardown did not stop the peer restarter")
	}
}
