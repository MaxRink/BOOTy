//go:build linux

package provision

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestPrepareOCIPrePullsWritesArtifacts(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	restore := stubOCIPrePuller(t, "registry.example.invalid/tcaas/pause@sha256:"+strings.Repeat("a", 64))
	defer restore()

	cfg := &config.OCIPrePullConfig{
		Enabled:         true,
		CacheDir:        "/var/lib/booty/prepulls",
		ImportNamespace: "k8s.io",
		Images: []config.OCIPrePullImageConfig{
			{Reference: "oci://registry.example.invalid/tcaas/pause:v1"},
		},
	}
	if err := c.PrepareOCIPrePulls(context.Background(), cfg); err != nil {
		t.Fatalf("PrepareOCIPrePulls() error = %v", err)
	}

	catalog := readOCIPrePullCatalog(t, filepath.Join(c.rootDir, "var/lib/booty/prepulls/catalog.json"))
	if catalog.APIVersion != ociPrePullCatalogAPIVersion || catalog.Kind != ociPrePullCatalogKind {
		t.Fatalf("unexpected catalog identity: %#v", catalog)
	}
	if catalog.CacheDir != "/var/lib/booty/prepulls" || catalog.ImportNamespace != "k8s.io" {
		t.Fatalf("unexpected catalog defaults: %#v", catalog)
	}
	if catalog.ArchiveFormat != "docker-archive" {
		t.Fatalf("ArchiveFormat = %q, want docker-archive", catalog.ArchiveFormat)
	}
	if len(catalog.Images) != 1 {
		t.Fatalf("catalog images = %d, want 1", len(catalog.Images))
	}

	image := catalog.Images[0]
	if image.Reference != "oci://registry.example.invalid/tcaas/pause:v1" {
		t.Fatalf("catalog reference = %q", image.Reference)
	}
	if image.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("catalog digest = %q", image.Digest)
	}
	if !strings.HasPrefix(image.Archive, "/var/lib/booty/prepulls/archives/image-") {
		t.Fatalf("catalog archive = %q", image.Archive)
	}
	if image.SizeBytes == 0 {
		t.Fatal("catalog sizeBytes should be set")
	}

	archivePath := filepath.Join(c.rootDir, strings.TrimPrefix(image.Archive, "/"))
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if !strings.Contains(string(data), "oci://registry.example.invalid/tcaas/pause:v1") {
		t.Fatalf("archive content = %q", string(data))
	}
	if info, err := os.Stat(archivePath); err != nil {
		t.Fatalf("stat archive: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %v, want 0600", info.Mode().Perm())
	}

	importList := readStringFile(t, filepath.Join(c.rootDir, "var/lib/booty/prepulls/import-list.tsv"))
	if !strings.Contains(importList, image.Archive+"\t"+image.Reference+"\t"+image.Digest) {
		t.Fatalf("import list does not contain catalog image: %q", importList)
	}
	assertOCIPrePullImporterInstalled(t, c.rootDir)
}

