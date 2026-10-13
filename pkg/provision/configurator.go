//go:build linux

package provision

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
	exec "github.com/telekom/BOOTy/pkg/executil"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/serialconsole"
)

const newroot = "/newroot"

var errEFIFallbackAssetMissing = errors.New("bundled EFI fallback loader missing")

const mstconfigOutputLimit = 256

var (
	efiRuntimeReady              = defaultEFIRuntimeReady
	isMountPoint                 = defaultIsMountPoint
	mountedSource                = defaultMountedSource
	hostCommandCombinedOutput    = defaultHostCommandCombinedOutput
	efiFirmwarePath              = "/sys/firmware/efi"
	efiVarsPath                  = "/sys/firmware/efi/efivars"
	efiFallbackAssetDirectory    = "/usr/lib/booty/efi"
	newEFIFallbackHandoffID      = defaultEFIFallbackHandoffID
	installEFIFallbackWithChroot = true
	provisionerFilesDir          = "/deploy/file-system"
	ramdiskRoot                  = "/"
	machineCommandsDir           = "/deploy/machine-commands"
)

// machineCommandsChrootDir is where legacy-compat machine command scripts
// are staged inside the target root while they run.
const machineCommandsChrootDir = "/tmp/booty-machine-commands"

var managedEFIVendors = [...]string{"ubuntu", "debian"}

// safeKernelParams matches only safe characters for kernel command line parameters.
var safeKernelParams = regexp.MustCompile(`^[a-zA-Z0-9=._\-/ ]*$`)

// virtualTerminalParam matches the kernel virtual terminals, which are valid
// console values but never carry a serial getty.
var virtualTerminalCandidate = regexp.MustCompile(`^tty\d{1,2}$`)
var virtualTerminalParam = regexp.MustCompile(`^tty(?:\d|[1-5]\d|6[0-3])$`)

// safeProvisionCommand matches basic command/argument characters while
// rejecting shell metacharacters that enable command chaining or substitution.
var safeProvisionCommand = regexp.MustCompile(`^[a-zA-Z0-9_./:=,@%+\-\s]+$`)

// safeChrootDevicePath allows stable Linux block-device paths without shell
// metacharacters. Some callers still interpolate these values into legacy
// chroot shell commands, so fail closed before command construction.
var safeChrootDevicePath = regexp.MustCompile(`^/dev/[a-zA-Z0-9._/+:-]+$`)

var blockedShellTokens = []string{"&&", "||", "|", "`", "$(", ")", ">", "<", ";", "\n", "\r"}

const sensitiveCommandKeyPattern = `(?:password|token|secret|key|credential|auth|` +
	`(?:[a-z0-9]+[-_])+(?:password|token|secret|key|credential|auth)(?:[-_][a-z0-9]+)*|` +
	`(?:password|token|secret|key|credential|auth)(?:[-_][a-z0-9]+)+)`

// sensitiveKeyPattern matches sensitive command key-value pairs in two forms:
//  1. CLI flag format: --key=value, -key=value, or --key value / -key value
//     (quoted values are recognized in the pattern but will be rejected earlier
//     by validateProvisionCommand which does not allow quote characters)
//  2. Assignment format: key=value or key: value, where key must appear as a complete token.
//
// The value portion is replaced with [REDACTED] in logs.
var sensitiveKeyPattern = regexp.MustCompile(
	`(?i)(?:` +
		`(--?)(` + sensitiveCommandKeyPattern + `)(?:=(?:"[^"]*"|'[^']*'|\S+)|\s+(?:"[^"]*"|'[^']*'|\S+))` +
		`|(?:^|(?P<pre>[\s]))(` + sensitiveCommandKeyPattern + `)\s*[=:]\s*(?:"[^"]*"|'[^']*'|\S+)` +
		`)`,
)

// redactCommandValues replaces sensitive key-value patterns in cmd with [REDACTED]
// so that credentials are not written to the system log verbatim.
func redactCommandValues(cmd string) string {
	return sensitiveKeyPattern.ReplaceAllStringFunc(cmd, func(match string) string {
		if strings.HasPrefix(match, "-") {
			eqIdx := strings.Index(match, "=")
			wsIdx := firstWhitespaceIndex(match)
			if eqIdx >= 0 && (wsIdx < 0 || eqIdx < wsIdx) {
				return match[:eqIdx+1] + "[REDACTED]"
			}
			if wsIdx >= 0 {
				return match[:wsIdx] + " [REDACTED]"
			}
			return match
		}
		if eqIdx := strings.IndexAny(match, "=:"); eqIdx >= 0 {
			return match[:eqIdx+1] + "[REDACTED]"
		}
		return match
	})
}

func firstWhitespaceIndex(s string) int {
	for i, r := range s {
		if unicode.IsSpace(r) {
			return i
		}
	}
	return -1
}

// Configurator handles post-image OS configuration.
type Configurator struct {
	disk    *disk.Manager
	rootDir string // allows override for testing (default: /newroot)
	// hostSysRoot and hostProcRoot point at the read-only host trees the
	// serial console resolver inspects. Empty means /sys and /proc.
	hostSysRoot  string
	hostProcRoot string
	// runDir is the initramfs directory the serial console resolution
	// artifact is published to. Empty means /run/booty.
	runDir string
	// serialConsole caches the single console resolution used for both the
	// kernel command line and the serial getty.
	serialConsole     serialconsole.Resolution
	serialConsoleErr  error
	serialConsoleDone bool
}

// NewConfigurator creates a Configurator.
func NewConfigurator(diskMgr *disk.Manager) *Configurator {
	return &Configurator{disk: diskMgr, rootDir: newroot}
}

// SetRootDir overrides the root directory (for testing).
func (c *Configurator) SetRootDir(dir string) { c.rootDir = dir }

// SetRunDir overrides the initramfs run directory used to publish artifacts
// for CAPRF (for testing).
func (c *Configurator) SetRunDir(dir string) { c.runDir = dir }

// SetHostRoots overrides the read-only host trees inspected for serial console
// evidence (for testing).
func (c *Configurator) SetHostRoots(sysRoot, procRoot string) {
	c.hostSysRoot, c.hostProcRoot = sysRoot, procRoot
	c.serialConsole, c.serialConsoleErr, c.serialConsoleDone = serialconsole.Resolution{}, nil, false
}

