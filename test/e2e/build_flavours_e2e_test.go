//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Size budgets in MiB, about 5% above main (default 37.3, gobgp 32.7,
// slim 23.6, micro 10.3 MiB). Every MB is up to 0.5 s of boot time on
// slow BMC virtual media; see docs/size-optimization.md.
const (
	maxMicroInitramfsMB   = 11.0
	maxSlimInitramfsMB    = 25.0
	maxGoBGPInitramfsMB   = 34.5
	maxDefaultInitramfsMB = 39.5
)

func dockerAvailable(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker not available")
	}
}

// buildTarget runs docker buildx build for the given Dockerfile target and
// extracts the output to dest. Returns the path to the initramfs archive.
func buildTarget(t *testing.T, target, dest string) string {
	t.Helper()
	dockerfile := filepath.Join(findRepoRoot(t), "initrd.Dockerfile")
	repoRoot := findRepoRoot(t)

	if !dockerBuildxAvailable() {
		return buildTargetWithDockerBuild(t, target, dest, dockerfile, repoRoot)
	}

	args := []string{"buildx", "build", "--platform", "linux/amd64"}
	if target != "" {
		args = append(args, "--target", target)
	}
	args = append(args, "--output", "type=local,dest="+dest, "-f", dockerfile, repoRoot)

	name := target
	if name == "" {
		name = "default"
	}
	runDockerBuild(t, "docker buildx build --target "+name, func() *exec.Cmd {
		cmd := exec.Command("docker", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	})

	// gobgp, default, and slim use zstd compression; micro uses gzip.
	out := filepath.Join(dest, "initramfs.cpio.gz")
	if target == "gobgp" || target == "" || target == "slim" {
		out = filepath.Join(dest, "initramfs.cpio.zst")
	}
	if target == "iso" {
		out = filepath.Join(dest, "booty.iso")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected output %s not found: %v", out, err)
	}
	return out
}

func dockerBuildxAvailable() bool {
	cmd := exec.Command("docker", "buildx", "version")
	return cmd.Run() == nil
}

func buildTargetWithDockerBuild(t *testing.T, target, dest, dockerfile, repoRoot string) string {
	t.Helper()
	name := target
	if name == "" {
		name = "default"
	}

	args := []string{"build", "--platform", "linux/amd64"}
	if target != "" {
		args = append(args, "--target", target)
	}
	args = append(args, "--output", "type=local,dest="+dest, "-f", dockerfile, repoRoot)

	runDockerBuild(t, "docker build --target "+name, func() *exec.Cmd {
		cmd := exec.Command("docker", args...)
		cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd
	})

	out := filepath.Join(dest, targetArtifactName(target))
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected output %s not found: %v", out, err)
	}
	return out
}

func runDockerBuild(t *testing.T, description string, newCommand func() *exec.Cmd) {
	t.Helper()

	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := newCommand().Run(); err != nil {
			lastErr = err
			if attempt < attempts {
				t.Logf("%s failed on attempt %d/%d: %v; retrying", description, attempt, attempts, err)
				time.Sleep(5 * time.Second)
				continue
			}
			break
		}
		return
	}
	t.Fatalf("%s failed after %d attempts: %v", description, attempts, lastErr)
}

func targetArtifactName(target string) string {
	switch target {
	case "micro":
		return "initramfs.cpio.gz"
	case "iso":
		return "booty.iso"
	case "gobgp-iso":
		return "booty-gobgp.iso"
	default:
		return "initramfs.cpio.zst"
	}
}

