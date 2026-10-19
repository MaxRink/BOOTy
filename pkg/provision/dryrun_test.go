//go:build linux

package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/stream"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	"github.com/telekom/BOOTy/pkg/health"
)

type dryRunProvider struct {
	lastStatus    config.Status
	lastMessage   string
	healthReports [][]health.CheckResult
}

type fakeFileInfo struct {
	name string
	mode os.FileMode
}

func (f fakeFileInfo) Name() string {
	if f.name != "" {
		return f.name
	}
	return "mock"
}

func (fakeFileInfo) Size() int64 { return 0 }

func (f fakeFileInfo) Mode() os.FileMode {
	if f.mode != 0 {
		return f.mode
	}
	return os.ModeDir
}

func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool      { return f.Mode().IsDir() }
func (fakeFileInfo) Sys() any           { return nil }

func (p *dryRunProvider) GetConfig(_ context.Context) (*config.MachineConfig, error) {
	return &config.MachineConfig{}, nil
}
func (p *dryRunProvider) ReportStatus(_ context.Context, s config.Status, msg string) error {
	p.lastStatus = s
	p.lastMessage = msg
	return nil
}
func (p *dryRunProvider) ShipLog(_ context.Context, _ string) error                  { return nil }
func (p *dryRunProvider) Heartbeat(_ context.Context) error                          { return nil }
func (p *dryRunProvider) FetchCommands(_ context.Context) ([]config.Command, error)  { return nil, nil }
func (p *dryRunProvider) AcknowledgeCommand(_ context.Context, _, _, _ string) error { return nil }
func (p *dryRunProvider) ReportInventory(_ context.Context, _ []byte) error          { return nil }
func (p *dryRunProvider) ReportFirmware(_ context.Context, _ []byte) error           { return nil }
func (p *dryRunProvider) ReportHealthChecks(_ context.Context, results []health.CheckResult) error {
	p.healthReports = append(p.healthReports, append([]health.CheckResult(nil), results...))
	return nil
}

func startDryRunOCIRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(registry.New())
}

func pushDryRunOCIImage(t *testing.T, srv *httptest.Server, repoTag string, data string) string {
	t.Helper()

	layer := stream.NewLayer(io.NopCloser(strings.NewReader(data)))
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("mutate.AppendLayers: %v", err)
	}

	ref, err := name.ParseReference(fmt.Sprintf("%s/%s", strings.TrimPrefix(srv.URL, "http://"), repoTag))
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	return ref.String()
}

func pushDryRunMultiLayerOCIImage(t *testing.T, srv *httptest.Server, repoTag string, payloads ...string) string {
	t.Helper()

	layers := make([]mutate.Addendum, 0, len(payloads))
	for _, payload := range payloads {
		layers = append(layers, mutate.Addendum{Layer: stream.NewLayer(io.NopCloser(strings.NewReader(payload)))})
	}
	img, err := mutate.Append(empty.Image, layers...)
	if err != nil {
		t.Fatalf("mutate.Append: %v", err)
	}

	ref, err := name.ParseReference(fmt.Sprintf("%s/%s", strings.TrimPrefix(srv.URL, "http://"), repoTag))
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
	return ref.String()
}

func withMockInterfaces(t *testing.T, fn func() ([]net.Interface, error)) {
	t.Helper()
	original := listInterfaces
	listInterfaces = fn
	t.Cleanup(func() {
		listInterfaces = original
	})
}

func withMockStat(t *testing.T, fn func(string) (os.FileInfo, error)) {
	t.Helper()
	original := statPath
	statPath = fn
	t.Cleanup(func() {
		statPath = original
	})
}

func withMockReadPath(t *testing.T, fn func(string) ([]byte, error)) {
	t.Helper()
	original := readPath
	readPath = fn
	t.Cleanup(func() {
		readPath = original
	})
}

func newTestImageServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			_, _ = w.Write(body)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDryRunConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.MachineConfig
		expect      DryRunStatus
		wantMessage string
		wantTarget  string
	}{
		{
			name:   "no images",
			cfg:    &config.MachineConfig{},
			expect: DryRunFail,
		},
		{
			name: "no hostname",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				return c
			}(),
			expect: DryRunWarn,
		},
		{
			name: "valid config",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = " Linux "
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				return c
			}(),
			expect:     DryRunPass,
			wantTarget: config.TargetOSLinux,
		},
		{
			name: "disk mirroring with A/B image mode",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Image.Mode = config.ImageModeAB
				c.Provision.Disk.MirroringMode = "raid-1"
				return c
			}(),
			expect:      DryRunFail,
			wantMessage: "mirroring",
		},
		{
			name: "missing target os",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				return c
			}(),
			expect:      DryRunFail,
			wantMessage: "before destructive storage steps",
		},
		{
			name: "rhel-like target hint",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1", OSFamily: "rhel"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				return c
			}(),
			expect:      DryRunFail,
			wantMessage: `osFamily="rhel"`,
		},
		{
			name: "layout without image and hostname",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunFail,
		},
		{
			name: "layout without image",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunFail,
		},
		{
			name: "layout with image url without hostname",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunWarn,
		},
		{
			name: "layout with image url and hostname",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunPass,
		},
		{
			name: "layout device conflicts with disk device",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Disk.Device = "/dev/sda"
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Device:     "/dev/sdb",
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunFail,
		},
		{
			name: "layout device missing",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Device:     "/dev/booty-dryrun-missing",
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunFail,
		},
		{
			name: "layout device is not block device",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Device:     "/dev/null",
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunFail,
		},
		{
			name: "partition image mode with layout",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.Mode = config.ImageModePartition
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Table:      "gpt",
					Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
				}
				return c
			}(),
			expect: DryRunFail,
		},
		{
			name: "unsupported layout mountpoint",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{Hostname: "node1"}
				c.Provision.TargetOS = config.TargetOSLinux
				c.Provision.Image.URLs = []string{"http://example.com/img"}
				c.Provision.Disk.PartitionLayout = &config.PartitionLayout{
					Table: "gpt",
					Partitions: []config.Partition{
						{Label: "root", Filesystem: "ext4", Mountpoint: "/"},
						{Label: "var", Filesystem: "ext4", Mountpoint: "/var"},
					},
				}
				return c
			}(),
			expect: DryRunFail,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := NewOrchestrator(tc.cfg, &dryRunProvider{}, disk.NewManager(nil))
			result := o.dryRunConfigValidation(context.Background())
			if result.Status != tc.expect {
				t.Errorf("got %s, want %s: %s", result.Status, tc.expect, result.Message)
			}
			if tc.wantMessage != "" && !strings.Contains(result.Message, tc.wantMessage) {
				t.Errorf("message = %q, want substring %q", result.Message, tc.wantMessage)
			}
			if tc.wantTarget != "" && tc.cfg.Provision.TargetOS != tc.wantTarget {
				t.Errorf("Provision.TargetOS = %q, want %q", tc.cfg.Provision.TargetOS, tc.wantTarget)
			}
		})
	}
}

func TestDryRunImageReachability(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{srv.URL + "/test.img"}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunPass {
		t.Errorf("got %s, want pass: %s", result.Status, result.Message)
	}
}

func TestDryRunImageUnreachable(t *testing.T) {
	// Use a closed server for fast, deterministic connection failure.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{srv.URL + "/unreachable.img"}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail: %s", result.Status, result.Message)
	}
}

func TestDryRunHealthChecks(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*config.MachineConfig)
		expect     DryRunStatus
		wantInText string
	}{
		{
			name: "disabled",
			configure: func(cfg *config.MachineConfig) {
				cfg.Health.Enabled = false
			},
			expect:     DryRunWarn,
			wantInText: "disabled",
		},
		{
			name: "enabled runs checks",
			configure: func(cfg *config.MachineConfig) {
				cfg.Health.Enabled = true
				cfg.Health.SkipChecks = "disk-presence,disk-ioerr,memory-ecc,nic-link-state,thermal-state"
			},
			expect:     DryRunPass,
			wantInText: "executed",
		},
		{
			name: "critical failure fails dry-run",
			configure: func(cfg *config.MachineConfig) {
				cfg.Health.Enabled = true
				cfg.Health.MinCPUs = 999999
				cfg.Health.SkipChecks = "disk-presence,disk-ioerr,memory-ecc,nic-link-state,thermal-state"
			},
			expect:     DryRunFail,
			wantInText: "minimum-cpu",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			tc.configure(cfg)
			o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))
			result := o.dryRunHealthChecks(context.Background())
			if result.Status != tc.expect {
				t.Errorf("got %s, want %s: %s", result.Status, tc.expect, result.Message)
			}
			if !strings.Contains(result.Message, tc.wantInText) {
				t.Errorf("message = %q, want substring %q", result.Message, tc.wantInText)
			}
		})
	}
}

