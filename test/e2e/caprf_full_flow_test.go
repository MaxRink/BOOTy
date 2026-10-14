//go:build e2e && linux

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/image"
	"github.com/telekom/BOOTy/pkg/kexec"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frr"
	"github.com/telekom/BOOTy/pkg/provision"
)

// ---------------------------------------------------------------------------
// Mock infrastructure
// ---------------------------------------------------------------------------

type mockCommander struct {
	mu      sync.Mutex
	calls   []cmdCall
	results map[string]cmdResult
}

type cmdCall struct {
	Name string
	Args []string
}

func (c cmdCall) String() string {
	return c.Name + " " + strings.Join(c.Args, " ")
}

type cmdResult struct {
	Output []byte
	Err    error
}

func newMockCommander() *mockCommander {
	return &mockCommander{results: make(map[string]cmdResult)}
}

// FakeDisks: this commander changes no real disk (disk.FakeDisks).
func (m *mockCommander) FakeDisks() {}

func (m *mockCommander) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, cmdCall{Name: name, Args: args})
	keys := []string{name}
	if len(args) > 0 {
		keys = append([]string{name + " " + args[0]}, keys...)
	}
	for _, key := range keys {
		if r, ok := m.results[key]; ok {
			return r.Output, r.Err
		}
	}
	return nil, nil
}

func (m *mockCommander) set(key string, output []byte, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[key] = cmdResult{Output: output, Err: err}
}

func (m *mockCommander) getCalls() []cmdCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]cmdCall, len(m.calls))
	copy(out, m.calls)
	return out
}

type mockProvider struct {
	mu       sync.Mutex
	statuses []statusEntry
	logs     []string
	cfg      *config.MachineConfig
}

type statusEntry struct {
	Status  config.Status
	Message string
}

func newMockProvider(cfg *config.MachineConfig) *mockProvider {
	return &mockProvider{cfg: cfg}
}

func (p *mockProvider) GetConfig(_ context.Context) (*config.MachineConfig, error) {
	return p.cfg, nil
}

func (p *mockProvider) ReportStatus(_ context.Context, status config.Status, message string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses = append(p.statuses, statusEntry{Status: status, Message: message})
	return nil
}

func (p *mockProvider) ShipLog(_ context.Context, msg string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs = append(p.logs, msg)
	return nil
}

func (p *mockProvider) Heartbeat(_ context.Context) error { return nil }
func (p *mockProvider) ReportFirmware(_ context.Context, _ []byte) error {
	return nil
}
func (p *mockProvider) FetchCommands(_ context.Context) ([]config.Command, error) {
	return nil, nil
}
func (p *mockProvider) AcknowledgeCommand(_ context.Context, _, _, _ string) error { return nil }
func (p *mockProvider) ReportInventory(_ context.Context, _ []byte) error          { return nil }

func (p *mockProvider) getStatuses() []statusEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]statusEntry, len(p.statuses))
	copy(out, p.statuses)
	return out
}

type caprfTestServer struct {
	mu       sync.Mutex
	statuses []string
	logs     []string
	debugs   []string
	images   map[string][]byte
}

func newCAPRFTestServer() *caprfTestServer {
	return &caprfTestServer{images: make(map[string][]byte)}
}

func (s *caprfTestServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/status/init", s.statusHandler("init"))
	mux.HandleFunc("/status/success", s.statusHandler("success"))
	mux.HandleFunc("/status/error", s.statusHandler("error"))
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.logs = append(s.logs, string(body))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.debugs = append(s.debugs, string(body))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/images/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/images/")
		s.mu.Lock()
		data, ok := s.images[name]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
	return mux
}

func (s *caprfTestServer) statusHandler(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		auth := r.Header.Get("Authorization")
		s.mu.Lock()
		s.statuses = append(s.statuses, fmt.Sprintf("%s:%s:%s", status, auth, string(body)))
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (s *caprfTestServer) getStatuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.statuses))
	copy(out, s.statuses)
	return out
}

func (s *caprfTestServer) getLogs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.logs))
	copy(out, s.logs)
	return out
}

func (s *caprfTestServer) getDebugs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.debugs))
	copy(out, s.debugs)
	return out
}

func startTestServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func gzipData(data []byte) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write(data)
	_ = gz.Close()
	return buf.Bytes()
}

type sequentialCmd struct {
	results []cmdResult
	idx     int32
}

// FakeDisks: this commander changes no real disk (disk.FakeDisks).
func (s *sequentialCmd) FakeDisks() {}

func (s *sequentialCmd) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	i := atomic.AddInt32(&s.idx, 1) - 1
	if int(i) >= len(s.results) {
		return nil, nil
	}
	r := s.results[i]
	return r.Output, r.Err
}

// ---------------------------------------------------------------------------
// Test 1: Full Provision Flow
// ---------------------------------------------------------------------------

func TestFullProvisionFlow(t *testing.T) {
	cmd := newMockCommander()
	cfg := &config.MachineConfig{
		Mode:     "provision",
		Hostname: "e2e-node-01",
		Transport: config.TransportConfig{
			Token: "bearer-token-42",
		},
		Provision: config.ProvisionConfig{
			ProviderID:        "redfish://10.0.0.1/Systems/1",
			FailureDomain:     "az-1",
			Region:            "eu-central",
			ExtraKernelParams: "console=ttyS0",
			Image: config.ImageConfig{
				URLs: []string{startE2ERawImageServer(t)},
			},
			Disk: config.DiskConfig{
				MinSizeGB: 0,
			},
		},
		Network: config.NetworkConfig{
			DNSResolvers: "8.8.8.8,1.1.1.1",
		},
	}
	provider := newMockProvider(cfg)

	sfdiskOut, _ := json.Marshal(map[string]any{
		"partitiontable": map[string]any{
			"device": "/dev/sda",
			"partitions": []map[string]any{
				{"node": "/dev/sda1", "start": 2048, "size": 1048576,
					"type": "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
				{"node": "/dev/sda2", "start": 1050624, "size": 209715200,
					"type": "0FC63DAF-8483-4772-8E79-3D69D8477DE4"},
			},
		},
	})
	cmd.set("sfdisk", sfdiskOut, nil)
	cmd.set("chroot", []byte(""), nil)
	cmd.set("e2fsck", []byte(""), nil)
	cmd.set("lvm", []byte(""), nil)
	cmd.set("growpart", []byte("NOCHANGE"), nil)
	cmd.set("resize2fs", []byte(""), nil)
	cmd.set("mdadm", []byte(""), nil)
	cmd.set("wipefs", []byte(""), nil)
	cmd.set("partprobe", []byte(""), nil)

	diskMgr := disk.NewManager(cmd)
	orch := provision.NewOrchestrator(cfg, provider, diskMgr)

	ctx := context.Background()
	err := orch.Provision(ctx)

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected at least one status report")
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want %q", statuses[0].Status, config.StatusInit)
	}

	last := statuses[len(statuses)-1]
	if err != nil {
		if last.Status != config.StatusError {
			t.Errorf("on failure, last status = %q, want %q", last.Status, config.StatusError)
		}
		t.Logf("Provision failed at expected step: %v", err)
	} else {
		if last.Status != config.StatusSuccess {
			t.Errorf("on success, last status = %q, want %q", last.Status, config.StatusSuccess)
		}
	}

	if orch.FirmwareChanged() {
		t.Error("expected firmwareChanged=false by default")
	}
}

