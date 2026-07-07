package caprf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/crash"
	"github.com/telekom/BOOTy/pkg/health"
)

// testServer is a minimal CAPRF server for testing.
type testServer struct {
	mu       sync.Mutex
	statuses []string
	logs     []string
	debugs   []string
	server   *httptest.Server
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := &testServer{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /status/init", ts.handleStatus("init"))
	mux.HandleFunc("POST /status/success", ts.handleStatus("success"))
	mux.HandleFunc("POST /status/error", ts.handleStatus("error"))
	mux.HandleFunc("POST /log", ts.handleLog)
	mux.HandleFunc("POST /debug", ts.handleDebug)

	ts.server = httptest.NewServer(mux)
	t.Cleanup(ts.server.Close)
	return ts
}

func (ts *testServer) handleStatus(name string) http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		ts.statuses = append(ts.statuses, name)
		// Verify auth header.
		if auth := r.Header.Get("Authorization"); auth != "" {
			ts.statuses = append(ts.statuses, "auth:"+auth)
		}
	}
}

func (ts *testServer) handleLog(_ http.ResponseWriter, r *http.Request) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	buf := new(strings.Builder)
	fmt.Fprintf(buf, "%s", r.Header.Get("Authorization"))
	ts.logs = append(ts.logs, buf.String())
}

func (ts *testServer) handleDebug(_ http.ResponseWriter, r *http.Request) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	buf := new(strings.Builder)
	fmt.Fprintf(buf, "%s", r.Header.Get("Authorization"))
	ts.debugs = append(ts.debugs, buf.String())
}

func TestParseVars(t *testing.T) {
	input := `export IMAGE="http://example.com/image1.gz http://example.com/image2.gz"
export HOSTNAME="worker-01"
export TOKEN="test-token-123"
export MACHINE_EXTRA_KERNEL_PARAMS="console=ttyS0 net.ifnames=0"
export FAILURE_DOMAIN="zone-a"
export REGION="eu-central-1"
export PROVIDER_ID="redfish://bmc.example.com/Systems/1"
export MODE="provision"
export MIN_DISK_SIZE_GB="100"
export DISK_SERIAL_NUMBER="RAID-DISK-1"
export LOG_URL="http://caprf.example.com/log"
export INIT_URL="http://caprf.example.com/status/init"
export ERROR_URL="http://caprf.example.com/status/error"
export SUCCESS_URL="http://caprf.example.com/status/success"
export DEBUG_URL="http://caprf.example.com/debug"
`

	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Provision.Image.URLs) != 2 {
		t.Fatalf("expected 2 image URLs, got %d", len(cfg.Provision.Image.URLs))
	}
	if cfg.Provision.Image.URLs[0] != "http://example.com/image1.gz" {
		t.Fatalf("unexpected image URL 0: %s", cfg.Provision.Image.URLs[0])
	}
	if cfg.Hostname != "worker-01" {
		t.Fatalf("unexpected hostname: %s", cfg.Hostname)
	}
	if cfg.Transport.Token != "test-token-123" {
		t.Fatalf("unexpected token: %s", cfg.Transport.Token)
	}
	if cfg.Provision.ExtraKernelParams != "console=ttyS0 net.ifnames=0" {
		t.Fatalf("unexpected kernel params: %s", cfg.Provision.ExtraKernelParams)
	}
	if cfg.Provision.FailureDomain != "zone-a" {
		t.Fatalf("unexpected failure domain: %s", cfg.Provision.FailureDomain)
	}
	if cfg.Provision.Region != "eu-central-1" {
		t.Fatalf("unexpected region: %s", cfg.Provision.Region)
	}
	if cfg.Provision.ProviderID != "redfish://bmc.example.com/Systems/1" {
		t.Fatalf("unexpected provider ID: %s", cfg.Provision.ProviderID)
	}
	if cfg.Mode != "provision" {
		t.Fatalf("unexpected mode: %s", cfg.Mode)
	}
	if cfg.Provision.Disk.MinSizeGB != 100 {
		t.Fatalf("unexpected min disk size: %d", cfg.Provision.Disk.MinSizeGB)
	}
	if cfg.Provision.Disk.SerialNumber != "RAID-DISK-1" {
		t.Fatalf("unexpected disk serial number: %s", cfg.Provision.Disk.SerialNumber)
	}
	if cfg.Transport.LogURL != "http://caprf.example.com/log" {
		t.Fatalf("unexpected log URL: %s", cfg.Transport.LogURL)
	}
	if cfg.Transport.InitURL != "http://caprf.example.com/status/init" {
		t.Fatalf("unexpected init URL: %s", cfg.Transport.InitURL)
	}
}