func TestBuildTargetWithDockerBuildUsesBuildKitLocalOutput(t *testing.T) {
	fakeBin := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "docker-args.log")
	dockerPath := filepath.Join(fakeBin, "docker")
	script := `#!/bin/sh
set -eu
printf '%s\n' "$@" > "$BOOTY_DOCKER_ARGS_LOG"
if [ "$1" != "build" ]; then
  echo "unexpected docker command: $1" >&2
  exit 42
fi
shift
dest=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      shift
      dest="${1#type=local,dest=}"
      ;;
  esac
  shift || true
done
if [ -z "$dest" ]; then
  echo "missing build output destination" >&2
  exit 43
fi
mkdir -p "$dest"
printf fake > "$dest/initramfs.cpio.zst"
printf fake > "$dest/initramfs.cpio.gz"
printf fake > "$dest/booty.iso"
printf fake > "$dest/booty-gobgp.iso"
`
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BOOTY_DOCKER_ARGS_LOG", argsLog)

	tmp := t.TempDir()
	dockerfile := filepath.Join(tmp, "Dockerfile")
	repoRoot := filepath.Join(tmp, "repo")
	dest := filepath.Join(tmp, "out")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("create repo root: %v", err)
	}
	if err := os.WriteFile(dockerfile, []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write dockerfile: %v", err)
	}

	out := buildTargetWithDockerBuild(t, "gobgp-iso", dest, dockerfile, repoRoot)
	if filepath.Base(out) != "booty-gobgp.iso" {
		t.Fatalf("unexpected output path: %s", out)
	}

	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("read fake docker args: %v", err)
	}
	argsText := string(args)
	for _, want := range []string{
		"build\n",
		"--platform\nlinux/amd64\n",
		"--target\ngobgp-iso\n",
		"--output\ntype=local,dest=" + dest + "\n",
	} {
		if !strings.Contains(argsText, want) {
			t.Fatalf("docker args missing %q in:\n%s", want, argsText)
		}
	}
	argLines := strings.Split(strings.TrimSpace(argsText), "\n")
	for _, forbidden := range []string{"create", "cp"} {
		for _, arg := range argLines {
			if arg == forbidden {
				t.Fatalf("fallback must not use docker %s for scratch targets; args:\n%s", forbidden, argsText)
			}
		}
	}
}

func TestOCIPushInitramfsBuildsIsolatedFlavorArtifact(t *testing.T) {
	makefilePath := filepath.Join(findRepoRoot(t), "Makefile")
	data, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	makefile := string(data)

	for _, want := range []string{
		"override OCI_INITRAMFS_DIR := dist/oci/$(OCI_FLAVOR)-$(OCI_ARCH)",
		"override INITRAMFS_PATH := $(OCI_INITRAMFS_DIR)/$(OCI_INITRAMFS_BASENAME)",
		"override INITRAMFS_MEDIA_TYPE :=",
		"export VERSION DOCKERTAG REPOSITORY TARGET OCI_FLAVOR OCI_ARCH OCI_INITRAMFS_DIR INITRAMFS_PATH INITRAMFS_MEDIA_TYPE",
		"check-oci-vars: check-docker-vars check-build-vars",
		`case "$$OCI_FLAVOR" in default|slim|micro|gobgp)`,
		`case "$$OCI_ARCH" in amd64|arm64)`,
	} {
		if !strings.Contains(makefile, want) {
			t.Fatalf("Makefile missing OCI isolation contract %q", want)
		}
	}

	recipe := makeTargetRecipe(t, makefile, "oci-push-initramfs")
	for _, want := range []string{
		`oci-push-initramfs: check-oci-vars`,
		`target_arg="--target=$$OCI_FLAVOR"`,
		`docker buildx build --platform "linux/$$OCI_ARCH" $$target_arg`,
		`--output "type=local,dest=$$OCI_INITRAMFS_DIR"`,
		`expected %s/%s artifact %s was not produced`,
	} {
		if !strings.Contains(recipe, want) {
			t.Fatalf("oci-push-initramfs recipe missing %q in:\n%s", want, recipe)
		}
	}
	if strings.Contains(recipe, "build the requested initramfs flavor first") {
		t.Fatalf("oci-push-initramfs must build the requested flavor instead of trusting a prebuilt root artifact:\n%s", recipe)
	}
}

