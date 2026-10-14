//go:build e2e

package kvm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests in this file give the standby agent a short-lived token and
// check that it renews it in time, and that it stops cleanly, reports the
// reason and reboots when it can not.

// A token without TOKEN_URL and with TOKEN_EXPIRES_IN or TOKEN_EXPIRES_AT:
// BOOTy stops the standby loop the margin before the expiry and reports why.
func TestStandbyTokenExpiresWithoutRenewalQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	for name, expiry := range map[string]func() (string, string){
		"expires in": func() (string, string) { return "TOKEN_EXPIRES_IN", "90" },
		"expires at": func() (string, string) {
			return "TOKEN_EXPIRES_AT", time.Now().Add(90 * time.Second).UTC().Format(time.RFC3339)
		},
	} {
		t.Run(name, func(t *testing.T) {
			caprf := startFakeCAPRF(t, "")
			targetDisk := filepath.Join(t.TempDir(), "standby-token.qcow2")
			run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

			vars := caprf.standbyVars()
			key, value := expiry()
			vars[key] = value
			vars["TOKEN_RENEW_MARGIN_SECONDS"] = "30"

			start := time.Now()
			output := runQEMUProvision(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 4*time.Minute)
			t.Logf("standby token expiry output tail:\n%s", tail(output, 3000))
			if d := time.Since(start); d > 2*time.Minute {
				t.Fatalf("guest ran %v, want a stop about 60 s after the start", d)
			}
			if !strings.Contains(string(output), "reboot: Restarting system") {
				t.Fatalf("guest did not reboot after the token stop. tail:\n%s", tail(output, 4000))
			}
			caprf.mu.Lock()
			defer caprf.mu.Unlock()
			if !strings.Contains(caprf.messages["error"], "token renewal failed, task stopped") ||
				!strings.Contains(caprf.messages["error"], "no TOKEN_URL") {
				t.Fatalf("error report = %q, statuses %v", caprf.messages["error"], caprf.statuses)
			}
			if caprf.heartbeats == 0 {
				t.Fatal("no heartbeat before the stop")
			}
		})
	}
}

// tokenServer is a TLS token endpoint for TOKEN_URL. It issues tokens with
// a short lifetime and fails every call after okCalls.
type tokenServer struct {
	caprf   *fakeCAPRF
	ttl     time.Duration
	okCalls int

	mu     sync.Mutex
	calls  int
	tokens []string
}

func (s *tokenServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !s.caprf.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	s.calls++
	if s.calls > s.okCalls {
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	tok := fmt.Sprintf("agent-token-%d", s.calls)
	s.tokens = append(s.tokens, tok)
	s.mu.Unlock()
	s.caprf.mu.Lock()
	if s.caprf.issued == nil {
		s.caprf.issued = map[string]time.Time{}
	}
	s.caprf.issued[tok] = time.Now().Add(s.ttl)
	s.caprf.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": int(s.ttl.Seconds())})
}

// startTLSTokenServer serves s over HTTPS for the guest (10.0.2.2) and
// returns its URL and the PEM of its self-signed certificate.
func startTLSTokenServer(t *testing.T, s *tokenServer) (url string, certPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake-caprf"},
		IPAddresses:           []net.IP{net.ParseIP("10.0.2.2")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		},
	}
	go func() { _ = srv.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return fmt.Sprintf("https://10.0.2.2:%d/agent/token", listener.Addr().(*net.TCPAddr).Port),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// With TOKEN_URL the agent renews its 20 s token before each expiry and
// uses the new tokens. When the endpoint fails for good, it stops, reports
// the reason and reboots before the token expires.
func TestStandbyTokenRenewalThenFailureQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	caprf := startFakeCAPRF(t, "")
	tokens := &tokenServer{caprf: caprf, ttl: 20 * time.Second, okCalls: 3}
	tokenURL, certPEM := startTLSTokenServer(t, tokens)
	targetDisk := filepath.Join(t.TempDir(), "standby-renewal.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "1G")

	vars := caprf.standbyVars()
	vars["TOKEN_URL"] = tokenURL
	vars["TOKEN_RENEW_RETRIES"] = "2"
	vars["TOKEN_RENEW_BACKOFF_SECONDS"] = "1"
	initramfs := buildProvisionInitramfsWithFiles(t, vars, map[string]string{
		"etc/ssl/certs/ca-certificates.crt": string(certPEM),
	})

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 4*time.Minute)
	t.Logf("standby token renewal output tail:\n%s", tail(output, 3000))
	if !strings.Contains(string(output), "reboot: Restarting system") {
		t.Fatalf("guest did not reboot after the renewal failure. tail:\n%s", tail(output, 4000))
	}
	if n := strings.Count(string(output), "jwt renewed"); n < 2 {
		t.Fatalf("jwt renewed %d times, want 2 renewals before the failure", n)
	}
	tokens.mu.Lock()
	calls := tokens.calls
	tokens.mu.Unlock()
	if calls != 3+2 {
		t.Fatalf("token endpoint calls = %d, want 1 exchange, 2 renewals and 2 failed attempts", calls)
	}
	caprf.mu.Lock()
	defer caprf.mu.Unlock()
	if msg := caprf.messages["error"]; !strings.Contains(msg, "token renewal failed, task stopped") ||
		!strings.Contains(msg, "renewal failed after 2 attempts") {
		t.Fatalf("error report = %q, statuses %v", msg, caprf.statuses)
	}
}
