package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// checkDockerfileModules verifies that a Dockerfile's "for m in ..." word list
// contains no inline comments (which break shell parsing) and includes all
// critical modules.
func checkDockerfileModules(t *testing.T, path string, critical []string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}

	forPattern := regexp.MustCompile(`(?m)^\s*for m in\b`)
	loc := forPattern.FindStringIndex(string(data))
	if loc == nil {
		t.Fatalf("cannot find 'for m in' loop in %s", path)
	}

	block := string(data)[loc[0]:]
	doIdx := strings.Index(block, "; do")
	if doIdx < 0 {
		t.Fatal("cannot find '; do' in for-loop")
	}
	wordList := block[:doIdx]

	for i, line := range strings.Split(wordList, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "for ") {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			t.Errorf("line %d: inline comment in for-loop word list breaks shell: %q", i+1, trimmed)
		}
	}

	// Build a set of tokens to avoid substring false positives
	// (e.g. "virtio_pci" matching "virtio_pci_modern_dev").
	tokens := make(map[string]struct{})
	for _, line := range strings.Split(wordList, "\n") {
		for _, field := range strings.Fields(line) {
			// Strip shell continuation characters.
			field = strings.TrimRight(field, "\\")
			tokens[field] = struct{}{}
		}
	}

	for _, mod := range critical {
		if _, ok := tokens[mod]; !ok {
			t.Errorf("critical module %q missing from %s for-loop", mod, path)
		}
	}
}

func TestDockerfileModuleLoopSyntax(t *testing.T) {
	checkDockerfileModules(t, "initrd.Dockerfile", []string{
		"ext4", "xfs", "fat", "vfat", "nls_cp437", "nls_iso8859-1", "scsi_mod", "sd_mod",
		"virtio_pci", "virtio_net", "virtio_blk", "vrf", "vxlan",
		// Storage controllers, software RAID and EFI variables are modules
		// of the Debian kernel.
		"nvme", "ahci", "megaraid_sas", "mpt3sas", "smartpqi", "hpsa", "loop",
		"md_mod", "raid1", "raid10", "raid456", "efivarfs", "8021q",
		"mlx5_core", "ice", "i40e", "bnxt_en", "tg3", "igb",
	})
}

// TestDockerfileFlavorsShipKernelModules checks that every flavor with disk
// tools ships the kernel modules. The micro flavor has no tools and expects
// a kernel with built-in drivers.
func TestDockerfileFlavorsShipKernelModules(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	stages := regexp.MustCompile(`(?m)^FROM .* AS (\S+)$`).FindAllStringSubmatchIndex(string(data), -1)
	body := map[string]string{}
	for i, m := range stages {
		end := len(data)
		if i+1 < len(stages) {
			end = stages[i+1][0]
		}
		body[string(data[m[2]:m[3]])] = string(data[m[0]:end])
	}
	for _, stage := range []string{"busybox", "slim-builder", "gobgp-builder"} {
		if !strings.Contains(body[stage], "COPY --from=kernel /modules/ modules/") {
			t.Errorf("stage %s does not copy the kernel modules", stage)
		}
	}
}

func TestVrnetlabDockerfileModuleLoopSyntax(t *testing.T) {
	checkDockerfileModules(t, "test/e2e/clab/vrnetlab/Dockerfile", []string{
		"ext4", "xfs", "fat", "vfat", "nls_cp437", "nls_iso8859-1", "scsi_mod", "sd_mod",
		"virtio_pci", "virtio_net", "virtio_blk", "virtio_scsi", "vrf", "vxlan",
	})
}

func TestDockerfileRemovesBusyboxMkfsVfatCollisions(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatalf("cannot read initrd.Dockerfile: %v", err)
	}
	text := string(data)
	for _, path := range []string{"bin/mkfs.vfat", "bin/mkfs.fat", "bin/mkdosfs"} {
		if strings.Count(text, path) < 2 {
			t.Errorf("%s must be removed in both full initramfs builders so dosfstools sbin/mkfs.vfat is used", path)
		}
	}
	lines := strings.Split(text, "\n")
	foundSafeGrowpartCopy := false
	for i, line := range lines {
		if strings.TrimSpace(line) != "COPY --from=busybox /build/initramfs/bin/growpart bin/growpart" || i == 0 {
			continue
		}
		previous := strings.TrimSpace(lines[i-1])
		if strings.HasPrefix(previous, "RUN rm -f ") && strings.Contains(previous, "bin/lsblk") {
			foundSafeGrowpartCopy = true
			break
		}
	}
	if !foundSafeGrowpartCopy {
		t.Error("bin/lsblk must be removed in the GoBGP builder before copying real lsblk, otherwise COPY corrupts bin/busybox")
	}
}

