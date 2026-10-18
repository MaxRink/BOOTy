package caprf

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/telekom/BOOTy/pkg/config"
)

// A deploy archive is a command payload that carries the whole per-job
// deploy tree, like the deploy.cpio of a CAPRF ISO: a gzip-compressed tar
// with deploy/vars and the directories deploy/machine-files,
// deploy/machine-commands and deploy/file-system. A standby agent that was
// booted for one job (a deprovision) needs them for the next job (the
// provision of another machine): the cloud-init seed and the machine files
// are files, not vars.
//
// BOOTy runs as root, so the archive is untrusted input:
//   - only directories and regular files; symlinks, hard links and every
//     other type fail the archive,
//   - every entry is below deploy/, without "..",
//   - files are created with O_EXCL|O_NOFOLLOW in a fresh staging directory,
//     with the permission bits of the entry (no setuid, setgid or sticky)
//     and the owner of BOOTy,
//   - at most maxArchiveEntries entries and created directories, paths of
//     at most 512 bytes and 16 levels, and maxDeployArchive bytes,
//   - deploy/vars is parsed and validated before /deploy changes.

// deployRoot is the deploy directory of the ramdisk; tests change it.
var deployRoot = "/deploy"

// deployArchiveDirs are the per-job directories that an archive replaces.
var deployArchiveDirs = []string{"machine-files", "machine-commands", "file-system"}

// Limits of a deploy archive; tests lower them.
var (
	maxDeployArchive  int64 = 64 << 20
	maxArchiveEntries       = 10000
)

func isDeployArchive(payload []byte) bool {
	return len(payload) > 2 && payload[0] == 0x1f && payload[1] == 0x8b
}

// errArchiveVars is the error of archive vars that do not parse or validate.
// It names no value: values can be secrets, and the error goes into the
// command ACK and the log.
var errArchiveVars = errors.New("deploy archive vars do not parse or validate")

// stageDeployArchive unpacks a deploy archive into a staging directory,
// validates its vars and returns the config of the vars. /deploy does not
// change yet: ApplyCommandFiles swaps the staged directories in once every
// check of the command passed, DiscardCommandFiles drops them. The settings
// that the running ramdisk uses (transport, agent, network, MODE, DRY_RUN,
// CAPRF_LEGACY_COMPAT and the bearer-token URLs) keep the values of the base
// vars: the agent keeps its token and network.
func (c *Client) stageDeployArchive(payload []byte) (cfg *config.MachineConfig, keys []string, err error) {
	ref, err := ParseVars(bytes.NewReader(c.vars))
	if err != nil {
		return nil, nil, fmt.Errorf("parse base vars: %w", err)
	}
	staging, err := os.MkdirTemp(filepath.Dir(deployRoot), ".booty-archive-")
	if err != nil {
		return nil, nil, fmt.Errorf("deploy archive staging: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(staging)
		}
	}()

	tree := filepath.Join(staging, "deploy")
	if err := unpackDeployArchive(payload, tree); err != nil {
		return nil, nil, err
	}
	cfg, keys, err = archiveVarsConfig(filepath.Join(tree, "vars"), c.vars, c.log)
	if err != nil {
		return nil, nil, err
	}
	if len(keys) == 0 {
		// Standby would run such a command with the base config, and the
		// files of the archive would not match it.
		return nil, nil, errors.New("deploy archive vars have no known key")
	}
	c.stagedDeploy = staging
	cfg.Transport, cfg.Agent, cfg.Network = ref.Transport, ref.Agent, ref.Network
	cfg.Mode, cfg.DryRun, cfg.LegacyCompat = ref.Mode, ref.DryRun, ref.LegacyCompat
	cfg.Provision.Inventory.URL = ref.Provision.Inventory.URL
	cfg.Provision.Firmware.URL = ref.Provision.Firmware.URL
	cfg.Health.ReportURL = ref.Health.ReportURL
	cfg.Telemetry.URL, cfg.Telemetry.MetricsURL, cfg.Telemetry.EventURL = ref.Telemetry.URL, ref.Telemetry.MetricsURL, ref.Telemetry.EventURL
	cfg.Provision.CrashArtifacts.PrepareURL = ref.Provision.CrashArtifacts.PrepareURL
	cfg.Provision.CrashArtifacts.UploadURL = ref.Provision.CrashArtifacts.UploadURL
	return cfg, keys, nil
}

