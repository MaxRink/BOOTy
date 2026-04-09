//go:build linux

package network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

// TestDHCPSetup_ParallelContextCancel verifies that canceling the context
// terminates all concurrent NIC probes promptly (well under N×15 s).
// In environments with no physical NICs, Setup returns immediately with an
// error — that case is also valid and proves no blocking.
func TestDHCPSetup_ParallelContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	d := &DHCPMode{}
	_ = d.Setup(ctx, &Config{})
	elapsed := time.Since(start)

	// Must complete well within 15 s (the per-NIC sequential worst-case).
	// If probing were serial, 2+ NICs would take 30 s+; parallel caps at ~15 s.
	if elapsed > 2*time.Second {
		t.Fatalf("Setup took %v — context cancellation did not propagate promptly", elapsed)
	}
}