func TestPrepareOCIPrePullsDisabledNoop(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	if err := c.PrepareOCIPrePulls(context.Background(), &config.OCIPrePullConfig{}); err != nil {
		t.Fatalf("PrepareOCIPrePulls() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.rootDir, "var/lib/booty/oci-prepulls")); !os.IsNotExist(err) {
		t.Fatalf("disabled pre-pulls should not create cache dir, stat err=%v", err)
	}
}

func TestPrepareOCIPrePullsEndToEndInstallsImporterAndImportsArchive(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	restore := stubOCIPrePuller(t, "registry.example.invalid/tcaas/pause@sha256:"+strings.Repeat("a", 64))
	defer restore()

	cfg := &config.OCIPrePullConfig{
		Enabled:         true,
		CacheDir:        "/var/lib/booty/prepulls",
		ImportNamespace: "e2e.io",
		Images: []config.OCIPrePullImageConfig{
			{Reference: "oci://registry.example.invalid/tcaas/pause:v1"},
		},
	}
	if err := c.PrepareOCIPrePulls(context.Background(), cfg); err != nil {
		t.Fatalf("PrepareOCIPrePulls() error = %v", err)
	}

	catalog := readOCIPrePullCatalog(t, filepath.Join(c.rootDir, "var/lib/booty/prepulls/catalog.json"))
	if len(catalog.Images) != 1 {
		t.Fatalf("catalog images = %d, want 1", len(catalog.Images))
	}
	fakeBin, runtimeLog := installFakeOCIRuntime(t)
	hostList := writeHostOCIPrePullImportList(t, c.rootDir, &catalog)
	stateDir := filepath.Join(t.TempDir(), "imported")

	runOCIPrePullImporter(t, c.rootDir, fakeBin, hostList, stateDir, runtimeLog, cfg.ImportNamespace)
	firstLog := readStringFile(t, runtimeLog)
	image := catalog.Images[0]
	archivePath := hostOCIPrePullArchivePath(c.rootDir, image.Archive)
	assertStringContains(t, firstLog, "-n e2e.io images import "+archivePath)

	statePath := filepath.Join(stateDir, filepath.Base(archivePath)+".done")
	if got := strings.TrimSpace(readStringFile(t, statePath)); got != image.Digest {
		t.Fatalf("import state digest = %q, want %q", got, image.Digest)
	}
	runOCIPrePullImporter(t, c.rootDir, fakeBin, hostList, stateDir, runtimeLog, cfg.ImportNamespace)
	if got := readStringFile(t, runtimeLog); got != firstLog {
		t.Fatalf("importer was not idempotent; runtime log changed from %q to %q", firstLog, got)
	}
}

func TestOCIPrePullArchiveNameDeterministic(t *testing.T) {
	first := ociPrePullArchiveName("oci://registry.example.invalid/tcaas/pause:v1", "sha256:"+strings.Repeat("a", 64))
	second := ociPrePullArchiveName("oci://registry.example.invalid/tcaas/pause:v1", "sha256:"+strings.Repeat("a", 64))
	if first != second {
		t.Fatalf("archive name is not deterministic: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, "image-") || !strings.HasSuffix(first, ".tar") {
		t.Fatalf("archive name = %q", first)
	}
}

func stubOCIPrePuller(t *testing.T, resolved string) func() {
	t.Helper()
	old := pullOCIPrePullImage
	pullOCIPrePullImage = func(_ context.Context, _ *config.OCIPrePullConfig, image *config.OCIPrePullImageConfig, archivePath string) (ociPrePullResult, error) {
		if err := os.WriteFile(archivePath, []byte("archive for "+image.Reference), 0o644); err != nil {
			return ociPrePullResult{}, err
		}
		return ociPrePullResult{
			ResolvedReference: resolved,
			Digest:            "sha256:" + strings.Repeat("a", 64),
		}, nil
	}
	return func() { pullOCIPrePullImage = old }
}

func readOCIPrePullCatalog(t *testing.T, path string) ociPrePullCatalog {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var catalog ociPrePullCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("unmarshal catalog: %v", err)
	}
	return catalog
}

func readStringFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertOCIPrePullImporterInstalled(t *testing.T, root string) {
	t.Helper()
	script := filepath.Join(root, "usr/lib/booty/oci-prepull-import")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("stat importer script: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("script mode = %v, want 0755", info.Mode().Perm())
	}

	unit := readStringFile(t, filepath.Join(root, "etc/systemd/system/booty-oci-prepull-import.service"))
	assertStringContains(t, unit, "ConditionPathExists=/var/lib/booty/prepulls/import-list.tsv")
	assertStringContains(t, unit, "Wants=containerd.service crio.service docker.service podman.service")
	assertStringContains(t, unit, "After=containerd.service crio.service docker.service podman.service")
	assertStringContains(t, unit, "Before=kubelet.service")

	link := filepath.Join(root, "etc/systemd/system/multi-user.target.wants/booty-oci-prepull-import.service")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read unit symlink: %v", err)
	}
	if target != "../booty-oci-prepull-import.service" {
		t.Fatalf("unit symlink = %q", target)
	}
}

func installFakeOCIRuntime(t *testing.T) (binDir string, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(t.TempDir(), "runtime.log")
	script := `#!/bin/sh
if [ "$1" != "-n" ] || [ "$3" != "images" ] || [ "$4" != "import" ] || [ ! -f "$5" ]; then
	echo "unexpected ctr invocation: $*" >&2
	exit 42
fi
printf '%s\n' "$*" >> "$BOOTY_FAKE_RUNTIME_LOG"
`
	if err := os.WriteFile(filepath.Join(binDir, "ctr"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ctr: %v", err)
	}
	return binDir, logPath
}

func writeHostOCIPrePullImportList(t *testing.T, root string, catalog *ociPrePullCatalog) string {
	t.Helper()
	hostCatalog := *catalog
	hostCatalog.Images = make([]ociPrePullCatalogImage, 0, len(catalog.Images))
	for _, image := range catalog.Images {
		image.Archive = hostOCIPrePullArchivePath(root, image.Archive)
		hostCatalog.Images = append(hostCatalog.Images, image)
	}
	listPath := filepath.Join(t.TempDir(), ociPrePullListName)
	if err := os.WriteFile(listPath, []byte(ociPrePullImportList(&hostCatalog)), 0o600); err != nil {
		t.Fatalf("write host import list: %v", err)
	}
	return listPath
}

func hostOCIPrePullArchivePath(root, archive string) string {
	return filepath.Join(root, strings.TrimPrefix(filepath.FromSlash(archive), string(filepath.Separator)))
}

func runOCIPrePullImporter(t *testing.T, root, fakeBin, listPath, stateDir, runtimeLog, namespace string) {
	t.Helper()
	scriptPath := filepath.Join(root, strings.TrimPrefix(ociPrePullImporterPath, string(filepath.Separator)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BOOTY_OCI_PREPULL_LIST="+listPath,
		"BOOTY_OCI_PREPULL_STATE_DIR="+stateDir,
		"BOOTY_OCI_IMPORT_NAMESPACE="+namespace,
		"BOOTY_FAKE_RUNTIME_LOG="+runtimeLog,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run importer: %s: %v", out, err)
	}
}

func assertStringContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Fatalf("%q does not contain %q", s, substr)
	}
}
