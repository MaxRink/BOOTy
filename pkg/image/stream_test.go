package image

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/stream"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRedactURLRemovesSensitiveParts(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "credentials query fragment",
			in:   "https://user:secret@example.com/image.raw?token=abc#frag",
			want: "https://example.com/image.raw",
		},
		{
			name: "query fragment without credentials",
			in:   "https://example.com/image.raw?token=abc#frag",
			want: "https://example.com/image.raw",
		},
		{
			name: "invalid url fails closed",
			in:   "https://example.com/%zz?token=secret",
			want: "[redacted invalid URL]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURL(tt.in); got != tt.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestStreamRaw(t *testing.T) {
	data := []byte("raw image content for testing")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestStreamGzip(t *testing.T) {
	data := []byte("gzipped image content for testing")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(data)
		_ = gz.Close()
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	// Magic bytes detect gzip regardless of URL suffix now.
	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestStreamZstd(t *testing.T) {
	data := []byte("zstd image content for testing")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		zw, _ := zstd.NewWriter(w)
		_, _ = zw.Write(data)
		_ = zw.Close()
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.zst", tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestStreamLZ4(t *testing.T) {
	data := []byte("lz4 image content for testing")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		lw := lz4.NewWriter(w)
		_, _ = lw.Write(data)
		_ = lw.Close()
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.lz4", tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestStreamXZ(t *testing.T) {
	data := []byte("xz image content for testing")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		xw, _ := xz.NewWriter(w)
		_, _ = xw.Write(data)
		_ = xw.Close()
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.xz", tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}
}

func TestStreamChecksumPass(t *testing.T) {
	data := []byte("data for checksum test")
	h := sha256.Sum256(data)
	checksum := hex.EncodeToString(h[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name(), StreamOpts{
		Checksum:     checksum,
		ChecksumType: "sha256",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStreamChecksumInfersTypeAndStripsPrefix(t *testing.T) {
	data := []byte("data for checksum inference")
	h := sha256.Sum256(data)
	checksum := "sha256:" + strings.ToUpper(hex.EncodeToString(h[:]))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name(), StreamOpts{
		Checksum: checksum,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeChecksumOptRejectsEmptyPrefixedDigest(t *testing.T) {
	for _, checksum := range []string{"sha256:", "sha512:"} {
		t.Run(checksum, func(t *testing.T) {
			_, err := normalizeChecksumOpt(StreamOpts{Checksum: checksum})
			if err == nil {
				t.Fatal("expected empty digest error")
			}
			if !strings.Contains(err.Error(), "requires a non-empty digest") {
				t.Fatalf("error = %q, want non-empty digest message", err.Error())
			}
		})
	}
}

func TestNormalizeChecksumOptRejectsMalformedExplicitDigest(t *testing.T) {
	tests := []struct {
		name         string
		checksum     string
		checksumType string
		want         string
	}{
		{
			name:         "short sha256",
			checksum:     "abc123",
			checksumType: "sha256",
			want:         "sha256 checksum length",
		},
		{
			name:         "non-hex sha256",
			checksum:     strings.Repeat("g", sha256.Size*2),
			checksumType: "sha256",
			want:         "invalid sha256 checksum hex",
		},
		{
			name:         "sha512 length for sha256",
			checksum:     strings.Repeat("0", sha512.Size*2),
			checksumType: "sha256",
			want:         "sha256 checksum length",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalizeChecksumOpt(StreamOpts{
				Checksum:     tt.checksum,
				ChecksumType: tt.checksumType,
			})
			if err == nil {
				t.Fatal("expected malformed checksum error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestStreamChecksumMismatch(t *testing.T) {
	data := []byte("data for checksum mismatch test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name(), StreamOpts{
		Checksum:     "0000000000000000000000000000000000000000000000000000000000000000",
		ChecksumType: "sha256",
	})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
}

func TestStreamChecksumMismatchDoesNotSyncTarget(t *testing.T) {
	data := []byte("data for checksum mismatch sync ordering test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	syncCalled := false
	withSyncImageTarget(t, func(_ fileSyncer, _ string) error {
		syncCalled = true
		return nil
	})

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name(), StreamOpts{
		Checksum:     strings.Repeat("0", sha256.Size*2),
		ChecksumType: "sha256",
	})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if syncCalled {
		t.Fatal("syncImageTarget called before checksum mismatch was returned")
	}
}

func withSyncImageTarget(t *testing.T, hook func(fileSyncer, string) error) {
	t.Helper()

	orig := syncImageTarget
	syncImageTarget = hook
	t.Cleanup(func() {
		syncImageTarget = orig
	})
}

func TestStreamChecksumSHA512(t *testing.T) {
	data := []byte("sha512 checksum test data")
	h := sha512.Sum512(data)
	checksum := hex.EncodeToString(h[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name(), StreamOpts{
		Checksum:     checksum,
		ChecksumType: "sha512",
	})
	if err != nil {
		t.Fatalf("sha512 stream failed: %v", err)
	}
}

func TestStreamUnsupportedChecksumType(t *testing.T) {
	data := []byte("unsupported hash")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name(), StreamOpts{
		Checksum:     "deadbeef",
		ChecksumType: "md5",
	})
	if err == nil {
		t.Fatal("expected error for unsupported checksum type")
	}
	if !strings.Contains(err.Error(), "unsupported checksum type") {
		t.Errorf("error = %q, want to contain 'unsupported checksum type'", err.Error())
	}
}

func TestStreamNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/missing.img", tmpFile.Name())
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestStreamRejectsMultiLayerOCIWithoutChangingTarget(t *testing.T) {
	srv := startTestRegistry(t)
	defer srv.Close()

	layer1 := stream.NewLayer(io.NopCloser(strings.NewReader("layer-1")))
	layer2 := stream.NewLayer(io.NopCloser(strings.NewReader("layer-2")))
	img, err := mutate.AppendLayers(empty.Image, layer1, layer2)
	if err != nil {
		t.Fatalf("mutate.AppendLayers: %v", err)
	}

	ref, err := name.ParseReference(strings.TrimPrefix(srv.URL, "http://") + "/test/multi-stream:v1")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}

	const original = "existing target bytes"
	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.WriteString(original); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	err = Stream(context.Background(), "oci://"+ref.String(), tmpPath)
	if err == nil {
		t.Fatal("expected multi-layer OCI stream rejection")
	}
	if !strings.Contains(err.Error(), "expected exactly one payload layer") {
		t.Fatalf("error = %q, want exactly one payload layer", err.Error())
	}
	got, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("target was modified: got %q, want %q", got, original)
	}
}

func TestStreamServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	err = Stream(context.Background(), srv.URL+"/image.img", tmpFile.Name())
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestStreamCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Stream(ctx, "http://127.0.0.1:1/image.img", "/dev/null")
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestIsOCIReference(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{"oci://ghcr.io/org/image:latest", true},
		{"http://example.com/image.gz", false},
		{"https://example.com/image.gz", false},
		{"oci://registry.example.com/repo@sha256:abc", true},
	}
	for _, tt := range tests {
		got := IsOCIReference(tt.url)
		if got != tt.want {
			t.Errorf("IsOCIReference(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func TestTrimOCIScheme(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"oci://ghcr.io/org/image:latest", "ghcr.io/org/image:latest"},
		{"ghcr.io/org/image:latest", "ghcr.io/org/image:latest"},
	}
	for _, tt := range tests {
		got := TrimOCIScheme(tt.url)
		if got != tt.want {
			t.Errorf("TrimOCIScheme(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

func TestHTTPGetWithRetry_TransientErrorsThenSuccess(t *testing.T) {
	retryBackoffBase = 0
	t.Cleanup(func() { retryBackoffBase = time.Second })

	const wantAttempts = 2
	attempts := 0
	data := []byte("image content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < wantAttempts {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 → triggers retry
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	rc, err := httpGetWithRetry(context.Background(), srv.URL+"/image.img")
	if err != nil {
		t.Fatalf("httpGetWithRetry() returned error: %v", err)
	}
	defer rc.Close()

	buf := new(strings.Builder)
	if _, err := io.Copy(buf, rc); err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if buf.String() != string(data) {
		t.Errorf("body = %q, want %q", buf.String(), data)
	}
	if attempts != wantAttempts {
		t.Errorf("server received %d requests, want %d", attempts, wantAttempts)
	}
}

func TestHTTPGetWithRetry_AllFail(t *testing.T) {
	retryBackoffBase = 0
	t.Cleanup(func() { retryBackoffBase = time.Second })

	const maxRetries = 3
	attempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadGateway) // 502 → always retried, exhausts retries
	}))
	defer srv.Close()

	_, err := httpGetWithRetry(context.Background(), srv.URL+"/image.img")
	if err == nil {
		t.Fatal("expected error after exhausting all retries")
	}
	if attempts != maxRetries {
		t.Errorf("server received %d requests, want %d (maxRetries)", attempts, maxRetries)
	}
}

func TestHTTPGetWithRetryErrorRedactsSensitiveURLParts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	source := strings.Replace(srv.URL, "http://", "http://user:secret@", 1) + "/image.img?token=abc#frag"
	_, err := httpGetWithRetry(context.Background(), source)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "#frag") {
		t.Fatalf("error leaked sensitive URL parts: %q", err.Error())
	}
	if !strings.Contains(err.Error(), srv.URL+"/image.img") {
		t.Fatalf("error = %q, want redacted URL context", err.Error())
	}
}

func TestHTTPGetWithRetryRedactsErrorAndPreservesUnwrap(t *testing.T) {
	retryBackoffBase = 0
	t.Cleanup(func() { retryBackoffBase = time.Second })

	previousClient := imageHTTPClient
	t.Cleanup(func() { imageHTTPClient = previousClient })

	sentinel := errors.New("temporary transport failure")
	imageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &sourceURLTestError{source: req.URL.String(), err: sentinel}
	})}

	source := "https://leaky-user:super-secret@example.com/image.raw?token=abc#frag"
	_, err := httpGetWithRetry(context.Background(), source)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not preserve wrapped transport error: %v", err)
	}
	for _, leaked := range []string{"leaky-user", "super-secret", "token=abc", "frag"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("error leaked %q: %s", leaked, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "https://example.com/image.raw") {
		t.Fatalf("error = %q, want redacted URL context", err.Error())
	}
}

type sourceURLTestError struct {
	source string
	err    error
}

func (e *sourceURLTestError) Error() string {
	return "request to " + e.source + ": " + e.err.Error()
}

func (e *sourceURLTestError) Unwrap() error {
	return e.err
}

func TestStreamQCOW2Detection(t *testing.T) {
	// Serve qcow2 magic bytes — Stream should detect and redirect to qcow2 hook.
	data := append([]byte{0x51, 0x46, 0x49, 0xfb}, make([]byte, 100)...)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	// Override hook to verify it's called.
	called := false
	orig := convertQCOW2Hook
	convertQCOW2Hook = func(_ context.Context, src io.Reader, sourceName, device string, _ StreamOpts) error {
		called = true
		if !strings.Contains(sourceName, srv.URL) {
			t.Errorf("expected hook source to contain %s, got %s", srv.URL, sourceName)
		}
		got, err := io.ReadAll(src)
		if err != nil {
			t.Fatalf("reading hook source: %v", err)
		}
		if string(got) != string(data) {
			t.Fatalf("hook source = %q, want %q", got, data)
		}
		return nil
	}
	defer func() { convertQCOW2Hook = orig }()

	err = Stream(context.Background(), srv.URL+"/image.qcow2", tmpFile.Name())
	if err != nil {
		t.Fatalf("Stream() = %v", err)
	}
	if !called {
		t.Error("convertQCOW2Hook was not invoked for qcow2 image")
	}
	if requests != 1 {
		t.Fatalf("server received %d requests, want 1", requests)
	}
}

func TestStreamRejectsUnsupportedVMwareContainerBeforeWrite(t *testing.T) {
	data := append([]byte{'K', 'D', 'M', 'V'}, []byte("vmdk payload")...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	before := []byte("existing target data")
	if _, err := tmpFile.Write(before); err != nil {
		t.Fatal(err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatal(err)
	}

	err = Stream(context.Background(), srv.URL+"/image.vmdk", tmpFile.Name())
	if err == nil {
		t.Fatal("expected unsupported VMDK error")
	}
	if !strings.Contains(err.Error(), "unsupported image format vmdk") {
		t.Fatalf("error = %q, want unsupported VMDK format", err.Error())
	}
	after, err := os.ReadFile(tmpFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("target changed after unsupported format: got %q, want %q", after, before)
	}
}

func TestStreamQCOW2ChecksumMismatch(t *testing.T) {
	data := append([]byte{0x51, 0x46, 0x49, 0xfb}, []byte("qcow2 payload for checksum")...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	tmpFile, err := os.CreateTemp(t.TempDir(), "disk-*")
	if err != nil {
		t.Fatal(err)
	}
	_ = tmpFile.Close()

	orig := convertQCOW2Hook
	convertQCOW2Hook = func(_ context.Context, src io.Reader, _ string, _ string, opt StreamOpts) error {
		checksummed, h, err := wrapChecksum(src, opt)
		if err != nil {
			return err
		}
		if _, err := io.Copy(io.Discard, checksummed); err != nil {
			return err
		}
		return verifyChecksum(h, opt)
	}
	defer func() { convertQCOW2Hook = orig }()

	err = Stream(context.Background(), srv.URL+"/image.qcow2", tmpFile.Name(), StreamOpts{
		Checksum:     "0000000000000000000000000000000000000000000000000000000000000000",
		ChecksumType: "sha256",
	})
	if err == nil {
		t.Fatal("expected checksum mismatch error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %q, want checksum mismatch", err.Error())
	}
}

type failingSyncer struct {
	err error
}

func (f failingSyncer) Sync() error {
	return f.err
}

func TestSyncImageTargetReturnsSyncError(t *testing.T) {
	syncErr := errors.New("flush failed")
	err := syncImageTarget(failingSyncer{err: syncErr}, "/dev/test")
	if !errors.Is(err, syncErr) {
		t.Fatalf("syncImageTarget() error = %v, want %v", err, syncErr)
	}
	if !strings.Contains(err.Error(), "syncing device /dev/test") {
		t.Fatalf("syncImageTarget() error = %q, want device context", err.Error())
	}
}

func TestOpenAndDecompressQCOW2KeepsDetectedSource(t *testing.T) {
	data := append([]byte{0x51, 0x46, 0x49, 0xfb}, []byte("payload after qcow2 header")...)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	src, cleanup, format, err := openAndDecompress(context.Background(), srv.URL+"/image.qcow2", nil)
	if err != nil {
		t.Fatalf("openAndDecompress() = %v", err)
	}
	defer cleanup()
	if format != FormatQCOW2 {
		t.Fatalf("format = %s, want %s", format, FormatQCOW2)
	}

	got, err := io.ReadAll(src)
	if err != nil {
		t.Fatalf("reading returned source: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("returned source = %q, want %q", got, data)
	}
	if requests != 1 {
		t.Fatalf("server received %d requests, want 1", requests)
	}
}

func TestOpenAndDecompressGzipQCOW2KeepsDetectedSource(t *testing.T) {
	data := append([]byte{0x51, 0x46, 0x49, 0xfb}, []byte("payload after qcow2 header")...)
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(data)
		_ = gz.Close()
	}))
	defer srv.Close()

	src, cleanup, format, err := openAndDecompress(context.Background(), srv.URL+"/image.qcow2.gz", nil)
	if err != nil {
		t.Fatalf("openAndDecompress() = %v", err)
	}
	defer cleanup()
	if format != FormatQCOW2 {
		t.Fatalf("format = %s, want %s", format, FormatQCOW2)
	}

	got, err := io.ReadAll(src)
	if err != nil {
		t.Fatalf("reading returned source: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("returned source = %q, want %q", got, data)
	}
	if requests != 1 {
		t.Fatalf("server received %d requests, want 1", requests)
	}
}

func TestOpenAndDecompressGzipRawReportsPayloadFormat(t *testing.T) {
	data := []byte("raw payload after gzip")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(data)
		_ = gz.Close()
	}))
	defer srv.Close()

	src, cleanup, format, err := openAndDecompress(context.Background(), srv.URL+"/image.raw.gz", nil)
	if err != nil {
		t.Fatalf("openAndDecompress() = %v", err)
	}
	defer cleanup()
	if format != FormatRaw {
		t.Fatalf("format = %s, want %s", format, FormatRaw)
	}

	got, err := io.ReadAll(src)
	if err != nil {
		t.Fatalf("reading returned source: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("returned source = %q, want %q", got, data)
	}
}