// SetHostname writes the hostname to /etc/hostname.
func (c *Configurator) SetHostname(cfg *config.MachineConfig) error {
	path := filepath.Join(c.rootDir, "etc", "hostname")
	slog.Info("setting hostname", "hostname", cfg.Hostname, "path", path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating hostname dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(cfg.Hostname+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing hostname: %w", err)
	}
	return nil
}

// ConfigureKubelet writes kubeadm-compatible kubelet extra args.
func (c *Configurator) ConfigureKubelet(cfg *config.MachineConfig) error {
	if cfg == nil {
		return errors.New("configuring kubelet requires machine config")
	}
	if err := config.ValidateKubeletProvisionFields(&cfg.Provision); err != nil {
		return err
	}
	args := kubeletExtraArgs(cfg)
	if len(args) == 0 {
		return nil
	}

	path, err := c.kubeletEnvPath(cfg)
	if err != nil {
		return err
	}
	slog.Info("writing kubelet extra args", "path", path)
	return updateKubeletExtraArgs(c.rootDir, path, args)
}

func kubeletExtraArgs(cfg *config.MachineConfig) []string {
	if cfg == nil {
		return nil
	}
	var labels []string
	if cfg.Provision.FailureDomain != "" {
		labels = append(labels, "topology.kubernetes.io/zone="+cfg.Provision.FailureDomain)
	}
	if cfg.Provision.Region != "" {
		labels = append(labels, "topology.kubernetes.io/region="+cfg.Provision.Region)
	}

	var args []string
	if cfg.Provision.ProviderID != "" {
		args = append(args, "--provider-id="+cfg.Provision.ProviderID)
	}
	if len(labels) > 0 {
		args = append(args, "--node-labels="+strings.Join(labels, ","))
	}
	return args
}

func (c *Configurator) kubeletEnvPath(cfg *config.MachineConfig) (string, error) {
	debPath := filepath.Join(c.rootDir, "etc", "default", "kubelet")
	rpmPath := filepath.Join(c.rootDir, "etc", "sysconfig", "kubelet")
	for _, path := range []string{debPath, rpmPath} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}

	family := targetKubeletFamily(c.rootDir, cfg)
	switch family {
	case "debian":
		return debPath, nil
	case "rpm":
		return rpmPath, nil
	case "flatcar":
		return "", fmt.Errorf("configuring kubelet extra args is unsupported for flatcar targets")
	default:
		return "", fmt.Errorf("configuring kubelet extra args requires /etc/default/kubelet, /etc/sysconfig/kubelet, or supported /etc/os-release")
	}
}

func targetKubeletFamily(rootDir string, cfg *config.MachineConfig) string {
	if cfg != nil {
		switch strings.ToLower(strings.TrimSpace(cfg.OSFamily)) {
		case "ubuntu":
			return "debian"
		case "rhel":
			return "rpm"
		case "flatcar":
			return "flatcar"
		}
	}
	return kubeletFamilyFromOSRelease(filepath.Join(rootDir, "etc", "os-release"))
}

func kubeletFamilyFromOSRelease(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // target root metadata
	if err != nil {
		return ""
	}
	fields := parseOSReleaseFields(string(data))
	for _, value := range append([]string{fields["ID"]}, strings.Fields(fields["ID_LIKE"])...) {
		switch strings.ToLower(value) {
		case "ubuntu", "debian":
			return "debian"
		case "rhel", "fedora", "centos", "rocky", "almalinux", "sles", "suse", "opensuse":
			return "rpm"
		case "flatcar":
			return "flatcar"
		}
	}
	return ""
}

func parseOSReleaseFields(content string) map[string]string {
	fields := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		fields[key] = value
	}
	return fields
}

func updateKubeletExtraArgs(rootDir, path string, managed []string) error {
	content, err := os.ReadFile(path) //nolint:gosec // target root config
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading kubelet env file: %w", err)
	}
	if err := ensureWithinRoot(rootDir, path); err != nil {
		return fmt.Errorf("validating kubelet env path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating kubelet env dir: %w", err)
	}
	if err := ensureTargetParentWithinRoot(rootDir, filepath.Dir(path)); err != nil {
		return fmt.Errorf("validating kubelet env dir: %w", err)
	}

	lines := splitConfigLines(string(content))
	replaced := false
	for i, line := range lines {
		if !isKubeletExtraArgsLine(line) {
			continue
		}
		lines[i] = formatKubeletExtraArgs(mergeKubeletArgs(line, managed))
		replaced = true
	}
	if !replaced {
		lines = append(lines, formatKubeletExtraArgs(managed))
	}

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil { //nolint:gosec // path is constrained to target root above
		return fmt.Errorf("writing kubelet env file: %w", err)
	}
	return nil
}

func splitConfigLines(content string) []string {
	content = strings.TrimRight(content, "\n")
	if content == "" {
		return nil
	}
	return strings.Split(content, "\n")
}

func isKubeletExtraArgsLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "KUBELET_EXTRA_ARGS=")
}

func mergeKubeletArgs(line string, managed []string) []string {
	_, value, _ := strings.Cut(line, "=")
	value = strings.TrimSpace(value)
	if unquoted, err := strconv.Unquote(value); err == nil {
		value = unquoted
	} else {
		value = strings.Trim(value, `"'`)
	}

	var merged []string
	var labels []string
	for _, arg := range strings.Fields(value) {
		switch {
		case strings.HasPrefix(arg, "--provider-id="):
			continue
		case strings.HasPrefix(arg, "--node-labels="):
			labels = append(labels, keepExistingNodeLabels(arg)...)
		default:
			merged = append(merged, arg)
		}
	}

	for _, arg := range managed {
		if strings.HasPrefix(arg, "--node-labels=") {
			labels = append(labels, splitNodeLabels(strings.TrimPrefix(arg, "--node-labels="))...)
			continue
		}
		merged = append(merged, arg)
	}
	if len(labels) > 0 {
		merged = append(merged, "--node-labels="+strings.Join(labels, ","))
	}
	return merged
}

func keepExistingNodeLabels(arg string) []string {
	labels := splitNodeLabels(strings.TrimPrefix(arg, "--node-labels="))
	kept := labels[:0]
	for _, label := range labels {
		if strings.HasPrefix(label, "topology.kubernetes.io/zone=") ||
			strings.HasPrefix(label, "topology.kubernetes.io/region=") {
			continue
		}
		kept = append(kept, label)
	}
	return kept
}

func splitNodeLabels(value string) []string {
	var labels []string
	for _, label := range strings.Split(value, ",") {
		if label == "" {
			continue
		}
		labels = append(labels, label)
	}
	return labels
}

func formatKubeletExtraArgs(args []string) string {
	return "KUBELET_EXTRA_ARGS=" + strconv.Quote(strings.Join(args, " "))
}

// ConfigureGRUB writes GRUB kernel parameters and runs update-grub via chroot.
//
// With CAPRF_LEGACY_COMPAT it follows the legacy provisioner instead: it
// appends "ds=nocloud console=ttyS0,115200n8" (ttyS1 on Lenovo) and the
// extra parameters verbatim, so several console= tokens can appear, and it
// skips update-grub when the image has none. The rest of this comment
// describes the default mode.
//
// The console parameter comes from the shared serial console resolver, which
// replaces the former vendor heuristic ("Lenovo means ttyS1"). Exactly one
// console= parameter is emitted: operator-supplied console tokens are folded
// into the resolution instead of being appended a second time.
func (c *Configurator) ConfigureGRUB(ctx context.Context, cfg *config.MachineConfig) error {
	if cfg != nil && cfg.LegacyCompat {
		return c.configureGRUBLegacy(ctx, cfg)
	}
	grubLine, consoleParam, err := c.grubCmdlineEntry(cfg)
	if err != nil {
		return err
	}

	// targetPath refuses parents that leave the target root through
	// symlinks, and writeFileAtomic replaces a symlink at the target path
	// instead of following it. This step reruns on resume, after machine
	// files with symlinks may already be in place.
	grubPath, err := c.targetPath("/etc/default/grub.d/10-caprf-kernel-params.cfg")
	if err != nil {
		return fmt.Errorf("grub drop-in path: %w", err)
	}
	slog.Info("writing GRUB config", "path", grubPath, "console", consoleParam)
	if err := writeFileAtomic(grubPath, []byte(grubLine), 0o644); err != nil {
		return fmt.Errorf("writing grub config: %w", err)
	}

	if _, err := c.targetPath("/boot/grub/grub.cfg"); err != nil {
		return fmt.Errorf("creating boot grub dir: %w", err)
	}

	// Run update-grub in chroot.
	out, err := c.disk.ChrootRun(ctx, c.rootDir, "update-grub")
	if err != nil {
		return fmt.Errorf("update-grub: %s: %w", string(out), err)
	}
	return nil
}

