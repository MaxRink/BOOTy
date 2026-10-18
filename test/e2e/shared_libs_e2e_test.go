//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildStageImage builds a specific Dockerfile stage as a Docker image (--load)
// and returns the image tag. The image is automatically removed via t.Cleanup.
func buildStageImage(t *testing.T, stage string) string {
	t.Helper()
	dockerAvailable(t)

	tag := "booty-test-" + stage + ":latest"
	dockerfile := filepath.Join(findRepoRoot(t), "initrd.Dockerfile")
	repoRoot := findRepoRoot(t)

	args := []string{"buildx", "build", "--platform", "linux/amd64", "--target", stage, "--load", "-t", tag, "-f", dockerfile, repoRoot}
	errPrefix := "docker buildx build"
	if !dockerBuildxAvailable() {
		args = []string{"build", "--platform", "linux/amd64", "--target", stage, "-t", tag, "-f", dockerfile, repoRoot}
		errPrefix = "docker build"
	}
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s --target %s failed: %v\n%s", errPrefix, stage, err, out)
	}

	t.Cleanup(func() {
		_ = exec.Command("docker", "rmi", "-f", tag).Run()
	})
	return tag
}

// lddCheck runs ldd on a binary inside a container and returns any libraries
// reported as "not found".
func lddCheck(t *testing.T, image, binary string) []string {
	t.Helper()
	// Set LD_LIBRARY_PATH so ldd can find libs copied into the initramfs workdir.
	// The Dockerfile copies shared libraries with --parents, preserving their
	// /lib/... and /usr/lib/... paths under /build/initramfs/.
	// FRR libraries live in a frr/ subdirectory (e.g., /usr/lib/x86_64-linux-gnu/frr/).
	ldPaths := strings.Join([]string{
		"/build/initramfs/lib",
		"/build/initramfs/usr/lib",
		"/build/initramfs/lib/x86_64-linux-gnu",
		"/build/initramfs/usr/lib/x86_64-linux-gnu",
		"/build/initramfs/usr/lib/x86_64-linux-gnu/frr",
	}, ":")
	args := []string{"run", "--rm", "--entrypoint", "",
		"-e", "LD_LIBRARY_PATH=" + ldPaths, image, "ldd", binary}
	cmd := exec.Command("docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker run ldd %s failed: %v\n%s", binary, err, out)
	}

	var missing []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "not found") {
			missing = append(missing, strings.TrimSpace(line))
		}
	}
	return missing
}

// checkSharedLibs is a table-driven helper that runs ldd on each binary inside
// the given builder stage image and reports any missing shared libraries.
func checkSharedLibs(t *testing.T, image string, binaries []string) {
	t.Helper()
	workdir := "/build/initramfs"
	var failures []string
	for _, bin := range binaries {
		fullPath := workdir + "/" + bin
		missing := lddCheck(t, image, fullPath)
		if len(missing) > 0 {
			failures = append(failures,
				fmt.Sprintf("%s: %s", bin, strings.Join(missing, "; ")))
		}
	}
	if len(failures) > 0 {
		t.Errorf("binaries with missing shared libraries:\n  %s",
			strings.Join(failures, "\n  "))
	}
}

// TestDefaultSharedLibsResolveE2E builds the default (busybox) builder stage
// and verifies every dynamically-linked binary can resolve all its shared
// library dependencies.  This catches missing .so files that cause runtime
// "No such file or directory" errors in the initramfs.
func TestDefaultSharedLibsResolveE2E(t *testing.T) {
	image := buildStageImage(t, "busybox")
	checkSharedLibs(t, image, []string{
		// Disk tools
		"bin/wipefs", "sbin/mdadm", "sbin/resize2fs", "sbin/e2fsck",
		"sbin/blkid", "bin/losetup",
		"sbin/mkfs.ext4", "sbin/mkfs.vfat",
		"sbin/xfs_growfs", "bin/btrfs", "bin/parted", "bin/sgdisk",
		"bin/partprobe", "bin/partx", "bin/qemu-img",
		// EFI
		"bin/efibootmgr",
		// System
		"bin/dmidecode", "bin/ethtool",
		// Network
		"bin/ip", "bin/bridge",
		// Secure erase
		"bin/hdparm", "bin/nvme",
		// Firmware
		"bin/mstconfig", "bin/mstflint",
		// SSH
		"bin/dropbear", "bin/dropbearkey",
		// Rescue/debug mode
		"bin/lsblk",
		// FRR
		"sbin/bgpd", "sbin/zebra", "sbin/bfdd", "bin/vtysh", "sbin/watchfrr",
	})
}

