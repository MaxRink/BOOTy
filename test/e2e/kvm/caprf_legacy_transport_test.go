//go:build e2e

package kvm

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// connectProxy is a minimal HTTP CONNECT proxy. It maps the host name
// caprf.via.proxy to the local CAPRF server, so the guest can only reach
// CAPRF through the proxy.
type connectProxy struct {
	url      string
	connects atomic.Int32
}

func startConnectProxy(t *testing.T, caprfPort int) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != fmt.Sprintf("caprf.via.proxy:%d", caprfPort) {
			http.Error(w, "unexpected proxy request", http.StatusForbidden)
			return
		}
		upstream, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", caprfPort))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		p.connects.Add(1)
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() {
			_, _ = io.Copy(upstream, client)
			_ = upstream.Close()
		}()
		go func() {
			_, _ = io.Copy(client, upstream)
			_ = client.Close()
		}()
	})
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	p.url = fmt.Sprintf("http://10.0.2.2:%d", listener.Addr().(*net.TCPAddr).Port)
	return p
}

// Legacy evidence: common.sh callback_curl, have_network and the image
// download in provision.sh use `curl --insecure`, and `source vars` exports
// every CAPRF additional variable, so https_proxy applies to all of them.
// Here CAPRF has a self-signed certificate and is only reachable through
// the proxy from the vars file.
func TestLegacyCompatTLSAndProxyQEMU(t *testing.T) {
	testTLSAndProxy(t, func(f *fakeCAPRF) map[string]string {
		vars := f.vars("provision")
		// No explicit transport opt-ins: CAPRF_LEGACY_COMPAT implies them.
		delete(vars, "INSECURE_TRANSPORT")
		return vars
	})
}

// SKIP_TLS_VERIFY and EXPORT_VARS give the same result without
// CAPRF_LEGACY_COMPAT. Without SKIP_TLS_VERIFY the certificate check fails,
// without EXPORT_VARS the guest cannot reach CAPRF.
func TestNativeParityTLSAndProxyQEMU(t *testing.T) {
	testTLSAndProxy(t, func(f *fakeCAPRF) map[string]string {
		vars := f.nativeVars("provision")
		delete(vars, "INSECURE_TRANSPORT")
		vars["SKIP_TLS_VERIFY"] = "true"
		vars["EXPORT_VARS"] = "true"
		vars["SUCCESS_ACTION"] = "poweroff"
		return vars
	})
}

func testTLSAndProxy(t *testing.T, varsFor func(*fakeCAPRF) map[string]string) {
	t.Helper()
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := &fakeCAPRF{messages: map[string]string{}}
	tlsServer := httptest.NewUnstartedServer(caprf.handler(compressGzip(t, rawDisk)))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsServer.Listener = listener
	tlsServer.StartTLS()
	t.Cleanup(tlsServer.Close)
	port := listener.Addr().(*net.TCPAddr).Port
	caprf.baseURL = fmt.Sprintf("https://caprf.via.proxy:%d", port)
	proxy := startConnectProxy(t, port)

	targetDisk := filepath.Join(t.TempDir(), "legacy-transport-target.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := varsFor(caprf)
	vars["IMAGE"] = caprf.baseURL + "/image.gz"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["https_proxy"] = proxy.url
	initramfs := buildProvisionInitramfs(t, vars)

	output := runQEMUProvision(t, findKernel(t), initramfs, targetDisk, 6*time.Minute)
	t.Logf("legacy transport output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	caprf.requireStatuses(t, "init", "success")
	if proxy.connects.Load() == 0 {
		t.Fatal("no request went through the https_proxy from the vars file")
	}
}