// grubCmdlineEntry renders the GRUB drop-in content and the console parameter
// it carries.
func (c *Configurator) grubCmdlineEntry(cfg *config.MachineConfig) (line, consoleParam string, err error) {
	cloudInitDatasourceParam, err := cloudInitKernelDatasourceParam(cfg)
	if err != nil {
		return "", "", err
	}
	// Validate operator input before touching firmware evidence so unsafe
	// parameters are reported as such instead of as a console failure.
	consoleParam, err = c.serialConsoleKernelParam(cfg)
	if err != nil {
		return "", "", err
	}
	extraParams, err := resolvedExtraKernelParams(cfg, consoleParam == "")
	if err != nil {
		return "", "", err
	}

	kernelParams := make([]string, 0, 2)
	if cloudInitDatasourceParam != "" {
		kernelParams = append(kernelParams, cloudInitDatasourceParam)
	}
	if consoleParam != "" {
		kernelParams = append(kernelParams, consoleParam)
	}

	grubLine := fmt.Sprintf("GRUB_CMDLINE_LINUX=\"%s", strings.Join(kernelParams, " "))
	if extraParams != "" {
		grubLine += " " + extraParams
	}
	abRootParam, err := abRootKernelParam(cfg)
	if err != nil {
		return "", "", err
	}
	if abRootParam != "" {
		grubLine += " " + abRootParam
	}
	return grubLine + "\"\n" + grubSerialTerminalConfig(&c.serialConsole), consoleParam, nil
}

// resolvedExtraKernelParams validates operator kernel parameters and removes
// console tokens already represented by the resolved console.
//
// Console tokens are validated against the strict console grammar rather than
// the generic parameter pattern, because a legitimate console value such as
// ttyS1,115200n8 contains a comma.
func resolvedExtraKernelParams(cfg *config.MachineConfig, preserveVirtualConsole bool) (string, error) {
	if cfg == nil || cfg.Provision.ExtraKernelParams == "" {
		return "", nil
	}
	kept, dropped := stripConsoleParams(cfg.Provision.ExtraKernelParams)
	virtualConsoleKept := false
	for _, param := range dropped {
		if err := validateConsoleKernelParam(param); err != nil {
			return "", err
		}
		if !preserveVirtualConsole {
			continue
		}
		value := strings.TrimPrefix(strings.TrimPrefix(param, "console="), "/dev/")
		if !virtualTerminalParam.MatchString(value) {
			continue
		}
		if virtualConsoleKept {
			return "", fmt.Errorf("multiple virtual terminal console parameters are unsupported")
		}
		if kept != "" {
			kept += " "
		}
		kept += param
		virtualConsoleKept = true
	}
	if !safeKernelParams.MatchString(kept) {
		return "", fmt.Errorf("unsafe characters in ExtraKernelParams: %q", cfg.Provision.ExtraKernelParams)
	}
	if len(dropped) > 0 {
		slog.Info("folded operator console parameters into serial console resolution",
			"dropped", strings.Join(dropped, " "))
	}
	return kept, nil
}

// validateConsoleKernelParam accepts serial console values and the kernel
// virtual terminals, and rejects anything else before it can reach the
// resolver or a log line.
func validateConsoleKernelParam(param string) error {
	value := strings.TrimPrefix(param, "console=")
	value = strings.TrimPrefix(value, "/dev/")
	if virtualTerminalCandidate.MatchString(value) {
		if virtualTerminalParam.MatchString(value) {
			return nil
		}
		return fmt.Errorf("unsupported virtual terminal console parameter: %q", param)
	}
	if _, err := serialconsole.ParseSpec(value); err != nil {
		return fmt.Errorf("unsupported console parameter in ExtraKernelParams %q: %w", param, err)
	}
	return nil
}

// grubSerialTerminalConfig points GRUB itself at the resolved port so the
// bootloader menu and the kernel share one console.
func grubSerialTerminalConfig(resolution *serialconsole.Resolution) string {
	if resolution.Console == nil {
		return ""
	}
	unit, ok := resolution.Console.GRUBSerialUnit()
	if !ok {
		return ""
	}
	return fmt.Sprintf("GRUB_TERMINAL=\"serial console\"\nGRUB_SERIAL_COMMAND=\"serial --unit=%d --speed=%d\"\n",
		unit, resolution.Console.Baud)
}

func cloudInitKernelDatasourceParam(cfg *config.MachineConfig) (string, error) {
	if cfg == nil || !cfg.Provision.CloudInit.Enabled {
		return "", nil
	}
	datasource := strings.ToLower(strings.TrimSpace(cfg.Provision.CloudInit.Datasource))
	switch datasource {
	case "", "nocloud":
		return "ds=nocloud", nil
	case "configdrive":
		return "ci.datasource=ConfigDrive", nil
	default:
		return "", fmt.Errorf("unsupported cloud-init datasource %q", cfg.Provision.CloudInit.Datasource)
	}
}

func abRootKernelParam(cfg *config.MachineConfig) (string, error) {
	if cfg == nil || !strings.EqualFold(strings.TrimSpace(cfg.Provision.Image.Mode), config.ImageModeAB) {
		return "", nil
	}
	ab := cfg.Provision.AB.WithDefaults()
	target, err := ab.ResolvedTargetSlot()
	if err != nil {
		return "", fmt.Errorf("resolve A/B target slot for GRUB root: %w", err)
	}
	switch target {
	case config.ABSlotA:
		return abRootKernelParamForSlot(config.ABSlotA, ab.Scheme), nil
	case config.ABSlotB:
		return abRootKernelParamForSlot(config.ABSlotB, ab.Scheme), nil
	default:
		return "", fmt.Errorf("invalid resolved A/B target slot %q", target)
	}
}

func abRootKernelParamForSlot(slot, scheme string) string {
	rootParam := fmt.Sprintf("root=PARTLABEL=BOOTY-ROOT-%s", strings.ToUpper(slot))
	if scheme == config.ABSchemeSystemAB {
		return rootParam + " ro"
	}
	return rootParam
}