func makeTargetRecipe(t *testing.T, makefile, target string) string {
	t.Helper()

	start := strings.Index(makefile, "\n"+target+":")
	if start == -1 {
		if strings.HasPrefix(makefile, target+":") {
			start = 0
		} else {
			t.Fatalf("target %q not found", target)
		}
	}
	next := strings.Index(makefile[start+1:], "\n\n")
	if next == -1 {
		return makefile[start:]
	}
	return makefile[start : start+1+next]
}

// findRepoRoot walks up from the test file to find the repo root (contains go.mod).
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (go.mod)")
		}
		dir = parent
	}
}

// listCPIOContents lists files in a cpio archive compressed with gzip or zstd.
func listCPIOContents(t *testing.T, cpioGzPath string) map[string]bool {
	t.Helper()

	// Choose decompressor based on file extension.
	var decompCmd *exec.Cmd
	if strings.HasSuffix(cpioGzPath, ".zst") {
		decompCmd = exec.Command("zstd", "-dc", cpioGzPath)
	} else {
		decompCmd = exec.Command("gzip", "-dc", cpioGzPath)
	}

	cpioCmd := exec.Command("cpio", "-t")
	cpioCmd.Stderr = nil // suppress cpio block count

	var err error
	cpioCmd.Stdin, err = decompCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe setup: %v", err)
	}

	if err := decompCmd.Start(); err != nil {
		t.Fatalf("decompressor start: %v", err)
	}

	out, err := cpioCmd.Output()
	if err != nil {
		t.Fatalf("cpio: %v", err)
	}

	if err := decompCmd.Wait(); err != nil {
		t.Fatalf("decompressor: %v", err)
	}

	files := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "." {
			continue
		}
		// Normalize: strip leading "./"
		line = strings.TrimPrefix(line, "./")
		if line != "" {
			files[line] = true
		}
	}
	return files
}

type cpioEntry struct {
	mode string
}

func listCPIOEntries(t *testing.T, cpioGzPath string) map[string]cpioEntry {
	t.Helper()

	var decompCmd *exec.Cmd
	if strings.HasSuffix(cpioGzPath, ".zst") {
		decompCmd = exec.Command("zstd", "-dc", cpioGzPath)
	} else {
		decompCmd = exec.Command("gzip", "-dc", cpioGzPath)
	}

	cpioCmd := exec.Command("cpio", "-tv")
	cpioCmd.Stderr = nil

	var err error
	cpioCmd.Stdin, err = decompCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe setup: %v", err)
	}
	if err := decompCmd.Start(); err != nil {
		t.Fatalf("decompressor start: %v", err)
	}
	out, err := cpioCmd.Output()
	if err != nil {
		t.Fatalf("cpio verbose listing: %v", err)
	}
	if err := decompCmd.Wait(); err != nil {
		t.Fatalf("decompressor: %v", err)
	}

	entries := make(map[string]cpioEntry)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		path := strings.TrimPrefix(fields[len(fields)-1], "./")
		if path == "" || path == "." {
			continue
		}
		entries[path] = cpioEntry{mode: fields[0]}
	}
	return entries
}

// assertContains checks that the file set contains the given path.
func assertContains(t *testing.T, files map[string]bool, path, desc string) {
	t.Helper()
	if !files[path] {
		t.Errorf("expected %s (%s) in initramfs, not found", path, desc)
	}
}

// assertNotContains checks that the file set does NOT contain the given path.
func assertNotContains(t *testing.T, files map[string]bool, path, desc string) {
	t.Helper()
	if files[path] {
		t.Errorf("did NOT expect %s (%s) in initramfs, but found it", path, desc)
	}
}

func assertKernelModuleContains(t *testing.T, files map[string]bool, module string) {
	t.Helper()
	prefix := "modules/" + module + ".ko"
	for path := range files {
		if strings.HasPrefix(path, prefix) {
			return
		}
	}
	t.Errorf("expected kernel module %s under modules/ in initramfs", module)
}

func assertRequiredInitDeviceNodes(t *testing.T, files map[string]bool) {
	t.Helper()
	for _, node := range []string{"dev/console", "dev/null", "dev/ttyS0"} {
		assertContains(t, files, node, "required initramfs device node")
	}
}

