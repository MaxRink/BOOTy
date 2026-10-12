//go:build linux

package crash

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestCollectIncludesArtifactsAndMetadata(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "crash", "vmcore"), "vmcore data")
	writeTestFile(t, filepath.Join(root, "var", "log", "kern.log"), "kernel panic - not syncing: test")

	outDir := t.TempDir()
	result, err := Collect(context.Background(), &CollectOptions{
		RootPath:      root,
		PstorePath:    filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:     outDir,
		TargetDisk:    "/dev/sda",
		RootPartition: "/dev/sda2",
		MountPoint:    root,
		MaxBytes:      1024 * 1024,
		Config: &config.MachineConfig{
			Hostname: "node-1",
			Provision: config.ProvisionConfig{
				ProviderID: "redfish://node-1",
				CrashArtifacts: config.CrashArtifactsConfig{
					IncludeMemoryDumps: true,
				},
			},
			Mode: "provision",
		},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if !result.EvidenceFound {
		t.Fatal("expected evidence")
	}
	entries := readArchive(t, result.ArchivePath)
	for _, name := range []string{"manifest.json", "metadata.json", "target-root/var/crash/vmcore", "target-root/var/log/kern.log"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("archive missing %s; entries=%v", name, keys(entries))
		}
	}

	var manifest Manifest
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.Metadata.Machine.Hostname != "node-1" {
		t.Fatalf("hostname = %q, want node-1", manifest.Metadata.Machine.Hostname)
	}
	if len(manifest.Metadata.BuildInfo) == 0 {
		t.Fatal("expected build info metadata")
	}
	if manifest.Scan.TargetDisk != "/dev/sda" || manifest.Scan.RootPartition != "/dev/sda2" {
		t.Fatalf("unexpected scan metadata: %+v", manifest.Scan)
	}
}