func TestParseVarsEmpty(t *testing.T) {
	cfg, err := ParseVars(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "" {
		t.Fatalf("expected empty hostname, got %s", cfg.Hostname)
	}
}

func TestParseVarsComments(t *testing.T) {
	input := `# This is a comment
export HOSTNAME="test-host"
# Another comment
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "test-host" {
		t.Fatalf("expected test-host, got %s", cfg.Hostname)
	}
}

func TestParseVarsPartitionLayoutSingleQuoted(t *testing.T) {
	input := `export PARTITION_LAYOUT='{"table":"gpt","partitions":[{"label":"root","filesystem":"ext4","mountpoint":"/"}]}'`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Disk.PartitionLayout == nil {
		t.Fatal("expected partition layout to be parsed")
	}
	if len(cfg.Provision.Disk.PartitionLayout.Partitions) != 1 {
		t.Fatalf("expected 1 partition, got %d", len(cfg.Provision.Disk.PartitionLayout.Partitions))
	}
	if cfg.Provision.Disk.PartitionLayout.Partitions[0].Label != "root" {
		t.Errorf("partition label = %q, want root", cfg.Provision.Disk.PartitionLayout.Partitions[0].Label)
	}
}

func TestParseVarsPartitionLayoutInvalidFails(t *testing.T) {
	input := `export PARTITION_LAYOUT='{"table":"gpt","partitions":[{"filesystem":"ext4","mountpoint":"/"}]}'`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for invalid partition layout")
	}
	if !strings.Contains(err.Error(), "invalid PARTITION_LAYOUT") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseVarsABConfig(t *testing.T) {
	input := `export IMAGE="oci://registry.example.com/tcaas/os:v2"
export IMAGE_MODE="ab"
export AB_SCHEME="dual-root"
export AB_ACTIVE_SLOT="a"
export AB_TARGET_SLOT="inactive"
export AB_PRESERVE_EXISTING="true"
export AB_BOOT_SIZE_MB="1024"
export AB_ROOT_SIZE_MB="65536"
export AB_STATE_SIZE_MB="8192"
export AB_SOURCE_ROOT_LABEL="rootfs"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Image.Mode != config.ImageModeAB {
		t.Fatalf("image mode = %q, want ab", cfg.Provision.Image.Mode)
	}
	if !cfg.Provision.AB.PreserveExisting {
		t.Fatal("expected preserveExisting=true")
	}
	if cfg.Provision.AB.ActiveSlot != "a" || cfg.Provision.AB.TargetSlot != "inactive" {
		t.Fatalf("unexpected slots: %#v", cfg.Provision.AB)
	}
	if cfg.Provision.AB.BootSizeMB != 1024 || cfg.Provision.AB.RootSizeMB != 65536 || cfg.Provision.AB.StateSizeMB != 8192 {
		t.Fatalf("unexpected sizes: %#v", cfg.Provision.AB)
	}
	if cfg.Provision.AB.SourceRootLabel != "rootfs" {
		t.Fatalf("sourceRootLabel = %q, want rootfs", cfg.Provision.AB.SourceRootLabel)
	}
}

func TestParseVarsABSystemDataPartitions(t *testing.T) {
	input := `export IMAGE_MODE="ab"
export AB_SCHEME="system-ab"
export AB_DATA_PARTITIONS='[{"label":"BOOTY-VAR","mountpoint":"/var","sizeMB":8192},{"label":"BOOTY-HOME","mountpoint":"/home"}]'
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.AB.Scheme != config.ABSchemeSystemAB {
		t.Fatalf("scheme = %q, want %q", cfg.Provision.AB.Scheme, config.ABSchemeSystemAB)
	}
	if len(cfg.Provision.AB.DataPartitions) != 2 {
		t.Fatalf("data partitions = %d, want 2", len(cfg.Provision.AB.DataPartitions))
	}
	if cfg.Provision.AB.DataPartitions[0].Label != "BOOTY-VAR" || cfg.Provision.AB.DataPartitions[0].Mountpoint != "/var" {
		t.Fatalf("first data partition = %+v", cfg.Provision.AB.DataPartitions[0])
	}
	if cfg.Provision.AB.DataPartitions[1].Filesystem != "ext4" {
		t.Fatalf("second data partition filesystem = %q, want ext4", cfg.Provision.AB.DataPartitions[1].Filesystem)
	}
}

func TestParseVarsABSourceRootPartition(t *testing.T) {
	input := `export IMAGE_MODE="ab"
export AB_SOURCE_ROOT_PARTITION="2"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.AB.SourceRootPartition != 2 {
		t.Fatalf("sourceRootPartition = %d, want 2", cfg.Provision.AB.SourceRootPartition)
	}
}

func TestParseVarsRootPartitionSelectors(t *testing.T) {
	input := `export ROOT_PARTITION_LABEL="ubuntu-root"
export ROOT_PARTITION_NUMBER="2"
`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected selector conflict")
	}
	if !strings.Contains(err.Error(), "rootPartitionLabel and provision.disk.rootPartitionNumber are mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}

	cfg, err := ParseVars(strings.NewReader(`export ROOT_PARTITION_NUMBER="2"`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Disk.RootPartitionNumber != 2 {
		t.Fatalf("rootPartitionNumber = %d, want 2", cfg.Provision.Disk.RootPartitionNumber)
	}

	cfg, err = ParseVars(strings.NewReader(`export ROOT_PARTITION_LABEL="ubuntu-root"`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Disk.RootPartitionLabel != "ubuntu-root" {
		t.Fatalf("rootPartitionLabel = %q, want ubuntu-root", cfg.Provision.Disk.RootPartitionLabel)
	}
}

func TestParseVarsImageSourceRootLabel(t *testing.T) {
	input := `export IMAGE_SOURCE_ROOT_LABEL="USR-A"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Image.SourceRootLabel != "USR-A" {
		t.Fatalf("image sourceRootLabel = %q, want USR-A", cfg.Provision.Image.SourceRootLabel)
	}
}

func TestParseVarsImageSourceRootPartition(t *testing.T) {
	input := `export IMAGE_SOURCE_ROOT_PARTITION="3"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Image.SourceRootPartition != 3 {
		t.Fatalf("image sourceRootPartition = %d, want 3", cfg.Provision.Image.SourceRootPartition)
	}
}

func TestParseVarsInsecureTransport(t *testing.T) {
	input := `export INSECURE_TRANSPORT="true"
export HOSTNAME="worker-01"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Transport.Insecure {
		t.Fatal("expected InsecureTransport=true")
	}
}

func TestClientReportStatus(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:      "my-token",
			InitURL:    ts.server.URL + "/status/init",
			SuccessURL: ts.server.URL + "/status/success",
			ErrorURL:   ts.server.URL + "/status/error",
			LogURL:     ts.server.URL + "/log",
			DebugURL:   ts.server.URL + "/debug",
		},
	}

	client := NewFromConfig(cfg)
	ctx := context.Background()

	// Report init.
	if err := client.ReportStatus(ctx, config.StatusInit, "starting"); err != nil {
		t.Fatal(err)
	}

	// Report success.
	if err := client.ReportStatus(ctx, config.StatusSuccess, "done"); err != nil {
		t.Fatal(err)
	}

	// Report error.
	if err := client.ReportStatus(ctx, config.StatusError, "failed"); err != nil {
		t.Fatal(err)
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	// Check statuses: each status + auth header.
	expectedStatuses := []string{"init", "auth:Bearer my-token", "success", "auth:Bearer my-token", "error", "auth:Bearer my-token"}
	if len(ts.statuses) != len(expectedStatuses) {
		t.Fatalf("expected %d status entries, got %d: %v", len(expectedStatuses), len(ts.statuses), ts.statuses)
	}
	for i, exp := range expectedStatuses {
		if ts.statuses[i] != exp {
			t.Errorf("status[%d] = %q, want %q", i, ts.statuses[i], exp)
		}
	}
}

func TestClientShipLog(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:  "log-token",
			LogURL: ts.server.URL + "/log",
		},
	}

	client := NewFromConfig(cfg)
	if err := client.ShipLog(context.Background(), "test log line"); err != nil {
		t.Fatal(err)
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if len(ts.logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(ts.logs))
	}
	if ts.logs[0] != "Bearer log-token" {
		t.Fatalf("expected auth header, got %s", ts.logs[0])
	}
}

func TestClientHeartbeatNoop(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientFetchCommandsNoop(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	cmds, err := client.FetchCommands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cmds != nil {
		t.Fatalf("expected nil commands, got %v", cmds)
	}
}

func TestClientReportInventory(t *testing.T) {
	var receivedBody []byte
	var receivedContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "test-token"},
		Provision: config.ProvisionConfig{Inventory: config.InventoryConfig{URL: srv.URL + "/inventory"}},
	})

	data := []byte(`{"system":{"vendor":"Dell"}}`)
	if err := client.ReportInventory(context.Background(), data); err != nil {
		t.Fatalf("ReportInventory() error: %v", err)
	}
	if receivedContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", receivedContentType)
	}
	if string(receivedBody) != string(data) {
		t.Errorf("body = %q, want %q", receivedBody, data)
	}
}

func TestClientReportInventoryNoURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	// Should be a no-op when no URL is configured.
	if err := client.ReportInventory(context.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("ReportInventory() with no URL should not error: %v", err)
	}
}

func TestReportCrashArtifactsPreparePresignedPUTNoAuthorization(t *testing.T) {
	archivePath := writeCrashArchiveFixture(t)
	var uploadAuth string
	var uploadBody string
	var prepareAuth string

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("POST /crash/prepare", func(w http.ResponseWriter, r *http.Request) {
		prepareAuth = r.Header.Get("Authorization")
		var req crash.PrepareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode prepare request: %v", err)
		}
		if req.Manifest.Metadata.Machine.Hostname != "node-1" {
			t.Fatalf("prepare hostname = %q, want node-1", req.Manifest.Metadata.Machine.Hostname)
		}
		resp := crash.PrepareResponse{
			UploadMode: crash.UploadModePresignedPUT,
			AuthMode:   crash.AuthModeNone,
			Method:     http.MethodPut,
			UploadURL:  srv.URL + "/upload?X-Amz-Signature=secret-signature",
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("PUT /upload", func(w http.ResponseWriter, r *http.Request) {
		uploadAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		uploadBody = string(body)
		w.WriteHeader(http.StatusOK)
	})

	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "crash-token"},
		Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{PrepareURL: srv.URL + "/crash/prepare"}},
	})
	err := client.ReportCrashArtifacts(context.Background(), crashRequestFixture(), archivePath)
	if err != nil {
		t.Fatalf("ReportCrashArtifacts() error: %v", err)
	}
	if prepareAuth != "Bearer crash-token" {
		t.Fatalf("prepare auth = %q, want bearer", prepareAuth)
	}
	if uploadAuth != "" {
		t.Fatalf("presigned upload auth = %q, want empty", uploadAuth)
	}
	if uploadBody != "archive-bytes" {
		t.Fatalf("upload body = %q, want archive-bytes", uploadBody)
	}
}

func TestReportCrashArtifactsRejectsPresignedPUTBearerAuth(t *testing.T) {
	archivePath := writeCrashArchiveFixture(t)
	uploadCalled := make(chan struct{}, 1)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("POST /crash/prepare", func(w http.ResponseWriter, _ *http.Request) {
		resp := crash.PrepareResponse{
			UploadMode: crash.UploadModePresignedPUT,
			AuthMode:   crash.AuthModeBearer,
			Method:     http.MethodPut,
			UploadURL:  srv.URL + "/upload?X-Amz-Signature=secret-signature",
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("PUT /upload", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case uploadCalled <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	})

	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "crash-token"},
		Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{PrepareURL: srv.URL + "/crash/prepare"}},
	})
	err := client.ReportCrashArtifacts(context.Background(), crashRequestFixture(), archivePath)
	if err == nil {
		t.Fatal("expected presigned bearer auth to be rejected")
	}
	if !strings.Contains(err.Error(), "only supported for") {
		t.Fatalf("error = %q, want auth mode rejection", err.Error())
	}
	select {
	case <-uploadCalled:
		t.Fatal("presigned upload endpoint was called")
	default:
	}
}

func TestReportCrashArtifactsDirectCAPRFUploadUsesBearer(t *testing.T) {
	archivePath := writeCrashArchiveFixture(t)
	var uploadAuth string
	var contentType string
	var body string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadAuth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "crash-token"},
		Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{UploadURL: srv.URL + "/crash/upload"}},
	})
	if err := client.ReportCrashArtifacts(context.Background(), crashRequestFixture(), archivePath); err != nil {
		t.Fatalf("ReportCrashArtifacts() error: %v", err)
	}
	if uploadAuth != "Bearer crash-token" {
		t.Fatalf("upload auth = %q, want bearer", uploadAuth)
	}
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		t.Fatalf("Content-Type = %q, want multipart/form-data", contentType)
	}
	if !strings.Contains(body, "node-1") || !strings.Contains(body, "archive-bytes") {
		t.Fatalf("multipart body missing manifest or archive: %q", body)
	}
}

func TestReportCrashArtifactsNoURLNoop(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	err := client.ReportCrashArtifacts(context.Background(), crashRequestFixture(), writeCrashArchiveFixture(t))
	if !errors.Is(err, crash.ErrNoUploadURL) {
		t.Fatalf("ReportCrashArtifacts() error = %v, want ErrNoUploadURL", err)
	}
}

func TestReportCrashArtifactsRejectsInsecureRemoteUpload(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{UploadURL: "http://example.com/crash/upload"}}})
	err := client.ReportCrashArtifacts(context.Background(), crashRequestFixture(), writeCrashArchiveFixture(t))
	if !errors.Is(err, errInsecureTransport) {
		t.Fatalf("ReportCrashArtifacts() error = %v, want errInsecureTransport", err)
	}
}

func crashRequestFixture() *crash.PrepareRequest {
	manifest := crash.Manifest{
		Version: 1,
		Metadata: crash.HostMetadata{Machine: crash.MachineMetadata{
			Hostname: "node-1",
			Mode:     "provision",
		}},
		Artifacts: []crash.Artifact{{ArchivePath: "target-root/var/crash/vmcore", SizeBytes: 13}},
	}
	return &crash.PrepareRequest{Manifest: manifest, ArchiveBytes: 13, ArtifactCount: 1, TotalBytes: 13}
}

func writeCrashArchiveFixture(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/crash.tar.gz"
	if err := os.WriteFile(path, []byte("archive-bytes"), 0o600); err != nil {
		t.Fatalf("write archive fixture: %v", err)
	}
	return path
}

func TestClientNoURLSkips(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})

	// ShipLog with no URL should be a no-op.
	if err := client.ShipLog(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// ShipDebug with no URL should be a no-op.
	if err := client.ShipDebug(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	// ReportStatus with no URL should warn but not error.
	if err := client.ReportStatus(context.Background(), config.StatusInit, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestParseVarsWithoutExport(t *testing.T) {
	input := `HOSTNAME="bare-host"
TOKEN="bare-token"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "bare-host" {
		t.Fatalf("expected bare-host, got %s", cfg.Hostname)
	}
	if cfg.Transport.Token != "bare-token" {
		t.Fatalf("expected bare-token, got %s", cfg.Transport.Token)
	}
}

func TestNew(t *testing.T) {
	// Create a temporary vars file.
	f, err := os.CreateTemp(t.TempDir(), "vars-*")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`export HOSTNAME="test-via-file"
export TOKEN="file-token"
export MODE="provision"
`)
	f.Close()

	client, err := New(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := client.GetConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "test-via-file" {
		t.Fatalf("expected test-via-file, got %s", cfg.Hostname)
	}
	if cfg.Transport.Token != "file-token" {
		t.Fatalf("expected file-token, got %s", cfg.Transport.Token)
	}
}

func TestNewFileNotFound(t *testing.T) {
	_, err := New("/nonexistent/path/to/vars")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestParseVarsNetworkTuning(t *testing.T) {
	input := `export asn_server="64497"
export provision_vni="2001000"
export underlay_subnet="10.50.0.0/16"
export overlay_subnet="fd21:0cc2:0981::/64"
export vrf_table_id="10"
export bgp_keepalive="30"
export bgp_hold="90"
export bfd_transmit_ms="150"
export bfd_receive_ms="150"
export dcgw_ips="10.10.10.1,10.10.10.2"
export vpn_rt="64497:1000"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.BGP.ASN != 64497 {
		t.Errorf("ASN = %d, want 64497", cfg.Network.BGP.ASN)
	}
	if cfg.Network.EVPN.ProvisionVNI != 2001000 {
		t.Errorf("ProvisionVNI = %d, want 2001000", cfg.Network.EVPN.ProvisionVNI)
	}
	if cfg.Network.VRF.TableID != 10 {
		t.Errorf("VRFTableID = %d, want 10", cfg.Network.VRF.TableID)
	}
	if cfg.Network.BGP.Keepalive != 30 {
		t.Errorf("BGPKeepalive = %d, want 30", cfg.Network.BGP.Keepalive)
	}
	if cfg.Network.BGP.Hold != 90 {
		t.Errorf("BGPHold = %d, want 90", cfg.Network.BGP.Hold)
	}
	if cfg.Network.BGP.BFDTransmitMS != 150 {
		t.Errorf("BFDTransmitMS = %d, want 150", cfg.Network.BGP.BFDTransmitMS)
	}
	if cfg.Network.BGP.BFDReceiveMS != 150 {
		t.Errorf("BFDReceiveMS = %d, want 150", cfg.Network.BGP.BFDReceiveMS)
	}
	if cfg.Network.EVPN.DCGWIPs != "10.10.10.1,10.10.10.2" {
		t.Errorf("DCGWIPs = %q, want %q", cfg.Network.EVPN.DCGWIPs, "10.10.10.1,10.10.10.2")
	}
	if cfg.Network.EVPN.VPNRT != "64497:1000" {
		t.Errorf("VPNRT = %q, want %q", cfg.Network.EVPN.VPNRT, "64497:1000")
	}
}

func TestGetConfig(t *testing.T) {
	cfg := &config.MachineConfig{
		Hostname: "config-host",
		Mode:     "provision",
	}
	client := NewFromConfig(cfg)
	got, err := client.GetConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != cfg {
		t.Fatal("expected same config pointer")
	}
}

func TestClientShipDebug(t *testing.T) {
	ts := newTestServer(t)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{
			Token:    "debug-token",
			DebugURL: ts.server.URL + "/debug",
		},
	}

	client := NewFromConfig(cfg)
	if err := client.ShipDebug(context.Background(), "debug message"); err != nil {
		t.Fatal(err)
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()

	if len(ts.debugs) != 1 {
		t.Fatalf("expected 1 debug entry, got %d", len(ts.debugs))
	}
	if ts.debugs[0] != "Bearer debug-token" {
		t.Fatalf("expected auth header, got %s", ts.debugs[0])
	}
}

func TestReportStatusUnknown(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	err := client.ReportStatus(context.Background(), config.Status("invalid"), "msg")
	if err == nil {
		t.Fatal("expected error for unknown status")
	}
	if !strings.Contains(err.Error(), "unknown status") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPostWithAuthNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.MachineConfig{
		Transport: config.TransportConfig{InitURL: srv.URL + "/status/init"},
	}
	client := NewFromConfig(cfg)
	err := client.ReportStatus(context.Background(), config.StatusInit, "test")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseVarsInvalidDiskSize(t *testing.T) {
	input := `export MIN_DISK_SIZE_GB="notanumber"
export HOSTNAME="host"
`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for invalid MIN_DISK_SIZE_GB, got nil")
	}
	if !strings.Contains(err.Error(), "MIN_DISK_SIZE_GB") {
		t.Fatalf("expected error to mention MIN_DISK_SIZE_GB, got: %v", err)
	}
}

func TestParseVarsInvalidUint32(t *testing.T) {
	input := `export asn_server="notanumber"
`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for invalid asn_server, got nil")
	}
	if !strings.Contains(err.Error(), "asn_server") {
		t.Fatalf("expected error to mention asn_server, got: %v", err)
	}
}

func TestParseVarsInvalidMode(t *testing.T) {
	input := `export MODE="invalid-mode"
`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for invalid MODE, got nil")
	}
	if !strings.Contains(err.Error(), "invalid mode") {
		t.Fatalf("expected error to mention 'invalid mode', got: %v", err)
	}
}

func TestParseVarsLineWithoutEquals(t *testing.T) {
	input := `export NOSEPARATOR
export HOSTNAME="works"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "works" {
		t.Fatalf("expected works, got %s", cfg.Hostname)
	}
}

// Compile-time check that Client implements config.Provider.
var _ config.Provider = (*Client)(nil)

func TestParseVarsNetworkFields(t *testing.T) {
	input := `underlay_subnet="192.168.4.0/24"
underlay_ip="10.50.12.13"
overlay_subnet="2a01:598:40a:5481::/64"
ipmi_subnet="172.30.0.0/24"
asn_server="65188"
provision_vni="2002002"
provision_ip="10.100.0.42/24"
dns_resolver="2003:0:af08:1005::1000"
dcgw_ips="10.10.10.1,10.10.10.2"
leaf_asn="65500"
local_asn="65501"
overlay_aggregate="2a01:598:40a:5481::/64"
vpn_rt="65188:2002"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.EVPN.UnderlaySubnet != "192.168.4.0/24" {
		t.Errorf("UnderlaySubnet = %q", cfg.Network.EVPN.UnderlaySubnet)
	}
	if cfg.Network.EVPN.UnderlayIP != "10.50.12.13" {
		t.Errorf("UnderlayIP = %q", cfg.Network.EVPN.UnderlayIP)
	}
	if cfg.Network.EVPN.OverlaySubnet != "2a01:598:40a:5481::/64" {
		t.Errorf("OverlaySubnet = %q", cfg.Network.EVPN.OverlaySubnet)
	}
	if cfg.Network.IPMI.Subnet != "172.30.0.0/24" {
		t.Errorf("IPMISubnet = %q", cfg.Network.IPMI.Subnet)
	}
	if cfg.Network.BGP.ASN != 65188 {
		t.Errorf("ASN = %d", cfg.Network.BGP.ASN)
	}
	if cfg.Network.EVPN.ProvisionVNI != 2002002 {
		t.Errorf("ProvisionVNI = %d", cfg.Network.EVPN.ProvisionVNI)
	}
	if cfg.Network.EVPN.ProvisionIP != "10.100.0.42/24" {
		t.Errorf("ProvisionIP = %q", cfg.Network.EVPN.ProvisionIP)
	}
	if cfg.Network.DNSResolvers != "2003:0:af08:1005::1000" {
		t.Errorf("DNSResolvers = %q", cfg.Network.DNSResolvers)
	}
	if cfg.Network.EVPN.DCGWIPs != "10.10.10.1,10.10.10.2" {
		t.Errorf("DCGWIPs = %q", cfg.Network.EVPN.DCGWIPs)
	}
	if cfg.Network.EVPN.LeafASN != 65500 {
		t.Errorf("LeafASN = %d", cfg.Network.EVPN.LeafASN)
	}
	if cfg.Network.EVPN.LocalASN != 65501 {
		t.Errorf("LocalASN = %d", cfg.Network.EVPN.LocalASN)
	}
	if cfg.Network.EVPN.OverlayAggregate != "2a01:598:40a:5481::/64" {
		t.Errorf("OverlayAggregate = %q", cfg.Network.EVPN.OverlayAggregate)
	}
	if cfg.Network.EVPN.VPNRT != "65188:2002" {
		t.Errorf("VPNRT = %q", cfg.Network.EVPN.VPNRT)
	}
}

func TestParseVarsNetworkMode(t *testing.T) {
	input := `NETWORK_MODE="gobgp"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.Mode != "gobgp" {
		t.Errorf("NetworkMode = %q, want gobgp", cfg.Network.Mode)
	}
}

func TestParseVarsProvisionTargetOS(t *testing.T) {
	input := `PROVISION_TARGET_OS=" Linux "
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.TargetOS != config.TargetOSLinux {
		t.Errorf("Provision.TargetOS = %q, want %q", cfg.Provision.TargetOS, config.TargetOSLinux)
	}
}

func TestParseVarsTargetOSAliasRejectsUnsupported(t *testing.T) {
	input := `TARGET_OS="windows"
`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("ParseVars() error = nil, want unsupported target OS")
	}
	if !strings.Contains(err.Error(), "Windows targets are not supported") {
		t.Fatalf("ParseVars() error = %q, want Windows unsupported context", err.Error())
	}
}

func TestParseVarsBGPPeering(t *testing.T) {
	input := `BGP_PEER_MODE="dual"
BGP_INTERFACES="eth1,eth2"
BGP_NEIGHBORS="10.0.0.1,10.0.0.2"
BGP_REMOTE_ASN="65100"
BGP_UNDERLAY_AF="ipv4"
BGP_OVERLAY_TYPE="l3vpn"
BGP_AUTH_PASSWORD="s3cr3t"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.BGP.PeerMode != "dual" {
		t.Errorf("BGPPeerMode = %q, want dual", cfg.Network.BGP.PeerMode)
	}
	if cfg.Network.BGP.Interfaces != "eth1,eth2" {
		t.Errorf("BGPInterfaces = %q, want eth1,eth2", cfg.Network.BGP.Interfaces)
	}
	if cfg.Network.BGP.Neighbors != "10.0.0.1,10.0.0.2" {
		t.Errorf("BGPNeighbors = %q, want 10.0.0.1,10.0.0.2", cfg.Network.BGP.Neighbors)
	}
	if cfg.Network.BGP.RemoteASN != 65100 {
		t.Errorf("BGPRemoteASN = %d, want 65100", cfg.Network.BGP.RemoteASN)
	}
	if cfg.Network.BGP.UnderlayAF != "ipv4" {
		t.Errorf("BGPUnderlayAF = %q, want ipv4", cfg.Network.BGP.UnderlayAF)
	}
	if cfg.Network.BGP.OverlayType != "l3vpn" {
		t.Errorf("BGPOverlayType = %q, want l3vpn", cfg.Network.BGP.OverlayType)
	}
	if cfg.Network.BGP.AuthPassword != "s3cr3t" {
		t.Errorf("BGPAuthPassword = %q, want s3cr3t", cfg.Network.BGP.AuthPassword)
	}
}

func TestParseVarsDocumentedNetworkUppercaseAliases(t *testing.T) {
	input := `BGP_KEEPALIVE="30"
BGP_HOLD="90"
BFD_TRANSMIT_MS="150"
BFD_RECEIVE_MS="175"
VRF_NAME="provision"
VRF_TABLE_ID="10"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.BGP.Keepalive != 30 {
		t.Errorf("BGPKeepalive = %d, want 30", cfg.Network.BGP.Keepalive)
	}
	if cfg.Network.BGP.Hold != 90 {
		t.Errorf("BGPHold = %d, want 90", cfg.Network.BGP.Hold)
	}
	if cfg.Network.BGP.BFDTransmitMS != 150 {
		t.Errorf("BFDTransmitMS = %d, want 150", cfg.Network.BGP.BFDTransmitMS)
	}
	if cfg.Network.BGP.BFDReceiveMS != 175 {
		t.Errorf("BFDReceiveMS = %d, want 175", cfg.Network.BGP.BFDReceiveMS)
	}
	if cfg.Network.VRF.Name != "provision" {
		t.Errorf("VRFName = %q, want provision", cfg.Network.VRF.Name)
	}
	if cfg.Network.VRF.TableID != 10 {
		t.Errorf("VRFTableID = %d, want 10", cfg.Network.VRF.TableID)
	}
}

func TestClientHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{Agent: config.AgentConfig{HeartbeatURL: srv.URL}})
	if err := client.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestClientFetchCommands(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"ID":"cmd-1","Type":"provision","Payload":null}]`))
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{Agent: config.AgentConfig{CommandsURL: srv.URL}})
	cmds, err := client.FetchCommands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("expected 1 command, got %d", len(cmds))
	}
	if cmds[0].ID != "cmd-1" || cmds[0].Type != "provision" {
		t.Errorf("unexpected command: %+v", cmds[0])
	}
}

func TestClientFetchCommandsNoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{Agent: config.AgentConfig{CommandsURL: srv.URL}})
	cmds, err := client.FetchCommands(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cmds != nil {
		t.Errorf("expected nil commands on 204, got %v", cmds)
	}
}

func TestClientFetchCommandsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{Agent: config.AgentConfig{CommandsURL: srv.URL}})
	_, err := client.FetchCommands(context.Background())
	if err == nil {
		t.Error("expected error on 500")
	}
}