func TestDockerfileStampsBuildInfo(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatalf("cannot read initrd.Dockerfile: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		"ARG BOOTY_VERSION=dev",
		"ARG BOOTY_BUILD=unknown",
		"ARG BOOTY_FLAVOR=full",
		"ARG BOOTY_FLAVOR=micro",
		"-X main.Version=${BOOTY_VERSION}",
		"-X main.Build=${BOOTY_BUILD}",
		"-X github.com/telekom/BOOTy/pkg/buildinfo.version=${BOOTY_VERSION}",
		"-X github.com/telekom/BOOTy/pkg/buildinfo.commit=${BOOTY_BUILD}",
		"-X github.com/telekom/BOOTy/pkg/buildinfo.flavor=${BOOTY_FLAVOR}",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("initrd.Dockerfile missing build metadata stamp %q", want)
		}
	}
}

func TestDockerfileLVMBuildStepPropagatesMakeFailures(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatalf("cannot read initrd.Dockerfile: %v", err)
	}

	text := string(data)
	if strings.Contains(text, "RUN make; exit 0") {
		t.Fatal("initrd.Dockerfile must not mask LVM make failures with '; exit 0'")
	}
	if !regexp.MustCompile(`(?m)^RUN make\s*$`).MatchString(text) {
		t.Fatal("initrd.Dockerfile must keep the LVM make step as a standalone failing RUN command")
	}
}

func TestReleaseAndNightlyPassInitramfsBuildInfoArgs(t *testing.T) {
	for _, path := range []string{
		".github/workflows/release-v2.yml",
		".github/workflows/nightly.yml",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cannot read %s: %v", path, err)
		}
		text := string(data)
		for _, want := range []string{
			`--build-arg "BOOTY_VERSION=`,
			`--build-arg "BOOTY_BUILD=`,
			`--build-arg "BOOTY_FLAVOR=`,
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing build metadata argument %q", path, want)
			}
		}
		if strings.Contains(text, `BOOTY_FLAVOR=default`) {
			t.Fatalf("%s must stamp default release artifacts as build-info flavor full, not default", path)
		}
		if strings.Contains(text, `BOOTY_FLAVOR=${{ matrix.flavor }}`) {
			t.Fatalf("%s must normalize matrix flavor before stamping build-info flavor", path)
		}
	}
}

func TestDockerfileUsesUtilLinuxLsblk(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatalf("cannot read initrd.Dockerfile: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	lsblkCopies := 0
	lsblkRemovals := 0
	inRmCommand := false
	rmCommandHasLsblk := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "COPY --from=tools /bin/lsblk bin/lsblk" ||
			trimmed == "COPY --from=tools /usr/bin/lsblk bin/lsblk" {
			lsblkCopies++
		}
		if strings.HasPrefix(trimmed, "RUN rm -f ") {
			inRmCommand = true
			rmCommandHasLsblk = false
		}
		if inRmCommand && strings.Contains(trimmed, "bin/lsblk") {
			rmCommandHasLsblk = true
		}
		if inRmCommand && !strings.HasSuffix(trimmed, "\\") {
			if rmCommandHasLsblk {
				lsblkRemovals++
			}
			inRmCommand = false
		}
	}
	if lsblkCopies != 3 {
		t.Fatalf("expected default, slim, and GoBGP builders to copy util-linux lsblk, got %d copies", lsblkCopies)
	}
	if lsblkRemovals != 3 {
		t.Fatalf("expected default, slim, and GoBGP builders to remove BusyBox lsblk before COPY, got %d removals", lsblkRemovals)
	}
}

func TestSlimDockerfileIncludesResizeAndRepairTools(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatalf("cannot read initrd.Dockerfile: %v", err)
	}
	text := string(data)
	start := strings.Index(text, "FROM debian:bookworm-slim AS slim-builder")
	if start < 0 {
		t.Fatal("cannot find slim-builder stage")
	}
	end := strings.Index(text[start+1:], "\nFROM ")
	if end < 0 {
		t.Fatal("cannot find end of slim-builder stage")
	}
	slimStage := text[start : start+1+end]
	for _, want := range []string{
		"COPY --from=tools /sbin/resize2fs sbin/resize2fs",
		"COPY --from=tools /usr/sbin/xfs_growfs sbin/xfs_growfs",
		"COPY --from=tools /usr/sbin/xfs_repair sbin/xfs_repair",
		"COPY --from=tools /usr/bin/btrfs bin/btrfs",
	} {
		if !strings.Contains(slimStage, want) {
			t.Fatalf("slim-builder missing filesystem tool copy %q", want)
		}
	}
}

