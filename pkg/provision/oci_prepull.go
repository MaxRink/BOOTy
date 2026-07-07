//go:build linux

package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/telekom/BOOTy/pkg/config"
	imageutil "github.com/telekom/BOOTy/pkg/image"
)

const (
	ociPrePullCatalogAPIVersion = "booty.telekom.de/v1alpha1"
	ociPrePullCatalogKind       = "OCIPrePullCatalog"
	ociPrePullImporterPath      = "/usr/lib/booty/oci-prepull-import"
	ociPrePullServiceName       = "booty-oci-prepull-import.service"
	ociPrePullListName          = "import-list.tsv"
)

var pullOCIPrePullImage = pullOCIPrePullImageArchive

type ociPrePullCatalog struct {
	APIVersion      string                   `json:"apiVersion"`
	Kind            string                   `json:"kind"`
	CacheDir        string                   `json:"cacheDir"`
	ImportNamespace string                   `json:"importNamespace"`
	ArchiveFormat   string                   `json:"archiveFormat"`
	Images          []ociPrePullCatalogImage `json:"images"`
}

type ociPrePullCatalogImage struct {
	Reference         string `json:"reference"`
	ResolvedReference string `json:"resolvedReference"`
	Digest            string `json:"digest"`
	Archive           string `json:"archive"`
	SizeBytes         int64  `json:"sizeBytes"`
}

type ociPrePullResult struct {
	ResolvedReference string
	Digest            string
}

// PrepareOCIPrePulls caches OCI images into the target root and installs the
// first-boot importer service.
func (c *Configurator) PrepareOCIPrePulls(ctx context.Context, cfg *config.OCIPrePullConfig) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	withDefaults := cfg.WithDefaults()
	if len(withDefaults.Images) == 0 {
		slog.Info("OCI pre-pulls enabled with no images")
		return nil
	}

	archiveDir, archiveImageDir, err := c.ociPrePullDir(withDefaults.CacheDir, "archives")
	if err != nil {
		return fmt.Errorf("OCI pre-pull archive dir: %w", err)
	}
	catalog := ociPrePullCatalog{
		APIVersion:      ociPrePullCatalogAPIVersion,
		Kind:            ociPrePullCatalogKind,
		CacheDir:        withDefaults.CacheDir,
		ImportNamespace: withDefaults.ImportNamespace,
		ArchiveFormat:   "docker-archive",
	}
	for i := range withDefaults.Images {
		image, err := c.prepareOCIPrePullImage(ctx, &withDefaults, &withDefaults.Images[i], archiveDir, archiveImageDir)
		if err != nil {
			return fmt.Errorf("OCI pre-pull image %d: %w", i, err)
		}
		catalog.Images = append(catalog.Images, image)
	}
	if err := c.writeOCIPrePullCatalog(&withDefaults, &catalog); err != nil {
		return err
	}
	return c.installOCIPrePullImporter(&withDefaults)
}