func TestParseVarsAgentURLs(t *testing.T) {
	input := `HOSTNAME="standby-host"
HEARTBEAT_URL="http://server/status/heartbeat"
COMMANDS_URL="http://server/commands"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.HeartbeatURL != "http://server/status/heartbeat" {
		t.Errorf("HeartbeatURL = %q", cfg.Agent.HeartbeatURL)
	}
	if cfg.Agent.CommandsURL != "http://server/commands" {
		t.Errorf("CommandsURL = %q", cfg.Agent.CommandsURL)
	}
}

func TestParseVarsStaticNetworking(t *testing.T) {
	input := `STATIC_IP="10.0.0.5/24"
STATIC_GATEWAY="10.0.0.1"
STATIC_IFACE="eth0"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.Static.IP != "10.0.0.5/24" {
		t.Errorf("StaticIP = %q, want %q", cfg.Network.Static.IP, "10.0.0.5/24")
	}
	if cfg.Network.Static.Gateway != "10.0.0.1" {
		t.Errorf("StaticGateway = %q, want %q", cfg.Network.Static.Gateway, "10.0.0.1")
	}
	if cfg.Network.Static.Iface != "eth0" {
		t.Errorf("StaticIface = %q, want %q", cfg.Network.Static.Iface, "eth0")
	}
}

