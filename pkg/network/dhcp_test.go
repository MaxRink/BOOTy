//go:build linux

package network

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dhclient "github.com/digineo/go-dhclient"
	"github.com/vishvananda/netlink"
)

func TestDHCPMode_Setup(t *testing.T) {
	d := &DHCPMode{}
	// DHCP setup is expected to fail in test environments (no real interfaces).
	// We just verify it doesn't panic.
	_ = d.Setup(context.Background(), &Config{})
}

func TestDHCPMode_Teardown(t *testing.T) {
	d := &DHCPMode{}
	if err := d.Teardown(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForHTTP_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := WaitForHTTP(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitForHTTP_Timeout(t *testing.T) {
	// Use localhost with a port nothing is listening on — connection refused is instant.
	err := WaitForHTTP(context.Background(), "http://127.0.0.1:19", 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForHTTP_EmptyTarget(t *testing.T) {
	err := WaitForHTTP(context.Background(), "", 1*time.Second)
	if err == nil {
		t.Fatal("expected error for empty target")
	}
}

func TestWaitForHTTP_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := WaitForHTTP(ctx, "http://192.0.2.1:1", 5*time.Second)
	if err == nil {
		t.Fatal("expected context cancel error")
	}
}

func TestWaitForHTTP_AuthUnauthorized(t *testing.T) {
	// A 401 response proves network connectivity — the server is reachable
	// even though the request is not authenticated.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := WaitForHTTP(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("expected 401 to count as connectivity, got error: %v", err)
	}
}

func TestDHCPMode_WaitForConnectivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &DHCPMode{}
	err := d.WaitForConnectivity(context.Background(), srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDHCPSetup_ContextCancelPropagates verifies that canceling the context
// terminates all concurrent NIC probes promptly.
// In environments with no physical NICs, Setup returns immediately with an
// error — that case is also valid and proves no blocking.
func TestDHCPSetup_ContextCancelPropagates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	d := &DHCPMode{}
	_ = d.Setup(ctx, &Config{})
	elapsed := time.Since(start)

	if elapsed > 700*time.Millisecond {
		t.Fatalf("Setup took %v — context cancellation did not propagate promptly", elapsed)
	}
}

func makeLease(ip, mask string) *dhclient.Lease {
	fixedIP := net.ParseIP(ip).To4()
	_, ipnet, _ := net.ParseCIDR(ip + "/" + mask)
	return &dhclient.Lease{
		FixedAddress: fixedIP,
		Netmask:      ipnet.Mask,
	}
}

func TestOnBoundWith_FirstLeaseWins(t *testing.T) {
	var addrDelCalls atomic.Int32
	addrAddOK := func(_ netlink.Link, _ *netlink.Addr) error { return nil }
	addrDelNoop := func(_ netlink.Link, _ *netlink.Addr) error {
		addrDelCalls.Add(1)
		return nil
	}

	var winner atomic.Int32
	leased1 := make(chan struct{}, 1)
	leased2 := make(chan struct{}, 1)

	d := &DHCPMode{}
	cb1 := d.onBoundWith(nil, "eth0", leased1, &winner, addrAddOK, addrDelNoop)
	cb2 := d.onBoundWith(nil, "eth1", leased2, &winner, addrAddOK, addrDelNoop)

	lease := makeLease("192.168.1.10", "24")

	cb1(lease)
	cb2(lease)

	if len(leased1) != 1 {
		t.Error("first goroutine should have signaled on leased channel")
	}
	if len(leased2) != 0 {
		t.Error("second goroutine should not signal — first already won")
	}
	if winner.Load() != 1 {
		t.Errorf("winner not set, got %d", winner.Load())
	}
	if addrDelCalls.Load() != 1 {
		t.Errorf("loser should have called addrDel once, got %d", addrDelCalls.Load())
	}
}

func TestOnBoundWith_AddrAddFailure_AllowsNextToWin(t *testing.T) {
	failErr := errors.New("addrAdd kernel error")
	addrAddFail := func(_ netlink.Link, _ *netlink.Addr) error { return failErr }
	addrAddOK := func(_ netlink.Link, _ *netlink.Addr) error { return nil }
	addrDelNoop := func(_ netlink.Link, _ *netlink.Addr) error { return nil }

	var winner atomic.Int32
	leased1 := make(chan struct{}, 1)
	leased2 := make(chan struct{}, 1)

	d := &DHCPMode{}
	cb1 := d.onBoundWith(nil, "eth0", leased1, &winner, addrAddFail, addrDelNoop)
	cb2 := d.onBoundWith(nil, "eth1", leased2, &winner, addrAddOK, addrDelNoop)

	lease := makeLease("10.0.0.5", "24")

	cb1(lease)

	if len(leased1) != 0 {
		t.Error("first goroutine should not win when AddrAdd fails")
	}
	if winner.Load() != 0 {
		t.Errorf("winner slot must remain 0 after AddrAdd failure, got %d", winner.Load())
	}

	cb2(lease)

	if len(leased2) != 1 {
		t.Error("second goroutine should win after first goroutine's AddrAdd failure")
	}
	if winner.Load() != 1 {
		t.Errorf("winner should be set after second goroutine succeeds, got %d", winner.Load())
	}
}