func TestDryRunHealthChecksReportsResults(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Health.Enabled = true
	cfg.Health.MinCPUs = 1
	cfg.Health.SkipChecks = "disk-presence,disk-ioerr,memory-ecc,nic-link-state,thermal-state"
	provider := &dryRunProvider{}
	o := NewOrchestrator(cfg, provider, disk.NewManager(nil))

	result := o.dryRunHealthChecks(context.Background())
	if result.Status != DryRunPass {
		t.Fatalf("got %s, want pass: %s", result.Status, result.Message)
	}
	if len(provider.healthReports) != 1 {
		t.Fatalf("health reports = %d, want 1", len(provider.healthReports))
	}
	if len(provider.healthReports[0]) == 0 {
		t.Fatal("reported health results were empty")
	}
}

func TestDryRunDiskDetection_Configured(t *testing.T) {
	// Non-device path should fail device-node check.
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = "/tmp"
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunDiskDetection(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for non-device path: %s", result.Status, result.Message)
	}
}

func TestDryRunMirrorPairSkipped(t *testing.T) {
	for _, dc := range []config.DiskConfig{
		{},
		{MirroringMode: "none"},
	} {
		cfg := &config.MachineConfig{}
		cfg.Provision.Disk = dc
		o := NewOrchestrator(cfg, &dryRunProvider{}, newTestDiskManager())
		if result, ok := o.dryRunMirrorPair(context.Background()); ok {
			t.Errorf("dryRunMirrorPair(%+v) = %+v, want skipped", dc, result)
		}
	}
}

func TestDryRunDiskDetection_MissingSerial(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.SerialNumber = "BOOTY-DRYRUN-NO-SUCH-SERIAL"
	cfg.Provision.Disk.MirroringMode = "raid-1"
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))
	if result := o.dryRunDiskDetection(context.Background()); result.Status != DryRunFail {
		t.Errorf("got %s, want fail for a missing disk serial: %s", result.Status, result.Message)
	}
}

func TestDryRunDiskDetection_Missing(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = "/dev/nonexistent-disk-xyz"
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunDiskDetection(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for missing device: %s", result.Status, result.Message)
	}
}

func TestDryRunImageReachability_NoURLs(t *testing.T) {
	o := NewOrchestrator(
		&config.MachineConfig{},
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for empty URLs: %s", result.Status, result.Message)
	}
}

func TestDryRunImageReachability_NoURLsWithPartitionLayout(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table:      "gpt",
		Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
	}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for partition layout empty URLs: %s", result.Status, result.Message)
	}
}

func TestDryRunImageReachability_OCI(t *testing.T) {
	srv := startDryRunOCIRegistry(t)
	defer srv.Close()
	ref := pushDryRunOCIImage(t, srv, "test/dryrun:v1", "dry-run payload")

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"oci://" + ref}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunPass {
		t.Errorf("got %s, want pass for reachable OCI URL: %s", result.Status, result.Message)
	}
	if strings.Contains(result.Message, "skipped") {
		t.Errorf("did not expect skipped message, got %q", result.Message)
	}
}

func TestDryRunImageReachability_MixedHTTPAndOCI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	registry := startDryRunOCIRegistry(t)
	defer registry.Close()
	ref := pushDryRunOCIImage(t, registry, "test/mixed:v1", "mixed payload")

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{srv.URL + "/image.raw", "oci://" + ref}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunPass {
		t.Errorf("got %s, want pass for mixed reachable URLs: %s", result.Status, result.Message)
	}
}

func TestDryRunImageReachability_OCINotFound(t *testing.T) {
	srv := startDryRunOCIRegistry(t)
	defer srv.Close()

	ref := strings.TrimPrefix(srv.URL, "http://") + "/test/missing:v1"
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"oci://" + ref}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for missing OCI URL: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "OCI image unreachable") {
		t.Errorf("expected OCI failure context, got %q", result.Message)
	}
}

func TestDryRunImageReachability_UnsupportedScheme(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"ftp://example.com/img"}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for unsupported scheme: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "ftp") {
		t.Errorf("expected unsupported scheme in message, got %q", result.Message)
	}
}

func TestDryRunImageReachability_InvalidURL(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"http://%zz"}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for invalid URL: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "invalid image URL") {
		t.Errorf("expected invalid URL error, got %q", result.Message)
	}
}