// TestGoBGPSharedLibsResolveE2E builds the GoBGP builder stage and verifies
// shared library resolution for all dynamically-linked binaries.
func TestGoBGPSharedLibsResolveE2E(t *testing.T) {
	image := buildStageImage(t, "gobgp-builder")
	checkSharedLibs(t, image, []string{
		// Disk tools
		"bin/wipefs", "sbin/mdadm", "sbin/resize2fs", "sbin/e2fsck",
		"sbin/blkid", "bin/losetup",
		"sbin/mkfs.ext4", "sbin/mkfs.vfat",
		"sbin/xfs_growfs", "bin/btrfs", "bin/parted", "bin/sgdisk",
		"bin/partprobe", "bin/partx", "bin/qemu-img",
		// EFI
		"bin/efibootmgr",
		// System
		"bin/dmidecode", "bin/ethtool",
		// Network
		"bin/ip", "bin/bridge",
		// Secure erase
		"bin/hdparm", "bin/nvme",
		// Firmware
		"bin/mstconfig", "bin/mstflint",
		// SSH
		"bin/dropbear", "bin/dropbearkey",
		// Rescue mode
		"bin/lsblk",
	})
}

func TestFullInitramfsMkfsVfatSupportsFATSizeFlagE2E(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
	}{
		{name: "default", stage: "busybox"},
		{name: "gobgp", stage: "gobgp-builder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := buildStageImage(t, tc.stage)
			checkSharedLibs(t, image, []string{"bin/lsblk"})
			ldPaths := strings.Join([]string{
				"/build/initramfs/lib",
				"/build/initramfs/usr/lib",
				"/build/initramfs/lib/x86_64-linux-gnu",
				"/build/initramfs/usr/lib/x86_64-linux-gnu",
			}, ":")
			script := strings.Join([]string{
				"PATH=/build/initramfs/bin:/build/initramfs/sbin:$PATH",
				"resolved=$(command -v mkfs.vfat)",
				`echo "resolved=$resolved"`,
				`[ "$resolved" = "/build/initramfs/sbin/mkfs.vfat" ]`,
				`sh -c 'test "$1" = ok' -- ok`,
				"mkfs.vfat --help >/tmp/mkfs-vfat-help 2>&1 || true",
				"cat /tmp/mkfs-vfat-help",
				"grep -q -- '-F' /tmp/mkfs-vfat-help",
				"! grep -qi busybox /tmp/mkfs-vfat-help",
			}, " && ")
			cmd := exec.Command("docker", "run", "--rm", "--entrypoint", "",
				"-e", "LD_LIBRARY_PATH="+ldPaths, image, "sh", "-ec", script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("mkfs.vfat PATH/FAT-size check failed: %v\n%s", err, out)
			}
		})
	}
}

func TestFullInitramfsLsblkSupportsRuntimeFlagsE2E(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
	}{
		{name: "default", stage: "busybox"},
		{name: "gobgp", stage: "gobgp-builder"},
		{name: "slim", stage: "slim-builder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := buildStageImage(t, tc.stage)
			ldPaths := strings.Join([]string{
				"/build/initramfs/lib",
				"/build/initramfs/usr/lib",
				"/build/initramfs/lib/x86_64-linux-gnu",
				"/build/initramfs/usr/lib/x86_64-linux-gnu",
			}, ":")
			checks := []string{
				"/build/initramfs/bin/lsblk --nodeps --noheadings --paths --output NAME,SERIAL >/dev/null",
				"/build/initramfs/bin/lsblk -rnpo NAME >/dev/null",
				"/build/initramfs/bin/lsblk --json >/dev/null",
				"! /build/initramfs/bin/lsblk --help 2>&1 | grep -qi busybox",
			}
			script := strings.Join(checks, " && ")
			cmd := exec.Command("docker", "run", "--rm", "--entrypoint", "",
				"-e", "LD_LIBRARY_PATH="+ldPaths, image, "sh", "-ec", script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("lsblk runtime flag check failed: %v\n%s", err, out)
			}
		})
	}
}