func assertRequiredInitDeviceNodeModes(t *testing.T, entries map[string]cpioEntry) {
	t.Helper()
	for path, wantMode := range map[string]string{
		"dev/console": "crw-------",
		"dev/null":    "crw-rw-rw-",
		"dev/ttyS0":   "crw-------",
	} {
		entry, ok := entries[path]
		if !ok {
			t.Errorf("expected %s metadata in initramfs, not found", path)
			continue
		}
		if entry.mode != wantMode {
			t.Errorf("%s mode = %q, want %q", path, entry.mode, wantMode)
		}
	}
}

// assertFileSize checks that the compressed initramfs artifact is within the expected size range.
func assertFileSize(t *testing.T, path string, minMB, maxMB float64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sizeMB := float64(info.Size()) / (1024 * 1024)
	if sizeMB < minMB || sizeMB > maxMB {
		t.Errorf("initramfs size %.1f MB outside expected range [%.1f, %.1f] MB", sizeMB, minMB, maxMB)
	}
}

// ── Slim build tests ─────────────────────────────────────────────────────

func TestSlimBuildSucceedsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)

	info, err := os.Stat(cpioGz)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("slim initramfs is empty")
	}
	t.Logf("Slim initramfs size: %.1f MB", float64(info.Size())/(1024*1024))
}

func TestSlimContainsBusyboxE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "bin/busybox", "busybox binary")
	assertContains(t, files, "bin/sh", "shell symlink")
}

func TestSlimContainsBootyInitE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "init", "BOOTy init binary")
}

func TestSlimContainsNetworkToolsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "bin/ip", "iproute2 ip command")
	assertContains(t, files, "bin/ethtool", "ethtool")
	// BOOTy fetches with net/http; busybox wget does HTTP and HTTPS in the shell.
	assertNotContains(t, files, "bin/curl", "curl")
	assertContains(t, files, "bin/wget", "busybox wget")
	assertContains(t, files, "etc/ssl/certs/ca-certificates.crt", "CA certificates")
}

func TestSlimContainsDiskToolsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "sbin/mdadm", "mdadm RAID")
	assertContains(t, files, "bin/wipefs", "wipefs")
	assertContains(t, files, "bin/sgdisk", "sgdisk GPT")
	assertContains(t, files, "bin/sfdisk", "sfdisk partitioner")
	assertContains(t, files, "bin/partprobe", "partprobe")
	assertContains(t, files, "bin/partx", "partx online partition updater")
	assertContains(t, files, "bin/growpart", "growpart")
	assertContains(t, files, "bin/lsblk", "util-linux lsblk")
	assertContains(t, files, "sbin/e2fsck", "e2fsck")
	assertContains(t, files, "sbin/resize2fs", "resize2fs")
	assertContains(t, files, "sbin/blkid", "util-linux blkid")
	assertContains(t, files, "bin/losetup", "util-linux losetup")
	assertContains(t, files, "sbin/mkfs.ext4", "mkfs.ext4")
	assertContains(t, files, "sbin/mkfs.vfat", "mkfs.vfat")
	assertContains(t, files, "sbin/mkfs.xfs", "mkfs.xfs")
	assertContains(t, files, "sbin/xfs_growfs", "xfs_growfs")
	assertContains(t, files, "sbin/xfs_repair", "xfs_repair")
	assertContains(t, files, "bin/btrfs", "btrfs")
	assertNotContains(t, files, "bin/blkid", "BusyBox blkid applet shadowing util-linux blkid")
	assertNotContains(t, files, "bin/mkfs.vfat", "BusyBox mkfs.vfat applet shadowing dosfstools")
	assertNotContains(t, files, "bin/mkfs.fat", "BusyBox mkfs.fat applet shadowing dosfstools")
	assertNotContains(t, files, "bin/mkdosfs", "BusyBox mkdosfs applet shadowing dosfstools")
}