func TestDryRunImageReachability_UppercaseScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	upperURL := strings.Replace(srv.URL, "http://", "HTTP://", 1)
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{upperURL + "/image.raw"}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunPass {
		t.Errorf("got %s, want pass for uppercase scheme: %s", result.Status, result.Message)
	}
}

// BOOTy converts qcow2 in process: the prerequisites need no qemu-img.
func TestDryRunImagePrerequisitesQCOW2PassesWithoutQemuImg(t *testing.T) {
	srv := newTestImageServer(t, append([]byte{0x51, 0x46, 0x49, 0xfb}, []byte("qcow2 payload")...))
	defer srv.Close()
	t.Setenv("PATH", t.TempDir())

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{srv.URL + "/image.qcow2"}
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))

	result := o.dryRunImagePrerequisites(context.Background())
	if result.Status != DryRunPass {
		t.Fatalf("got %s, want pass: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "qcow2") {
		t.Fatalf("message = %q, want qcow2 context", result.Message)
	}
}

func TestDryRunImagePrerequisitesRawPassesWithoutQemuImg(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()
	t.Setenv("PATH", t.TempDir())

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{srv.URL + "/image.raw"}
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))

	result := o.dryRunImagePrerequisites(context.Background())
	if result.Status != DryRunPass {
		t.Fatalf("got %s, want pass: %s", result.Status, result.Message)
	}
}

func TestDryRunImagePrerequisitesTrimsImageURLs(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()
	t.Setenv("PATH", t.TempDir())

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{" ", "\t" + srv.URL + "/image.raw  "}
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))

	result := o.dryRunImagePrerequisites(context.Background())
	if result.Status != DryRunPass {
		t.Fatalf("got %s, want pass: %s", result.Status, result.Message)
	}
}

func TestDryRunImagePrerequisitesFailsWhenURLsAreBlank(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{" ", "\t"}
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))

	result := o.dryRunImagePrerequisites(context.Background())
	if result.Status != DryRunFail {
		t.Fatalf("got %s, want fail: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "no image URLs configured") {
		t.Fatalf("message = %q, want no image URLs context", result.Message)
	}
}

func TestDryRunImagePrerequisitesFailsWhenPartitionLayoutHasNoImageURLs(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.PartitionLayout = &config.PartitionLayout{
		Table:      "gpt",
		Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}},
	}
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))

	result := o.dryRunImagePrerequisites(context.Background())
	if result.Status != DryRunFail {
		t.Fatalf("got %s, want fail: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "no image URLs configured") {
		t.Fatalf("message = %q, want no image URLs context", result.Message)
	}
}

func TestDryRunImagePrerequisitesSkipsOCIWithoutPullingLayer(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{"oci://registry.example.invalid/team/node:latest"}
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))

	result := o.dryRunImagePrerequisites(context.Background())
	if result.Status != DryRunWarn {
		t.Fatalf("got %s, want warn: %s", result.Status, result.Message)
	}
	if !strings.Contains(result.Message, "OCI image selected") {
		t.Fatalf("message = %q, want OCI context", result.Message)
	}
}

func TestDryRunImageChecksum(t *testing.T) {
	tests := []struct {
		name         string
		checksum     string
		checksumType string
		cfg          *config.MachineConfig
		expect       DryRunStatus
	}{
		{"no checksum", "", "", nil, DryRunWarn},
		{"sha256", strings.Repeat("a", 64), "sha256", nil, DryRunPass},
		{"sha512", strings.Repeat("a", 128), "sha512", nil, DryRunPass},
		{"uppercase type", strings.Repeat("a", 128), "SHA512", nil, DryRunPass},
		{"trimmed uppercase type", strings.Repeat("a", 64), " SHA256 ", nil, DryRunPass},
		{"empty type infers sha256", strings.Repeat("a", 64), "", nil, DryRunPass},
		{"short sha256", "abc123", "sha256", nil, DryRunFail},
		{"non-hex sha256", strings.Repeat("g", 64), "sha256", nil, DryRunFail},
		{"unsupported type", "abc123", "md5", nil, DryRunFail},
		{"layout without checksum", "", "", func() *config.MachineConfig {
			c := &config.MachineConfig{}
			c.Provision.Disk.PartitionLayout = &config.PartitionLayout{Table: "gpt", Partitions: []config.Partition{{Label: "root", Mountpoint: "/"}}}
			return c
		}(), DryRunWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg == nil {
				cfg = &config.MachineConfig{}
			}
			cfg.Provision.Image.Checksum = tc.checksum
			cfg.Provision.Image.ChecksumType = tc.checksumType
			o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))
			result := o.dryRunImageChecksum(context.Background())
			if result.Status != tc.expect {
				t.Errorf("got %s, want %s: %s", result.Status, tc.expect, result.Message)
			}
		})
	}
}