func TestDockerfileUsesUtilLinuxDiskProbeTools(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatalf("cannot read initrd.Dockerfile: %v", err)
	}
	text := string(data)
	for _, tool := range []struct {
		name             string
		copy             string
		path             string
		removeBeforeCopy bool
	}{
		{
			name:             "blkid",
			copy:             "COPY --from=tools /sbin/blkid sbin/blkid",
			path:             "bin/blkid",
			removeBeforeCopy: false,
		},
		{
			name:             "losetup",
			copy:             "COPY --from=tools /usr/sbin/losetup bin/losetup",
			path:             "bin/losetup",
			removeBeforeCopy: true,
		},
	} {
		if got := strings.Count(text, tool.copy); got != 3 {
			t.Fatalf("util-linux %s must be copied into default, slim, and GoBGP builders; got %d copies", tool.name, got)
		}
		for _, stage := range []string{"busybox", "slim-builder", "gobgp-builder"} {
			t.Run(tool.name+"/"+stage, func(t *testing.T) {
				if tool.removeBeforeCopy {
					requireStageRemovesBusyboxToolBeforeCopy(t, text, stage, tool.name, tool.copy, tool.path)
					return
				}
				requireStageRemovesBusyboxTool(t, text, stage, tool.name, tool.path)
			})
		}
	}
}

func requireStageRemovesBusyboxToolBeforeCopy(t *testing.T, text, stage, toolName, copyCommand, busyboxPath string) {
	t.Helper()

	block := dockerfileStageBlock(t, text, stage)
	copyIndex := strings.Index(block, copyCommand)
	if copyIndex < 0 {
		t.Fatalf("%s stage must copy util-linux %s", stage, toolName)
	}

	beforeCopy := block[:copyIndex]
	removeIndex := strings.LastIndex(beforeCopy, "RUN rm -f")
	if removeIndex >= 0 && strings.Contains(beforeCopy[removeIndex:], busyboxPath) {
		return
	}
	t.Fatalf("%s stage must remove BusyBox %s before copying util-linux %s", stage, busyboxPath, toolName)
}

func requireStageRemovesBusyboxTool(t *testing.T, text, stage, toolName, busyboxPath string) {
	t.Helper()

	block := dockerfileStageBlock(t, text, stage)
	removeIndex := strings.LastIndex(block, "RUN rm -f")
	if removeIndex >= 0 && strings.Contains(block[removeIndex:], busyboxPath) {
		return
	}
	t.Fatalf("%s stage must remove BusyBox %s before packaging util-linux %s", stage, busyboxPath, toolName)
}

func dockerfileStageBlock(t *testing.T, text, stage string) string {
	t.Helper()

	stagePattern := regexp.MustCompile(`(?m)^FROM\s+.*\s+AS\s+` + regexp.QuoteMeta(stage) + `\s*$`)
	loc := stagePattern.FindStringIndex(text)
	if loc == nil {
		t.Fatalf("cannot find Dockerfile stage %q", stage)
	}
	block := text[loc[0]:]
	fromPattern := regexp.MustCompile(`(?m)^FROM\s+`)
	matches := fromPattern.FindAllStringIndex(block, 2)
	if len(matches) == 2 {
		return block[:matches[1][0]]
	}
	return block
}

// dockerfilesWithDownloads lists the Dockerfiles that fetch from the network
// during a build. Every fetch in these files must retry, because one transient
// TLS or DNS failure otherwise breaks the whole build.
var dockerfilesWithDownloads = []string{
	"initrd.Dockerfile",
	"test/e2e/clab/vrnetlab/Dockerfile",
	"test/e2e/clab/booty-test.Dockerfile",
	"test/e2e/clab/booty-gobgp-test.Dockerfile",
	"test/e2e/clab/dhcpd-test.Dockerfile",
}

// downloadRetryChecks maps a network command to the marker that proves it
// retries. The pattern matches the command in command position only, so a
// COPY of the curl binary or a package named curl does not count.
var downloadRetryChecks = []struct {
	name    string
	pattern *regexp.Regexp
	marker  string
	hint    string
}{
	{name: "apt-get", pattern: regexp.MustCompile(`apt-get\s+(update|install|download)\b`), marker: "Acquire::Retries", hint: "-o Acquire::Retries=5"},
	{name: "apk add", pattern: regexp.MustCompile(`(^|RUN\s+|[;&|]\s*|until\s+|then\s+|do\s+)apk\s+add\b`), marker: "until apk add", hint: "an until retry loop"},
	{name: "wget", pattern: regexp.MustCompile(`(^|RUN\s+|[;&|]\s*|until\s+|then\s+|do\s+)wget\s`), marker: "--tries=", hint: "--tries=5 --waitretry=10"},
	{name: "curl", pattern: regexp.MustCompile(`(^|RUN\s+|[;&|]\s*|until\s+|then\s+|do\s+)curl\s`), marker: "--retry ", hint: "--retry 5 --retry-delay 5"},
	{name: "git clone", pattern: regexp.MustCompile(`(^|RUN\s+|[;&|]\s*|until\s+|then\s+|do\s+)git\s+clone\b`), marker: "until git clone", hint: "an until retry loop"},
	{name: "git fetch", pattern: regexp.MustCompile(`(^|RUN\s+|[;&|]\s*|until\s+|then\s+|do\s+)git\s+fetch\b`), marker: "until git fetch", hint: "an until retry loop"},
	{name: "go mod download", pattern: regexp.MustCompile(`(^|RUN\s+|[;&|]\s*|until\s+|then\s+|do\s+)go\s+mod\s+download\b`), marker: "until go mod download", hint: "an until retry loop"},
}