func (c *Configurator) prepareOCIPrePullImage(
	ctx context.Context,
	cfg *config.OCIPrePullConfig,
	image *config.OCIPrePullImageConfig,
	archiveDir string,
	archiveImageDir string,
) (ociPrePullCatalogImage, error) {
	tmp, err := os.CreateTemp(archiveDir, ".oci-prepull-*.tar")
	if err != nil {
		return ociPrePullCatalogImage{}, fmt.Errorf("create archive temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath) //nolint:gosec // temp file is constrained to archiveDir
		return ociPrePullCatalogImage{}, fmt.Errorf("close archive temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }() //nolint:gosec // temp file is constrained to archiveDir

	result, err := pullOCIPrePullImage(ctx, cfg, image, tmpPath)
	if err != nil {
		return ociPrePullCatalogImage{}, err
	}

	archiveName := ociPrePullArchiveName(image.Reference, result.Digest)
	archivePath := filepath.Join(archiveDir, archiveName)
	if err := os.Rename(tmpPath, archivePath); err != nil {
		return ociPrePullCatalogImage{}, fmt.Errorf("install archive %s: %w", archiveName, err)
	}
	if err := os.Chmod(archivePath, 0o644); err != nil {
		return ociPrePullCatalogImage{}, fmt.Errorf("chmod archive %s: %w", archiveName, err)
	}
	if err := syncParentDir(archivePath); err != nil {
		return ociPrePullCatalogImage{}, fmt.Errorf("fsync archive directory %s: %w", archiveName, err)
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		return ociPrePullCatalogImage{}, fmt.Errorf("stat archive %s: %w", archiveName, err)
	}

	return ociPrePullCatalogImage{
		Reference:         strings.TrimSpace(image.Reference),
		ResolvedReference: result.ResolvedReference,
		Digest:            result.Digest,
		Archive:           path.Join(archiveImageDir, archiveName),
		SizeBytes:         info.Size(),
	}, nil
}

func pullOCIPrePullImageArchive(
	ctx context.Context,
	cfg *config.OCIPrePullConfig,
	image *config.OCIPrePullImageConfig,
	archivePath string,
) (ociPrePullResult, error) {
	rawRef := strings.TrimSpace(imageutil.TrimOCIScheme(image.Reference))
	parseOpts := []name.Option{name.StrictValidation}
	if cfg.AllowInsecure {
		parseOpts = append(parseOpts, name.Insecure)
	}
	ref, err := name.ParseReference(rawRef, parseOpts...)
	if err != nil {
		return ociPrePullResult{}, fmt.Errorf("parse OCI reference %q: %w", imageutil.RedactOCIRef(rawRef), err)
	}

	slog.Info("pulling OCI pre-pull image", "ref", imageutil.RedactOCIRef(rawRef))
	img, err := remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain), remote.WithContext(ctx))
	if err != nil {
		return ociPrePullResult{}, fmt.Errorf("pull OCI image %q: %w", imageutil.RedactOCIRef(rawRef), err)
	}
	digest, err := img.Digest()
	if err != nil {
		return ociPrePullResult{}, fmt.Errorf("resolve OCI image digest %q: %w", imageutil.RedactOCIRef(rawRef), err)
	}
	if err := tarball.WriteToFile(archivePath, ref, img); err != nil {
		return ociPrePullResult{}, fmt.Errorf("write OCI image archive %q: %w", imageutil.RedactOCIRef(rawRef), err)
	}
	resolved := ref.Context().Name() + "@" + digest.String()
	return ociPrePullResult{ResolvedReference: resolved, Digest: digest.String()}, nil
}

func (c *Configurator) writeOCIPrePullCatalog(cfg *config.OCIPrePullConfig, catalog *ociPrePullCatalog) error {
	catalogPath, err := c.ociPrePullTargetPath(path.Join(cfg.CacheDir, "catalog.json"))
	if err != nil {
		return fmt.Errorf("OCI pre-pull catalog path: %w", err)
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal OCI pre-pull catalog: %w", err)
	}
	if err := writeFileAtomic(catalogPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write OCI pre-pull catalog: %w", err)
	}

	listPath, err := c.ociPrePullTargetPath(path.Join(cfg.CacheDir, ociPrePullListName))
	if err != nil {
		return fmt.Errorf("OCI pre-pull import list path: %w", err)
	}
	if err := writeFileAtomic(listPath, []byte(ociPrePullImportList(catalog)), 0o644); err != nil {
		return fmt.Errorf("write OCI pre-pull import list: %w", err)
	}
	return nil
}

func ociPrePullImportList(catalog *ociPrePullCatalog) string {
	var b strings.Builder
	b.WriteString("# archive\treference\tdigest\n")
	for _, image := range catalog.Images {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", image.Archive, image.Reference, image.Digest)
	}
	return b.String()
}

func (c *Configurator) installOCIPrePullImporter(cfg *config.OCIPrePullConfig) error {
	scriptPath, err := c.ociPrePullTargetPath(ociPrePullImporterPath)
	if err != nil {
		return fmt.Errorf("OCI pre-pull importer path: %w", err)
	}
	if err := writeFileAtomic(scriptPath, []byte(ociPrePullImporterScript), 0o755); err != nil {
		return fmt.Errorf("write OCI pre-pull importer: %w", err)
	}

	unitPath, err := c.ociPrePullTargetPath(path.Join("/etc/systemd/system", ociPrePullServiceName))
	if err != nil {
		return fmt.Errorf("OCI pre-pull systemd unit path: %w", err)
	}
	if err := writeFileAtomic(unitPath, []byte(ociPrePullSystemdUnit(cfg)), 0o644); err != nil {
		return fmt.Errorf("write OCI pre-pull systemd unit: %w", err)
	}
	return c.enableOCIPrePullUnit()
}

func (c *Configurator) enableOCIPrePullUnit() error {
	wantsDir, err := c.ociPrePullTargetPath("/etc/systemd/system/multi-user.target.wants")
	if err != nil {
		return fmt.Errorf("OCI pre-pull wants dir: %w", err)
	}
	if err := os.MkdirAll(wantsDir, 0o755); err != nil {
		return fmt.Errorf("create OCI pre-pull wants dir: %w", err)
	}
	linkPath := filepath.Join(wantsDir, ociPrePullServiceName)
	if err := os.Remove(linkPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace OCI pre-pull unit symlink: %w", err)
	}
	if err := os.Symlink("../"+ociPrePullServiceName, linkPath); err != nil {
		return fmt.Errorf("enable OCI pre-pull unit: %w", err)
	}
	return nil
}

func ociPrePullSystemdUnit(cfg *config.OCIPrePullConfig) string {
	listPath := path.Join(cfg.CacheDir, ociPrePullListName)
	stateDir := path.Join(cfg.CacheDir, "imported")
	return fmt.Sprintf(`[Unit]
Description=Import BOOTy pre-pulled OCI image archives
ConditionPathExists=%s
After=containerd.service crio.service docker.service podman.service
Before=kubelet.service

[Service]
Type=oneshot
Environment=BOOTY_OCI_PREPULL_LIST=%s
Environment=BOOTY_OCI_PREPULL_STATE_DIR=%s
Environment=BOOTY_OCI_IMPORT_NAMESPACE=%s
ExecStart=%s
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`, listPath, listPath, stateDir, cfg.ImportNamespace, ociPrePullImporterPath)
}

func (c *Configurator) ociPrePullDir(cacheDir string, elem ...string) (hostPath, imagePath string, err error) {
	imagePath = path.Join(append([]string{cacheDir}, elem...)...)
	hostPath, err = c.ociPrePullTargetPath(imagePath)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		return "", "", fmt.Errorf("create target directory: %w", err)
	}
	return hostPath, imagePath, nil
}