func TestCollectSkipsMemoryDumpsByDefault(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "crash", "vmcore"), "vmcore data")
	writeTestFile(t, filepath.Join(root, "var", "crash", "vmcore-dmesg.txt"), "kernel panic\nTOKEN=kdump-log-secret")

	result, err := Collect(context.Background(), &CollectOptions{
		RootPath:   root,
		PstorePath: filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:  t.TempDir(),
		MaxBytes:   1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if !result.EvidenceFound {
		t.Fatal("expected evidence from text kdump log")
	}
	entries := readArchive(t, result.ArchivePath)
	if _, ok := entries["target-root/var/crash/vmcore"]; ok {
		t.Fatal("raw vmcore should not be archived by default")
	}
	kdumpLog := string(entries["target-root/var/crash/vmcore-dmesg.txt"])
	assertNotContains(t, kdumpLog, "kdump-log-secret")
	if !strings.Contains(kdumpLog, "TOKEN=[REDACTED]") {
		t.Fatalf("kdump text log was not redacted: %s", kdumpLog)
	}
	reasons := make([]string, 0, len(result.Manifest.Skipped))
	for _, skipped := range result.Manifest.Skipped {
		reasons = append(reasons, skipped.Reason)
	}
	if !contains(reasons, "memory_dump_upload_disabled") {
		t.Fatalf("expected memory dump skip, got %v", reasons)
	}
}

func TestCollectOnlyMemoryDumpsByDefaultSkipsArchive(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "crash", "vmcore"), "vmcore data")

	result, err := Collect(context.Background(), &CollectOptions{
		RootPath:   root,
		PstorePath: filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:  t.TempDir(),
		MaxBytes:   1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if result.EvidenceFound {
		t.Fatal("expected no evidence when only memory dumps are present")
	}
	if result.ArchivePath != "" {
		t.Fatalf("ArchivePath = %q, want empty", result.ArchivePath)
	}
}

func TestCollectRedactsTextArtifactsAndMetadata(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "log", "kern.log"), strings.Join([]string{
		"kernel panic - not syncing: test",
		strings.Repeat("x", 70*1024),
		"Authorization: Bearer bearer-secret",
		"TOKEN=token-secret",
		`PASSWORD="password-secret"`,
		"SECRET='secret-secret'",
		"BGP_AUTH_PASSWORD=bgp-secret",
		"MOK_PASSWORD=mok-secret",
		"url=https://user:pass@example.com/crash?token=query-secret#frag.",
	}, "\n"))
	writeTestFile(t, filepath.Join(root, "var", "crash", "crash.log"), "kernel panic\nMOK_PASSWORD=crash-log-secret")

	result, err := Collect(context.Background(), &CollectOptions{
		RootPath:   root,
		PstorePath: filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:  t.TempDir(),
		MaxBytes:   1024 * 1024,
		Config: &config.MachineConfig{
			Hostname: "node-1",
			Provision: config.ProvisionConfig{
				ProviderID: "https://user:pass@example.com/provider?token=provider-secret#frag",
			},
		},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	entries := readArchive(t, result.ArchivePath)
	archiveText := string(entries["target-root/var/log/kern.log"])
	crashText := string(entries["target-root/var/crash/crash.log"])
	metadataText := string(entries["metadata.json"])
	manifestText := string(entries["manifest.json"])
	var metadata HostMetadata
	if err := json.Unmarshal(entries["metadata.json"], &metadata); err != nil {
		t.Fatalf("redacted metadata is not valid JSON: %v", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(entries["manifest.json"], &manifest); err != nil {
		t.Fatalf("redacted manifest is not valid JSON: %v", err)
	}

	for _, secret := range []string{
		"bearer-secret",
		"token-secret",
		"password-secret",
		"secret-secret",
		"bgp-secret",
		"mok-secret",
		"user:pass",
		"query-secret",
		"provider-secret",
		"crash-log-secret",
	} {
		assertNotContains(t, archiveText, secret)
		assertNotContains(t, crashText, secret)
		assertNotContains(t, metadataText, secret)
		assertNotContains(t, manifestText, secret)
	}
	if !strings.Contains(archiveText, "Authorization: Bearer [REDACTED]") {
		t.Fatalf("archive log did not redact bearer token: %s", archiveText)
	}
	if !strings.Contains(archiveText, "https://example.com/crash.") {
		t.Fatalf("archive log did not redact URL credentials: %s", archiveText)
	}
	if result.Manifest.Metadata.Machine.ProviderID != "https://example.com/provider" {
		t.Fatalf("ProviderID = %q, want redacted URL", result.Manifest.Metadata.Machine.ProviderID)
	}
}

func TestRedactRawMessageRedactsJSONSecretFields(t *testing.T) {
	raw := json.RawMessage(`{
		"token": "json-token-secret",
		"nested": {
			"password": "json-password-secret",
			"url": "https://user:pass@example.com/path?token=query-secret"
		},
		"items": [{"BGP_AUTH_PASSWORD": "json-bgp-secret"}]
	}`)

	redacted := redactRawMessage(raw)
	var decoded map[string]any
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("redacted JSON is invalid: %v", err)
	}
	text := string(redacted)
	for _, secret := range []string{
		"json-token-secret",
		"json-password-secret",
		"json-bgp-secret",
		"user:pass",
		"query-secret",
	} {
		assertNotContains(t, text, secret)
	}
	for _, want := range []string{`"token":"[REDACTED]"`, `"password":"[REDACTED]"`, `"BGP_AUTH_PASSWORD":"[REDACTED]"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("redacted JSON missing %s: %s", want, text)
		}
	}
}

func TestCollectCreatesPrivateArchive(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "log", "kern.log"), "kernel panic - not syncing: test")
	outDir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir output dir: %v", err)
	}

	result, err := Collect(context.Background(), &CollectOptions{
		RootPath:   root,
		PstorePath: filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:  outDir,
		MaxBytes:   1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	assertPerm(t, outDir, 0o700)
	assertPerm(t, result.ArchivePath, 0o600)
}

func TestCollectNoEvidenceSkipsArchive(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "log", "kern.log"), "normal boot log")

	result, err := Collect(context.Background(), &CollectOptions{
		RootPath:   root,
		PstorePath: filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:  t.TempDir(),
		MaxBytes:   1024 * 1024,
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if result.EvidenceFound {
		t.Fatal("expected no evidence")
	}
	if result.ArchivePath != "" {
		t.Fatalf("ArchivePath = %q, want empty", result.ArchivePath)
	}
}

func TestCollectSkipsSymlinksAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "log", "kern.log"), "kernel panic with call trace")
	writeTestFile(t, filepath.Join(root, "var", "crash", "large-vmcore"), strings.Repeat("x", 128*1024))
	if err := os.Symlink("/etc/passwd", filepath.Join(root, "var", "crash", "passwd-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	result, err := Collect(context.Background(), &CollectOptions{
		RootPath:   root,
		PstorePath: filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:  t.TempDir(),
		MaxBytes:   64 * 1024,
		Config: &config.MachineConfig{
			Provision: config.ProvisionConfig{
				CrashArtifacts: config.CrashArtifactsConfig{
					IncludeMemoryDumps: true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if !result.EvidenceFound {
		t.Fatal("expected evidence")
	}
	reasons := make([]string, 0, len(result.Manifest.Skipped))
	for _, skipped := range result.Manifest.Skipped {
		reasons = append(reasons, skipped.Reason)
	}
	if !contains(reasons, "symlink_skipped") {
		t.Fatalf("expected symlink skip, got %v", reasons)
	}
	if !contains(reasons, "size_limit") {
		t.Fatalf("expected size limit skip, got %v", reasons)
	}
}

func TestCollectRemovesArchiveWhenFinalArchiveExceedsLimit(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "var", "log", "kern.log"), "kernel panic with call trace")
	outDir := t.TempDir()

	_, err := Collect(context.Background(), &CollectOptions{
		RootPath:   root,
		PstorePath: filepath.Join(t.TempDir(), "missing-pstore"),
		OutputDir:  outDir,
		MaxBytes:   1,
	})
	if err == nil {
		t.Fatal("expected final archive size error")
	}
	if !strings.Contains(err.Error(), "crash archive size") || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "crash-artifacts.tar.gz")); !os.IsNotExist(statErr) {
		t.Fatalf("oversized archive should be removed, stat error: %v", statErr)
	}
}

func TestShouldInspectModeGuards(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *config.MachineConfig
		wantOK bool
		reason string
	}{
		{name: "disabled", cfg: &config.MachineConfig{}, reason: "disabled"},
		{name: "default provision", cfg: &config.MachineConfig{Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{Enabled: true}}}, wantOK: true},
		{name: "deprovision", cfg: &config.MachineConfig{Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{Enabled: true}}, Mode: "deprovision"}, wantOK: true},
		{name: "hard", cfg: &config.MachineConfig{Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{Enabled: true}}, Mode: "hard"}, wantOK: true},
		{name: "decommission", cfg: &config.MachineConfig{Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{Enabled: true}}, Mode: "decommission"}, wantOK: true},
		{name: "dry-run", cfg: &config.MachineConfig{Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{Enabled: true}}, Mode: "dry-run"}, reason: "dry-run"},
		{name: "standby", cfg: &config.MachineConfig{Provision: config.ProvisionConfig{CrashArtifacts: config.CrashArtifactsConfig{Enabled: true}}, Mode: "standby"}, reason: "standby"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOK, gotReason := ShouldInspect(tt.cfg)
			if gotOK != tt.wantOK || gotReason != tt.reason {
				t.Fatalf("ShouldInspect() = (%v, %q), want (%v, %q)", gotOK, gotReason, tt.wantOK, tt.reason)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := make(map[string][]byte)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		entries[header.Name] = data
	}
	return entries
}

func keys(entries map[string][]byte) []string {
	out := make([]string, 0, len(entries))
	for key := range entries {
		out = append(out, key)
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func assertNotContains(t *testing.T, got, forbidden string) {
	t.Helper()
	if strings.Contains(got, forbidden) {
		t.Fatalf("output contains %q: %s", forbidden, got)
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}