func TestSlimExcludesFRRE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)
	files := listCPIOContents(t, cpioGz)

	assertNotContains(t, files, "sbin/bgpd", "FRR bgpd")
	assertNotContains(t, files, "sbin/zebra", "FRR zebra")
	assertNotContains(t, files, "sbin/bfdd", "FRR bfdd")
	assertNotContains(t, files, "bin/vtysh", "FRR vtysh")
	assertNotContains(t, files, "sbin/watchfrr", "FRR watchfrr")
}

func TestSlimExcludesLVME2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)
	files := listCPIOContents(t, cpioGz)

	assertNotContains(t, files, "sbin/lvm", "LVM tooling")
	assertNotContains(t, files, "bin/gpgv", "GPG verifier")
	assertNotContains(t, files, "bin/qemu-img", "qemu-img")
}

func TestSlimSizeSmallerThanDefaultE2E(t *testing.T) {
	dockerAvailable(t)

	// Build both default and slim, compare sizes
	defaultDest := t.TempDir()
	slimDest := t.TempDir()

	defaultCpio := buildTarget(t, "", defaultDest)
	slimCpio := buildTarget(t, "slim", slimDest)

	defaultInfo, _ := os.Stat(defaultCpio)
	slimInfo, _ := os.Stat(slimCpio)

	t.Logf("Default size: %.1f MB, Slim size: %.1f MB",
		float64(defaultInfo.Size())/(1024*1024),
		float64(slimInfo.Size())/(1024*1024))

	if slimInfo.Size() >= defaultInfo.Size() {
		t.Errorf("slim (%d bytes) should be smaller than default (%d bytes)",
			slimInfo.Size(), defaultInfo.Size())
	}
}

// ── Micro build tests ────────────────────────────────────────────────────

func TestMicroBuildSucceedsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "micro", dest)

	info, err := os.Stat(cpioGz)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("micro initramfs is empty")
	}
	t.Logf("Micro initramfs size: %.1f MB", float64(info.Size())/(1024*1024))
}

func TestMicroContainsOnlyInitE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "micro", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "init", "BOOTy init binary")
	assertContains(t, files, "etc/ssl/certs/ca-certificates.crt", "CA certificates")

	// Should NOT contain any external tools
	assertNotContains(t, files, "bin/busybox", "busybox")
	assertNotContains(t, files, "bin/sh", "shell")
	assertNotContains(t, files, "sbin/bgpd", "FRR bgpd")
	assertNotContains(t, files, "sbin/lvm", "LVM")
	assertNotContains(t, files, "bin/sfdisk", "sfdisk")
	assertNotContains(t, files, "bin/ip", "iproute2")
	assertNotContains(t, files, "bin/curl", "curl")
}

func TestMicroHasMinimalDirsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "micro", dest)
	files := listCPIOContents(t, cpioGz)

	// Verify minimal directory structure for Linux init
	for _, dir := range []string{"dev", "proc", "sys", "tmp", "etc"} {
		assertContains(t, files, dir, "required directory")
	}
	assertRequiredInitDeviceNodes(t, files)
}

func TestMicroSizeSmallerThanSlimE2E(t *testing.T) {
	dockerAvailable(t)

	slimDest := t.TempDir()
	microDest := t.TempDir()

	slimCpio := buildTarget(t, "slim", slimDest)
	microCpio := buildTarget(t, "micro", microDest)

	slimInfo, _ := os.Stat(slimCpio)
	microInfo, _ := os.Stat(microCpio)

	t.Logf("Slim size: %.1f MB, Micro size: %.1f MB",
		float64(slimInfo.Size())/(1024*1024),
		float64(microInfo.Size())/(1024*1024))

	if microInfo.Size() >= slimInfo.Size() {
		t.Errorf("micro (%d bytes) should be smaller than slim (%d bytes)",
			microInfo.Size(), slimInfo.Size())
	}
}

func TestMicroIsPureGoE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "micro", dest)

	// Micro should be very small — the pure-Go binary + certs + dirs only
	// Expected: under 20 MB compressed (compared to ~50+ MB for default)
	assertFileSize(t, cpioGz, 0.1, 20.0)
}

// ── Cross-flavour comparison test ────────────────────────────────────────