// InstallEFIFallbackLoader installs a removable UEFI loader into /boot/efi
// without writing firmware NVRAM. BOOTy first uses its bundled standalone
// loader so minimal immutable target roots do not need grub-efi packages. If
// the asset is unavailable, it falls back to the target-root grub-install path
// to preserve compatibility with older initramfs builds.
func (c *Configurator) InstallEFIFallbackLoader(ctx context.Context, diskDev, rootDev string) error {
	var err error
	diskDev, err = validateChrootDevicePath("EFI fallback disk device", diskDev)
	if err != nil {
		return err
	}
	err = c.installBundledEFIFallbackLoader(rootDev)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errEFIFallbackAssetMissing) || !installEFIFallbackWithChroot {
		return err
	}
	slog.Warn("bundled EFI fallback loader unavailable, falling back to target-root grub-install", "error", err)

	target, err := grubEFITarget(runtime.GOARCH)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf(
		"grub-install --target=%s --efi-directory=/boot/efi --bootloader-id=%s --removable --no-nvram --recheck %s",
		target,
		efiFallbackBootloaderID(c.rootDir),
		diskDev,
	)
	out, err := c.disk.ChrootRun(ctx, c.rootDir, cmd)
	if err != nil {
		return fmt.Errorf("install EFI fallback loader: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *Configurator) installBundledEFIFallbackLoader(rootDev string) error {
	loaderName, err := efiRemovableLoaderName(runtime.GOARCH)
	if err != nil {
		return err
	}
	assetPath := filepath.Join(efiFallbackAssetDirectory, loaderName)
	if _, err := os.Stat(assetPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", errEFIFallbackAssetMissing, assetPath)
		}
		return fmt.Errorf("stat bundled EFI fallback loader %s: %w", assetPath, err)
	}
	targetGrub := filepath.Join(c.rootDir, "boot", "grub", "grub.cfg")
	if _, err := os.Stat(targetGrub); err != nil {
		return fmt.Errorf("target GRUB config %s is required for EFI fallback handoff: %w", targetGrub, err)
	}

	handoffID, err := newEFIFallbackHandoffID()
	if err != nil {
		return fmt.Errorf("creating EFI fallback handoff id: %w", err)
	}
	markerRel := filepath.Join("etc", "booty", "grub-target-"+handoffID)
	markerPath := filepath.Join(c.rootDir, markerRel)
	if err := ensureTargetParentWithinRoot(c.rootDir, filepath.Dir(markerPath)); err != nil {
		return fmt.Errorf("EFI fallback marker directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
		return fmt.Errorf("creating EFI fallback marker directory: %w", err)
	}
	if err := writeFileAtomic(markerPath, []byte(rootDev+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing EFI fallback marker: %w", err)
	}

	efiBootDir := filepath.Join(c.rootDir, "boot", "efi", "EFI", "BOOT")
	if err := ensureTargetParentWithinRoot(c.rootDir, efiBootDir); err != nil {
		return fmt.Errorf("EFI fallback directory: %w", err)
	}
	if err := os.MkdirAll(efiBootDir, 0o755); err != nil {
		return fmt.Errorf("creating EFI fallback directory: %w", err)
	}
	if err := copyRegularFile(assetPath, filepath.Join(efiBootDir, loaderName), 0o644); err != nil {
		return fmt.Errorf("copy bundled EFI fallback loader: %w", err)
	}

	markerGRUBPath := "/" + filepath.ToSlash(markerRel)
	grubConfig := fmt.Sprintf("search --no-floppy --set=booty_root --file %s\nset prefix=($booty_root)/boot/grub\nconfigfile ($booty_root)/boot/grub/grub.cfg\n", markerGRUBPath)
	if err := writeFileAtomic(filepath.Join(efiBootDir, "grub.cfg"), []byte(grubConfig), 0o644); err != nil {
		return fmt.Errorf("writing EFI fallback grub config: %w", err)
	}
	slog.Info("installed bundled EFI fallback loader", "loader", filepath.Join(efiBootDir, loaderName), "root", rootDev, "marker", markerGRUBPath)
	return nil
}

func validateChrootDevicePath(name, value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", strings.ToLower(name))
	}
	if trimmed != value || strings.Contains(trimmed, "..") || !safeChrootDevicePath.MatchString(trimmed) {
		return "", fmt.Errorf("unsafe %s %q", name, value)
	}
	return trimmed, nil
}

func defaultEFIFallbackHandoffID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("read random EFI fallback handoff id: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source file %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".booty-copy-*")
	if err != nil {
		return fmt.Errorf("create temporary file next to %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy %s to temporary file %s: %w", src, tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file %s: %w", tmpName, err)
	}
	// #nosec G703 -- caller validates the destination is inside the target root.
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename temporary file %s to %s: %w", tmpName, dst, err)
	}
	return nil
}

func efiRemovableLoaderName(arch string) (string, error) {
	switch arch {
	case "amd64":
		return "BOOTX64.EFI", nil
	case "arm64":
		return "BOOTAA64.EFI", nil
	default:
		return "", fmt.Errorf("unsupported architecture for EFI fallback loader: %s", arch)
	}
}

func grubEFITarget(arch string) (string, error) {
	switch arch {
	case "amd64":
		return "x86_64-efi", nil
	case "arm64":
		return "arm64-efi", nil
	default:
		return "", fmt.Errorf("unsupported architecture for EFI fallback loader: %s", arch)
	}
}

// CopyProvisionerFiles copies files from /deploy/file-system/ to the root.
func (c *Configurator) CopyProvisionerFiles(ctx context.Context) error {
	return c.copyTreeIntoChroot(ctx, provisionerFilesDir, "provisioner files")
}

// SetProvisionerFilesDirForTest points the provisioner files directory at
// dir and returns a function that restores it. Tests only.
func SetProvisionerFilesDirForTest(dir string) func() {
	old := provisionerFilesDir
	provisionerFilesDir = dir
	return func() { provisionerFilesDir = old }
}

// HasProvisionerResolvConf reports whether the CAPRF provisioner files carry
// an /etc/resolv.conf for the ramdisk. Like copyTree it does not follow
// symlinks: a linked etc directory or resolv.conf is never applied.
func HasProvisionerResolvConf() bool {
	etc, err := os.Lstat(filepath.Join(provisionerFilesDir, "etc"))
	if err != nil || !etc.IsDir() {
		return false
	}
	info, err := os.Lstat(filepath.Join(provisionerFilesDir, "etc", "resolv.conf"))
	return err == nil && info.Mode().IsRegular()
}

// ApplyProvisionerFilesToRamdisk copies the CAPRF provisioner files from
// /deploy/file-system/ into the running ramdisk, like copy_files in the CAPRF
// legacy common.sh. Netplan files get mode 0600, as in the legacy script.
func ApplyProvisionerFilesToRamdisk(ctx context.Context) error {
	if _, err := os.Stat(provisionerFilesDir); os.IsNotExist(err) {
		slog.Info("no provisioner files to apply to the ramdisk", "path", provisionerFilesDir)
		return nil
	}
	slog.Info("applying provisioner files to the ramdisk", "src", provisionerFilesDir, "dst", ramdiskRoot)
	if err := copyTree(ctx, provisionerFilesDir, ramdiskRoot); err != nil {
		return err
	}
	netplanSrc := filepath.Join(provisionerFilesDir, "etc", "netplan")
	entries, err := os.ReadDir(netplanSrc)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", netplanSrc, err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		dst := filepath.Join(ramdiskRoot, "etc", "netplan", entry.Name())
		if err := os.Chmod(dst, 0o600); err != nil {
			return fmt.Errorf("chmod %s: %w", dst, err)
		}
	}
	return nil
}

// CopyMachineFiles copies files from /deploy/machine-files/ to the root.
// Symlinks are kept as links, like `cp -rf` in the CAPRF legacy
// provisioner. This step runs after every host-side writer into the root,
// so a link cannot redirect a later BOOTy write.
func (c *Configurator) CopyMachineFiles(ctx context.Context) error {
	const srcBase = "/deploy/machine-files"
	if _, err := os.Stat(srcBase); os.IsNotExist(err) {
		slog.Info("no directory found", "label", "machine files", "path", srcBase)
		return nil
	}
	slog.Info("copying files", "label", "machine files", "src", srcBase)
	return copyTreeWith(ctx, srcBase, c.rootDir, true)
}

// chrootBindDirs are bind-mounted from the ramdisk while the chroot is set
// up. Machine files below them would land in the live ramdisk, not in the
// installed system, so they are skipped.
var chrootBindDirs = map[string]bool{"dev": true, "proc": true, "sys": true, "run": true}

