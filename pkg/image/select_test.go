package image

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSelectBestSource(t *testing.T) {
	t.Helper()

	tests := []struct {
		name    string
		urls    []string
		wantErr bool
	}{
		{
			name:    "empty urls",
			urls:    []string{},
			wantErr: true,
		},
		{
			name:    "single url returned as-is",
			urls:    []string{"http://example.com/image.raw"},
			wantErr: false,
		},
		{
			name:    "single oci url returned as-is",
			urls:    []string{"oci://registry.example.com/image:latest"},
			wantErr: false,
		},
		{
			name:    "all oci urls returns first",
			urls:    []string{"oci://a.example.com/img:v1", "oci://b.example.com/img:v1"},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectBestSource(context.Background(), tc.urls)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == "" {
				t.Fatal("expected non-empty URL")
			}
		})
	}
}

func TestSelectBestSourcePicksFastest(t *testing.T) {
	t.Helper()

	// Slow server — delays response using a channel.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	// Fast server — responds immediately.
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fast.Close()

	got, err := SelectBestSource(context.Background(), []string{slow.URL + "/image.raw", fast.URL + "/image.raw"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != fast.URL+"/image.raw" {
		t.Errorf("expected fast URL %s, got %s", fast.URL+"/image.raw", got)
	}
}

func TestSelectBestSourceFallsBackOnFailure(t *testing.T) {
	t.Helper()

	// Broken server — returns 500.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	// Working server.
	working := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer working.Close()

	got, err := SelectBestSource(context.Background(), []string{broken.URL + "/img", working.URL + "/img"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != working.URL+"/img" {
		t.Errorf("expected working URL %s, got %s", working.URL+"/img", got)
	}
}

func TestSelectBestSourceAllFailed(t *testing.T) {
	t.Helper()

	// Both servers return errors.
	s1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer s1.Close()
	s2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer s2.Close()

	// Should fall back to first URL when all probes fail.
	got, err := SelectBestSource(context.Background(), []string{s1.URL + "/img", s2.URL + "/img"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != s1.URL+"/img" {
		t.Errorf("expected first URL as fallback %s, got %s", s1.URL+"/img", got)
	}
}

func TestProbeURLTreatsGETErrorsAsUnreachable(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		if _, err := probeURL(context.Background(), srv.URL+"/image.raw"); err == nil {
			t.Errorf("probeURL accepted status %d, the GET download would fail", status)
		}
		srv.Close()
	}
}

func TestRedactHost(t *testing.T) {
	t.Helper()

	tests := []struct {
		input string
		want  string
	}{
		{"http://example.com/image.raw", "example.com"},
		{"https://user:pass@registry.io/img", "registry.io"},
		{"://invalid", "<invalid-url>"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := redactHost(tc.input)
			if got != tc.want {
				t.Errorf("redactHost(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// Legacy evidence: provision.sh pick_best_source probes with
// `curl --range 0-0` (a GET) because presigned URLs bound to GET reject HEAD.
func TestSelectBestSourceProbesPresignedURLsWithRangedGET(t *testing.T) {
	var presignedRange string
	presigned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		presignedRange = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	defer presigned.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(300 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	got, err := SelectBestSource(context.Background(), []string{slow.URL + "/img", presigned.URL + "/img?X-Amz-Signature=abc"})
	if err != nil {
		t.Fatal(err)
	}
	if got != presigned.URL+"/img?X-Amz-Signature=abc" {
		t.Fatalf("SelectBestSource() = %s, want the faster presigned source", got)
	}
	if presignedRange != "bytes=0-0" {
		t.Fatalf("probe Range header = %q, want bytes=0-0", presignedRange)
	}
}

func TestProbeURLRejectsStalledBody(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := probeURL(ctx, stalled.URL+"/image.raw"); err == nil {
		t.Fatal("probeURL accepted a source that sent headers but no image byte")
	}
}