func TestBuildFlavourSizeOrderE2E(t *testing.T) {
	dockerAvailable(t)

	defaultDest := t.TempDir()
	slimDest := t.TempDir()
	microDest := t.TempDir()

	defaultCpio := buildTarget(t, "", defaultDest)
	slimCpio := buildTarget(t, "slim", slimDest)
	microCpio := buildTarget(t, "micro", microDest)

	defaultInfo, _ := os.Stat(defaultCpio)
	slimInfo, _ := os.Stat(slimCpio)
	microInfo, _ := os.Stat(microCpio)

	t.Logf("Size order: micro (%.1f MB) < slim (%.1f MB) < default (%.1f MB)",
		float64(microInfo.Size())/(1024*1024),
		float64(slimInfo.Size())/(1024*1024),
		float64(defaultInfo.Size())/(1024*1024))

	if microInfo.Size() >= slimInfo.Size() {
		t.Error("micro should be smaller than slim")
	}
	if slimInfo.Size() >= defaultInfo.Size() {
		t.Error("slim should be smaller than default")
	}
}

// ── Default build composition tests ──────────────────────────────────────

func TestDefaultBuildSucceedsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)

	info, err := os.Stat(cpioGz)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("default initramfs is empty")
	}
	t.Logf("Default initramfs size: %.1f MB", float64(info.Size())/(1024*1024))
}

func TestBuildFlavoursContainInitDeviceNodesE2E(t *testing.T) {
	dockerAvailable(t)
	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "default"},
		{name: "slim", target: "slim"},
		{name: "micro", target: "micro"},
		{name: "gobgp", target: "gobgp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			cpioGz := buildTarget(t, tc.target, dest)
			files := listCPIOContents(t, cpioGz)
			entries := listCPIOEntries(t, cpioGz)

			assertRequiredInitDeviceNodes(t, files)
			assertRequiredInitDeviceNodeModes(t, entries)
		})
	}
}

func TestDefaultContainsFRRE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "sbin/bgpd", "FRR bgpd")
	assertContains(t, files, "sbin/zebra", "FRR zebra")
	assertContains(t, files, "sbin/bfdd", "FRR bfdd")
	assertContains(t, files, "bin/vtysh", "FRR vtysh")
	assertContains(t, files, "sbin/watchfrr", "FRR watchfrr")
}

func TestDefaultContainsLVME2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "sbin/lvm", "LVM tooling")
	for _, cmd := range []string{"pvcreate", "vgs", "vgcreate", "lvcreate"} {
		assertContains(t, files, "sbin/"+cmd, "LVM "+cmd)
	}
	assertContains(t, files, "bin/sfdisk", "sfdisk partitioner")
}

func TestDefaultContainsDiskToolsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "bin/wipefs", "wipefs")
	assertContains(t, files, "sbin/mdadm", "mdadm RAID")
	assertContains(t, files, "sbin/resize2fs", "resize2fs")
	assertContains(t, files, "sbin/e2fsck", "e2fsck")
	assertContains(t, files, "sbin/blkid", "util-linux blkid")
	assertContains(t, files, "bin/losetup", "util-linux losetup")
	assertContains(t, files, "bin/parted", "parted")
	assertContains(t, files, "bin/sgdisk", "sgdisk GPT")
	assertContains(t, files, "bin/partprobe", "partprobe")
	assertContains(t, files, "bin/partx", "partx online partition updater")
	assertContains(t, files, "bin/lsblk", "util-linux lsblk")
	// BOOTy converts qcow2 images in process.
	assertNotContains(t, files, "bin/qemu-img", "qemu-img")
	assertContains(t, files, "bin/efibootmgr", "efibootmgr")
	assertNotContains(t, files, "bin/blkid", "BusyBox blkid applet shadowing util-linux blkid")
}