// TestDockerfileDownloadsRetry fails when a network fetch carries no retry.
// A retry loop must also fail closed, so every loop needs an explicit exit
// once the attempts run out. Compare kubernetes-sigs/image-builder#2138,
// where retries were present but never took effect.
func TestDockerfileDownloadsRetry(t *testing.T) {
	for _, path := range dockerfilesWithDownloads {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("cannot read %s: %v", path, err)
			}
			text := string(data)

			for _, cmd := range dockerfileCommands(text) {
				for _, check := range downloadRetryChecks {
					if !check.pattern.MatchString(cmd.text) {
						continue
					}
					if strings.Contains(cmd.text, check.marker) {
						continue
					}
					t.Errorf("%s:%d: %s needs %s", path, cmd.line, check.name, check.hint)
				}
			}

			loops := strings.Count(text, "until ")
			exits := strings.Count(text, `attempts" >&2; exit 1;`)
			if loops != exits {
				t.Errorf("%s has %d retry loops but %d fail-closed exits", path, loops, exits)
			}
		})
	}
}

type dockerfileCommand struct {
	line int
	text string
}

// dockerfileCommands joins line continuations so a command and its flags read
// as one string, and keeps the line where each command starts.
func dockerfileCommands(text string) []dockerfileCommand {
	var commands []dockerfileCommand
	var current []string
	start := 0

	for i, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(current) == 0 {
			if strings.HasPrefix(trimmed, "#") || trimmed == "" {
				continue
			}
			start = i + 1
		}
		current = append(current, strings.TrimSuffix(trimmed, "\\"))
		if strings.HasSuffix(trimmed, "\\") {
			continue
		}
		commands = append(commands, dockerfileCommand{line: start, text: strings.Join(current, " ")})
		current = nil
	}
	if len(current) > 0 {
		commands = append(commands, dockerfileCommand{line: start, text: strings.Join(current, " ")})
	}
	return commands
}

// TestDockerfileBusyboxAppletLinksAreRelative guards against
// `busybox --install -s`, which writes absolute links to the build directory
// that dangle in the booted ramdisk.
func TestDockerfileBusyboxAppletLinksAreRelative(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^RUN .*busybox --install`).Match(data) {
		t.Fatal("initrd.Dockerfile installs busybox applets with --install, which creates absolute symlinks")
	}
}

// TestDockerfileFlavorsShipEFIBootMgr: on UEFI a missing efibootmgr fails
// provisioning, so every flavor with disk tools ships it.
func TestDockerfileFlavorsShipEFIBootMgr(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	stages := regexp.MustCompile(`(?m)^FROM .* AS (\S+)$`).FindAllStringSubmatchIndex(string(data), -1)
	for i, m := range stages {
		name := string(data[m[2]:m[3]])
		if name != "busybox" && name != "slim-builder" && name != "gobgp-builder" {
			continue
		}
		end := len(data)
		if i+1 < len(stages) {
			end = stages[i+1][0]
		}
		if !strings.Contains(string(data[m[0]:end]), "COPY --from=tools /usr/bin/efibootmgr bin/efibootmgr") {
			t.Errorf("stage %s does not ship efibootmgr", name)
		}
	}
}

// TestDockerfilePrunesUnusedLibsBeforePacking: the builders copy the library
// sets of every tool, so each flavor must prune the libraries of the tools it
// does not ship, and must do so after the last COPY and before cpio runs.
func TestDockerfilePrunesUnusedLibsBeforePacking(t *testing.T) {
	data, err := os.ReadFile("initrd.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"busybox", "slim-builder", "gobgp-builder"} {
		block := dockerfileStageBlock(t, string(data), stage)
		prune := strings.Index(block, "RUN prune-initramfs-libs.sh .")
		pack := strings.Index(block, "cpio --null")
		lastCopy := strings.LastIndex(block, "\nCOPY --from=")
		if prune < 0 || pack < 0 || prune > pack || lastCopy > prune {
			t.Errorf("stage %s must run prune-initramfs-libs.sh after its last COPY --from and before cpio", stage)
		}
	}
}