func TestParseVarsNetworkPersistence(t *testing.T) {
	input := `PERSIST_NETWORK="true"
OS_FAMILY="Flatcar"
STATIC_IP="10.0.0.5/24"
STATIC_IFACE="eth0"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PersistNetwork {
		t.Fatal("PersistNetwork = false, want true")
	}
	if cfg.OSFamily != "flatcar" {
		t.Errorf("OSFamily = %q, want flatcar", cfg.OSFamily)
	}
}

func TestParseVarsRejectsFlatcarCloudInit(t *testing.T) {
	input := `PERSIST_NETWORK="true"
OS_FAMILY="Flatcar"
STATIC_IP="10.0.0.5/24"
STATIC_IFACE="eth0"
CLOUDINIT_ENABLED="true"
CLOUDINIT_DATASOURCE="nocloud"
`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("ParseVars() error = nil, want Flatcar cloud-init rejection")
	}
	if !strings.Contains(err.Error(), "Flatcar first-boot provisioning requires Ignition") {
		t.Fatalf("ParseVars() error = %q, want Flatcar Ignition context", err.Error())
	}
}

func TestParseVarsBondConfig(t *testing.T) {
	input := `BOND_INTERFACES="eth0,eth1"
BOND_MODE="802.3ad"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.Bond.Interfaces != "eth0,eth1" {
		t.Errorf("BondInterfaces = %q, want %q", cfg.Network.Bond.Interfaces, "eth0,eth1")
	}
	if cfg.Network.Bond.Mode != "802.3ad" {
		t.Errorf("BondMode = %q, want %q", cfg.Network.Bond.Mode, "802.3ad")
	}
}

