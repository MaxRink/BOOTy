package network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Legacy evidence: common.sh have_network probes INIT_URL with
// `curl --insecure`, so a CAPRF endpoint with an untrusted certificate is
// reachable.
func TestWaitForHTTPSkipTLSVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Cleanup(func() { SetSkipTLSVerify(false) })

	SetSkipTLSVerify(false)
	if err := WaitForHTTP(context.Background(), srv.URL, 1500*time.Millisecond); err == nil {
		t.Fatal("WaitForHTTP() succeeded against an untrusted certificate with verification on")
	}

	SetSkipTLSVerify(true)
	if err := WaitForHTTP(context.Background(), srv.URL, 5*time.Second); err != nil {
		t.Fatalf("WaitForHTTP() = %v, want success with verification off", err)
	}
}

func TestNewHTTPClientUsesEnvironmentProxy(t *testing.T) {
	transport, ok := NewHTTPClient(0).Transport.(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatal("NewHTTPClient transport does not honor proxy environment variables")
	}
}