// copyTreeIntoChroot copies all files from srcBase into the chroot root.
// If srcBase does not exist, it logs and returns nil.
func (c *Configurator) copyTreeIntoChroot(ctx context.Context, srcBase, label string) error {
	if _, err := os.Stat(srcBase); os.IsNotExist(err) {
		slog.Info("no directory found", "label", label, "path", srcBase)
		return nil
	}
	slog.Info("copying files", "label", label, "src", srcBase)
	return copyTree(ctx, srcBase, c.rootDir)
}

// copyTree copies all files from srcBase into destRoot, preserving directory structure.
// Symlinks are skipped with a warning; paths that escape destRoot are rejected to prevent path traversal.
func copyTree(ctx context.Context, srcBase, destRoot string) error {
	return copyTreeWith(ctx, srcBase, destRoot, false)
}

// copyTreeWith copies srcBase into destRoot. With keepSymlinks, symlinks are
// recreated as links and never followed; otherwise they are skipped.
func copyTreeWith(ctx context.Context, srcBase, destRoot string, keepSymlinks bool) error {
	destRoot, err := filepath.Abs(destRoot)
	if err != nil {
		return fmt.Errorf("resolve dest root: %w", err)
	}
	validatedParents := make(map[string]struct{})
	if err := filepath.WalkDir(srcBase, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("copy tree canceled: %w", err)
		}
		// DirEntry.Type can be zero on some filesystems, so use Lstat info.
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		isSymlink := info.Mode()&os.ModeSymlink != 0
		if skip, skipErr := skipCopyTreeEntry(srcBase, path, d, isSymlink, keepSymlinks); skip {
			return skipErr
		}
		destPath, err := copyTreeDestination(srcBase, destRoot, path, validatedParents)
		if err != nil {
			return err
		}
		if skip, skipErr := skipBindMountAlias(destRoot, destPath, d, keepSymlinks); skip {
			return skipErr
		}
		if isSymlink {
			return copyKeptSymlink(path, destPath)
		}
		if d.IsDir() {
			return os.MkdirAll(destPath, 0o755) //nolint:gosec // destPath and its parent are checked by copyTreeDestination
		}
		return copyFile(ctx, path, destPath)
	}); err != nil {
		return fmt.Errorf("copy tree from %s: %w", srcBase, err)
	}
	return nil
}

// skipCopyTreeEntry reports whether copyTreeWith skips path. A skipped
// directory returns filepath.SkipDir as the error.
func skipCopyTreeEntry(srcBase, path string, d os.DirEntry, isSymlink, keepSymlinks bool) (bool, error) {
	if !keepSymlinks {
		if isSymlink {
			// Skip symlinks with a warning to prevent following links that escape the tree.
			slog.Warn("skipping symlink in copy tree", "path", path)
			return true, nil
		}
		return false, nil
	}
	rel, err := filepath.Rel(srcBase, path)
	if err != nil {
		return false, fmt.Errorf("resolve copy path %s relative to %s: %w", path, srcBase, err)
	}
	if !chrootBindDirs[strings.SplitN(rel, string(filepath.Separator), 2)[0]] {
		return false, nil
	}
	slog.Warn("skipping machine file below a chroot bind mount", "path", path)
	if d.IsDir() {
		return true, filepath.SkipDir
	}
	return true, nil
}

// skipBindMountAlias reports whether destPath, after resolving the existing
// symlinks in the target, lands at or below a chroot bind mount. This
// catches an image link such as alias -> dev, which the lexical check in
// skipCopyTreeEntry cannot see. A skipped directory returns
// filepath.SkipDir as the error.
func skipBindMountAlias(destRoot, destPath string, d os.DirEntry, keepSymlinks bool) (bool, error) {
	if !keepSymlinks || destPath == destRoot {
		return false, nil
	}
	existing, suffix := filepath.Dir(destPath), filepath.Base(destPath)
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return true, fmt.Errorf("inspect copy destination %s: %w", existing, err)
		}
		existing, suffix = filepath.Dir(existing), filepath.Join(filepath.Base(existing), suffix)
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return true, fmt.Errorf("resolve copy destination %s: %w", existing, err)
	}
	realRoot, err := filepath.EvalSymlinks(destRoot)
	if err != nil {
		return true, fmt.Errorf("resolve copy root %s: %w", destRoot, err)
	}
	rel, err := filepath.Rel(realRoot, filepath.Join(realExisting, suffix))
	if err != nil {
		return true, fmt.Errorf("resolve copy destination %s: %w", destPath, err)
	}
	if !chrootBindDirs[strings.SplitN(rel, string(filepath.Separator), 2)[0]] {
		return false, nil
	}
	slog.Warn("skipping machine file that resolves below a chroot bind mount", "path", destPath)
	if d.IsDir() {
		return true, filepath.SkipDir
	}
	return true, nil
}

// copyKeptSymlink recreates a link, but like cp never replaces a directory.
func copyKeptSymlink(path, destPath string) error {
	if existing, err := os.Lstat(destPath); err == nil && existing.IsDir() {
		return fmt.Errorf("cannot replace directory %s with symlink %s", destPath, path)
	}
	return copySymlink(path, destPath)
}

// copyTreeDestination maps a source path to its destination under destRoot
// and checks that the destination and its parent stay inside destRoot.
func copyTreeDestination(srcBase, destRoot, path string, validatedParents map[string]struct{}) (string, error) {
	relPath, err := filepath.Rel(srcBase, path)
	if err != nil {
		return "", fmt.Errorf("resolve copy path %s relative to %s: %w", path, srcBase, err)
	}
	destPath := filepath.Join(destRoot, relPath)
	if err := ensureWithinRoot(destRoot, destPath); err != nil {
		return "", fmt.Errorf("validating copy destination %s: %w", destPath, err)
	}
	if relPath == "." {
		return destPath, nil
	}
	parent := filepath.Dir(destPath)
	if _, ok := validatedParents[parent]; !ok {
		if err := ensureTargetParentWithinRoot(destRoot, parent); err != nil {
			return "", fmt.Errorf("validating copy destination parent %s: %w", parent, err)
		}
		validatedParents[parent] = struct{}{}
	}
	return destPath, nil
}