func TestFullInitramfsDiskProbeToolsUseUtilLinuxE2E(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage string
	}{
		{name: "default", stage: "busybox"},
		{name: "gobgp", stage: "gobgp-builder"},
		{name: "slim", stage: "slim-builder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := buildStageImage(t, tc.stage)
			ldPaths := strings.Join([]string{
				"/build/initramfs/lib",
				"/build/initramfs/usr/lib",
				"/build/initramfs/lib/x86_64-linux-gnu",
				"/build/initramfs/usr/lib/x86_64-linux-gnu",
			}, ":")
			script := strings.Join([]string{
				"PATH=/build/initramfs/bin:/build/initramfs/sbin:$PATH",
				"resolved_blkid=$(command -v blkid)",
				`echo "resolved_blkid=$resolved_blkid"`,
				`[ "$resolved_blkid" = "/build/initramfs/sbin/blkid" ]`,
				"blkid --version >/tmp/blkid-version 2>&1",
				"cat /tmp/blkid-version",
				"grep -qi 'util-linux' /tmp/blkid-version",
				"! grep -qi busybox /tmp/blkid-version",
				"resolved_losetup=$(command -v losetup)",
				`echo "resolved_losetup=$resolved_losetup"`,
				`[ "$resolved_losetup" = "/build/initramfs/bin/losetup" ]`,
				"losetup --help >/tmp/losetup-help 2>&1",
				"cat /tmp/losetup-help",
				"grep -q -- '--show' /tmp/losetup-help",
				"grep -q -- '--partscan' /tmp/losetup-help",
				"grep -q -- '--detach' /tmp/losetup-help",
				"! grep -qi busybox /tmp/losetup-help",
			}, " && ")
			cmd := exec.Command("docker", "run", "--rm", "--entrypoint", "",
				"-e", "LD_LIBRARY_PATH="+ldPaths, image, "sh", "-ec", script)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("disk probe tool PATH/help check failed: %v\n%s", err, out)
			}
		})
	}
}

// TestSlimSharedLibsResolveE2E builds the slim builder stage and verifies
// shared library resolution for all dynamically-linked binaries.
func TestSlimSharedLibsResolveE2E(t *testing.T) {
	image := buildStageImage(t, "slim-builder")
	checkSharedLibs(t, image, []string{
		"bin/ip", "bin/ethtool",
		"bin/partprobe", "bin/partx", "bin/lsblk", "sbin/e2fsck", "sbin/blkid", "bin/losetup", "sbin/resize2fs",
		"sbin/mkfs.ext4", "sbin/mkfs.vfat", "sbin/mkfs.xfs",
		"sbin/xfs_growfs", "sbin/xfs_repair", "bin/btrfs",
	})
}

// TestDefaultInitramfsHasLinkerE2E verifies the dynamic linker (ld-linux) is
// present in the default initramfs.  Without it, ALL dynamically-linked
// binaries fail with "No such file or directory".
func TestDefaultInitramfsHasLinkerE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	hasLinker := false
	for path := range files {
		if strings.Contains(path, "ld-linux") || strings.Contains(path, "ld-musl") {
			hasLinker = true
			break
		}
	}
	if !hasLinker {
		t.Error("dynamic linker (ld-linux-x86-64.so or ld-musl) not found in default initramfs")
	}
}

// TestGoBGPInitramfsHasLinkerE2E verifies the dynamic linker is present in the
// GoBGP initramfs.
func TestGoBGPInitramfsHasLinkerE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	hasLinker := false
	for path := range files {
		if strings.Contains(path, "ld-linux") || strings.Contains(path, "ld-musl") {
			hasLinker = true
			break
		}
	}
	if !hasLinker {
		t.Error("dynamic linker (ld-linux-x86-64.so or ld-musl) not found in gobgp initramfs")
	}
}