// ---------------------------------------------------------------------------
// Test 2: Full Deprovision Hard Flow
// ---------------------------------------------------------------------------

func TestFullDeprovisionHardFlow(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("mdadm", []byte(""), nil)
	cmd.set("wipefs", []byte(""), nil)
	cmd.set("chroot", []byte("BootCurrent: 0001\nBootOrder: 0001"), nil)

	cfg := &config.MachineConfig{
		Mode: "hard",
		Network: config.NetworkConfig{
			DNSResolvers: "8.8.8.8",
		},
	}
	provider := newMockProvider(cfg)
	diskMgr := disk.NewManager(cmd)

	orch := provision.NewOrchestrator(cfg, provider, diskMgr)
	err := orch.Deprovision(context.Background())

	statuses := provider.getStatuses()
	if len(statuses) < 2 {
		t.Fatalf("expected >= 2 status reports, got %d", len(statuses))
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init", statuses[0].Status)
	}
	last := statuses[len(statuses)-1]
	if err == nil && last.Status != config.StatusSuccess {
		t.Errorf("on success, last status = %q, want success", last.Status)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Full Deprovision Soft Flow
// ---------------------------------------------------------------------------

func TestFullDeprovisionSoftFlow(t *testing.T) {
	cmd := newMockCommander()
	cfg := &config.MachineConfig{
		Mode: "soft",
		Network: config.NetworkConfig{
			DNSResolvers: "8.8.8.8",
		},
	}
	provider := newMockProvider(cfg)
	diskMgr := disk.NewManager(cmd)

	orch := provision.NewOrchestrator(cfg, provider, diskMgr)
	err := orch.Deprovision(context.Background())

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected at least one status report")
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init", statuses[0].Status)
	}
	if err != nil {
		t.Logf("Soft deprovision failed as expected without real disks: %v", err)
		last := statuses[len(statuses)-1]
		if last.Status != config.StatusError {
			t.Errorf("expected error status on failure, got %q", last.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 4: CAPRF Server Full Protocol E2E
// ---------------------------------------------------------------------------

func TestCAPRFServerFullProtocol(t *testing.T) {
	srv := newCAPRFTestServer()
	baseURL := startTestServer(t, srv.handler())
	ctx := context.Background()

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:      "e2e-token",
			InitURL:    baseURL + "/status/init",
			SuccessURL: baseURL + "/status/success",
			ErrorURL:   baseURL + "/status/error",
			LogURL:     baseURL + "/log",
			DebugURL:   baseURL + "/debug",
		},
	}
	client := caprf.NewFromConfig(cfg)

	if err := client.ReportStatus(ctx, config.StatusInit, "provisioning started"); err != nil {
		t.Fatal(err)
	}
	if err := client.ShipLog(ctx, "step 1: detecting disk"); err != nil {
		t.Fatal(err)
	}
	if err := client.ShipLog(ctx, "step 2: streaming image"); err != nil {
		t.Fatal(err)
	}
	if err := client.ShipDebug(ctx, "debug: partition table parsed"); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportStatus(ctx, config.StatusSuccess, "provisioning complete"); err != nil {
		t.Fatal(err)
	}

	statuses := srv.getStatuses()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d: %v", len(statuses), statuses)
	}
	if !strings.Contains(statuses[0], "init:Bearer e2e-token:provisioning started") {
		t.Errorf("unexpected init: %s", statuses[0])
	}
	if !strings.Contains(statuses[1], "success:Bearer e2e-token:provisioning complete") {
		t.Errorf("unexpected success: %s", statuses[1])
	}

	logs := srv.getLogs()
	if len(logs) != 2 {
		t.Fatalf("expected 2 logs, got %d", len(logs))
	}
	if logs[0] != "step 1: detecting disk" {
		t.Errorf("log[0] = %q", logs[0])
	}

	debugs := srv.getDebugs()
	if len(debugs) != 1 {
		t.Fatalf("expected 1 debug, got %d", len(debugs))
	}
	if debugs[0] != "debug: partition table parsed" {
		t.Errorf("debug[0] = %q", debugs[0])
	}
}

// ---------------------------------------------------------------------------
// Test 5: CAPRF Error Flow
// ---------------------------------------------------------------------------

func TestCAPRFErrorFlowWithServer(t *testing.T) {
	srv := newCAPRFTestServer()
	baseURL := startTestServer(t, srv.handler())
	ctx := context.Background()

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:    "err-token",
			InitURL:  baseURL + "/status/init",
			ErrorURL: baseURL + "/status/error",
		},
	}
	client := caprf.NewFromConfig(cfg)

	_ = client.ReportStatus(ctx, config.StatusInit, "starting")
	_ = client.ReportStatus(ctx, config.StatusError, "step detect-disk failed: no suitable disk")

	statuses := srv.getStatuses()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if !strings.Contains(statuses[1], "error:") {
		t.Errorf("second status should be error: %s", statuses[1])
	}
	if !strings.Contains(statuses[1], "no suitable disk") {
		t.Errorf("error should contain message: %s", statuses[1])
	}
}

// ---------------------------------------------------------------------------
// Test 6: FRR Config Rendering
// ---------------------------------------------------------------------------