func TestParseVarsSecureErase(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{`SECURE_ERASE="true"`, true},
		{`SECURE_ERASE="1"`, true},
		{`SECURE_ERASE="yes"`, true},
		{`SECURE_ERASE="false"`, false},
		{`SECURE_ERASE="0"`, false},
	}
	for _, tt := range tests {
		cfg, err := ParseVars(strings.NewReader(tt.input))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Provision.Disk.SecureErase != tt.want {
			t.Errorf("SecureErase for %q = %v, want %v", tt.input, cfg.Provision.Disk.SecureErase, tt.want)
		}
	}
}

func TestParseVarsPostProvisionCmds(t *testing.T) {
	input := `POST_PROVISION_CMDS="apt update;systemctl enable foo;echo done"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Provision.PostProvisionCmds) != 3 {
		t.Fatalf("PostProvisionCmds len = %d, want 3", len(cfg.Provision.PostProvisionCmds))
	}
	if cfg.Provision.PostProvisionCmds[0] != "apt update" {
		t.Errorf("cmd[0] = %q, want %q", cfg.Provision.PostProvisionCmds[0], "apt update")
	}
	if cfg.Provision.PostProvisionCmds[2] != "echo done" {
		t.Errorf("cmd[2] = %q, want %q", cfg.Provision.PostProvisionCmds[2], "echo done")
	}
}

func TestParseVarsUnsupportedLUKSVar(t *testing.T) {
	input := `LUKS_ENABLE="true"`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for unsupported LUKS var")
	}
	if !strings.Contains(err.Error(), "LUKS_ENABLE is not supported yet") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseVarsImageChecksum(t *testing.T) {
	input := `IMAGE_CHECKSUM="abc123def456"
IMAGE_CHECKSUM_TYPE="sha256"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Image.Checksum != "abc123def456" {
		t.Errorf("ImageChecksum = %q", cfg.Provision.Image.Checksum)
	}
	if cfg.Provision.Image.ChecksumType != "sha256" {
		t.Errorf("ImageChecksumType = %q", cfg.Provision.Image.ChecksumType)
	}
}

func TestParseVarsSecureBootPinnedDigests(t *testing.T) {
	input := `SECUREBOOT_SHIM_SHA256="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
SECUREBOOT_GRUB_SHA256="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
SECUREBOOT_KERNEL_SHA256="cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Provision.SecureBoot.PinnedDigests
	if got["shim"] != strings.Repeat("a", 64) {
		t.Errorf("shim digest = %q", got["shim"])
	}
	if got["grub"] != "sha256:"+strings.Repeat("b", 64) {
		t.Errorf("grub digest = %q", got["grub"])
	}
	if got["kernel"] != strings.Repeat("c", 64) {
		t.Errorf("kernel digest = %q", got["kernel"])
	}
}

func TestParseVarsSecureBootPinnedDigestsJSON(t *testing.T) {
	input := `SECUREBOOT_PINNED_DIGESTS='{"shim":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","grub":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}'
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Provision.SecureBoot.PinnedDigests
	if got["shim"] != strings.Repeat("a", 64) {
		t.Errorf("shim digest = %q", got["shim"])
	}
	if got["grub"] != strings.Repeat("b", 64) {
		t.Errorf("grub digest = %q", got["grub"])
	}
}

func TestParseVarsSecureBootPinnedDigestsJSONDoesNotOverrideComponentPins(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name: "json before component pin",
			input: `SECUREBOOT_PINNED_DIGESTS='{"shim":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","grub":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","kernel":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}'
SECUREBOOT_KERNEL_SHA256="dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
`,
		},
		{
			name: "json after component pin",
			input: `SECUREBOOT_KERNEL_SHA256="dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
SECUREBOOT_PINNED_DIGESTS='{"shim":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","grub":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","kernel":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}'
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseVars(strings.NewReader(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			got := cfg.Provision.SecureBoot.PinnedDigests
			if got["shim"] != strings.Repeat("a", 64) {
				t.Errorf("shim digest = %q", got["shim"])
			}
			if got["grub"] != strings.Repeat("b", 64) {
				t.Errorf("grub digest = %q", got["grub"])
			}
			if got["kernel"] != strings.Repeat("d", 64) {
				t.Errorf("kernel digest = %q", got["kernel"])
			}
		})
	}
}

func TestParseVarsSysextConfig(t *testing.T) {
	input := `SYSEXT_ENABLED="true"
SYSEXT_DEFAULT_MODE="preload"
SYSEXT_CATALOG_DIR="/usr/lib/tcaas-sysext/preloaded"
SYSEXT_ACTIVE_DIR="/var/lib/extensions"
SYSEXT_ALLOW_INSECURE_HTTP="true"
SYSEXT_LAYERS='[{"name":"node-tuning","version":"v1","source":"https://example.invalid/node-tuning.raw","fileName":"node-tuning.raw","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mode":"preload"}]'
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provision.Sysext.Enabled {
		t.Fatal("Sysext.Enabled should be true")
	}
	if cfg.Provision.Sysext.DefaultMode != "preload" {
		t.Errorf("Sysext.DefaultMode = %q", cfg.Provision.Sysext.DefaultMode)
	}
	if cfg.Provision.Sysext.CatalogDir != "/usr/lib/tcaas-sysext/preloaded" {
		t.Errorf("Sysext.CatalogDir = %q", cfg.Provision.Sysext.CatalogDir)
	}
	if cfg.Provision.Sysext.ActiveDir != "/var/lib/extensions" {
		t.Errorf("Sysext.ActiveDir = %q", cfg.Provision.Sysext.ActiveDir)
	}
	if !cfg.Provision.Sysext.AllowInsecureHTTP {
		t.Error("Sysext.AllowInsecureHTTP should be true")
	}
	if len(cfg.Provision.Sysext.Layers) != 1 {
		t.Fatalf("Sysext.Layers len = %d, want 1", len(cfg.Provision.Sysext.Layers))
	}
	layer := cfg.Provision.Sysext.Layers[0]
	if layer.Name != "node-tuning" || layer.Source != "https://example.invalid/node-tuning.raw" {
		t.Fatalf("unexpected sysext layer: %#v", layer)
	}
}

func TestParseVarsOCIPrePullConfig(t *testing.T) {
	input := `OCI_PREPULL_ENABLED="true"
OCI_PREPULL_IMAGES="oci://registry.example.invalid/tcaas/pause:v1,registry.example.invalid/tcaas/cni:v2"
OCI_PREPULL_CACHE_DIR="/var/lib/booty/prepulls"
OCI_PREPULL_IMPORT_NAMESPACE="k8s.io"
OCI_PREPULL_ALLOW_INSECURE="true"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	prePulls := cfg.Provision.OCIPrePulls
	if !prePulls.Enabled {
		t.Fatal("OCIPrePulls.Enabled should be true")
	}
	if len(prePulls.Images) != 2 {
		t.Fatalf("OCIPrePulls.Images len = %d, want 2", len(prePulls.Images))
	}
	if prePulls.Images[0].Reference != "oci://registry.example.invalid/tcaas/pause:v1" {
		t.Fatalf("first reference = %q", prePulls.Images[0].Reference)
	}
	if prePulls.Images[1].Reference != "registry.example.invalid/tcaas/cni:v2" {
		t.Fatalf("second reference = %q", prePulls.Images[1].Reference)
	}
	if prePulls.CacheDir != "/var/lib/booty/prepulls" {
		t.Fatalf("CacheDir = %q", prePulls.CacheDir)
	}
	if prePulls.ImportNamespace != "k8s.io" {
		t.Fatalf("ImportNamespace = %q", prePulls.ImportNamespace)
	}
	if !prePulls.AllowInsecure {
		t.Fatal("OCIPrePulls.AllowInsecure should be true")
	}
}

func TestParseVarsSysextConfigGoQuotedJSON(t *testing.T) {
	layersJSON := `[
		{"name":"node-tuning","version":"v1","source":"oci://registry.example.com/tcaas/sysext-node-tuning:v1","fileName":"node-tuning.raw","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","mode":"preload"}
	]`
	input := fmt.Sprintf("export SYSEXT_ENABLED=%q\nexport SYSEXT_LAYERS=%q\n", "true", layersJSON)

	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provision.Sysext.Enabled {
		t.Fatal("Sysext.Enabled should be true")
	}
	if len(cfg.Provision.Sysext.Layers) != 1 {
		t.Fatalf("Sysext.Layers len = %d, want 1", len(cfg.Provision.Sysext.Layers))
	}
	layer := cfg.Provision.Sysext.Layers[0]
	if layer.Source != "oci://registry.example.com/tcaas/sysext-node-tuning:v1" {
		t.Fatalf("Source = %q", layer.Source)
	}
	if layer.FileName != "node-tuning.raw" {
		t.Fatalf("FileName = %q", layer.FileName)
	}
}

func TestParseVarsSysextLayersInvalidJSON(t *testing.T) {
	input := `SYSEXT_LAYERS="not-json"`
	_, err := ParseVars(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected invalid SYSEXT_LAYERS error")
	}
	if !strings.Contains(err.Error(), "invalid SYSEXT_LAYERS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseVarsNumVFs(t *testing.T) {
	input := `NUM_VFS="64"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Disk.NumVFs != 64 {
		t.Errorf("NumVFs = %d, want 64", cfg.Provision.Disk.NumVFs)
	}
}