func TestFullFlavorsUseDosfstoolsMkfsVfatE2E(t *testing.T) {
	dockerAvailable(t)
	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "default"},
		{name: "gobgp", target: "gobgp"},
		{name: "slim", target: "slim"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			cpioGz := buildTarget(t, tc.target, dest)
			files := listCPIOContents(t, cpioGz)

			assertContains(t, files, "sbin/mkfs.vfat", "dosfstools mkfs.vfat")
			assertNotContains(t, files, "bin/mkfs.vfat", "BusyBox mkfs.vfat applet shadowing dosfstools")
			assertNotContains(t, files, "bin/mkfs.fat", "BusyBox mkfs.fat applet shadowing dosfstools")
			assertNotContains(t, files, "bin/mkdosfs", "BusyBox mkdosfs applet shadowing dosfstools")
		})
	}
}

func TestFullFlavorsContainEFIFallbackLoaderE2E(t *testing.T) {
	dockerAvailable(t)
	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "default"},
		{name: "gobgp", target: "gobgp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			cpioGz := buildTarget(t, tc.target, dest)
			files := listCPIOContents(t, cpioGz)

			assertContains(t, files, "usr/lib/booty/efi/BOOTX64.EFI", "bundled removable EFI fallback loader")
		})
	}
}

func TestDefaultContainsNetworkAndSSHE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "bin/ip", "iproute2 ip")
	assertContains(t, files, "bin/bridge", "iproute2 bridge")
	assertContains(t, files, "bin/ethtool", "ethtool")
	// BOOTy fetches with net/http; busybox wget does HTTP and HTTPS in the shell.
	assertNotContains(t, files, "bin/curl", "curl")
	assertContains(t, files, "bin/wget", "busybox wget")
	assertContains(t, files, "bin/dropbear", "dropbear SSH")
	assertContains(t, files, "bin/dropbearkey", "dropbearkey")
	// LLDP diagnosis is "booty lldp"; lldpd and lldpcli are not shipped.
	assertNotContains(t, files, "bin/lldpcli", "LLDP client")
	assertNotContains(t, files, "sbin/lldpd", "LLDP daemon")
	assertContains(t, files, "etc/ssl/certs/ca-certificates.crt", "CA certificates")
	assertContains(t, files, "bin/gpgv", "GPG signature verifier")
}

func TestDefaultContainsKernelModulesE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	// At least one .ko or .ko.zst file should exist under lib/modules/
	hasModules := false
	for path := range files {
		if strings.HasPrefix(path, "modules/") &&
			(strings.HasSuffix(path, ".ko") || strings.HasSuffix(path, ".ko.zst") ||
				strings.HasSuffix(path, ".ko.xz") || strings.HasSuffix(path, ".ko.gz")) {
			hasModules = true
			break
		}
	}
	if !hasModules {
		t.Error("no kernel modules found under modules/ in default initramfs")
	}
	assertKernelModuleContains(t, files, "nls_ascii")
}

// ── GoBGP build composition tests ────────────────────────────────────────

func TestGoBGPBuildSucceedsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)

	info, err := os.Stat(cpioGz)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("gobgp initramfs is empty")
	}
	t.Logf("GoBGP initramfs size: %.1f MB", float64(info.Size())/(1024*1024))
}

func TestGoBGPExcludesFRRE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	assertNotContains(t, files, "sbin/bgpd", "FRR bgpd")
	assertNotContains(t, files, "sbin/zebra", "FRR zebra")
	assertNotContains(t, files, "sbin/bfdd", "FRR bfdd")
	assertNotContains(t, files, "bin/vtysh", "FRR vtysh")
	assertNotContains(t, files, "sbin/watchfrr", "FRR watchfrr")
}

func TestGoBGPContainsLVME2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "sbin/lvm", "LVM tooling")
	for _, cmd := range []string{"pvcreate", "vgs", "vgcreate", "lvcreate"} {
		assertContains(t, files, "sbin/"+cmd, "LVM "+cmd)
	}
	assertContains(t, files, "bin/sfdisk", "sfdisk partitioner")
}

func TestGoBGPContainsDiskToolsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "bin/wipefs", "wipefs")
	assertContains(t, files, "sbin/mdadm", "mdadm RAID")
	// BOOTy converts qcow2 images in process.
	assertNotContains(t, files, "bin/qemu-img", "qemu-img")
	assertContains(t, files, "bin/efibootmgr", "efibootmgr")
	assertContains(t, files, "bin/lsblk", "lsblk for rescue mode")
	assertContains(t, files, "sbin/blkid", "util-linux blkid")
	assertContains(t, files, "bin/losetup", "util-linux losetup")
	assertNotContains(t, files, "bin/blkid", "BusyBox blkid applet shadowing util-linux blkid")
}

func TestGoBGPContainsNetworkAndSSHE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	assertContains(t, files, "bin/ip", "iproute2 ip")
	assertContains(t, files, "bin/bridge", "iproute2 bridge")
	assertContains(t, files, "bin/ethtool", "ethtool")
	// BOOTy fetches with net/http; busybox wget does HTTP and HTTPS in the shell.
	assertNotContains(t, files, "bin/curl", "curl")
	assertContains(t, files, "bin/wget", "busybox wget")
	assertContains(t, files, "bin/dropbear", "dropbear SSH")
	assertNotContains(t, files, "bin/lldpcli", "LLDP client")
	assertNotContains(t, files, "sbin/lldpd", "LLDP daemon")
	assertContains(t, files, "etc/ssl/certs/ca-certificates.crt", "CA certificates")
	assertContains(t, files, "bin/gpgv", "GPG signature verifier")
}

func TestGoBGPContainsKernelModulesE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	hasModules := false
	for path := range files {
		if strings.HasPrefix(path, "modules/") &&
			(strings.HasSuffix(path, ".ko") || strings.HasSuffix(path, ".ko.zst") ||
				strings.HasSuffix(path, ".ko.xz") || strings.HasSuffix(path, ".ko.gz")) {
			hasModules = true
			break
		}
	}
	if !hasModules {
		t.Error("no kernel modules found under modules/ in gobgp initramfs")
	}
	assertKernelModuleContains(t, files, "nls_ascii")
}

// ── Cross-flavour comparison with GoBGP ──────────────────────────────────

func TestBuildFlavourSizeOrderWithGoBGPE2E(t *testing.T) {
	dockerAvailable(t)

	defaultDest := t.TempDir()
	gobgpDest := t.TempDir()
	slimDest := t.TempDir()
	microDest := t.TempDir()

	defaultCpio := buildTarget(t, "", defaultDest)
	gobgpCpio := buildTarget(t, "gobgp", gobgpDest)
	slimCpio := buildTarget(t, "slim", slimDest)
	microCpio := buildTarget(t, "micro", microDest)

	defaultInfo, _ := os.Stat(defaultCpio)
	gobgpInfo, _ := os.Stat(gobgpCpio)
	slimInfo, _ := os.Stat(slimCpio)
	microInfo, _ := os.Stat(microCpio)

	t.Logf("Size order: micro (%.1f MB) < slim (%.1f MB) < gobgp (%.1f MB) <= default (%.1f MB)",
		float64(microInfo.Size())/(1024*1024),
		float64(slimInfo.Size())/(1024*1024),
		float64(gobgpInfo.Size())/(1024*1024),
		float64(defaultInfo.Size())/(1024*1024))

	if microInfo.Size() >= slimInfo.Size() {
		t.Error("micro should be smaller than slim")
	}
	if slimInfo.Size() >= gobgpInfo.Size() {
		t.Error("slim should be smaller than gobgp")
	}
	// GoBGP may be similar size to default (no FRR, but same tools).
	// Only check that GoBGP is not vastly larger than default.
	if gobgpInfo.Size() > defaultInfo.Size()*2 {
		t.Error("gobgp should not be more than 2x the default size")
	}

	assertFileSize(t, microCpio, 0.1, maxMicroInitramfsMB)
	assertFileSize(t, slimCpio, 0.1, maxSlimInitramfsMB)
	assertFileSize(t, gobgpCpio, 0.1, maxGoBGPInitramfsMB)
	assertFileSize(t, defaultCpio, 0.1, maxDefaultInitramfsMB)
}
