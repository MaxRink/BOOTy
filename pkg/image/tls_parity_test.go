package image

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/network"
)

// Legacy evidence: provision.sh downloads the image with `curl --insecure`.
func TestRefreshHTTPClientsAppliesSkipTLSVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "image")
	}))
	defer srv.Close()
	oldBackoff := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() {
		retryBackoffBase = oldBackoff
		network.SetSkipTLSVerify(false)
		RefreshHTTPClients()
	})

	network.SetSkipTLSVerify(false)
	RefreshHTTPClients()
	if body, err := httpGetWithRetry(context.Background(), srv.URL); err == nil {
		_ = body.Close()
		t.Fatal("download succeeded against an untrusted certificate with verification on")
	}

	network.SetSkipTLSVerify(true)
	RefreshHTTPClients()
	body, err := httpGetWithRetry(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("download with verification off: %v", err)
	}
	_ = body.Close()
}