// TestSlimInitramfsHasLinkerE2E verifies the dynamic linker is present in the
// slim initramfs.
func TestSlimInitramfsHasLinkerE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "slim", dest)
	files := listCPIOContents(t, cpioGz)

	hasLinker := false
	for path := range files {
		if strings.Contains(path, "ld-linux") || strings.Contains(path, "ld-musl") {
			hasLinker = true
			break
		}
	}
	if !hasLinker {
		t.Error("dynamic linker (ld-linux-x86-64.so or ld-musl) not found in slim initramfs")
	}
}

// TestDefaultInitramfsHasLibefivarE2E verifies libefivar.so is present in the
// default initramfs (required by efibootmgr).
func TestDefaultInitramfsHasLibefivarE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	hasLibefivar := false
	for path := range files {
		if strings.Contains(path, "libefivar") {
			hasLibefivar = true
			break
		}
	}
	if !hasLibefivar {
		t.Error("libefivar.so not found in default initramfs (required by efibootmgr)")
	}
}

// TestDefaultInitramfsHasLibmnlE2E verifies libmnl.so is present in the
// default initramfs (required by ip/bridge from iproute2).
func TestDefaultInitramfsHasLibmnlE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "", dest)
	files := listCPIOContents(t, cpioGz)

	hasLibmnl := false
	for path := range files {
		if strings.Contains(path, "libmnl") {
			hasLibmnl = true
			break
		}
	}
	if !hasLibmnl {
		t.Error("libmnl.so not found in default initramfs (required by iproute2 ip/bridge)")
	}
}

// TestGoBGPInitramfsHasLibefivarE2E verifies libefivar.so is present in the
// GoBGP initramfs (required by efibootmgr).
func TestGoBGPInitramfsHasLibefivarE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	hasLibefivar := false
	for path := range files {
		if strings.Contains(path, "libefivar") {
			hasLibefivar = true
			break
		}
	}
	if !hasLibefivar {
		t.Error("libefivar.so not found in gobgp initramfs (required by efibootmgr)")
	}
}

// TestGoBGPInitramfsHasLibmnlE2E verifies libmnl.so is present in the
// GoBGP initramfs (required by ip/bridge from iproute2).
func TestGoBGPInitramfsHasLibmnlE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "gobgp", dest)
	files := listCPIOContents(t, cpioGz)

	hasLibmnl := false
	for path := range files {
		if strings.Contains(path, "libmnl") {
			hasLibmnl = true
			break
		}
	}
	if !hasLibmnl {
		t.Error("libmnl.so not found in gobgp initramfs (required by iproute2 ip/bridge)")
	}
}

func TestFullInitramfsFlavorsHavePartitionFormattersE2E(t *testing.T) {
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
			for _, want := range []string{"sbin/mkfs.ext4", "sbin/mkfs.vfat"} {
				if !files[want] && !files["./"+want] {
					t.Fatalf("initramfs missing %s; A/B partition layout formatting cannot run", want)
				}
			}
		})
	}
}

// TestMicroInitramfsNoDynamicLibsE2E verifies the micro initramfs has no
// shared libraries — the init binary is statically compiled with CGO_ENABLED=0.
func TestMicroInitramfsNoDynamicLibsE2E(t *testing.T) {
	dockerAvailable(t)
	dest := t.TempDir()
	cpioGz := buildTarget(t, "micro", dest)
	files := listCPIOContents(t, cpioGz)

	for path := range files {
		if strings.HasSuffix(path, ".so") || strings.Contains(path, ".so.") {
			if strings.Contains(path, "ca-certificates") {
				continue
			}
			t.Errorf("unexpected shared library in micro initramfs: %s", path)
		}
	}
}

// TestMicroInitramfsStaticBinaryE2E verifies the micro init binary is
// statically linked by running ldd inside the Docker build stage.
func TestMicroInitramfsStaticBinaryE2E(t *testing.T) {
	image := buildStageImage(t, "micro-builder")

	// ldd on a static binary prints "not a dynamic executable" or "statically linked"
	args := []string{"run", "--rm", "--entrypoint", "", image, "ldd", "/build/initramfs/init"}
	cmd := exec.Command("docker", args...)
	out, _ := cmd.CombinedOutput()
	output := string(out)

	if strings.Contains(output, "not found") {
		t.Errorf("micro init binary has missing shared libraries: %s", output)
	}
}
