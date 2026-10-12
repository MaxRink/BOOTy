package image

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyGPGSignature_OCI_URL(t *testing.T) {
	// Create a real pubkey file so the function reaches the OCI check.
	tmpKey := t.TempDir() + "/key.gpg"
	if err := os.WriteFile(tmpKey, []byte("fake-key"), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sig-data"))
	}))
	defer ts.Close()

	for _, imageURL := range []string{
		"oci://registry.example.com/image:tag",
		"OCI://registry.example.com/image:tag",
	} {
		t.Run(imageURL, func(t *testing.T) {
			err := VerifyGPGSignature(context.Background(), imageURL, ts.URL+"/sig", tmpKey)
			if err == nil {
				t.Error("expected error for OCI URL")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "oci://") {
				t.Errorf("error should mention oci://, got: %v", err)
			}
		})
	}
}

func TestDownloadToTemp_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("signature-data"))
	}))
	defer ts.Close()

	path, err := downloadToTemp(context.Background(), ts.URL+"/sig", "gpg-test-*")
	if err != nil {
		t.Fatalf("downloadToTemp: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "signature-data" {
		t.Errorf("content = %q", string(data))
	}
}

func TestVerifyGPGSignature_DownloadFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sig") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("image-data"))
	}))
	defer ts.Close()

	tmpKey := t.TempDir() + "/key.gpg"
	if err := os.WriteFile(tmpKey, []byte("fake-key"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := VerifyGPGSignature(context.Background(), ts.URL+"/img", ts.URL+"/sig", tmpKey)
	if err == nil {
		t.Error("expected error when sig download fails")
	}
}

func TestVerifyGPGSignatureRedactsURLsInErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "sig") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("image-data"))
	}))
	defer ts.Close()

	tmpKey := t.TempDir() + "/key.gpg"
	if err := os.WriteFile(tmpKey, []byte("fake-key"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := VerifyGPGSignature(
		context.Background(),
		ts.URL+"/img?image-token=secret#image-fragment",
		ts.URL+"/sig?signature-token=secret#signature-fragment",
		tmpKey,
	)
	if err == nil {
		t.Fatal("expected error when signature download fails")
	}
	msg := err.Error()
	for _, sensitive := range []string{"signature-token", "image-token", "secret", "signature-fragment", "image-fragment"} {
		if strings.Contains(msg, sensitive) {
			t.Fatalf("error leaked %q: %s", sensitive, msg)
		}
	}
	if !strings.Contains(msg, ts.URL+"/sig") {
		t.Fatalf("error did not preserve redacted signature URL context: %s", msg)
	}
}

func TestDownloadToTempTransportErrorRedactsURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sig-data"))
	}))
	rawURL := sensitiveTestURL(t, ts.URL, "/sig")
	ts.Close()

	_, err := downloadToTemp(context.Background(), rawURL, "gpg-test-*")
	if err == nil {
		t.Fatal("expected signature download transport error")
	}
	assertNoSensitiveURLParts(t, err.Error())
	assertPreservesURLError(t, err)
	if !strings.Contains(err.Error(), "/sig") {
		t.Fatalf("error did not preserve redacted signature path context: %s", err)
	}
}

func TestVerifyGPGSignatureImageTransportErrorRedactsURL(t *testing.T) {
	sigServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sig-data"))
	}))
	defer sigServer.Close()

	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("image-data"))
	}))
	imageURL := sensitiveTestURL(t, imageServer.URL, "/image.raw")
	imageServer.Close()

	tmpKey := t.TempDir() + "/key.gpg"
	if err := os.WriteFile(tmpKey, []byte("fake-key"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := VerifyGPGSignature(context.Background(), imageURL, sigServer.URL+"/sig", tmpKey)
	if err == nil {
		t.Fatal("expected image stream transport error")
	}
	assertNoSensitiveURLParts(t, err.Error())
	assertPreservesURLError(t, err)
	if !strings.Contains(err.Error(), "/image.raw") {
		t.Fatalf("error did not preserve redacted image path context: %s", err)
	}
}

func sensitiveTestURL(t *testing.T, baseURL, path string) string {
	t.Helper()

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	u.Path = path
	u.User = url.UserPassword("robot", "secret")
	u.RawQuery = "token=abc"
	u.Fragment = "frag"
	return u.String()
}

func assertNoSensitiveURLParts(t *testing.T, msg string) {
	t.Helper()

	for _, sensitive := range []string{"robot", "secret", "token=abc", "frag"} {
		if strings.Contains(msg, sensitive) {
			t.Fatalf("error leaked %q: %s", sensitive, msg)
		}
	}
}

func assertPreservesURLError(t *testing.T, err error) {
	t.Helper()

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("error chain did not preserve *url.Error: %v", err)
	}
}

