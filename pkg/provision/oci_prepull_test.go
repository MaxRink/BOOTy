//go:build linux

package provision

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if !strings.Contains(unit, "ConditionPathExists=/var/lib/booty/prepulls/import-list.tsv") {
		t.Fatalf("unit missing import-list condition: %s", unit)
	}
	if !strings.Contains(unit, "Before=kubelet.service") {
		t.Fatalf("unit missing kubelet ordering: %s", unit)
	}

	link := filepath.Join(root, "etc/systemd/system/multi-user.target.wants/booty-oci-prepull-import.service")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read unit symlink: %v", err)
	}
	if target != "../booty-oci-prepull-import.service" {
		t.Fatalf("unit symlink = %q", target)
	}
}