func TestDryRunImageSignature(t *testing.T) {
	pubKey := "/keys/pub.gpg"
	withMockStat(t, func(path string) (os.FileInfo, error) {
		if path == pubKey {
			return fakeFileInfo{name: "pub.gpg", mode: 0o600}, nil
		}
		return nil, os.ErrNotExist
	})

	tests := []struct {
		name       string
		cfg        func() *config.MachineConfig
		expect     DryRunStatus
		wantSubstr string
	}{
		{
			name: "no signature URL",
			cfg: func() *config.MachineConfig {
				return &config.MachineConfig{}
			},
			expect:     DryRunWarn,
			wantSubstr: "GPG verification disabled",
		},
		{
			name: "signature without checksum",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{}
				c.Provision.Image.SignatureURL = "https://images.example.invalid/node.raw.sig"
				c.Provision.Image.GPGPubKey = pubKey
				return c
			},
			expect:     DryRunFail,
			wantSubstr: imageSignatureChecksumRequiredMessage,
		},
		{
			name: "whitespace signature URL",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{}
				c.Provision.Image.SignatureURL = "   "
				c.Provision.Image.GPGPubKey = pubKey
				return c
			},
			expect:     DryRunWarn,
			wantSubstr: "GPG verification disabled",
		},
		{
			name: "signature with checksum",
			cfg: func() *config.MachineConfig {
				c := &config.MachineConfig{}
				c.Provision.Image.SignatureURL = "https://images.example.invalid/node.raw.sig"
				c.Provision.Image.GPGPubKey = pubKey
				c.Provision.Image.Checksum = strings.Repeat("a", 64)
				c.Provision.Image.ChecksumType = "sha256"
				return c
			},
			expect:     DryRunPass,
			wantSubstr: "image signature verification configured",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := NewOrchestrator(tc.cfg(), &dryRunProvider{}, disk.NewManager(nil))
			result := o.dryRunImageSignature(context.Background())
			if result.Status != tc.expect {
				t.Fatalf("got %s, want %s: %s", result.Status, tc.expect, result.Message)
			}
			if !strings.Contains(result.Message, tc.wantSubstr) {
				t.Fatalf("message = %q, want %q", result.Message, tc.wantSubstr)
			}
		})
	}
}

func TestDryRunNetworkLink(t *testing.T) {
	tests := []struct {
		name       string
		ifaces     []net.Interface
		err        error
		expect     DryRunStatus
		wantSubstr string
	}{
		{
			name:       "physical interface up",
			ifaces:     []net.Interface{{Name: "eth0", Flags: net.FlagUp}},
			expect:     DryRunPass,
			wantSubstr: "interfaces up",
		},
		{
			name:       "only loopback up",
			ifaces:     []net.Interface{{Name: "lo", Flags: net.FlagUp | net.FlagLoopback}},
			expect:     DryRunFail,
			wantSubstr: "no physical non-loopback interfaces are up",
		},
		{
			name:       "only virtual interfaces up",
			ifaces:     []net.Interface{{Name: "docker0", Flags: net.FlagUp}},
			expect:     DryRunFail,
			wantSubstr: "no physical non-loopback interfaces are up",
		},
		{
			name:       "interface enumeration error",
			err:        errors.New("boom"),
			expect:     DryRunFail,
			wantSubstr: "cannot list interfaces",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withMockReadPath(t, func(string) ([]byte, error) {
				return []byte("1\n"), nil
			})

			withMockInterfaces(t, func() ([]net.Interface, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return tc.ifaces, nil
			})

			o := NewOrchestrator(&config.MachineConfig{}, &dryRunProvider{}, disk.NewManager(nil))
			result := o.dryRunNetworkLink(context.Background())
			if result.Status != tc.expect {
				t.Errorf("got %s, want %s: %s", result.Status, tc.expect, result.Message)
			}
			if !strings.Contains(result.Message, tc.wantSubstr) {
				t.Errorf("message %q does not contain %q", result.Message, tc.wantSubstr)
			}
		})
	}
}