func (c *Configurator) ociPrePullTargetPath(imagePath string) (string, error) {
	cleanPath := path.Clean("/" + strings.TrimSpace(imagePath))
	if cleanPath == "/" {
		return "", fmt.Errorf("target path must not be root")
	}
	hostPath := filepath.Join(c.rootDir, strings.TrimPrefix(filepath.FromSlash(cleanPath), string(filepath.Separator)))
	if err := ensureWithinRoot(c.rootDir, hostPath); err != nil {
		return "", err
	}
	if err := ensureTargetParentWithinRoot(c.rootDir, filepath.Dir(hostPath)); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return "", fmt.Errorf("create target parent directory: %w", err)
	}
	return hostPath, nil
}

func ociPrePullArchiveName(reference, digest string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(reference) + "\n" + strings.TrimSpace(digest)))
	return "image-" + hex.EncodeToString(h[:16]) + ".tar"
}

const ociPrePullImporterScript = `#!/bin/sh
set -eu

LIST="${BOOTY_OCI_PREPULL_LIST:-/var/lib/booty/oci-prepulls/import-list.tsv}"
STATE_DIR="${BOOTY_OCI_PREPULL_STATE_DIR:-/var/lib/booty/oci-prepulls/imported}"
IMPORT_NAMESPACE="${BOOTY_OCI_IMPORT_NAMESPACE:-k8s.io}"

log() {
	printf '%s\n' "$*" >&2
}

find_runtime() {
	for runtime in ctr nerdctl podman docker; do
		if command -v "$runtime" >/dev/null 2>&1; then
			printf '%s\n' "$runtime"
		fi
	done
}

import_archive() {
	runtime="$1"
	archive="$2"
	case "$runtime" in
		ctr)
			ctr -n "$IMPORT_NAMESPACE" images import "$archive"
			;;
		nerdctl)
			nerdctl -n "$IMPORT_NAMESPACE" load -i "$archive"
			;;
		podman)
			podman load -i "$archive"
			;;
		docker)
			docker load -i "$archive"
			;;
		*)
			log "unsupported OCI import runtime: $runtime"
			return 1
			;;
	esac
}

import_with_available_runtime() {
	archive="$1"
	if [ -n "${BOOTY_OCI_RUNTIME:-}" ]; then
		import_archive "$BOOTY_OCI_RUNTIME" "$archive"
		return
	fi
	runtimes="$(find_runtime || true)"
	if [ -z "$runtimes" ]; then
		log "no supported OCI import runtime found"
		return 1
	fi
	for runtime in $runtimes; do
		if import_archive "$runtime" "$archive"; then
			return 0
		fi
		log "OCI import with $runtime failed, trying next runtime if available"
	done
	return 1
}

if [ ! -f "$LIST" ]; then
	log "BOOTy OCI pre-pull list not found: $LIST"
	exit 0
fi

mkdir -p "$STATE_DIR"
while IFS='	' read -r archive reference digest; do
	case "$archive" in
		""|\#*)
			continue
			;;
	esac
	state="$STATE_DIR/${archive##*/}.done"
	if [ -f "$state" ]; then
		log "already imported $reference"
		continue
	fi
	if [ ! -f "$archive" ]; then
		log "archive missing for $reference: $archive"
		exit 1
	fi
	log "importing $reference"
	import_with_available_runtime "$archive"
	printf '%s\n' "$digest" > "$state"
done < "$LIST"
`