func TestParseVarsBGPMinPeers(t *testing.T) {
	input := `BGP_MIN_PEERS="3"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.BGP.MinPeers != 3 {
		t.Errorf("BGPMinPeers = %d, want 3", cfg.Network.BGP.MinPeers)
	}
}

func TestParseVarsDisableKexec(t *testing.T) {
	input := `DISABLE_KEXEC="true"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provision.DisableKexec {
		t.Error("DisableKexec should be true")
	}
}

func TestParseVarsVLANs(t *testing.T) {
	input := `VLANS="200:eno1:10.200.0.42/24,300:eno2"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Network.VLAN.Config != "200:eno1:10.200.0.42/24,300:eno2" {
		t.Errorf("VLANs = %q, want %q", cfg.Network.VLAN.Config, "200:eno1:10.200.0.42/24,300:eno2")
	}
}

func TestParseVarsInventory(t *testing.T) {
	input := `INVENTORY_ENABLED="true"
INVENTORY_URL="http://caprf.example.com/inventory"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provision.Inventory.Enabled {
		t.Error("InventoryEnabled should be true")
	}
	if cfg.Provision.Inventory.URL != "http://caprf.example.com/inventory" {
		t.Errorf("InventoryURL = %q", cfg.Provision.Inventory.URL)
	}
}

func TestParseVarsFirmwareConfig(t *testing.T) {
	input := strings.Join([]string{
		`FIRMWARE_REPORT="true"`,
		`FIRMWARE_URL="http://caprf/firmware"`,
		`FIRMWARE_MIN_BIOS="U50"`,
		`FIRMWARE_MIN_BMC="2.72"`,
	}, "\n")
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provision.Firmware.Enabled {
		t.Error("FirmwareEnabled should be true")
	}
	if cfg.Provision.Firmware.URL != "http://caprf/firmware" {
		t.Errorf("FirmwareURL = %q", cfg.Provision.Firmware.URL)
	}
	if cfg.Provision.Firmware.MinBIOS != "U50" {
		t.Errorf("FirmwareMinBIOS = %q", cfg.Provision.Firmware.MinBIOS)
	}
	if cfg.Provision.Firmware.MinBMC != "2.72" {
		t.Errorf("FirmwareMinBMC = %q", cfg.Provision.Firmware.MinBMC)
	}
}

func TestParseVarsCrashArtifactsConfig(t *testing.T) {
	input := strings.Join([]string{
		`CRASH_ARTIFACTS_ENABLED="true"`,
		`CRASH_ARTIFACTS_PREPARE_URL="https://caprf.example.com/crash/prepare"`,
		`CRASH_ARTIFACTS_UPLOAD_URL="https://caprf.example.com/crash/upload"`,
		`CRASH_ARTIFACTS_MAX_MB="128"`,
		`CRASH_ARTIFACTS_UPLOAD_TIMEOUT_SEC="180"`,
		`CRASH_ARTIFACTS_INCLUDE_MEMORY_DUMPS="true"`,
	}, "\n")
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provision.CrashArtifacts.Enabled {
		t.Error("CrashArtifactsEnabled should be true")
	}
	if cfg.Provision.CrashArtifacts.PrepareURL != "https://caprf.example.com/crash/prepare" {
		t.Errorf("CrashArtifactsPrepareURL = %q", cfg.Provision.CrashArtifacts.PrepareURL)
	}
	if cfg.Provision.CrashArtifacts.UploadURL != "https://caprf.example.com/crash/upload" {
		t.Errorf("CrashArtifactsUploadURL = %q", cfg.Provision.CrashArtifacts.UploadURL)
	}
	if cfg.Provision.CrashArtifacts.MaxMB != 128 {
		t.Errorf("CrashArtifactsMaxMB = %d, want 128", cfg.Provision.CrashArtifacts.MaxMB)
	}
	if cfg.Provision.CrashArtifacts.UploadTimeoutSec != 180 {
		t.Errorf("CrashArtifactsUploadTimeoutSec = %d, want 180", cfg.Provision.CrashArtifacts.UploadTimeoutSec)
	}
	if !cfg.Provision.CrashArtifacts.IncludeMemoryDumps {
		t.Error("CrashArtifactsIncludeMemoryDumps should be true")
	}
}

func TestParseVarsNVMeNamespaces(t *testing.T) {
	input := `NVME_NAMESPACES="[{\"controller\":\"/dev/nvme0\",\"namespaces\":[{\"label\":\"os\",\"sizePct\":100}]}]"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provision.Disk.NVMeNamespaces == "" {
		t.Fatal("NVMeNamespaces should be populated")
	}
	if !strings.Contains(cfg.Provision.Disk.NVMeNamespaces, "/dev/nvme0") {
		t.Errorf("NVMeNamespaces = %q, want controller /dev/nvme0", cfg.Provision.Disk.NVMeNamespaces)
	}
}

func TestParseVarsSingleQuoteStripping(t *testing.T) {
	input := `NVME_NAMESPACES='[{"controller":"/dev/nvme0","namespaces":[{"label":"os","sizePct":100}]}]'`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cfg.Provision.Disk.NVMeNamespaces, "/dev/nvme0") {
		t.Errorf("single-quoted value not parsed correctly: NVMeNamespaces = %q", cfg.Provision.Disk.NVMeNamespaces)
	}
}

func TestParseVarsDoubleQuotedShellEscapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "escaped quotes",
			input: `TOKEN="token with \"quoted\" segment"`,
			want:  `token with "quoted" segment`,
		},
		{
			name:  "literal newline escape",
			input: `TOKEN="line\nnext"`,
			want:  `line\nnext`,
		},
		{
			name:  "unknown escape preserved",
			input: `TOKEN="path\qvalue"`,
			want:  `path\qvalue`,
		},
		{
			name:  "shell special escapes",
			input: "TOKEN=\"dollar \\$ backtick \\` slash \\\\\"",
			want:  "dollar $ backtick ` slash \\",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseVars(strings.NewReader(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Transport.Token != tc.want {
				t.Fatalf("Token = %q, want %q", cfg.Transport.Token, tc.want)
			}
		})
	}
}

func TestParseVarsUnmatchedQuotesPreserved(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single-leading double-trailing",
			input: `TOKEN='abc"`,
			want:  `'abc"`,
		},
		{
			name:  "double-leading single-trailing",
			input: `TOKEN="abc'`,
			want:  `"abc'`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseVars(strings.NewReader(tc.input))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Transport.Token != tc.want {
				t.Fatalf("Token = %q, want %q", cfg.Transport.Token, tc.want)
			}
		})
	}
}

func TestReportFirmware(t *testing.T) {
	var received []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		received = body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "test-token"},
		Provision: config.ProvisionConfig{Firmware: config.FirmwareConfig{URL: srv.URL}},
	})

	data := []byte(`{"bios":{"version":"U50"}}`)
	if err := client.ReportFirmware(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	if string(received) != string(data) {
		t.Errorf("received = %q, want %q", received, data)
	}
}

func TestReportFirmwareNoURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	err := client.ReportFirmware(context.Background(), []byte(`{}`))
	if err != nil {
		t.Errorf("expected nil error when no URL, got %v", err)
	}
}