func TestFRRConfigRenderingE2E(t *testing.T) {
	netCfg := &network.Config{
		UnderlaySubnet: "10.0.0.0/24",
		UnderlayIP:     "10.0.0.42",
		OverlaySubnet:  "fd00::/64",
		IPMISubnet:     "172.16.0.0/24",
		IPMIMAC:        "aa:bb:cc:dd:ee:ff",
		IPMIIP:         "172.16.0.42",
		ASN:            65001,
		ProvisionVNI:   10100,
		DNSResolvers:   "8.8.8.8",
		BridgeName:     "br.provision",
		VRFName:        "Vrf_underlay",
		MTU:            9000,
	}

	if !netCfg.IsFRRMode() {
		t.Fatal("expected FRR mode")
	}

	rendered, err := frr.RenderConfig(netCfg, "10.0.0.42", "", []string{"eth0", "eth1"})
	if err != nil {
		t.Fatal(err)
	}

	checks := []string{
		"router bgp 65001 vrf Vrf_underlay",
		"bgp router-id 10.0.0.42",
		"neighbor eth0 interface peer-group fabric",
		"neighbor eth1 interface peer-group fabric",
		"neighbor fabric remote-as external",
		"advertise-all-vni",
		"address-family l2vpn evpn",
	}
	for _, check := range checks {
		if !strings.Contains(rendered, check) {
			t.Errorf("rendered config missing %q", check)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 7: FRR Onefabric Config
// ---------------------------------------------------------------------------

func TestFRROnefabricConfigE2E(t *testing.T) {
	netCfg := &network.Config{
		UnderlayIP:       "10.0.0.42",
		ASN:              65001,
		VRFName:          "Vrf_underlay",
		DCGWIPs:          "10.99.0.1,10.99.0.2",
		LeafASN:          65100,
		LocalASN:         65200,
		OverlayAggregate: "fd00::/48",
		VPNRT:            "65001:10100",
	}

	rendered, err := frr.RenderConfig(netCfg, "10.0.0.42", "", []string{"eth0"})
	if err != nil {
		t.Fatal(err)
	}

	checks := []string{
		"neighbor 10.99.0.1 remote-as internal",
		"neighbor 10.99.0.2 remote-as internal",
		"aggregate-address fd00::/48",
		"route-target both 65001:10100",
	}
	for _, check := range checks {
		if !strings.Contains(rendered, check) {
			t.Errorf("onefabric config missing %q", check)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 8: FRR Address Derivation
// ---------------------------------------------------------------------------

func TestFRRAddressDerivationE2E(t *testing.T) {
	netCfg := &network.Config{
		IPMIIP:         "172.16.0.42",
		IPMISubnet:     "172.16.0.0/24",
		UnderlaySubnet: "10.0.0.0/24",
		OverlaySubnet:  "fd00::/64",
		IPMIMAC:        "aa:bb:cc:dd:ee:ff",
		ASN:            65001,
	}

	underlayIP, overlayIP, bridgeMAC, err := frr.DeriveAddresses(netCfg)
	if err != nil {
		t.Fatal(err)
	}

	if underlayIP != "10.0.0.42" {
		t.Errorf("underlayIP = %q, want 10.0.0.42", underlayIP)
	}
	if overlayIP != "fd00::2a" {
		t.Errorf("overlayIP = %q, want fd00::2a", overlayIP)
	}
	if bridgeMAC != "02:54:cc:dd:ee:ff" {
		t.Errorf("bridgeMAC = %q, want 02:54:cc:dd:ee:ff", bridgeMAC)
	}
}

// ---------------------------------------------------------------------------
// Test 9: Image Streaming Gzip
// ---------------------------------------------------------------------------

func TestImageStreamingGzipE2E(t *testing.T) {
	testData := bytes.Repeat([]byte("BOOTIMAGE"), 1024)
	compressed := gzipData(testData)

	mux := http.NewServeMux()
	mux.HandleFunc("/images/test.img.gz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed)
	})
	srvURL := startTestServer(t, mux)

	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "disk.img")
	if err := os.WriteFile(tmpPath, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}

	err := image.Stream(context.Background(), srvURL+"/images/test.img.gz", tmpPath)
	if err != nil {
		t.Fatalf("image.Stream failed: %v", err)
	}

	written, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, testData) {
		t.Errorf("written data (%d bytes) != original (%d bytes)", len(written), len(testData))
	}
}

// ---------------------------------------------------------------------------
// Test 10: Image Streaming Raw
// ---------------------------------------------------------------------------

func TestImageStreamingRawE2E(t *testing.T) {
	testData := bytes.Repeat([]byte("RAWIMAGE"), 512)

	mux := http.NewServeMux()
	mux.HandleFunc("/images/test.img", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(testData)
	})
	srvURL := startTestServer(t, mux)

	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "disk.img")
	if err := os.WriteFile(tmpPath, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}

	err := image.Stream(context.Background(), srvURL+"/images/test.img", tmpPath)
	if err != nil {
		t.Fatalf("image.Stream failed: %v", err)
	}

	written, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, testData) {
		t.Errorf("written data (%d bytes) != original (%d bytes)", len(written), len(testData))
	}
}

// ---------------------------------------------------------------------------
// Test 11: Image Streaming 404
// ---------------------------------------------------------------------------

func TestImageStreamingNotFoundE2E(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srvURL := startTestServer(t, mux)

	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "disk.img")
	if err := os.WriteFile(tmpPath, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}

	err := image.Stream(context.Background(), srvURL+"/images/missing.img", tmpPath)
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

// ---------------------------------------------------------------------------
// Test 12: Provision Step Failure Reports Error
// ---------------------------------------------------------------------------

func TestProvisionStepFailureReportsError(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("mdadm", nil, fmt.Errorf("mdadm: no arrays found"))

	cfg := &config.MachineConfig{
		Mode:     "provision",
		Hostname: "fail-node",
		Network: config.NetworkConfig{
			DNSResolvers: "8.8.8.8",
		},
	}
	provider := newMockProvider(cfg)
	diskMgr := disk.NewManager(cmd)

	orch := provision.NewOrchestrator(cfg, provider, diskMgr)
	err := orch.Provision(context.Background())

	if err == nil {
		t.Fatal("expected provision to fail")
	}

	statuses := provider.getStatuses()
	if len(statuses) < 2 {
		t.Fatalf("expected >= 2 statuses, got %d: %v", len(statuses), statuses)
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init", statuses[0].Status)
	}
	last := statuses[len(statuses)-1]
	if last.Status != config.StatusError {
		t.Errorf("last status = %q, want error", last.Status)
	}
}

// ---------------------------------------------------------------------------
// Test 13: Configurator File Operations
// ---------------------------------------------------------------------------

func TestConfiguratorFileOperationsE2E(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("chroot", []byte(""), nil)

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	root := t.TempDir()
	c.SetRootDir(root)
	// Keep serial console resolution hermetic: point the host evidence trees at
	// empty directories so the outcome does not depend on the runner's UARTs.
	c.SetHostRoots(t.TempDir(), t.TempDir())
	c.SetRunDir(t.TempDir())

	cfg := &config.MachineConfig{
		Hostname: "config-node",
		OSFamily: "ubuntu",
		Provision: config.ProvisionConfig{
			ProviderID:        "redfish://bmc/Systems/1",
			FailureDomain:     "dc1-az2",
			Region:            "eu-west",
			ExtraKernelParams: "audit=0 quiet",
			CloudInit:         config.CloudInitConfig{Enabled: true},
		},
		Network: config.NetworkConfig{
			DNSResolvers: "8.8.8.8,1.1.1.1",
		},
	}

	// SetHostname
	if err := c.SetHostname(cfg); err != nil {
		t.Fatalf("SetHostname: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "etc", "hostname"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "config-node") {
		t.Errorf("hostname = %q", string(data))
	}

	// ConfigureKubelet
	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatalf("ConfigureKubelet: %v", err)
	}
	kubeletConf, err := os.ReadFile(filepath.Join(root, "etc", "default", "kubelet"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"redfish://bmc/Systems/1",
		"topology.kubernetes.io/zone=dc1-az2",
		"topology.kubernetes.io/region=eu-west",
	} {
		if !strings.Contains(string(kubeletConf), want) {
			t.Errorf("kubelet env missing %s: %q", want, string(kubeletConf))
		}
	}

	// ConfigureDNS
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("ConfigureDNS: %v", err)
	}
	dns, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dns), "nameserver 8.8.8.8") {
		t.Errorf("resolv.conf missing 8.8.8.8: %q", string(dns))
	}
	if !strings.Contains(string(dns), "nameserver 1.1.1.1") {
		t.Errorf("resolv.conf missing 1.1.1.1: %q", string(dns))
	}

	// ConfigureGRUB
	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
	grub, err := os.ReadFile(filepath.Join(root, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(grub), "ds=nocloud") {
		t.Errorf("grub config missing ds=nocloud: %q", string(grub))
	}
	if !strings.Contains(string(grub), "audit=0 quiet") {
		t.Errorf("grub config missing extra params: %q", string(grub))
	}
}