func TestDryRunNetworkLink_CarrierDown(t *testing.T) {
	withMockInterfaces(t, func() ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0", Flags: net.FlagUp}}, nil
	})
	withMockReadPath(t, func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "/carrier") {
			return []byte("0\n"), nil
		}
		return nil, os.ErrNotExist
	})

	o := NewOrchestrator(&config.MachineConfig{}, &dryRunProvider{}, disk.NewManager(nil))
	result := o.dryRunNetworkLink(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail when carrier is down: %s", result.Status, result.Message)
	}
}

func TestInterfaceHasCarrier(t *testing.T) {
	tests := []struct {
		name     string
		readPath func(string) ([]byte, error)
		expected bool
	}{
		{
			name: "carrier up",
			readPath: func(path string) ([]byte, error) {
				if strings.HasSuffix(path, "/carrier") {
					return []byte("1\n"), nil
				}
				return nil, os.ErrNotExist
			},
			expected: true,
		},
		{
			name: "carrier down",
			readPath: func(path string) ([]byte, error) {
				if strings.HasSuffix(path, "/carrier") {
					return []byte("0\n"), nil
				}
				return nil, os.ErrNotExist
			},
			expected: false,
		},
		{
			name: "fallback operstate up",
			readPath: func(path string) ([]byte, error) {
				if strings.HasSuffix(path, "/carrier") {
					return nil, os.ErrNotExist
				}
				if strings.HasSuffix(path, "/operstate") {
					return []byte("up\n"), nil
				}
				return nil, os.ErrNotExist
			},
			expected: true,
		},
		{
			name: "fallback operstate down",
			readPath: func(path string) ([]byte, error) {
				if strings.HasSuffix(path, "/carrier") {
					return nil, os.ErrNotExist
				}
				if strings.HasSuffix(path, "/operstate") {
					return []byte("down\n"), nil
				}
				return nil, os.ErrNotExist
			},
			expected: false,
		},
		{
			name: "both probes unavailable",
			readPath: func(string) ([]byte, error) {
				return nil, os.ErrNotExist
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withMockReadPath(t, tc.readPath)
			if got := interfaceHasCarrier("eth0"); got != tc.expected {
				t.Errorf("interfaceHasCarrier() = %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestDryRunEFIBoot(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect DryRunStatus
	}{
		{name: "efi present", err: nil, expect: DryRunPass},
		{name: "efi missing", err: os.ErrNotExist, expect: DryRunWarn},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withMockStat(t, func(string) (os.FileInfo, error) {
				if tc.err != nil {
					return nil, tc.err
				}
				return fakeFileInfo{}, nil
			})

			o := NewOrchestrator(&config.MachineConfig{}, &dryRunProvider{}, disk.NewManager(nil))
			result := o.dryRunEFIBoot(context.Background())
			if result.Status != tc.expect {
				t.Errorf("got %s, want %s: %s", result.Status, tc.expect, result.Message)
			}
		})
	}
}

func TestDryRunInventoryProbe(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		expect  DryRunStatus
	}{
		{"disabled", false, DryRunWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.MachineConfig{}
			cfg.Provision.Inventory.Enabled = tc.enabled
			o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))
			result := o.dryRunInventoryProbe(context.Background())
			if result.Status != tc.expect {
				t.Errorf("got %s, want %s: %s", result.Status, tc.expect, result.Message)
			}
		})
	}
}

func TestDryRunInventoryProbe_Enabled(t *testing.T) {
	cfg := &config.MachineConfig{}
	cfg.Provision.Inventory.Enabled = true
	o := NewOrchestrator(cfg, &dryRunProvider{}, disk.NewManager(nil))

	t.Run("dmi accessible", func(t *testing.T) {
		withMockStat(t, func(_ string) (os.FileInfo, error) { return os.Stat(os.DevNull) })
		result := o.dryRunInventoryProbe(context.Background())
		if result.Status != DryRunPass {
			t.Errorf("expected pass when DMI accessible, got %s: %s", result.Status, result.Message)
		}
	})

	t.Run("dmi not accessible", func(t *testing.T) {
		withMockStat(t, func(_ string) (os.FileInfo, error) { return nil, os.ErrNotExist })
		result := o.dryRunInventoryProbe(context.Background())
		if result.Status != DryRunWarn {
			t.Errorf("expected warn when DMI not accessible, got %s: %s", result.Status, result.Message)
		}
	})
}