func TestParseBoolVar(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"true", true},
		{"TRUE", true},
		{"True", true},
		{"1", true},
		{"yes", true},
		{"YES", true},
		{"Yes", true},
		{" true ", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"no", false},
	}
	for _, tt := range tests {
		got := parseBoolVar(tt.input)
		if got != tt.want {
			t.Errorf("parseBoolVar(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseVarsHealthChecks(t *testing.T) {
	input := `HEALTH_CHECKS_ENABLED="true"
HEALTH_MIN_MEMORY_GB="16"
HEALTH_MIN_CPUS="4"
HEALTH_SKIP_CHECKS="disk-ioerr,thermal-state"
HEALTH_CHECK_URL="http://caprf.example.com/health"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Health.Enabled {
		t.Error("HealthChecksEnabled should be true")
	}
	if cfg.Health.MinMemoryGB != 16 {
		t.Errorf("HealthMinMemoryGB = %d, want 16", cfg.Health.MinMemoryGB)
	}
	if cfg.Health.MinCPUs != 4 {
		t.Errorf("HealthMinCPUs = %d, want 4", cfg.Health.MinCPUs)
	}
	if cfg.Health.SkipChecks != "disk-ioerr,thermal-state" {
		t.Errorf("HealthSkipChecks = %q", cfg.Health.SkipChecks)
	}
	if cfg.Health.ReportURL != "http://caprf.example.com/health" {
		t.Errorf("HealthCheckURL = %q", cfg.Health.ReportURL)
	}
}

func TestParseVarsHealthChecksDisabled(t *testing.T) {
	input := `HEALTH_CHECKS_ENABLED="false"`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Health.Enabled {
		t.Error("HealthChecksEnabled should be false")
	}
}

func TestParseVarsDryRun(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{`DRY_RUN="true"`, true},
		{`DRY_RUN="TRUE"`, true},
		{`DRY_RUN="1"`, true},
		{`DRY_RUN="false"`, false},
		{`DRY_RUN="0"`, false},
	}
	for _, tt := range tests {
		cfg, err := ParseVars(strings.NewReader(tt.input))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DryRun != tt.want {
			t.Errorf("ParseVars(%q): DryRun = %v, want %v", tt.input, cfg.DryRun, tt.want)
		}
	}
}

func TestClientReportHealthChecks(t *testing.T) {
	var receivedBody string
	var receivedContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{Health: config.HealthConfig{ReportURL: srv.URL}})
	results := []health.CheckResult{
		{Name: "disk-presence", Status: "pass", Severity: "critical", Message: "ok"},
	}
	err := client.ReportHealthChecks(context.Background(), results)
	if err != nil {
		t.Fatal(err)
	}

	if receivedContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", receivedContentType)
	}

	if !strings.Contains(receivedBody, "disk-presence") {
		t.Errorf("body does not contain expected check name: %s", receivedBody)
	}
}

func TestClientReportHealthChecksNoURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	err := client.ReportHealthChecks(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientAcknowledgeCommand(t *testing.T) {
	var receivedMethod, receivedPath, receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{Agent: config.AgentConfig{CommandsURL: srv.URL + "/commands"}})
	err := client.AcknowledgeCommand(context.Background(), "cmd-123", "completed", "done")
	if err != nil {
		t.Fatal(err)
	}

	if receivedMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", receivedMethod)
	}
	if receivedPath != "/commands/ack" {
		t.Errorf("path = %q, want /commands/ack", receivedPath)
	}
	if !strings.Contains(receivedBody, `"id":"cmd-123"`) {
		t.Errorf("body missing cmd ID: %s", receivedBody)
	}
	if !strings.Contains(receivedBody, `"status":"completed"`) {
		t.Errorf("body missing status: %s", receivedBody)
	}
}

func TestClientAcknowledgeCommandNoURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{})
	err := client.AcknowledgeCommand(context.Background(), "cmd-1", "completed", "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientFetchCommandsRejectsInsecureRemoteURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{Agent: config.AgentConfig{CommandsURL: "http://caprf.example.com/commands"}})

	_, err := client.FetchCommands(context.Background())
	if err == nil {
		t.Fatal("expected error for insecure commands URL")
	}
	if !strings.Contains(err.Error(), "requires HTTPS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientAcknowledgeCommandRejectsInsecureRemoteURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{Agent: config.AgentConfig{CommandsURL: "http://caprf.example.com/commands"}})

	err := client.AcknowledgeCommand(context.Background(), "cmd-1", "completed", "")
	if err == nil {
		t.Fatal("expected error for insecure commands URL")
	}
	if !strings.Contains(err.Error(), "requires HTTPS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquireTokenNoURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{Transport: config.TransportConfig{Token: "bootstrap"}})
	if err := client.AcquireToken(context.Background()); err != nil {
		t.Fatalf("AcquireToken() with no URL should succeed: %v", err)
	}
}

func TestAcquireTokenRequiresHostname(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{
			Token:    "bootstrap-token",
			TokenURL: "https://auth.example.com/token",
		},
	})

	err := client.AcquireToken(context.Background())
	if err == nil {
		t.Fatal("expected error when TokenURL is configured and Hostname is empty")
	}
	if !strings.Contains(err.Error(), "hostname is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquireTokenRequiresBootstrapToken(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{
		Hostname:  "test-host",
		Transport: config.TransportConfig{TokenURL: "https://auth.example.com/token"},
	})
	err := client.AcquireToken(context.Background())
	if err == nil {
		t.Fatal("expected error when Token is empty but TokenURL is set")
	}
	if !strings.Contains(err.Error(), "no bootstrap token") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquireTokenRejectsInvalidAlgorithm(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{
		Hostname: "worker-01",
		Transport: config.TransportConfig{
			Token:          "bootstrap-token",
			TokenURL:       "https://auth.example.com/token",
			TokenAlgorithm: "HS256",
		},
	})

	err := client.AcquireToken(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported token algorithm")
	}
	if !strings.Contains(err.Error(), "unsupported token algorithm") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAcquireTokenWithServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bootstrap-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"jwt-token-123","token_type":"Bearer","expires_in":3600}`)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{
		Hostname:  "test-host",
		Transport: config.TransportConfig{Token: "bootstrap-token", TokenURL: srv.URL},
	})
	if err := client.AcquireToken(context.Background()); err != nil {
		t.Fatalf("AcquireToken() error: %v", err)
	}
	cfg, _ := client.GetConfig(context.Background())
	if cfg.Transport.Token != "jwt-token-123" {
		t.Errorf("Token = %q, want %q", cfg.Transport.Token, "jwt-token-123")
	}
}

func TestAcquireTokenErrorRedactsTokenURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	tokenURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tokenURL.User = url.UserPassword("leaky-user", "super-secret")
	tokenURL.RawQuery = "token=abc"
	tokenURL.Fragment = "frag"

	client := NewFromConfig(&config.MachineConfig{
		Hostname: "test-host",
		Transport: config.TransportConfig{
			Token:    "bootstrap-token",
			TokenURL: tokenURL.String(),
		},
	})
	err = client.AcquireToken(context.Background())
	if err == nil {
		t.Fatal("expected token acquisition error")
	}
	for _, leaked := range []string{"leaky-user", "super-secret", "token=abc", "frag"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("AcquireToken error leaked sensitive URL part %q: %q", leaked, err.Error())
		}
		for current := err; current != nil; current = errors.Unwrap(current) {
			if strings.Contains(current.Error(), leaked) {
				t.Fatalf("AcquireToken error chain leaked sensitive URL part %q: %q", leaked, current.Error())
			}
			if strings.Contains(fmt.Sprintf("%#v", current), leaked) {
				t.Fatalf("AcquireToken error wrapper retained sensitive URL part %q: %#v", leaked, current)
			}
		}
	}
	if !strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("AcquireToken error = %q, want redacted URL context %q", err.Error(), srv.URL)
	}
}

func TestReportMetrics_TelemetryDisabled(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{
		Telemetry: config.TelemetryConfig{Enabled: false, MetricsURL: "http://example.com/metrics"},
	})
	// Should no-op when telemetry is disabled.
	if err := client.ReportMetrics(context.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("ReportMetrics() error: %v", err)
	}
}

func TestReportMetrics_TelemetryEnabled(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "test-token"},
		Telemetry: config.TelemetryConfig{Enabled: true, MetricsURL: srv.URL + "/metrics"},
	})
	data := []byte(`{"stepRetries":3}`)
	if err := client.ReportMetrics(context.Background(), data); err != nil {
		t.Fatalf("ReportMetrics() error: %v", err)
	}
	if receivedBody != string(data) {
		t.Errorf("body = %q, want %q", receivedBody, data)
	}
}

func TestReportMetrics_FallbackToTelemetryURL(t *testing.T) {
	var received bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		received = true
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{
		Telemetry: config.TelemetryConfig{Enabled: true, URL: srv.URL + "/telemetry"},
	})
	if err := client.ReportMetrics(context.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("ReportMetrics() error: %v", err)
	}
	if !received {
		t.Error("expected request to TelemetryURL fallback")
	}
}

func TestReportMetrics_NoURL(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{Telemetry: config.TelemetryConfig{Enabled: true}})
	// No MetricsURL or TelemetryURL — should silently no-op.
	if err := client.ReportMetrics(context.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("ReportMetrics() error: %v", err)
	}
}

func TestSendEvent_TelemetryDisabled(t *testing.T) {
	client := NewFromConfig(&config.MachineConfig{
		Telemetry: config.TelemetryConfig{Enabled: false, EventURL: "http://example.com/events"},
	})
	if err := client.SendEvent(context.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("SendEvent() error: %v", err)
	}
}

func TestSendEvent_TelemetryEnabled(t *testing.T) {
	var received bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		received = true
	}))
	defer srv.Close()

	client := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "test-token"},
		Telemetry: config.TelemetryConfig{Enabled: true, EventURL: srv.URL + "/events"},
	})
	if err := client.SendEvent(context.Background(), []byte(`{"event":"started"}`)); err != nil {
		t.Fatalf("SendEvent() error: %v", err)
	}
	if !received {
		t.Error("expected event to be sent")
	}
}

func TestParseVarsTokenFields(t *testing.T) {
	input := `export TOKEN_URL="https://auth.example.com/token"
export TOKEN_ALGORITHM="ES256"
`
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport.TokenURL != "https://auth.example.com/token" {
		t.Errorf("TokenURL = %q, want %q", cfg.Transport.TokenURL, "https://auth.example.com/token")
	}
	if cfg.Transport.TokenAlgorithm != "ES256" {
		t.Errorf("TokenAlgorithm = %q, want %q", cfg.Transport.TokenAlgorithm, "ES256")
	}
}