// RunMachineCommands executes commands from /deploy/machine-commands/ in
// chroot, in the order of the CAPRF MachineConfig.Commands list.
//
// By default each file must hold one command without shell metacharacters.
// With legacyCompat each file runs as a script, like run_script_in_chroot in
// the CAPRF legacy provision.sh: a file with a "#!" line runs with that
// interpreter, any other file runs with /bin/sh.
func (c *Configurator) RunMachineCommands(ctx context.Context, legacyCompat bool) error {
	entries, err := os.ReadDir(machineCommandsDir)
	if os.IsNotExist(err) {
		slog.Info("no machine commands directory found", "path", machineCommandsDir)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading machine-commands dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	slices.SortStableFunc(names, compareMachineCommandNames)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(machineCommandsDir, name))
		if err != nil {
			return fmt.Errorf("reading command file %s: %w", name, err)
		}
		if legacyCompat {
			err = c.runMachineCommandScript(ctx, name, data)
		} else {
			err = c.runMachineCommand(ctx, name, string(data))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Configurator) runMachineCommand(ctx context.Context, name, content string) error {
	cmd := strings.TrimSpace(content)
	if cmd == "" {
		return nil
	}
	if err := validateProvisionCommand(cmd); err != nil {
		return fmt.Errorf("machine command %s rejected: %w", name, err)
	}
	slog.Info("running machine command", "file", name, "command", redactCommand(cmd))
	out, err := c.disk.ChrootRun(ctx, c.rootDir, cmd)
	if err != nil {
		return fmt.Errorf("machine command %s: %s: %w", name, redactCommand(string(out)), err)
	}
	return nil
}

// runMachineCommandScript copies the script into the target root, runs it
// there and removes it again.
func (c *Configurator) runMachineCommandScript(ctx context.Context, name string, content []byte) (retErr error) {
	if strings.TrimSpace(string(content)) == "" {
		return nil
	}
	scriptPath := filepath.Join(machineCommandsChrootDir, name)
	// os.Root keeps every path operation inside the target root, also when
	// the image or the script itself plants symlinks.
	root, err := os.OpenRoot(c.rootDir)
	if err != nil {
		return fmt.Errorf("machine command %s: open target root: %w", name, err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	rel := strings.TrimPrefix(scriptPath, "/")
	if err := writeScriptInRoot(root, rel, content); err != nil {
		return fmt.Errorf("machine command %s: write script: %w", name, err)
	}
	defer func() {
		// The script may hold credentials: a leftover copy fails the step.
		if err := root.Remove(rel); err != nil && !os.IsNotExist(err) {
			retErr = errors.Join(retErr, fmt.Errorf("machine command %s: remove staged script: %w", name, err))
		}
	}()

	argv := []string{scriptPath}
	if !bytes.HasPrefix(content, []byte("#!")) {
		argv = []string{"/bin/sh", scriptPath}
	}
	slog.Info("running machine command script", "file", name)
	// Stream the output to the log while the script runs, like the legacy
	// `run_script_in_chroot ... | while read line; do log ...; done`.
	err = runStreamingCommand(ctx, func(line string) {
		slog.Info("machine command output", "file", name, "line", redactCommand(line))
	}, c.rootDir, argv)
	if err != nil {
		return &redactedCommandError{msg: fmt.Sprintf("machine command %s: %s", name, redactCommand(err.Error())), err: err}
	}
	return nil
}

// redactedCommandError shows a redacted message but keeps the cause for
// errors.Is and errors.As, for example context.Canceled.
type redactedCommandError struct {
	msg string
	err error
}

func (e *redactedCommandError) Error() string { return e.msg }

func (e *redactedCommandError) Unwrap() error { return e.err }

// writeScriptInRoot replaces rel inside root with a new executable file.
func writeScriptInRoot(root *os.Root, rel string, content []byte) error {
	// The staging directory lives in /tmp. Create a missing /tmp with the
	// usual shared sticky mode, never root-only.
	if _, err := root.Stat("tmp"); os.IsNotExist(err) {
		if err := root.Mkdir("tmp", 0o755); err != nil {
			return fmt.Errorf("create /tmp: %w", err)
		}
		if err := root.Chmod("tmp", os.ModeSticky|0o777); err != nil {
			return fmt.Errorf("chmod /tmp: %w", err)
		}
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(rel), err)
	}
	if err := root.Remove(rel); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s: %w", rel, err)
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = root.Remove(rel)
		return fmt.Errorf("write %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(rel)
		return fmt.Errorf("close %s: %w", rel, err)
	}
	return nil
}

// compareMachineCommandNames orders CAPRF "commands-<index>.sh" files by
// index, so commands-10.sh runs after commands-2.sh. Other names sort after
// them by name.
func compareMachineCommandNames(a, b string) int {
	ai, aok := machineCommandIndex(a)
	bi, bok := machineCommandIndex(b)
	switch {
	case aok && bok:
		return cmp.Compare(ai, bi)
	case aok:
		return -1
	case bok:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func machineCommandIndex(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, "commands-")
	if !ok {
		return 0, false
	}
	digits, ok = strings.CutSuffix(digits, ".sh")
	if !ok {
		return 0, false
	}
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

// RunPostProvisionCmds executes custom commands in the chroot after provisioning.
// Each command is run via /bin/bash -c in the chroot environment.
func (c *Configurator) RunPostProvisionCmds(ctx context.Context, cmds []string) error {
	for i, cmd := range cmds {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		if err := validateProvisionCommand(cmd); err != nil {
			return fmt.Errorf("post-provision cmd %d rejected: %w", i, err)
		}
		slog.Info("running post-provision command", "index", i, "command", redactCommand(cmd))
		out, err := c.disk.ChrootRun(ctx, c.rootDir, cmd)
		if err != nil {
			return fmt.Errorf("post-provision cmd %d (%s): %s: %w", i, redactCommand(cmd), redactCommand(string(out)), err)
		}
		if len(out) > 0 {
			slog.Debug("post-provision command output", "index", i, "output", redactCommand(string(out)))
		}
	}
	return nil
}

func validateProvisionCommand(cmd string) error {
	for _, token := range blockedShellTokens {
		if strings.Contains(cmd, token) {
			return fmt.Errorf("contains blocked shell token %q", token)
		}
	}
	if !safeProvisionCommand.MatchString(cmd) {
		return fmt.Errorf("contains unsupported characters")
	}
	return nil
}

// ConfigureDNS writes resolv.conf to the chroot.
func (c *Configurator) ConfigureDNS(cfg *config.MachineConfig) error {
	if cfg.Network.DNSResolvers == "" {
		return nil
	}
	lines, err := network.ResolverLines(cfg.Network.DNSResolvers)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		return nil
	}
	etcDir := filepath.Join(c.rootDir, "etc")
	if _, err := os.Stat(etcDir); err != nil {
		if os.IsNotExist(err) {
			slog.Warn("root /etc/ not mounted, skipping DNS configuration", "path", etcDir)
			return nil
		}
		return fmt.Errorf("stat etc dir %s: %w", etcDir, err)
	}
	path := filepath.Join(etcDir, "resolv.conf")
	slog.Info("configuring DNS", "resolvers", cfg.Network.DNSResolvers)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace resolv.conf symlink: %w", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat resolv.conf: %w", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing resolv.conf: %w", err)
	}
	return nil
}

// MountEFIVars loads the efivarfs kernel module and mounts the efivarfs
// filesystem at /sys/firmware/efi/efivars if not already mounted.
// This is required before any efibootmgr operations.
func (c *Configurator) MountEFIVars(ctx context.Context) error {
	// Load the efivarfs module (best-effort — may already be built-in).
	if out, err := hostCommandCombinedOutput(ctx, "modprobe", "efivarfs"); err != nil {
		slog.Info("modprobe efivarfs failed (may be built-in)", "output", strings.TrimSpace(string(out)))
	}

	// Check if already mounted.
	if isMountPoint(efiVarsPath) {
		slog.Info("efivarfs already mounted")
		return nil
	}

	// On non-EFI systems /sys/firmware/efi does not exist; skip gracefully.
	if _, err := os.Stat(efiFirmwarePath); os.IsNotExist(err) {
		slog.Info("non-EFI system detected, skipping efivarfs mount")
		return nil
	}

	if err := os.MkdirAll(efiVarsPath, 0o755); err != nil {
		slog.Warn("create efivarfs mountpoint failed, continuing without EFI variable access", "error", err, "path", efiVarsPath)
		return nil
	}
	if err := syscall.Mount("efivarfs", efiVarsPath, "efivarfs", 0, ""); err != nil {
		slog.Warn("mount efivarfs failed, continuing without EFI variable access", "error", err, "path", efiVarsPath)
		return nil
	}
	slog.Info("mounted efivarfs", "path", efiVarsPath)
	return nil
}

func defaultHostCommandCombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // host command names are fixed by callers
	if err != nil {
		return out, fmt.Errorf("run host command %s: %w", name, err)
	}
	return out, nil
}

func defaultEFIRuntimeReady() (ready bool, reason string) {
	if _, err := os.Stat(efiFirmwarePath); os.IsNotExist(err) {
		return false, "system not booted in EFI mode"
	} else if err != nil {
		return false, fmt.Sprintf("cannot stat %s: %v", efiFirmwarePath, err)
	}
	if !isMountPoint(efiVarsPath) {
		return false, "efivarfs not mounted"
	}
	return true, ""
}

// defaultIsMountPoint checks whether a path is already a mount point by reading /proc/mounts.
func defaultIsMountPoint(path string) bool {
	_, mounted := defaultMountedSource(path)
	return mounted
}

func defaultMountedSource(path string) (string, bool) {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == path {
			return fields[0], true
		}
	}
	return "", false
}

