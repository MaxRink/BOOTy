//go:build e2e

package kvm

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The guest RTC starts in 2001, so every TLS certificate of the fake CAPRF
// looks not yet valid. BOOTy must set the clock before the first callback
// and provision over verified TLS, without SKIP_TLS_VERIFY.
const timeSyncGuestRTC = "2001-01-01T00:00:00"

// TestTimeSyncNTPQEMU syncs the clock from NTP_SERVERS.
func TestTimeSyncNTPQEMU(t *testing.T) {
	ntp := startFakeNTPServer(t)
	output, caprf := runTimeSyncProvision(t, map[string]string{
		"TIME_SYNC":   "require",
		"NTP_SERVERS": fmt.Sprintf("10.0.2.2:%d", ntp.port),
	})
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	requireClockSynced(t, output, "source=ntp://10.0.2.2:")
	if ntp.queries.Load() == 0 {
		t.Fatal("the fake NTP server got no query")
	}
}

// TestTimeSyncCAPRFDateQEMU syncs the clock from the authenticated Date
// header of the CAPRF connectivity target, without NTP.
func TestTimeSyncCAPRFDateQEMU(t *testing.T) {
	output, caprf := runTimeSyncProvision(t, map[string]string{"TIME_SYNC": "require"})
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	requireClockSynced(t, output, "source=caprf-date")
}

// TestTimeSyncOffNamesSkewQEMU keeps the wrong RTC time: the TLS callbacks
// fail with an error that names the clock skew.
func TestTimeSyncOffNamesSkewQEMU(t *testing.T) {
	output, caprf := runTimeSyncProvision(t, map[string]string{"TIME_SYNC": "off"})
	text := string(output)
	if !strings.Contains(text, "BOOTY-TIM-002") || !strings.Contains(text, "behind") {
		t.Fatalf("no clock skew error (BOOTY-TIM-002) in the output. tail:\n%s", tail(output, 4000))
	}
	caprf.mu.Lock()
	defer caprf.mu.Unlock()
	if len(caprf.statuses) != 0 {
		t.Fatalf("CAPRF got status callbacks %v with the wrong clock", caprf.statuses)
	}
}

func runTimeSyncProvision(t *testing.T, extraVars map[string]string) ([]byte, *fakeCAPRF) {
	t.Helper()
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	caPEM, serverCert := timeSyncPKI(t)
	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := &fakeCAPRF{messages: map[string]string{}}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:           caprf.handler(compressGzip(t, rawDisk)),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{serverCert}, MinVersion: tls.VersionTLS12},
	}
	go func() { _ = srv.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	caprf.baseURL = fmt.Sprintf("https://10.0.2.2:%d", listener.Addr().(*net.TCPAddr).Port)

	targetDisk := filepath.Join(t.TempDir(), "time-sync-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.nativeVars("provision")
	// Verified TLS only: no plain HTTP token, no SKIP_TLS_VERIFY.
	delete(vars, "INSECURE_TRANSPORT")
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["SUCCESS_ACTION"] = "poweroff"
	for k, v := range extraVars {
		vars[k] = v
	}
	initramfs := buildProvisionInitramfsWithFiles(t, vars, map[string]string{
		// Go reads the system roots from this file.
		"etc/ssl/certs/ca-certificates.crt": caPEM,
	})

	t.Setenv("QEMU_EXTRA_ARGS", strings.TrimSpace(os.Getenv("QEMU_EXTRA_ARGS")+" -rtc base="+timeSyncGuestRTC))
	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("time sync output tail:\n%s", tail(output, 3000))
	return output, caprf
}

var skewPattern = regexp.MustCompile(`measured clock skew.* skew_seconds=([0-9.e+]+)`)

// requireClockSynced checks that BOOTy measured the RTC skew of about 25
// years, stepped the clock from the wanted source and wrote the RTC.
func requireClockSynced(t *testing.T, output []byte, source string) {
	t.Helper()
	text := string(output)
	m := skewPattern.FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no measured clock skew in the output. tail:\n%s", tail(output, 4000))
	}
	skew, err := strconv.ParseFloat(m[1], 64)
	if err != nil || skew < 20*365*24*3600 {
		t.Fatalf("measured skew %q, want more than 20 years: the guest RTC was not set back", m[1])
	}
	for _, want := range []string{"clock synchronized", source, "RTC updated"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output does not contain %q. tail:\n%s", want, tail(output, 4000))
		}
	}
}

// timeSyncPKI returns a CA in PEM and a server certificate for 10.0.2.2,
// the host address in QEMU user networking, valid from one hour ago.
func timeSyncPKI(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "BOOTy time sync test CA"},
		NotBefore:             now.Add(-2 * time.Hour),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "fake CAPRF"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(10, 0, 2, 2)},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return caPEM, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}
}

type fakeNTPServer struct {
	port    int
	queries atomic.Int32
}

// startFakeNTPServer answers SNTP requests with the host time. QEMU user
// networking forwards UDP to 10.0.2.2 to the host.
func startFakeNTPServer(t *testing.T) *fakeNTPServer {
	t.Helper()
	conn, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &fakeNTPServer{port: conn.LocalAddr().(*net.UDPAddr).Port}
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < 48 {
				continue
			}
			s.queries.Add(1)
			resp := make([]byte, 48)
			resp[0] = 4<<3 | 4 // version 4, server mode
			resp[1] = 1        // stratum 1
			copy(resp[12:16], "GPS\x00")
			copy(resp[24:32], buf[40:48])
			putFakeNTPTime(resp[32:40], time.Now())
			putFakeNTPTime(resp[40:48], time.Now())
			_, _ = conn.WriteTo(resp, addr)
		}
	}()
	return s
}

func putFakeNTPTime(b []byte, t time.Time) {
	const ntpEpochOffset = 2208988800
	binary.BigEndian.PutUint32(b[0:4], uint32(t.Unix()+ntpEpochOffset))
	binary.BigEndian.PutUint32(b[4:8], uint32((int64(t.Nanosecond())<<32)/int64(time.Second)))
}