func TestParseVarsRescueMode(t *testing.T) {
	vars := "RESCUE_MODE=shell\n" +
		"RESCUE_SSH_PUBKEY=ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest admin@ops\n" +
		"RESCUE_PASSWORD_HASH=$6$rounds=5000$salt$hash\n" +
		"RESCUE_TIMEOUT=3600\n" +
		"RESCUE_AUTO_MOUNT=true\n"

	cfg, err := ParseVars(strings.NewReader(vars))
	if err != nil {
		t.Fatalf("ParseVars() error: %v", err)
	}
	if cfg.Rescue.Mode != "shell" {
		t.Errorf("RescueMode = %q, want %q", cfg.Rescue.Mode, "shell")
	}
	if cfg.Rescue.SSHPubKey != "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest admin@ops" {
		t.Errorf("RescueSSHPubKey = %q", cfg.Rescue.SSHPubKey)
	}
	if cfg.Rescue.PasswordHash != "$6$rounds=5000$salt$hash" {
		t.Errorf("RescuePasswordHash = %q", cfg.Rescue.PasswordHash)
	}
	if cfg.Rescue.Timeout != 3600 {
		t.Errorf("RescueTimeout = %d, want 3600", cfg.Rescue.Timeout)
	}
	if !cfg.Rescue.AutoMountDisks {
		t.Error("RescueAutoMountDisks = false, want true")
	}
}

func TestParseVarsCloudInitConfig(t *testing.T) {
	input := strings.Join([]string{
		`CLOUDINIT_ENABLED="true"`,
		`CLOUDINIT_DATASOURCE="nocloud"`,
	}, "\n")
	cfg, err := ParseVars(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Provision.CloudInit.Enabled {
		t.Error("CloudInitEnabled should be true")
	}
	if cfg.Provision.CloudInit.Datasource != "nocloud" {
		t.Errorf("CloudInitDatasource = %q, want nocloud", cfg.Provision.CloudInit.Datasource)
	}
}

func TestSetAuth(t *testing.T) {
	tests := []struct {
		name              string
		token             string
		url               string
		insecureTransport bool
		wantAuth          bool
		wantErr           bool
	}{
		{"https with token", "tok", "https://example.com/status", false, true, false},
		{"http remote rejected", "tok", "http://10.0.0.1/status", false, false, true},
		{"http localhost allowed", "tok", "http://127.0.0.1/status", false, true, false},
		{"http 127 hostname rejected", "tok", "http://127.evil.example/status", false, false, true},
		{"http ::1 allowed", "tok", "http://[::1]/status", false, true, false},
		{"no token", "", "https://example.com/status", false, false, false},
		{"http remote allowed with insecure transport", "tok", "http://10.0.0.1/status", true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{
				log: slog.Default().With("component", "caprf"),
				cfg: &config.MachineConfig{Transport: config.TransportConfig{Token: tt.token, Insecure: tt.insecureTransport}},
			}
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, tt.url, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			err = c.setAuth(req)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := req.Header.Get("Authorization")
			if tt.wantAuth && got == "" {
				t.Error("expected Authorization header, got none")
			}
			if !tt.wantAuth && got != "" {
				t.Errorf("expected no Authorization header, got %q", got)
			}
		})
	}
}

func TestRedactedURLRemovesCredentialsQueryAndFragment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "credentials query fragment",
			in:   "https://user:secret@example.com/status?token=abc#frag",
			want: "https://example.com/status",
		},
		{
			name: "query fragment without credentials",
			in:   "https://example.com/status?token=abc#frag",
			want: "https://example.com/status",
		},
		{
			name: "invalid URL",
			in:   "::://bad-url",
			want: "<invalid-url>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactedURL(tt.in); got != tt.want {
				t.Fatalf("redactedURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRequireSecureEndpointParseErrorRedactsSensitiveURLParts(t *testing.T) {
	c := NewFromConfig(&config.MachineConfig{})
	err := c.requireSecureEndpoint(malformedCAPRFEndpointWithSensitiveParts(), "commands polling")
	if err == nil {
		t.Fatal("expected parse error")
	}
	assertCAPRFErrorNoSensitiveURLParts(t, err)
	assertCAPRFErrorHasInvalidURLCause(t, err)
}

func TestFetchCommandsCreateRequestErrorRedactsSensitiveURLParts(t *testing.T) {
	c := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Insecure: true},
		Agent:     config.AgentConfig{CommandsURL: malformedCAPRFEndpointWithSensitiveParts()},
	})
	_, err := c.FetchCommands(context.Background())
	if err == nil {
		t.Fatal("expected request construction error")
	}
	assertCAPRFErrorNoSensitiveURLParts(t, err)
	assertCAPRFErrorHasInvalidURLCause(t, err)
}

func TestAcknowledgeCommandBuildURLErrorRedactsSensitiveURLParts(t *testing.T) {
	c := NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Insecure: true},
		Agent:     config.AgentConfig{CommandsURL: malformedCAPRFEndpointWithSensitiveParts()},
	})
	err := c.AcknowledgeCommand(context.Background(), "cmd-placeholder", "completed", "")
	if err == nil {
		t.Fatal("expected ack URL build error")
	}
	assertCAPRFErrorNoSensitiveURLParts(t, err)
	assertCAPRFErrorHasInvalidURLCause(t, err)
}

func TestDoPostCreateRequestErrorRedactsSensitiveURLParts(t *testing.T) {
	c := NewFromConfig(&config.MachineConfig{})
	err := c.doPost(context.Background(), malformedCAPRFEndpointWithSensitiveParts(), "body")
	if err == nil {
		t.Fatal("expected request construction error")
	}
	assertCAPRFErrorNoSensitiveURLParts(t, err)
	assertCAPRFErrorHasInvalidURLCause(t, err)
}

func malformedCAPRFEndpointWithSensitiveParts() string {
	return "https://user-placeholder:pass-placeholder@example.com/%zz?token=placeholder-token#fragment-placeholder"
}

func assertCAPRFErrorHasInvalidURLCause(t *testing.T, err error) {
	t.Helper()
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("expected error chain to contain *url.Error, got %T: %v", err, err)
	}
	if urlErr.URL != "<invalid-url>" {
		t.Fatalf("url.Error.URL = %q, want redacted invalid URL marker", urlErr.URL)
	}
}

func assertCAPRFErrorNoSensitiveURLParts(t *testing.T, err error) {
	t.Helper()
	assertCAPRFErrorNoSensitiveURLPartsSeen(t, err, map[error]struct{}{})
}

func assertCAPRFErrorNoSensitiveURLPartsSeen(t *testing.T, err error, seen map[error]struct{}) {
	t.Helper()
	if err == nil {
		return
	}
	if _, ok := seen[err]; ok {
		return
	}
	seen[err] = struct{}{}
	assertNoCAPRFSensitiveURLParts(t, err.Error())
	assertNoCAPRFSensitiveURLParts(t, fmt.Sprintf("%#v", err))

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			assertCAPRFErrorNoSensitiveURLPartsSeen(t, child, seen)
		}
		return
	}
	assertCAPRFErrorNoSensitiveURLPartsSeen(t, errors.Unwrap(err), seen)
}

func assertNoCAPRFSensitiveURLParts(t *testing.T, msg string) {
	t.Helper()
	for _, leaked := range []string{
		"user-placeholder",
		"pass-placeholder",
		"token=placeholder-token",
		"fragment-placeholder",
	} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("error leaked sensitive CAPRF URL part %q: %q", leaked, msg)
		}
	}
}

func TestSetAuthErrorRedactsQueryToken(t *testing.T) {
	c := &Client{
		log: slog.Default().With("component", "caprf"),
		cfg: &config.MachineConfig{Transport: config.TransportConfig{Token: "tok"}},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://10.0.0.1/status?token=abc#frag", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	err = c.setAuth(req)
	if err == nil {
		t.Fatal("expected insecure transport error")
	}
	if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "#frag") {
		t.Fatalf("setAuth error leaked sensitive URL parts: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "http://10.0.0.1/status") {
		t.Fatalf("setAuth error = %q, want redacted URL context", err.Error())
	}
}

func TestWithRetryErrorRedactsQueryToken(t *testing.T) {
	c := &Client{
		log: slog.Default().With("component", "caprf"),
		cfg: &config.MachineConfig{},
	}
	err := c.withRetry(context.Background(), "https://example.com/status?token=abc#frag", func() error {
		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected retry error")
	}
	if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "#frag") {
		t.Fatalf("retry error leaked sensitive URL parts: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "https://example.com/status") {
		t.Fatalf("retry error = %q, want redacted URL context", err.Error())
	}
}

func TestDoPostErrorRedactsQueryToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewFromConfig(&config.MachineConfig{})
	err := c.doPost(context.Background(), srv.URL+"/status?token=abc#frag", "body")
	if err == nil {
		t.Fatal("expected POST status error")
	}
	if strings.Contains(err.Error(), "token=abc") || strings.Contains(err.Error(), "#frag") {
		t.Fatalf("POST error leaked sensitive URL parts: %q", err.Error())
	}
	if !strings.Contains(err.Error(), srv.URL+"/status") {
		t.Fatalf("POST error = %q, want redacted URL context", err.Error())
	}
}
