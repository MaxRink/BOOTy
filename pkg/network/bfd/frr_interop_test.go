//go:build bfd_frr

package bfd

import (
	"log/slog"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"
)

// A session with the fabric timers (150/150/3) against a real FRR bfdd
// (BFD_FRR_PEER, run by test/e2e/bfd/frr-interop.sh) must come Up and
// never go Down again within 2 s of an Up for BFD_FRR_DURATION (default
// 60 s). The script shuts the FRR peer down and up once meanwhile; that
// Down is not a flap, the session must come back and stay Up.
//
// Kindmetal (FRR 10.2.1 leaves) saw such flaps: Up, then Down with diag 3
// about 450 ms later, over and over. FRR advertises its slow Required Min
// RX until its own Poll completes and then detects in 3 x 150 ms, so the
// next packet must follow the lower interval at once.
func TestFRRInterop(t *testing.T) {
	peer := netip.MustParseAddr(os.Getenv("BFD_FRR_PEER"))
	duration := 60 * time.Second
	if d, err := time.ParseDuration(os.Getenv("BFD_FRR_DURATION")); err == nil {
		duration = d
	}
	var mu sync.Mutex
	var ups, flaps, downs int
	var lastUp time.Time
	m := NewManager(slog.Default(), func(_ *Peer, _, to State, diag Diag) {
		mu.Lock()
		defer mu.Unlock()
		switch to {
		case StateUp:
			ups++
			lastUp = time.Now()
		case StateDown:
			downs++
			if !lastUp.IsZero() && time.Since(lastUp) < 2*time.Second {
				flaps++
				t.Logf("flap: Down (diag %v) %s after Up", diag, time.Since(lastUp).Round(time.Millisecond))
			}
		}
	})
	t.Cleanup(m.Close)
	if err := m.Add(&Peer{Name: "frr", Addr: peer, Timers: Timers{
		DesiredMinTx: 150 * time.Millisecond, RequiredMinRx: 150 * time.Millisecond, DetectMult: 3,
	}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(duration)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("ups %d, downs %d, flaps %d in %s", ups, downs, flaps, duration)
	if ups == 0 || flaps > 0 {
		t.Fatalf("ups %d, flaps %d; want Up and no flap", ups, flaps)
	}
}