func TestIsVirtualInterface(t *testing.T) {
	tests := []struct {
		name    string
		virtual bool
	}{
		{"eth0", false},
		{"eno1", false},
		{"enp3s0", false},
		{"veth123abc", true},
		{"docker0", true},
		{"br-abc123", true},
		{"virbr0", true},
		{"cni0", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVirtualInterface(tc.name); got != tc.virtual {
				t.Errorf("isVirtualInterface(%q) = %v, want %v", tc.name, got, tc.virtual)
			}
		})
	}
}

func TestDryRunDiskDetection_CharDevice(t *testing.T) {
	// /dev/null is a character device and should fail the block device check.
	cfg := &config.MachineConfig{}
	cfg.Provision.Disk.Device = "/dev/null"
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunDiskDetection(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for char device: %s", result.Status, result.Message)
	}
}

func TestDryRunImageReachability_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &config.MachineConfig{}
	cfg.Provision.Image.URLs = []string{srv.URL + "/missing.img"}
	o := NewOrchestrator(
		cfg,
		&dryRunProvider{},
		disk.NewManager(nil),
	)
	result := o.dryRunImageReachability(context.Background())
	if result.Status != DryRunFail {
		t.Errorf("got %s, want fail for 404: %s", result.Status, result.Message)
	}
}

func TestDryRunAggregation(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()

	provider := &dryRunProvider{}

	// DryRun with a reachable image server and valid hostname, but /dev/null
	// as DiskDevice (a char device, not a block device) so disk check warns/fails.
	// Verifies aggregation and provider status reporting run without panic.
	aggCfg := &config.MachineConfig{Hostname: "test-host"}
	aggCfg.Provision.Image.URLs = []string{srv.URL + "/image.raw"}
	aggCfg.Provision.Disk.Device = "/dev/null"
	o := NewOrchestrator(
		aggCfg,
		provider,
		disk.NewManager(nil),
	)

	_ = o.DryRun(context.Background())
	// Some checks may warn/fail in test environments (e.g. no EFI, no disk),
	// but the aggregation and status reporting must not panic.
	if provider.lastStatus == "" {
		t.Error("DryRun did not report status to provider")
	}

	// Verify that a fully missing config fails with error.
	provFail := &dryRunProvider{}
	oFail := NewOrchestrator(
		&config.MachineConfig{},
		provFail,
		disk.NewManager(nil),
	)
	err := oFail.DryRun(context.Background())
	if err == nil {
		t.Error("expected DryRun to fail with empty config")
	}
	if provFail.lastStatus != config.StatusError {
		t.Errorf("expected StatusError, got %s", provFail.lastStatus)
	}
}

func TestDryRunAggregation_WarningsReported(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()

	provider := &dryRunProvider{}

	withMockStat(t, func(path string) (os.FileInfo, error) {
		switch path {
		case "/dev/mock0":
			return fakeFileInfo{name: "mock0", mode: os.ModeDevice}, nil
		case "/sys/firmware/efi":
			return fakeFileInfo{name: "efi", mode: os.ModeDir}, nil
		default:
			return nil, os.ErrNotExist
		}
	})
	withMockInterfaces(t, func() ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0", Flags: net.FlagUp}}, nil
	})
	withMockReadPath(t, func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "/carrier") {
			return []byte("1\n"), nil
		}
		return nil, os.ErrNotExist
	})

	warnCfg := &config.MachineConfig{Hostname: "test-host"}
	warnCfg.Provision.TargetOS = config.TargetOSLinux
	warnCfg.Provision.Image.URLs = []string{srv.URL + "/image.raw"}
	warnCfg.Provision.Disk.Device = "/dev/mock0"
	o := NewOrchestrator(
		warnCfg,
		provider,
		disk.NewManager(nil),
	)

	err := o.DryRun(context.Background())
	if err != nil {
		t.Fatalf("expected nil error with warnings only, got %v", err)
	}
	if provider.lastStatus != config.StatusSuccess {
		t.Fatalf("expected StatusSuccess, got %s", provider.lastStatus)
	}
	if !strings.Contains(provider.lastMessage, "warning(s)") {
		t.Fatalf("expected warning summary, got %q", provider.lastMessage)
	}
}