// ---------------------------------------------------------------------------
// Test 14: Mellanox Firmware Detection
// ---------------------------------------------------------------------------

func TestMellanoxFirmwareDetectionE2E(t *testing.T) {
	restore := provision.SetPCIVendorCheckFunc(func(string) (bool, error) { return true, nil })
	defer restore()
	seqCmd := &sequentialCmd{
		results: []cmdResult{
			{Output: []byte("mt4125_pciconf0\n")},
			{Output: []byte("Applied")},
		},
	}

	diskMgr := disk.NewManager(seqCmd)
	c := provision.NewConfigurator(diskMgr)
	c.SetRootDir(t.TempDir())

	changed, err := c.SetupMellanox(context.Background(), 32)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("expected firmwareChanged=true after Mellanox config")
	}
}

func TestFirmwareChangedDefaultFalse(t *testing.T) {
	cfg := &config.MachineConfig{}
	orch := provision.NewOrchestrator(cfg, newMockProvider(cfg), disk.NewManager(newMockCommander()))
	if orch.FirmwareChanged() {
		t.Error("new orchestrator should have firmwareChanged=false")
	}
}

// ---------------------------------------------------------------------------
// Test 15: Kexec Grub Parsing
// ---------------------------------------------------------------------------

func TestKexecFullGrubParsingE2E(t *testing.T) {
	grubCfg := `
set default="0"
set timeout=5

menuentry 'Ubuntu 22.04 LTS' --class ubuntu {
	linux /boot/vmlinuz-5.15.0-91-generic root=UUID=abc-123 ro console=ttyS0,115200 quiet
	initrd /boot/initrd.img-5.15.0-91-generic
}

menuentry 'Ubuntu Recovery Mode' --class ubuntu {
	linux /boot/vmlinuz-5.15.0-91-generic root=UUID=abc-123 ro single
	initrd /boot/initrd.img-5.15.0-91-generic
}
`
	entries, err := kexec.ParseGrubCfg(strings.NewReader(grubCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	defEntry, err := kexec.GetDefaultEntry(entries)
	if err != nil {
		t.Fatal(err)
	}
	if defEntry.Kernel != "/boot/vmlinuz-5.15.0-91-generic" {
		t.Errorf("kernel = %q", defEntry.Kernel)
	}
	if defEntry.Initramfs != "/boot/initrd.img-5.15.0-91-generic" {
		t.Errorf("initrd = %q", defEntry.Initramfs)
	}
	if !strings.Contains(defEntry.KernelArgs, "console=ttyS0,115200") {
		t.Errorf("args should contain console: %q", defEntry.KernelArgs)
	}
}

// ---------------------------------------------------------------------------
// Test 16: Network Mode Detection from Vars
// ---------------------------------------------------------------------------

func TestNetworkModeDetectionFromVarsE2E(t *testing.T) {
	vars := fmt.Sprintf(`export IMAGE="%s"
export HOSTNAME="net-node"
export TOKEN="net-token"
export MODE="provision"
underlay_subnet="10.0.0.0/24"
underlay_ip="10.0.0.5"
overlay_subnet="fd00::/64"
ipmi_subnet="172.16.0.0/24"
asn_server="65001"
provision_vni="10100"
dns_resolver="8.8.8.8,1.1.1.1"
`, startE2ERawImageServer(t))
	cfg, err := caprf.ParseVars(strings.NewReader(vars))
	if err != nil {
		t.Fatal(err)
	}

	netCfg := &network.Config{
		UnderlaySubnet: cfg.Network.EVPN.UnderlaySubnet,
		UnderlayIP:     cfg.Network.EVPN.UnderlayIP,
		OverlaySubnet:  cfg.Network.EVPN.OverlaySubnet,
		IPMISubnet:     cfg.Network.IPMI.Subnet,
		ASN:            cfg.Network.BGP.ASN,
		ProvisionVNI:   cfg.Network.EVPN.ProvisionVNI,
		DNSResolvers:   cfg.Network.DNSResolvers,
	}

	if !netCfg.IsFRRMode() {
		t.Error("expected FRR mode with underlay+ASN")
	}

	dhcpCfg := &network.Config{UnderlaySubnet: cfg.Network.EVPN.UnderlaySubnet}
	if dhcpCfg.IsFRRMode() {
		t.Error("expected DHCP mode without ASN")
	}
}

// ---------------------------------------------------------------------------
// Test 17: Concurrent Status Reporting (race detector)
// ---------------------------------------------------------------------------

func TestConcurrentStatusReportingE2E(t *testing.T) {
	srv := newCAPRFTestServer()
	baseURL := startTestServer(t, srv.handler())

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:      "race-token",
			InitURL:    baseURL + "/status/init",
			SuccessURL: baseURL + "/status/success",
			ErrorURL:   baseURL + "/status/error",
			LogURL:     baseURL + "/log",
		},
	}
	client := caprf.NewFromConfig(cfg)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = client.ShipLog(ctx, fmt.Sprintf("concurrent log %d", n))
			_ = client.ReportStatus(ctx, config.StatusInit, fmt.Sprintf("init %d", n))
		}(i)
	}
	wg.Wait()

	statuses := srv.getStatuses()
	logs := srv.getLogs()
	if len(statuses) != 20 {
		t.Errorf("expected 20 statuses, got %d", len(statuses))
	}
	if len(logs) != 20 {
		t.Errorf("expected 20 logs, got %d", len(logs))
	}
}

// ---------------------------------------------------------------------------
// Test 18: Context Cancellation
// ---------------------------------------------------------------------------