// ApplyCommandFiles swaps the staged per-job directories of the deploy
// archive of the current command into /deploy and reports whether there
// was one.
func (c *Client) ApplyCommandFiles() (bool, error) {
	if c.stagedDeploy == "" {
		return false, nil
	}
	staging := c.stagedDeploy
	c.stagedDeploy = ""
	defer os.RemoveAll(staging) //nolint:errcheck // best-effort cleanup of the staging tree
	if err := swapDeployDirs(filepath.Join(staging, "deploy"), deployRoot, filepath.Join(staging, "old")); err != nil {
		return false, err
	}
	return true, nil
}

// DiscardCommandFiles drops the staged directories of a deploy archive whose
// command does not run.
func (c *Client) DiscardCommandFiles() {
	if c.stagedDeploy != "" {
		_ = os.RemoveAll(c.stagedDeploy)
		c.stagedDeploy = ""
	}
}

// archiveVarsConfig reads and validates deploy/vars of the staging tree.
// Unknown keys are logged and ignored, like the vars of an ISO (CAPRF
// renders the archive with the same builder); errors name keys only.
// CAPRF_LEGACY_COMPAT comes from the base vars, so the presets that it sets
// match the running ramdisk.
func archiveVarsConfig(varsPath string, base []byte, log *slog.Logger) (cfg *config.MachineConfig, keys []string, err error) {
	vars, err := os.ReadFile(varsPath) //nolint:gosec // path in our staging directory
	if err != nil {
		return nil, nil, errors.New("deploy archive has no deploy/vars")
	}
	all, err := payloadKeys(vars)
	if err != nil {
		return nil, nil, errArchiveVars
	}
	var unknown []string
	for _, k := range all {
		if knownVarKey(k) {
			keys = append(keys, k)
		} else {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 && log != nil {
		log.Warn("deploy archive vars have unknown keys, ignored", "keys", strings.Join(unknown, ","))
	}
	cfg, err = ParseVars(bytes.NewReader(withVarLines(vars, base, legacyCompatKey)))
	if err != nil {
		return nil, nil, errArchiveVars
	}
	return cfg, keys, nil
}

// legacyCompatKey selects the CAPRF legacy presets.
const legacyCompatKey = "CAPRF_LEGACY_COMPAT"

// withVarLines returns vars without its key lines, plus the key lines of
// base.
func withVarLines(vars, base []byte, key string) []byte {
	var out bytes.Buffer
	keep := func(src []byte, want bool) {
		for line := range strings.Lines(string(src)) {
			k, _, ok := parseVarLine(line)
			if (ok && k == key) == want {
				out.WriteString(strings.TrimRight(line, "\n"))
				out.WriteByte('\n')
			}
		}
	}
	keep(vars, false)
	keep(base, true)
	return out.Bytes()
}

// renameFunc is os.Rename; tests make it fail.
var renameFunc = os.Rename

// swapDeployDirs moves the per-job directories of root to old and the
// ones of tree into root. A failure moves the old ones back.
func swapDeployDirs(tree, root, old string) error {
	if err := os.Mkdir(old, 0o700); err != nil {
		return fmt.Errorf("deploy archive: %w", err)
	}
	var moved []string
	restore := func() {
		for _, dir := range moved {
			_ = os.RemoveAll(filepath.Join(root, dir))
			_ = renameFunc(filepath.Join(old, dir), filepath.Join(root, dir))
		}
	}
	for _, dir := range deployArchiveDirs {
		if err := renameFunc(filepath.Join(root, dir), filepath.Join(old, dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
			restore()
			return fmt.Errorf("replace %s: %w", dir, err)
		}
		moved = append(moved, dir)
	}
	for _, dir := range deployArchiveDirs {
		if err := renameFunc(filepath.Join(tree, dir), filepath.Join(root, dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
			restore()
			return fmt.Errorf("replace %s: %w", dir, err)
		}
	}
	return nil
}

// unpackDeployArchive writes the deploy/ entries of a tar.gz into dst, a
// directory that must not exist yet.
func unpackDeployArchive(payload []byte, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("deploy archive: %w", err)
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return fmt.Errorf("deploy archive: %w", err)
	}
	u := &unpacker{dst: dst, budget: maxDeployArchive}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("deploy archive: %w", err)
		}
		if err := u.entry(tr, hdr); err != nil {
			return err
		}
	}
}

// unpacker writes the entries of one archive. nodes counts the entries and
// every directory it creates, also the parents that an entry implies, so a
// few long paths can not create many directories.
type unpacker struct {
	dst    string
	budget int64
	nodes  int
}

func (u *unpacker) entry(tr *tar.Reader, hdr *tar.Header) error {
	if err := u.count(); err != nil {
		return err
	}
	rel, err := archivePath(hdr.Name)
	if err != nil || rel == "" {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		err = u.mkdirs(rel)
	case tar.TypeReg:
		if slices.Contains(deployArchiveDirs, rel) {
			return fmt.Errorf("deploy archive entry %q must be a directory", hdr.Name)
		}
		if err = u.mkdirs(path.Dir(rel)); err == nil {
			u.budget, err = writeArchiveFile(tr, filepath.Join(u.dst, filepath.FromSlash(rel)), hdr.Mode, u.budget)
		}
	default:
		// Symlinks and hard links would let a later entry write through
		// them, outside the staging directory.
		return fmt.Errorf("deploy archive entry %q has the unsupported type %q: only directories and regular files", hdr.Name, hdr.Typeflag)
	}
	if err != nil {
		return fmt.Errorf("deploy archive entry %q: %w", hdr.Name, err)
	}
	return nil
}

func (u *unpacker) count() error {
	u.nodes++
	if u.nodes > maxArchiveEntries {
		return fmt.Errorf("deploy archive has more than %d entries and directories", maxArchiveEntries)
	}
	return nil
}

// mkdirs creates the directories of rel (slash separated, below dst) one by
// one; each new one counts as a node. An existing non-directory fails.
func (u *unpacker) mkdirs(rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	cur := u.dst
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
			continue
		case err == nil:
			return fmt.Errorf("%s is not a directory", part)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("check directory: %w", err)
		}
		if err := u.count(); err != nil {
			return err
		}
		if err := os.Mkdir(cur, 0o755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}
	return nil
}