func TestDryRun_AllPass(t *testing.T) {
	srv := newTestImageServer(t, []byte("raw payload"))
	defer srv.Close()

	// Create a dummy GPG pubkey file so the image-signature check passes.
	pubKey := filepath.Join(t.TempDir(), "pub.gpg")
	if err := os.WriteFile(pubKey, []byte("dummy"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := &dryRunProvider{}

	withMockStat(t, func(path string) (os.FileInfo, error) {
		switch path {
		case "/dev/mock0":
			return fakeFileInfo{name: "mock0", mode: os.ModeDevice}, nil
		case "/sys/firmware/efi":
			return fakeFileInfo{name: "efi", mode: os.ModeDir}, nil
		case "/sys/class/dmi/id/sys_vendor":
			return fakeFileInfo{name: "sys_vendor", mode: os.ModeDir}, nil
		case pubKey:
			return fakeFileInfo{name: "pub.gpg", mode: 0o600}, nil
		default:
			return nil, os.ErrNotExist
		}
	})
	withMockInterfaces(t, func() ([]net.Interface, error) {
		return []net.Interface{{Name: "eth0", Flags: net.FlagUp}}, nil
	})
	withMockReadPath(t, func(path string) ([]byte, error) {
		if strings.HasSuffix(path, "/carrier") {
			return []byte("1\n"), nil
		}
		return nil, os.ErrNotExist
	})

	passCfg := &config.MachineConfig{Hostname: "test-host"}
	passCfg.Provision.TargetOS = config.TargetOSLinux
	passCfg.Provision.Image.URLs = []string{srv.URL + "/image.raw"}
	passCfg.Provision.Image.SignatureURL = srv.URL + "/image.raw.sig"
	passCfg.Provision.Image.GPGPubKey = pubKey
	passCfg.Provision.Disk.Device = "/dev/mock0"
	passCfg.Health.Enabled = true
	passCfg.Health.SkipChecks = "disk-presence,disk-ioerr,memory-ecc,nic-link-state,thermal-state"
	passCfg.Provision.Inventory.Enabled = true
	passCfg.Provision.Image.Checksum = strings.Repeat("a", 64)
	passCfg.Provision.Image.ChecksumType = "sha256"
	o := NewOrchestrator(
		passCfg,
		provider,
		disk.NewManager(nil),
	)

	err := o.DryRun(context.Background())
	if err != nil {
		t.Fatalf("expected dry-run to pass, got %v", err)
	}
	if provider.lastStatus != config.StatusSuccess {
		t.Fatalf("expected StatusSuccess, got %s", provider.lastStatus)
	}
	if !strings.Contains(provider.lastMessage, "passed all checks") {
		t.Fatalf("expected pass summary, got %q", provider.lastMessage)
	}
}

func TestRedactImageURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "removes credentials and query",
			in:   "https://user:secret@example.com/image.raw?token=abc#frag",
			want: "https://example.com/image.raw",
		},
		{
			name: "invalid URL fails closed",
			in:   "::://bad-url",
			want: "[redacted invalid URL]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := redactImageURL(tc.in)
			if got != tc.want {
				t.Errorf("redactImageURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactURLError(t *testing.T) {
	raw := "https://user:secret@example.com/image.raw?token=abc"
	err := fmt.Errorf("request failed for %s", raw)

	redacted := redactURLError(err, raw)
	if strings.Contains(redacted, "secret") || strings.Contains(redacted, "token=abc") {
		t.Fatalf("redacted error leaked sensitive data: %q", redacted)
	}
	if !strings.Contains(redacted, "https://example.com/image.raw") {
		t.Fatalf("expected redacted URL in error, got %q", redacted)
	}
}

func TestRedactURLErrorHandlesStdlibRedactedURLVariants(t *testing.T) {
	raw := "https://user:secret@example.com/image.raw?token=abc#frag"
	err := &url.Error{
		Op:  "Head",
		URL: "https://user:***@example.com/image.raw?token=abc",
		Err: errors.New("connection refused"),
	}

	redacted := redactURLError(err, raw)
	for _, sensitive := range []string{"user", "secret", "token=abc", "#frag"} {
		if strings.Contains(redacted, sensitive) {
			t.Fatalf("redacted error leaked %q: %q", sensitive, redacted)
		}
	}
	if !strings.Contains(redacted, "https://example.com/image.raw") {
		t.Fatalf("expected redacted URL in error, got %q", redacted)
	}
}