func TestRunGPGVerifyStream_WithInvalidBinary(t *testing.T) {
	// Create a temp dir with a fake gpgv that always fails.
	tmpDir := t.TempDir()
	fakeBin := tmpDir + "/gpgv"
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmpDir)

	err := runGPGVerifyStream(context.Background(), "/nonexistent/key", "/nonexistent/sig", strings.NewReader("data"))
	if err == nil {
		t.Error("expected error from failing gpgv")
	}
}

// fakeGPGTool writes an executable named name into dir. The script records
// its arguments to name.args and its stdin to name.stdin in dir, prints a
// marker line, and exits with exitCode.
func fakeGPGTool(t *testing.T, dir, name string, exitCode int) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done > %q\n/bin/cat > %q\necho %s-ran\nexit %d\n",
		filepath.Join(dir, name+".args"), filepath.Join(dir, name+".stdin"), name, exitCode)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// readFakeGPGRecord returns the recorded args and stdin of a fake tool, and
// whether it ran at all.
func readFakeGPGRecord(t *testing.T, dir, name string) (args []string, stdin string, ran bool) {
	t.Helper()
	rawArgs, err := os.ReadFile(filepath.Join(dir, name+".args"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	rawStdin, err := os.ReadFile(filepath.Join(dir, name+".stdin"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(rawArgs)), string(rawStdin), true
}

func TestVerifyGPGSignature_GPGVSuccessPipesImage(t *testing.T) {
	payload := "image-payload-\x00\x01\x02-end"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sig":
			_, _ = w.Write([]byte("signature"))
		case "/image.raw":
			_, _ = w.Write([]byte(payload))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	binDir := t.TempDir()
	fakeGPGTool(t, binDir, "gpgv", 0)
	fakeGPGTool(t, binDir, "gpg", 1)
	t.Setenv("PATH", binDir)

	key := filepath.Join(t.TempDir(), "key.gpg")
	if err := os.WriteFile(key, []byte("fake-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := VerifyGPGSignature(context.Background(), ts.URL+"/image.raw", ts.URL+"/sig", key); err != nil {
		t.Fatalf("VerifyGPGSignature() = %v, want nil", err)
	}

	args, stdin, ran := readFakeGPGRecord(t, binDir, "gpgv")
	if !ran {
		t.Fatal("gpgv was not run")
	}
	if stdin != payload {
		t.Fatalf("gpgv stdin = %q, want image payload %q", stdin, payload)
	}
	if len(args) != 4 || args[0] != "--keyring" || args[1] != key || args[3] != "-" {
		t.Fatalf("gpgv args = %q, want [--keyring %s <sig> -]", args, key)
	}
	if _, _, gpgRan := readFakeGPGRecord(t, binDir, "gpg"); gpgRan {
		t.Fatal("gpg ran although gpgv succeeded")
	}
}

func TestRunGPGVerifyStream_GPGFallbackFailureRejects(t *testing.T) {
	binDir := t.TempDir()
	fakeGPGTool(t, binDir, "gpg", 2)
	t.Setenv("PATH", binDir)

	err := runGPGVerifyStream(context.Background(), "/keys/k.gpg", "/tmp/img.sig", strings.NewReader("unsigned-image"))
	if err == nil {
		t.Fatal("runGPGVerifyStream() = nil, want error for failing gpg")
	}
	if !strings.Contains(err.Error(), "gpg signature verification failed") || !strings.Contains(err.Error(), "gpg-ran") {
		t.Fatalf("error = %q, want gpg failure with tool output", err)
	}
	args, stdin, ran := readFakeGPGRecord(t, binDir, "gpg")
	if !ran {
		t.Fatal("gpg fallback was not run")
	}
	if stdin != "unsigned-image" {
		t.Fatalf("gpg stdin = %q, want %q", stdin, "unsigned-image")
	}
	want := []string{"--no-default-keyring", "--keyring", "/keys/k.gpg", "--verify", "/tmp/img.sig", "-"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Fatalf("gpg args = %q, want %q", args, want)
	}
}

func TestRunGPGVerifyStream_GPGFallbackSuccess(t *testing.T) {
	binDir := t.TempDir()
	fakeGPGTool(t, binDir, "gpg", 0)
	t.Setenv("PATH", binDir)

	payload := strings.Repeat("signed-image-block", 4096)
	if err := runGPGVerifyStream(context.Background(), "/keys/k.gpg", "/tmp/img.sig", strings.NewReader(payload)); err != nil {
		t.Fatalf("runGPGVerifyStream() = %v, want nil", err)
	}
	_, stdin, ran := readFakeGPGRecord(t, binDir, "gpg")
	if !ran {
		t.Fatal("gpg fallback was not run")
	}
	if stdin != payload {
		t.Fatalf("gpg stdin has %d bytes, want the %d byte payload", len(stdin), len(payload))
	}
}