// RemoveEFIBootEntries removes old EFI boot entries matching supported Linux loaders.
// Runs efibootmgr directly on the host (not in chroot) since it operates
// on the host's EFI variables via /sys/firmware/efi/efivars.
func (c *Configurator) RemoveEFIBootEntries(ctx context.Context) error {
	slog.Info("removing old EFI boot entries")
	out, err := hostCommandCombinedOutput(ctx, "efibootmgr")
	if err != nil {
		slog.Warn("efibootmgr list failed (non-EFI system?)", "output", string(out), "error", err)
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !isManagedEFIBootEntryLine(line) {
			continue
		}
		bootNum := line[4:8]
		slog.Info("removing EFI boot entry", "entry", bootNum)
		if out, err := hostCommandCombinedOutput(ctx, "efibootmgr", "-b", bootNum, "-B"); err != nil {
			slog.Warn("failed to remove EFI entry", "entry", bootNum, "output", string(out))
		}
	}
	return nil
}

// RemoveAllEFIBootEntries deletes every entry in the EFI BootOrder on a
// best-effort basis, like remove_all_efi_boot_entries in the CAPRF legacy
// common.sh. Failures are logged and never returned.
func (c *Configurator) RemoveAllEFIBootEntries(ctx context.Context) {
	out, err := hostCommandCombinedOutput(ctx, "efibootmgr")
	if err != nil {
		slog.Warn("efibootmgr list failed (non-EFI system?)", "output", string(out), "error", err)
		return
	}
	for _, entry := range efiBootOrder(string(out)) {
		slog.Info("removing EFI boot entry", "entry", entry)
		if out, err := hostCommandCombinedOutput(ctx, "efibootmgr", "-b", entry, "-B"); err != nil {
			slog.Warn("failed to remove EFI entry", "entry", entry, "output", string(out), "error", err)
		}
	}
}

// efiBootOrder returns the entries of the "BootOrder:" line of efibootmgr
// output. Only four-digit hex entries are returned.
func efiBootOrder(out string) []string {
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "BootOrder:")
		if !ok {
			continue
		}
		var entries []string
		for _, entry := range strings.Split(strings.TrimSpace(rest), ",") {
			entry = strings.TrimSpace(entry)
			if len(entry) == 4 && isHexString(entry) {
				entries = append(entries, entry)
			}
		}
		return entries
	}
	return nil
}