func TestProvisionContextCancellationE2E(t *testing.T) {
	cmd := newMockCommander()
	cfg := &config.MachineConfig{Mode: "provision", Hostname: "cancel-node"}
	provider := newMockProvider(cfg)
	diskMgr := disk.NewManager(cmd)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	orch := provision.NewOrchestrator(cfg, provider, diskMgr)
	err := orch.Provision(ctx)
	if err != nil {
		t.Logf("Provision correctly failed on cancelled context: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 19: Full Vars->Client->Provision Pipeline
// ---------------------------------------------------------------------------

func TestFullVarsToProvisionPipelineE2E(t *testing.T) {
	srv := newCAPRFTestServer()
	baseURL := startTestServer(t, srv.handler())

	vars := fmt.Sprintf(`export IMAGE="%s"
export HOSTNAME="pipeline-node"
export TOKEN="pipeline-token"
export MODE="provision"
export PROVIDER_ID="redfish://10.0.0.1/Systems/1"
export FAILURE_DOMAIN="az-1"
export REGION="eu-central"
export MACHINE_EXTRA_KERNEL_PARAMS="console=ttyS0"
export MIN_DISK_SIZE_GB="0"
export INIT_URL="%s/status/init"
export SUCCESS_URL="%s/status/success"
export ERROR_URL="%s/status/error"
export LOG_URL="%s/log"
export DEBUG_URL="%s/debug"
dns_resolver="8.8.8.8"
`, startE2ERawImageServer(t), baseURL, baseURL, baseURL, baseURL, baseURL)

	cfg, err := caprf.ParseVars(strings.NewReader(vars))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Hostname != "pipeline-node" {
		t.Errorf("hostname = %q", cfg.Hostname)
	}
	if cfg.Transport.Token != "pipeline-token" {
		t.Errorf("token = %q", cfg.Transport.Token)
	}
	if cfg.Provision.ProviderID != "redfish://10.0.0.1/Systems/1" {
		t.Errorf("providerID = %q", cfg.Provision.ProviderID)
	}

	netCfg := &network.Config{
		UnderlaySubnet: cfg.Network.EVPN.UnderlaySubnet,
		ASN:            cfg.Network.BGP.ASN,
	}
	if netCfg.IsFRRMode() {
		t.Error("expected DHCP mode (no ASN in vars)")
	}

	client := caprf.NewFromConfig(cfg)
	ctx := context.Background()

	cmd := newMockCommander()
	cmd.set("mdadm", []byte(""), nil)
	cmd.set("wipefs", []byte(""), nil)
	cmd.set("chroot", []byte(""), nil)

	diskMgr := disk.NewManager(cmd)
	orch := provision.NewOrchestrator(cfg, client, diskMgr)
	err = orch.Provision(ctx)

	statuses := srv.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("server received no status reports")
	}
	if !strings.Contains(statuses[0], "init:Bearer pipeline-token") {
		t.Errorf("first status should be init with token: %s", statuses[0])
	}
	if err != nil {
		t.Logf("Pipeline failed at expected step: %v", err)
		last := statuses[len(statuses)-1]
		if !strings.Contains(last, "error:") {
			t.Errorf("expected error status, got: %s", last)
		}
	} else {
		last := statuses[len(statuses)-1]
		if !strings.Contains(last, "success:") {
			t.Errorf("expected success status, got: %s", last)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 20: Mock Network Mode
// ---------------------------------------------------------------------------

type mockNetworkMode struct {
	mu             sync.Mutex
	setupCalled    bool
	teardownCalled bool
	connectCalled  bool
}

func (m *mockNetworkMode) Setup(_ context.Context, _ *network.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setupCalled = true
	return nil
}

func (m *mockNetworkMode) WaitForConnectivity(_ context.Context, _ string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connectCalled = true
	return nil
}

func (m *mockNetworkMode) Teardown(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.teardownCalled = true
	return nil
}

func TestMockNetworkModeIntegrationE2E(t *testing.T) {
	netMode := &mockNetworkMode{}
	ctx := context.Background()

	netCfg := &network.Config{
		UnderlaySubnet: "10.0.0.0/24",
		ASN:            65001,
		ProvisionVNI:   10100,
	}

	if err := netMode.Setup(ctx, netCfg); err != nil {
		t.Fatal(err)
	}
	if !netMode.setupCalled {
		t.Error("Setup should have been called")
	}

	if err := netMode.WaitForConnectivity(ctx, "http://server:8080", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if !netMode.connectCalled {
		t.Error("WaitForConnectivity should have been called")
	}

	if err := netMode.Teardown(ctx); err != nil {
		t.Fatal(err)
	}
	if !netMode.teardownCalled {
		t.Error("Teardown should have been called")
	}
}

// ---------------------------------------------------------------------------
// Test 21: DHCP WaitForConnectivity
// ---------------------------------------------------------------------------

func TestDHCPWaitForConnectivityE2E(t *testing.T) {
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	srvURL := startTestServer(t, mux)

	go func() {
		time.Sleep(500 * time.Millisecond)
		ready.Store(true)
	}()

	mode := &network.DHCPMode{}
	err := mode.WaitForConnectivity(context.Background(), srvURL, 10*time.Second)
	if err != nil {
		t.Fatalf("DHCP WaitForConnectivity failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 22: Partition Number
// ---------------------------------------------------------------------------

func TestPartitionNumberE2E(t *testing.T) {
	tests := []struct {
		node string
		d    string
		want int
	}{
		{"/dev/sda1", "/dev/sda", 1},
		{"/dev/sda2", "/dev/sda", 2},
		{"/dev/nvme0n1p1", "/dev/nvme0n1", 1},
		{"/dev/nvme0n1p3", "/dev/nvme0n1", 3},
	}
	for _, tt := range tests {
		got := disk.PartitionNumber(tt.node, tt.d)
		if got != tt.want {
			t.Errorf("PartitionNumber(%q, %q) = %d, want %d", tt.node, tt.d, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 23: Configurator No-op File Operations
// ---------------------------------------------------------------------------

func TestMachineFilesAndCommandsNoopE2E(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("chroot", []byte("command-output"), nil)

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	root := t.TempDir()
	c.SetRootDir(root)

	if err := c.CopyProvisionerFiles(context.Background()); err != nil {
		t.Fatalf("CopyProvisionerFiles: %v", err)
	}
	if err := c.CopyMachineFiles(context.Background()); err != nil {
		t.Fatalf("CopyMachineFiles: %v", err)
	}
	if err := c.RunMachineCommands(context.Background(), false); err != nil {
		t.Fatalf("RunMachineCommands: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 24: Deprovision Default Mode
// ---------------------------------------------------------------------------

func TestDeprovisionDefaultMode(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("mdadm", []byte(""), nil)
	cmd.set("wipefs", []byte(""), nil)
	cmd.set("chroot", []byte("BootCurrent: 0001"), nil)

	cfg := &config.MachineConfig{Mode: ""}
	provider := newMockProvider(cfg)
	diskMgr := disk.NewManager(cmd)
	orch := provision.NewOrchestrator(cfg, provider, diskMgr)

	_ = orch.Deprovision(context.Background())

	statuses := provider.getStatuses()
	if len(statuses) == 0 {
		t.Fatal("expected status reports")
	}
	if statuses[0].Status != config.StatusInit {
		t.Errorf("first status = %q, want init", statuses[0].Status)
	}
}

// ---------------------------------------------------------------------------
// Test 25: Bridge MAC Derivation
// ---------------------------------------------------------------------------

func TestBridgeMACDerivationE2E(t *testing.T) {
	tests := []struct {
		mac  string
		want string
	}{
		{"aa:bb:cc:dd:ee:ff", "02:54:cc:dd:ee:ff"},
		{"00:11:22:33:44:55", "02:54:22:33:44:55"},
		{"aa-bb-cc-dd-ee-ff", "02:54:cc:dd:ee:ff"},
	}
	for _, tt := range tests {
		got := frr.DeriveBridgeMAC(tt.mac)
		if got != tt.want {
			t.Errorf("DeriveBridgeMAC(%q) = %q, want %q", tt.mac, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Test 26: CAPRF Client Empty URLs
// ---------------------------------------------------------------------------

func TestCAPRFClientEmptyURLs(t *testing.T) {
	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token: "test-token",
		},
	}
	client := caprf.NewFromConfig(cfg)
	ctx := context.Background()

	if err := client.ReportStatus(ctx, config.StatusInit, "init"); err != nil {
		t.Errorf("ReportStatus with empty URL should succeed: %v", err)
	}
	if err := client.ShipLog(ctx, "test log"); err != nil {
		t.Errorf("ShipLog with empty URL should succeed: %v", err)
	}
	if err := client.ShipDebug(ctx, "test debug"); err != nil {
		t.Errorf("ShipDebug with empty URL should succeed: %v", err)
	}
	if err := client.Heartbeat(ctx); err != nil {
		t.Errorf("Heartbeat should succeed: %v", err)
	}
	cmds, err := client.FetchCommands(ctx)
	if err != nil || cmds != nil {
		t.Errorf("FetchCommands should return nil, nil: %v, %v", cmds, err)
	}
}

// ---------------------------------------------------------------------------
// Test 27: CAPRF Image Serve + Stream
// ---------------------------------------------------------------------------

func TestCAPRFImageServeAndStreamE2E(t *testing.T) {
	imageData := bytes.Repeat([]byte("UBUNTU-IMAGE-"), 2048)
	compressed := gzipData(imageData)

	srv := newCAPRFTestServer()
	srv.images["ubuntu-22.04.img.gz"] = compressed

	baseURL := startTestServer(t, srv.handler())

	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "sda")
	if err := os.WriteFile(tmpPath, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}

	err := image.Stream(context.Background(), baseURL+"/images/ubuntu-22.04.img.gz", tmpPath)
	if err != nil {
		t.Fatalf("image.Stream from CAPRF server failed: %v", err)
	}

	written, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, imageData) {
		t.Errorf("image data mismatch: got %d bytes, want %d", len(written), len(imageData))
	}

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:      "img-token",
			SuccessURL: baseURL + "/status/success",
		},
	}
	client := caprf.NewFromConfig(cfg)
	if err := client.ReportStatus(context.Background(), config.StatusSuccess, "image written"); err != nil {
		t.Fatal(err)
	}

	statuses := srv.getStatuses()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if !strings.Contains(statuses[0], "success:Bearer img-token:image written") {
		t.Errorf("unexpected status: %s", statuses[0])
	}
}

// ---------------------------------------------------------------------------
// Test 28: RemoveEFIBootEntries
// ---------------------------------------------------------------------------

func TestRemoveEFIBootEntriesE2E(t *testing.T) {
	cmd := newMockCommander()

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	root := t.TempDir()
	c.SetRootDir(root)

	// RemoveEFIBootEntries now runs efibootmgr directly on the host
	// (not via chroot). On non-EFI systems it logs a warning and returns nil.
	if err := c.RemoveEFIBootEntries(context.Background()); err != nil {
		t.Fatalf("RemoveEFIBootEntries: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 29: ParseVars Full Field Coverage
// ---------------------------------------------------------------------------

func TestParseVarsFullFieldCoverageE2E(t *testing.T) {
	vars := `export IMAGE_ALLOW_INSECURE_HTTP="true"
export IMAGE="http://img.local/a.gz,http://img.local/b.gz"
export HOSTNAME="full-node"
export TOKEN="full-token"
export MODE="provision"
export PROVIDER_ID="redfish://bmc/Systems/1"
export FAILURE_DOMAIN="az-2"
export REGION="eu-central"
export MACHINE_EXTRA_KERNEL_PARAMS="audit=0"
export MIN_DISK_SIZE_GB="100"
export DISABLE_KEXEC="true"
export INIT_URL="http://caprf/init"
export SUCCESS_URL="http://caprf/success"
export ERROR_URL="http://caprf/error"
export LOG_URL="http://caprf/log"
export DEBUG_URL="http://caprf/debug"
export STATIC_IP="10.1.0.5/24"
export STATIC_GATEWAY="10.1.0.1"
export STATIC_IFACE="eth0"
export BOND_INTERFACES="eth0,eth1"
export BOND_MODE="802.3ad"
export SECURE_ERASE="true"
export POST_PROVISION_CMDS="apt update;systemctl enable foo"
export NUM_VFS="64"
export IMAGE_CHECKSUM="sha256hash"
export IMAGE_CHECKSUM_TYPE="sha256"
underlay_subnet="10.0.0.0/24"
underlay_ip="10.0.0.5"
overlay_subnet="fd00::/64"
ipmi_subnet="172.16.0.0/24"
asn_server="65001"
provision_vni="10100"
dns_resolver="8.8.8.8"
dcgw_ips="10.99.0.1,10.99.0.2"
leaf_asn="65100"
local_asn="65200"
overlay_aggregate="fd00::/48"
vpn_rt="65001:10100"
`
	cfg, err := caprf.ParseVars(strings.NewReader(vars))
	if err != nil {
		t.Fatal(err)
	}

	checks := []struct {
		field string
		got   string
		want  string
	}{
		{"Hostname", cfg.Hostname, "full-node"},
		{"Token", cfg.Transport.Token, "full-token"},
		{"Mode", cfg.Mode, "provision"},
		{"ProviderID", cfg.Provision.ProviderID, "redfish://bmc/Systems/1"},
		{"FailureDomain", cfg.Provision.FailureDomain, "az-2"},
		{"Region", cfg.Provision.Region, "eu-central"},
		{"ExtraKernelParams", cfg.Provision.ExtraKernelParams, "audit=0"},
		{"InitURL", cfg.Transport.InitURL, "http://caprf/init"},
		{"SuccessURL", cfg.Transport.SuccessURL, "http://caprf/success"},
		{"ErrorURL", cfg.Transport.ErrorURL, "http://caprf/error"},
		{"LogURL", cfg.Transport.LogURL, "http://caprf/log"},
		{"DebugURL", cfg.Transport.DebugURL, "http://caprf/debug"},
		{"UnderlaySubnet", cfg.Network.EVPN.UnderlaySubnet, "10.0.0.0/24"},
		{"UnderlayIP", cfg.Network.EVPN.UnderlayIP, "10.0.0.5"},
		{"OverlaySubnet", cfg.Network.EVPN.OverlaySubnet, "fd00::/64"},
		{"IPMISubnet", cfg.Network.IPMI.Subnet, "172.16.0.0/24"},
		{"DNSResolvers", cfg.Network.DNSResolvers, "8.8.8.8"},
		{"DCGWIPs", cfg.Network.EVPN.DCGWIPs, "10.99.0.1,10.99.0.2"},
		{"OverlayAggregate", cfg.Network.EVPN.OverlayAggregate, "fd00::/48"},
		{"VPNRT", cfg.Network.EVPN.VPNRT, "65001:10100"},
		{"StaticIP", cfg.Network.Static.IP, "10.1.0.5/24"},
		{"StaticGateway", cfg.Network.Static.Gateway, "10.1.0.1"},
		{"StaticIface", cfg.Network.Static.Iface, "eth0"},
		{"BondInterfaces", cfg.Network.Bond.Interfaces, "eth0,eth1"},
		{"BondMode", cfg.Network.Bond.Mode, "802.3ad"},
		{"ImageChecksum", cfg.Provision.Image.Checksum, "sha256hash"},
		{"ImageChecksumType", cfg.Provision.Image.ChecksumType, "sha256"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}

	if cfg.Provision.Disk.MinSizeGB != 100 {
		t.Errorf("MinDiskSizeGB = %d, want 100", cfg.Provision.Disk.MinSizeGB)
	}
	if !cfg.Provision.DisableKexec {
		t.Error("DisableKexec should be true")
	}
	if cfg.Network.BGP.ASN != 65001 {
		t.Errorf("ASN = %d, want 65001", cfg.Network.BGP.ASN)
	}
	if cfg.Network.EVPN.ProvisionVNI != 10100 {
		t.Errorf("ProvisionVNI = %d, want 10100", cfg.Network.EVPN.ProvisionVNI)
	}
	if cfg.Network.EVPN.LeafASN != 65100 {
		t.Errorf("LeafASN = %d, want 65100", cfg.Network.EVPN.LeafASN)
	}
	if cfg.Network.EVPN.LocalASN != 65200 {
		t.Errorf("LocalASN = %d, want 65200", cfg.Network.EVPN.LocalASN)
	}
	if cfg.Provision.Disk.NumVFs != 64 {
		t.Errorf("NumVFs = %d, want 64", cfg.Provision.Disk.NumVFs)
	}
	if !cfg.Provision.Disk.SecureErase {
		t.Error("SecureErase should be true")
	}
	if len(cfg.Provision.PostProvisionCmds) != 2 {
		t.Errorf("PostProvisionCmds len = %d, want 2", len(cfg.Provision.PostProvisionCmds))
	}
	if len(cfg.Provision.Image.URLs) != 2 {
		t.Errorf("ImageURLs = %v, want 2 entries", cfg.Provision.Image.URLs)
	}
}

// ---------------------------------------------------------------------------
// Test 30: FRR IP Derivation IPv4 + IPv6
// ---------------------------------------------------------------------------

func TestFRRIPDerivationE2E(t *testing.T) {
	ip, err := frr.DeriveIPFromOffset("172.16.0.42", "172.16.0.0/24", "10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.42" {
		t.Errorf("IPv4 derivation = %q, want 10.0.0.42", ip)
	}

	ip6, err := frr.DeriveIPFromOffset("172.16.0.42", "172.16.0.0/24", "fd00::/64")
	if err != nil {
		t.Fatal(err)
	}
	if ip6 != "fd00::2a" {
		t.Errorf("IPv6 derivation = %q, want fd00::2a", ip6)
	}
}

// ---------------------------------------------------------------------------
// Test 31: CAPRF Server Unreachable
// ---------------------------------------------------------------------------

func TestCAPRFServerUnreachable(t *testing.T) {
	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:   "test-token",
			InitURL: "http://127.0.0.1:1/status/init",
		},
	}
	client := caprf.NewFromConfig(cfg)
	err := client.ReportStatus(context.Background(), config.StatusInit, "test")
	if err == nil {
		t.Error("expected error for unreachable server")
	}
}

// ---------------------------------------------------------------------------
// Test 32: Mellanox No NICs
// ---------------------------------------------------------------------------

func TestMellanoxNoNICsFailWhenRequestedE2E(t *testing.T) {
	restore := provision.SetPCIVendorCheckFunc(func(string) (bool, error) { return false, nil })
	defer restore()
	cmd := newMockCommander()
	cmd.set("chroot", []byte(""), nil)

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	c.SetRootDir(t.TempDir())

	changed, err := c.SetupMellanox(context.Background(), 32)
	if err == nil {
		t.Fatal("expected NUM_VFS request without Mellanox NICs to fail")
	}
	if !strings.Contains(err.Error(), "no Mellanox NICs found") {
		t.Fatalf("error = %v, want no Mellanox NICs found", err)
	}
	if changed {
		t.Error("expected firmwareChanged=false without Mellanox NICs")
	}
}

// ---------------------------------------------------------------------------
// Test 33: DHCP WaitForConnectivity Any HTTP Response
// ---------------------------------------------------------------------------

func TestDHCPWaitForConnectivityAnyHTTPResponseE2E(t *testing.T) {
	// A server returning any HTTP response (even 503) proves network
	// connectivity — WaitForConnectivity should succeed.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srvURL := startTestServer(t, mux)

	mode := &network.DHCPMode{}
	err := mode.WaitForConnectivity(context.Background(), srvURL, 1*time.Second)
	if err != nil {
		t.Errorf("expected success (any HTTP response proves connectivity), got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test 34: ConfigureDNS No Resolvers
// ---------------------------------------------------------------------------

func TestConfigureDNSNoResolversE2E(t *testing.T) {
	cmd := newMockCommander()
	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	root := t.TempDir()
	c.SetRootDir(root)

	cfg := &config.MachineConfig{
		Network: config.NetworkConfig{
			DNSResolvers: "",
		},
	}
	if err := c.ConfigureDNS(cfg); err != nil {
		t.Fatalf("ConfigureDNS with empty resolvers should succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc", "resolv.conf")); !os.IsNotExist(err) {
		t.Error("resolv.conf should not be created with empty resolvers")
	}
}

// ---------------------------------------------------------------------------
// Test 35: Image Streaming Context Cancellation
// ---------------------------------------------------------------------------

func TestImageStreamingContextCancellationE2E(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/images/hang.img", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("header"))
		select {}
	})
	srvURL := startTestServer(t, mux)

	tmpDir := t.TempDir()
	tmpPath := filepath.Join(tmpDir, "disk.img")
	if err := os.WriteFile(tmpPath, []byte{}, 0o666); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := image.Stream(ctx, srvURL+"/images/hang.img", tmpPath)
	if err == nil {
		t.Error("expected error on context cancellation")
	}
}

// ---------------------------------------------------------------------------
// Test 36: DisableKexec Config
// ---------------------------------------------------------------------------

func TestKexecDisabledViaConfigE2E(t *testing.T) {
	cfg := &config.MachineConfig{
		Provision: config.ProvisionConfig{
			DisableKexec: true,
		},
	}
	if !cfg.Provision.DisableKexec {
		t.Error("DisableKexec should be true")
	}

	vars := `export DISABLE_KEXEC="true"
export MODE="provision"
export HOSTNAME="kexec-node"
`
	parsed, err := caprf.ParseVars(strings.NewReader(vars))
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Provision.DisableKexec {
		t.Error("parsed DisableKexec should be true")
	}
}

// ---------------------------------------------------------------------------
// Test 37: Kubelet No Labels
// ---------------------------------------------------------------------------

func TestConfiguratorKubeletNoLabelsE2E(t *testing.T) {
	cmd := newMockCommander()
	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	root := t.TempDir()
	c.SetRootDir(root)

	cfg := &config.MachineConfig{
		OSFamily: "ubuntu",
		Provision: config.ProvisionConfig{
			ProviderID: "redfish://bmc/Systems/1",
		},
	}

	if err := c.ConfigureKubelet(cfg); err != nil {
		t.Fatal(err)
	}

	pidConf, err := os.ReadFile(filepath.Join(root, "etc", "default", "kubelet"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pidConf), "redfish://bmc/Systems/1") {
		t.Errorf("unexpected content: %q", string(pidConf))
	}

	if strings.Contains(string(pidConf), "--node-labels=") {
		t.Errorf("node labels should not exist without failure domain or region: %q", string(pidConf))
	}
}

// ---------------------------------------------------------------------------
// Test: CreateEFIBootEntry
// ---------------------------------------------------------------------------

func TestCreateEFIBootEntryE2E(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("chroot", []byte("Boot0001* ubuntu"), nil)

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	root := t.TempDir()
	c.SetRootDir(root)

	shimName := "shimx64.efi"
	if runtime.GOARCH == "arm64" {
		shimName = "shimaa64.efi"
	}

	// Create arch-specific shim so loader detection works.
	efiDir := filepath.Join(root, "boot", "efi", "EFI", "ubuntu")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(efiDir, shimName), []byte("shim"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.CreateEFIBootEntry(context.Background(), "/dev/sda", "/dev/sda1"); err != nil {
		t.Fatalf("CreateEFIBootEntry: %v", err)
	}

	calls := cmd.getCalls()
	found := false
	for _, call := range calls {
		if strings.Contains(call.String(), "efibootmgr -c") {
			found = true
			if !strings.Contains(call.String(), "-d /dev/sda") {
				t.Errorf("expected -d /dev/sda in call: %s", call)
			}
			if !strings.Contains(call.String(), "-p 1") {
				t.Errorf("expected -p 1 in call: %s", call)
			}
			if !strings.Contains(call.String(), shimName) {
				t.Errorf("expected %s in call: %s", shimName, call)
			}
		}
	}
	if !found {
		t.Errorf("expected efibootmgr -c call, got: %v", calls)
	}
}

func TestCreateEFIBootEntryGrubFallbackE2E(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("chroot", []byte("Boot0001* ubuntu"), nil)

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	root := t.TempDir()
	c.SetRootDir(root)

	grubName := "grubx64.efi"
	if runtime.GOARCH == "arm64" {
		grubName = "grubaa64.efi"
	}

	// No shim but arch-specific grub present — should fallback to grub.
	efiDir := filepath.Join(root, "boot", "efi", "EFI", "ubuntu")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(efiDir, grubName), []byte("grub"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.CreateEFIBootEntry(context.Background(), "/dev/nvme0n1", "/dev/nvme0n1p1"); err != nil {
		t.Fatalf("CreateEFIBootEntry: %v", err)
	}

	calls := cmd.getCalls()
	found := false
	for _, call := range calls {
		if strings.Contains(call.String(), "efibootmgr -c") {
			found = true
			if !strings.Contains(call.String(), grubName) {
				t.Errorf("expected %s fallback in call: %s", grubName, call)
			}
			if !strings.Contains(call.String(), "-p 1") {
				t.Errorf("expected -p 1 for nvme0n1p1 in call: %s", call)
			}
		}
	}
	if !found {
		t.Errorf("expected efibootmgr -c call, got: %v", calls)
	}
}

func TestCreateEFIBootEntryEmptyPartitionE2E(t *testing.T) {
	cmd := newMockCommander()
	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	c.SetRootDir(t.TempDir())

	// Empty partition should skip gracefully.
	if err := c.CreateEFIBootEntry(context.Background(), "/dev/sda", ""); err != nil {
		t.Fatalf("CreateEFIBootEntry with empty partition: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: DisableLVM
// ---------------------------------------------------------------------------

func TestDisableLVME2E(t *testing.T) {
	cmd := newMockCommander()
	diskMgr := disk.NewManager(cmd)

	if err := diskMgr.DisableLVM(context.Background()); err != nil {
		t.Fatalf("DisableLVM: %v", err)
	}

	calls := cmd.getCalls()
	found := false
	for _, call := range calls {
		if strings.Contains(call.String(), "lvm") && strings.Contains(call.String(), "vgchange") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected lvm vgchange call, got: %v", calls)
	}
}

func TestDisableLVMNotPresentE2E(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("lvm", nil, fmt.Errorf("lvm: command not found"))

	diskMgr := disk.NewManager(cmd)

	// Should not return error even when lvm is not present.
	if err := diskMgr.DisableLVM(context.Background()); err != nil {
		t.Fatalf("DisableLVM should not fail: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: CreateRAIDArray
// ---------------------------------------------------------------------------

func TestCreateRAIDArrayE2E(t *testing.T) {
	cmd := newMockCommander()
	diskMgr := disk.NewManager(cmd)

	err := diskMgr.CreateRAIDArray(context.Background(), "md0", 1, []string{"/dev/sda", "/dev/sdb"})
	if err != nil {
		t.Fatalf("CreateRAIDArray: %v", err)
	}

	calls := cmd.getCalls()
	found := false
	for _, call := range calls {
		if strings.Contains(call.String(), "mdadm") && strings.Contains(call.String(), "--create") {
			found = true
			if !strings.Contains(call.String(), "/dev/md0") {
				t.Errorf("expected /dev/md0 in call: %s", call)
			}
		}
	}
	if !found {
		t.Errorf("expected mdadm call, got: %v", calls)
	}
}

func TestCreateRAIDArrayTooFewDevicesE2E(t *testing.T) {
	cmd := newMockCommander()
	diskMgr := disk.NewManager(cmd)

	err := diskMgr.CreateRAIDArray(context.Background(), "md0", 1, []string{"/dev/sda"})
	if err == nil {
		t.Fatal("expected error with single device")
	}
}

// ---------------------------------------------------------------------------
// Test: RunPostProvisionCmds
// ---------------------------------------------------------------------------

func TestRunPostProvisionCmdsE2E(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("chroot", []byte("ok"), nil)

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	c.SetRootDir(t.TempDir())

	cmds := []string{"apt update", "systemctl enable foo", "echo done"}
	if err := c.RunPostProvisionCmds(context.Background(), cmds); err != nil {
		t.Fatalf("RunPostProvisionCmds: %v", err)
	}
}

func TestRunPostProvisionCmdsEmptyE2E(t *testing.T) {
	cmd := newMockCommander()
	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	c.SetRootDir(t.TempDir())

	// Empty cmds should be a no-op.
	if err := c.RunPostProvisionCmds(context.Background(), nil); err != nil {
		t.Fatalf("RunPostProvisionCmds with nil: %v", err)
	}
}

func TestRunPostProvisionCmdsFailE2E(t *testing.T) {
	cmd := newMockCommander()
	cmd.set("chroot", nil, fmt.Errorf("command failed"))

	diskMgr := disk.NewManager(cmd)
	c := provision.NewConfigurator(diskMgr)
	c.SetRootDir(t.TempDir())

	cmds := []string{"failing-command"}
	if err := c.RunPostProvisionCmds(context.Background(), cmds); err == nil {
		t.Fatal("expected error when post-provision command fails")
	}
}