// Path limits of an archive entry: bytes of the whole name, and levels
// below deploy/.
const (
	maxArchivePathLen   = 512
	maxArchivePathDepth = 16
)

// archivePath returns the path of an entry below deploy/, or "" for the
// deploy directory itself. Paths outside deploy/, with ".." or absolute
// paths fail.
func archivePath(name string) (string, error) {
	if len(name) > maxArchivePathLen {
		return "", fmt.Errorf("deploy archive entry path is longer than %d bytes", maxArchivePathLen)
	}
	// Any "..", also inside a file name: simple to check, and the form of
	// the check that static analysis (CodeQL zip slip) knows.
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("deploy archive entry %q contains \"..\"", name)
	}
	trimmed := strings.TrimPrefix(name, "./")
	clean := path.Clean(trimmed)
	if clean == "deploy" {
		return "", nil
	}
	rel, ok := strings.CutPrefix(clean, "deploy/")
	if !ok || path.IsAbs(trimmed) || rel == "" || rel == "." {
		return "", fmt.Errorf("deploy archive entry %q is outside deploy/", name)
	}
	if strings.Count(rel, "/")+1 > maxArchivePathDepth {
		return "", fmt.Errorf("deploy archive entry %q is more than %d levels below deploy/", name, maxArchivePathDepth)
	}
	return rel, nil
}

// writeArchiveFile creates a new file: O_EXCL fails on any existing name,
// O_NOFOLLOW on a symlink. The mode is the permission bits of the archive
// entry, so a 0600 secret (SSH host key, kubeconfig, join token) stays
// private; setuid, setgid and sticky are dropped. It returns the byte
// budget that is left.
func writeArchiveFile(r io.Reader, target string, mode, budget int64) (int64, error) {
	perm := os.FileMode(mode) & os.ModePerm                                                   //nolint:gosec // only the permission bits are kept
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, perm) //nolint:gosec // path checked by archivePath
	if err != nil {
		return budget, fmt.Errorf("create file: %w", err)
	}
	n, err := io.Copy(f, io.LimitReader(r, budget+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return budget, err
	}
	if n > budget {
		return 0, fmt.Errorf("deploy archive is larger than %d bytes", maxDeployArchive)
	}
	// The mode is set explicitly: OpenFile applies the umask.
	if err := os.Chmod(target, perm); err != nil {
		return budget, fmt.Errorf("set mode: %w", err)
	}
	return budget - n, nil
}