func isHexString(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// CreateEFIBootEntry creates a new EFI boot entry for the installed OS.
// Skips gracefully when the system is not booted in EFI mode (e.g. BIOS VMs),
// since efibootmgr requires EFI firmware NVRAM access.
func (c *Configurator) CreateEFIBootEntry(ctx context.Context, diskDev, bootPart string) error {
	if bootPart == "" {
		slog.Warn("no EFI partition found, skipping EFI boot entry creation")
		return nil
	}

	if ok, reason := efiRuntimeReady(); !ok {
		slog.Warn("skipping EFI boot entry creation", "reason", reason)
		return nil
	}

	var err error
	diskDev, err = validateChrootDevicePath("EFI boot disk device", diskDev)
	if err != nil {
		return err
	}
	bootPart, err = validateChrootDevicePath("EFI boot partition device", bootPart)
	if err != nil {
		return err
	}

	slog.Info("creating EFI boot entry", "disk", diskDev, "partition", bootPart)

	// Detect EFI loader path — architecture-aware shimx64/shimaa64 with grub fallback.
	loader, err := efiLoaderPath(c.rootDir, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("detect EFI loader: %w", err)
	}
	label := efiBootEntryLabel(loader)

	// Determine partition number from the partition device path.
	partNum := partNumberFromDevice(bootPart)

	out, err := c.disk.Run(ctx, "efibootmgr", "-c", "-d", diskDev, "-p", partNum, "-L", label, "-l", loader)
	if err != nil {
		return fmt.Errorf("efibootmgr create: %s: %w", string(out), err)
	}
	slog.Info("EFI boot entry created", "output", string(out))
	return nil
}

// efiLoaderNames returns the shim and grub EFI binary names for the given architecture.
func efiLoaderNames(arch string) (shimName, grubName string, err error) {
	switch arch {
	case "amd64":
		return "shimx64.efi", "grubx64.efi", nil
	case "arm64":
		return "shimaa64.efi", "grubaa64.efi", nil
	default:
		return "", "", fmt.Errorf("unsupported architecture: %s", arch)
	}
}

// efiLoaderPath determines the EFI loader path, preferring shim with grub fallback.
func efiLoaderPath(rootDir, arch string) (string, error) {
	shimName, grubName, err := efiLoaderNames(arch)
	if err != nil {
		return "", fmt.Errorf("resolve efi loader names: %w", err)
	}
	removableName, removableErr := efiRemovableLoaderName(arch)
	if removableErr != nil {
		return "", removableErr
	}
	removablePath := filepath.Join(rootDir, "boot", "efi", "EFI", "BOOT", removableName)

	var checked []string
	for _, vendor := range managedEFIVendors {
		shimPath := filepath.Join(rootDir, "boot", "efi", "EFI", vendor, shimName)
		checked = append(checked, shimPath)
		err = statEFILoader(rootDir, shimPath)
		if err == nil {
			return `\EFI\` + vendor + `\` + shimName, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat shim %s: %w", shimPath, err)
		}

		grubPath := filepath.Join(rootDir, "boot", "efi", "EFI", vendor, grubName)
		checked = append(checked, grubPath)
		err = statEFILoader(rootDir, grubPath)
		if err == nil {
			return `\EFI\` + vendor + `\` + grubName, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("stat grub %s: %w", grubPath, err)
		}
	}
	checked = append(checked, removablePath)
	err = statEFILoader(rootDir, removablePath)
	if err == nil {
		return `\EFI\BOOT\` + removableName, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat removable EFI loader %s: %w", removablePath, err)
	}
	return "", fmt.Errorf("no EFI loader found: checked %s", strings.Join(checked, ", "))
}

// statEFILoader checks that loader is a regular file inside rootDir. A
// link, or a link in an ancestor that leaves the target root, does not
// count, because firmware cannot follow it on the ESP. A missing loader
// returns an error for which os.IsNotExist is true.
func statEFILoader(rootDir, loader string) error {
	if err := ensureTargetParentWithinRoot(rootDir, filepath.Dir(loader)); err != nil {
		return err
	}
	info, err := os.Lstat(loader)
	if err != nil {
		return err //nolint:wrapcheck // callers check os.IsNotExist
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("EFI loader %s is not a regular file", loader)
	}
	return nil
}

func isManagedEFIBootEntryLine(line string) bool {
	if len(line) <= 8 || !strings.HasPrefix(line, "Boot") {
		return false
	}
	label := efiBootEntryLineLabel(line)
	if label == "" {
		return false
	}
	for _, vendor := range managedEFIVendors {
		if strings.EqualFold(label, vendor) {
			return true
		}
	}
	return false
}

func efiBootEntryLineLabel(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	label := fields[1]
	return strings.TrimSuffix(label, "*")
}

func efiBootEntryLabel(loader string) string {
	parts := strings.Split(strings.Trim(loader, `\`), `\`)
	if len(parts) >= 2 && strings.EqualFold(parts[0], "EFI") {
		for _, vendor := range managedEFIVendors {
			if strings.EqualFold(parts[1], vendor) {
				return vendor
			}
		}
	}
	return "ubuntu"
}

func efiFallbackBootloaderID(rootDir string) string {
	if loader, err := efiLoaderPath(rootDir, runtime.GOARCH); err == nil {
		return efiBootEntryLabel(loader)
	}
	for _, vendor := range managedEFIVendors {
		efiDir := filepath.Join(rootDir, "boot", "efi", "EFI", vendor)
		if info, err := os.Stat(efiDir); err == nil && info.IsDir() {
			return vendor
		}
	}
	return "ubuntu"
}

// partNumberFromDevice extracts the partition number from a device path.
// e.g. "/dev/sda1" → "1", "/dev/nvme0n1p2" → "2".
func partNumberFromDevice(dev string) string {
	for i := len(dev) - 1; i >= 0; i-- {
		if dev[i] < '0' || dev[i] > '9' {
			return dev[i+1:]
		}
	}
	return "1"
}

// SetupMellanox detects and configures Mellanox ConnectX NICs.
// Returns true if firmware values were changed (requiring a hard reboot for reinit).
func (c *Configurator) SetupMellanox(ctx context.Context, numVFs int) (bool, error) {
	slog.Info("checking for Mellanox NICs")

	if numVFs <= 0 {
		slog.Info("mellanox SR-IOV setup disabled", "numVFs", numVFs)
		return false, nil
	}

	// Detect Mellanox NICs via sysfs (vendor 0x15b3) instead of lspci.
	found, err := hasPCIVendorFunc("15b3")
	if err != nil {
		return false, fmt.Errorf("detect Mellanox PCI devices: %w", err)
	}
	if !found {
		return false, fmt.Errorf("requested NUM_VFS=%d but no Mellanox NICs found", numVFs)
	}

	// Enumerate all Mellanox mst pciconf devices dynamically.
	listOut, err := c.disk.ChrootRun(ctx, c.rootDir, "ls /dev/mst/")
	if err != nil {
		return false, fmt.Errorf("list Mellanox mst devices: %w", err)
	}

	changed := false
	validDevices := 0
	var errs []string
	for _, entry := range strings.Fields(string(listOut)) {
		if !strings.Contains(entry, "pciconf") {
			continue
		}
		// Validate device name with allowlist to prevent shell injection.
		if !isSafeDeviceName(entry) {
			errs = append(errs, fmt.Sprintf("invalid mst device name %q", entry))
			continue
		}
		validDevices++
		devPath := "/dev/mst/" + entry
		cmd := fmt.Sprintf("mstconfig -d %s -y set SRIOV_EN=True NUM_OF_VFS=%d", devPath, numVFs)
		slog.Info("configuring Mellanox SR-IOV", "device", devPath, "numVFs", numVFs)
		out, err := c.disk.ChrootRun(ctx, c.rootDir, cmd)
		if err != nil {
			errs = append(errs, formatMstconfigError(devPath, out, err))
			continue
		}
		changed = true
	}
	if validDevices == 0 {
		errs = append(errs, "no Mellanox mst pciconf devices found in /dev/mst")
	}
	if len(errs) > 0 {
		return changed, fmt.Errorf("configure Mellanox SR-IOV: %s", strings.Join(errs, "; "))
	}

	if changed {
		slog.Info("mellanox firmware values changed, hard reboot required")
	}
	return changed, nil
}

func formatMstconfigError(devPath string, out []byte, err error) string {
	output := compactMstconfigOutput(out)
	if output == "" {
		return fmt.Sprintf("mstconfig %s failed: %v", devPath, err)
	}
	return fmt.Sprintf("mstconfig %s failed: %v [output: %s]", devPath, err, output)
}

func compactMstconfigOutput(out []byte) string {
	output := strings.Join(strings.Fields(string(out)), " ")
	if len(output) > mstconfigOutputLimit {
		output = output[:mstconfigOutputLimit] + "...(truncated)"
	}
	return output
}

// isSafeDeviceName validates that a device name contains only safe characters
// (letters, digits, dots, underscores, hyphens).
func isSafeDeviceName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// hasPCIVendorFunc is the PCI vendor check function, replaceable in tests.
var hasPCIVendorFunc = hasPCIVendor

// SetPCIVendorCheckFunc overrides the PCI vendor detection for testing.
// Returns a restore function that resets to the original implementation.
func SetPCIVendorCheckFunc(fn func(string) (bool, error)) func() {
	old := hasPCIVendorFunc
	hasPCIVendorFunc = fn
	return func() { hasPCIVendorFunc = old }
}

// hasPCIVendor checks if any PCI device with the given vendor ID exists via sysfs.
func hasPCIVendor(vendorID string) (bool, error) {
	entries, err := os.ReadDir("/sys/bus/pci/devices")
	if err != nil {
		return false, fmt.Errorf("reading PCI devices: %w", err)
	}
	target := "0x" + vendorID
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join("/sys/bus/pci/devices", entry.Name(), "vendor")) //nolint:gocritic // absolute sysfs path is correct
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) == target {
			return true, nil
		}
	}
	return false, nil
}

// copyFile copies a file preserving mode bits and modification time. The copy
// respects context cancellation: if ctx is canceled mid-copy, the source file is
// closed which terminates the in-progress io.Copy and the error is returned.
func copyFile(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy file canceled: %w", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if err := prepareCopyDestination(dst); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, metadataMode(info.Mode()))
	if err != nil {
		return fmt.Errorf("open dest %s: %w", dst, err)
	}
	outClosed := false
	defer func() {
		if !outClosed {
			_ = out.Close()
		}
	}()

	copyDone := make(chan error, 1)
	go func() {
		_, cpErr := io.Copy(out, in)
		copyDone <- cpErr
	}()

	select {
	case <-ctx.Done():
		_ = in.Close()
		return fmt.Errorf("copy %s -> %s canceled: %w", src, dst, ctx.Err())
	case cpErr := <-copyDone:
		if cpErr != nil {
			return fmt.Errorf("copy %s -> %s: %w", src, dst, cpErr)
		}
		closeErr := out.Close()
		outClosed = true
		if closeErr != nil {
			return fmt.Errorf("close dest %s: %w", dst, closeErr)
		}
		if err := applyPathMetadata(copiedPathMetadataFromInfo(dst, info)); err != nil {
			return err
		}
		return nil
	}
}

func prepareCopyDestination(dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("create dir for %s: %w", dst, err)
	}
	existing, err := os.Lstat(dst)
	if err == nil && existing.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("replace symlink dest %s: %w", dst, err)
		}
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat dest %s: %w", dst, err)
	}
	return nil
}
