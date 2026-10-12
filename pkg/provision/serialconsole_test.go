//go:build linux

package provision

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/serialconsole"
)

// serialHostFixture builds a read-only host tree for the console resolver.
type serialHostFixture struct {
	t    *testing.T
	root string
}

func newSerialHostFixture(t *testing.T, c *Configurator) *serialHostFixture {
	t.Helper()
	root := t.TempDir()
	c.SetHostRoots(filepath.Join(root, "sys"), filepath.Join(root, "proc"))
	c.SetRunDir(filepath.Join(root, "run", "booty"))
	return &serialHostFixture{t: t, root: root}
}

func (f *serialHostFixture) write(relative string, content []byte) {
	f.t.Helper()
	full := filepath.Join(f.root, relative)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatalf("create fixture dir: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		f.t.Fatalf("write fixture %s: %v", relative, err)
	}
}

func (f *serialHostFixture) addUART(device string, ioPort string) {
	f.t.Helper()
	base := filepath.Join("sys", "class", "tty", device)
	f.write(filepath.Join(base, "type"), []byte("16550A\n"))
	f.write(filepath.Join(base, "port"), []byte(ioPort+"\n"))
}

func (f *serialHostFixture) addDMI(vendor string) {
	f.t.Helper()
	f.write(filepath.Join("sys", "class", "dmi", "id", "sys_vendor"), []byte(vendor+"\n"))
}

// addSPCR writes a minimal valid SPCR table pointing at a system I/O address.
func (f *serialHostFixture) addSPCR(address uint64) {
	f.t.Helper()
	table := make([]byte, 80)
	copy(table[0:4], "SPCR")
	binary.LittleEndian.PutUint32(table[4:8], uint32(len(table)))
	table[8] = 2
	table[36] = 0x00 // 16550 interface
	table[40] = 0x01 // system I/O address space
	binary.LittleEndian.PutUint64(table[44:52], address)
	table[58] = 7 // 115200
	table[60] = 1 // one stop bit
	f.write(filepath.Join("sys", "firmware", "acpi", "tables", "SPCR"), table)
}

func readGrubConfig(t *testing.T, c *Configurator) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(c.rootDir, "etc", "default", "grub.d", "10-caprf-kernel-params.cfg"))
	if err != nil {
		t.Fatalf("read grub config: %v", err)
	}
	return string(data)
}

func countConsoleParams(t *testing.T, content string) int {
	t.Helper()
	count := 0
	for _, field := range strings.Fields(content) {
		if strings.HasPrefix(strings.TrimPrefix(field, "GRUB_CMDLINE_LINUX=\""), "console=") ||
			strings.HasPrefix(field, "console=") {
			count++
		}
	}
	return count
}

// TestConfigureGRUBUsesFirmwareEvidenceNotVendor is the regression test for the
// removed "Lenovo means ttyS1" heuristic.
func TestConfigureGRUBUsesFirmwareEvidenceNotVendor(t *testing.T) {
	tests := []struct {
		name        string
		vendor      string
		spcrAddress uint64
		wantConsole string
	}{
		{name: "lenovo com2", vendor: "Lenovo", spcrAddress: 0x2f8, wantConsole: "console=ttyS1,115200n8"},
		{name: "lenovo com1", vendor: "Lenovo", spcrAddress: 0x3f8, wantConsole: "console=ttyS0,115200n8"},
		{name: "dell com2", vendor: "Dell Inc.", spcrAddress: 0x2f8, wantConsole: "console=ttyS1,115200n8"},
		{name: "supermicro com1", vendor: "Supermicro", spcrAddress: 0x3f8, wantConsole: "console=ttyS0,115200n8"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestConfigurator(t, newMockCommander())
			host := newSerialHostFixture(t, c)
			host.addDMI(tc.vendor)
			host.addUART("ttyS0", "0x3f8")
			host.addUART("ttyS1", "0x2f8")
			host.addSPCR(tc.spcrAddress)

			if err := c.ConfigureGRUB(context.Background(), &config.MachineConfig{}); err != nil {
				t.Fatalf("ConfigureGRUB: %v", err)
			}
			content := readGrubConfig(t, c)
			if !strings.Contains(content, tc.wantConsole) {
				t.Fatalf("grub config %q does not contain %q", content, tc.wantConsole)
			}
			if got := countConsoleParams(t, content); got != 1 {
				t.Fatalf("grub config has %d console parameters, want exactly 1: %s", got, content)
			}
		})
	}
}

func TestConfigureGRUBEmitsExactlyOneConsoleWithOperatorOverride(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS0", "0x3f8")
	host.addUART("ttyS1", "0x2f8")
	host.addSPCR(0x3f8)

	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "console=ttyS1,115200n8 quiet"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
	content := readGrubConfig(t, c)
	if got := countConsoleParams(t, content); got != 1 {
		t.Fatalf("grub config has %d console parameters, want exactly 1: %s", got, content)
	}
	if !strings.Contains(content, "console=ttyS1,115200n8") {
		t.Fatalf("operator console intent lost: %s", content)
	}
	if !strings.Contains(content, "quiet") {
		t.Fatalf("non-console operator parameters must be preserved: %s", content)
	}
}

func TestConfigureGRUBHonorsSerialConsoleOverride(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS0", "0x3f8")
	host.addSPCR(0x3f8)

	cfg := &config.MachineConfig{}
	cfg.Provision.SerialConsole = "ttyS2,57600n8"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
	content := readGrubConfig(t, c)
	if !strings.Contains(content, "console=ttyS2,57600n8") {
		t.Fatalf("explicit override ignored: %s", content)
	}
	if !strings.Contains(content, "GRUB_SERIAL_COMMAND=\"serial --unit=2 --speed=57600\"") {
		t.Fatalf("grub serial terminal not configured for the resolved port: %s", content)
	}
}

func TestConfigureGRUBWithoutEvidenceEmitsNoConsole(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	newSerialHostFixture(t, c)

	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "quiet splash"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
	content := readGrubConfig(t, c)
	if strings.Contains(content, "console=") {
		t.Fatalf("no evidence must not guess a console: %s", content)
	}
	if !strings.Contains(content, "quiet splash") {
		t.Fatalf("operator parameters lost: %s", content)
	}
}

func TestConfigureGRUBFailsClosedOnAmbiguousEvidence(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS0", "0x3f8")
	host.addUART("ttyS1", "0x2f8")

	err := c.ConfigureGRUB(context.Background(), &config.MachineConfig{})
	if err == nil {
		t.Fatal("ConfigureGRUB() = nil error, want fail-closed on ambiguous evidence")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error %v does not explain the ambiguity", err)
	}
}

func TestConfigureSerialConsoleWritesMatchingGetty(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addDMI("Lenovo")
	host.addUART("ttyS0", "0x3f8")
	host.addUART("ttyS1", "0x2f8")
	host.addSPCR(0x2f8)

	cfg := &config.MachineConfig{}
	if err := c.ConfigureSerialConsole(cfg); err != nil {
		t.Fatalf("ConfigureSerialConsole: %v", err)
	}
	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}

	dropIn := filepath.Join(c.rootDir, "etc", "systemd", "system",
		"serial-getty@ttyS1.service.d", serialConsoleDropInName)
	data, err := os.ReadFile(dropIn)
	if err != nil {
		t.Fatalf("read serial getty drop-in: %v", err)
	}
	if !strings.Contains(string(data), "-8 -o") ||
		!strings.Contains(string(data), "--keep-baud 115200 %I $TERM") {
		t.Fatalf("drop-in does not pin compatible 8N1 framing and baud: %s", data)
	}
	if !strings.Contains(string(data), "ExecStart=\n") {
		t.Fatalf("drop-in must reset the inherited ExecStart: %s", data)
	}

	link := filepath.Join(c.rootDir, "etc", "systemd", "system", "getty.target.wants",
		"serial-getty@ttyS1.service")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read serial getty symlink: %v", err)
	}
	if !strings.HasSuffix(target, "serial-getty@.service") {
		t.Fatalf("symlink target = %q, want the serial-getty template", target)
	}

	// The kernel console and the getty instance must name the same device.
	grub := readGrubConfig(t, c)
	if !strings.Contains(grub, "console=ttyS1,115200n8") {
		t.Fatalf("kernel console does not match the getty instance: %s", grub)
	}
}

func TestConfigureSerialConsoleRemovesConflictingGettys(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS1", "0x2f8")
	host.addSPCR(0x2f8)

	wantsDir := filepath.Join(c.rootDir, "etc", "systemd", "system", "getty.target.wants")
	if err := os.MkdirAll(wantsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(wantsDir, "serial-getty@ttyS0.service")
	if err := os.Symlink("/lib/systemd/system/serial-getty@.service", stale); err != nil {
		t.Fatal(err)
	}
	keepConsole := filepath.Join(wantsDir, "getty@tty1.service")
	if err := os.Symlink("/lib/systemd/system/getty@.service", keepConsole); err != nil {
		t.Fatal(err)
	}
	staleDropIn := filepath.Join(c.rootDir, "etc", "systemd", "system",
		"serial-getty@ttyS0.service.d", serialConsoleDropInName)
	if err := os.MkdirAll(filepath.Dir(staleDropIn), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleDropIn, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.ConfigureSerialConsole(&config.MachineConfig{}); err != nil {
		t.Fatalf("ConfigureSerialConsole: %v", err)
	}

	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatal("conflicting serial getty unit was not removed")
	}
	if _, err := os.Lstat(staleDropIn); !os.IsNotExist(err) {
		t.Fatal("conflicting serial getty drop-in was not removed")
	}
	if _, err := os.Lstat(keepConsole); err != nil {
		t.Fatalf("virtual terminal getty must be preserved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(wantsDir, "serial-getty@ttyS1.service")); err != nil {
		t.Fatalf("resolved serial getty missing: %v", err)
	}
}

func readSerialArtifact(t *testing.T, path string) serialconsole.Artifact {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read serial console artifact: %v", err)
	}
	var artifact serialconsole.Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		t.Fatalf("decode serial console artifact: %v", err)
	}
	return artifact
}

func TestConfigureSerialConsolePersistsArtifactForCAPRF(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addDMI("Lenovo")
	host.addUART("ttyS1", "0x2f8")
	host.addSPCR(0x2f8)

	if err := c.ConfigureSerialConsole(&config.MachineConfig{}); err != nil {
		t.Fatalf("ConfigureSerialConsole: %v", err)
	}

	targetArtifact := filepath.Join(c.rootDir, "var", "lib", "booty", serialconsole.ArtifactFileName)
	artifact := readSerialArtifact(t, targetArtifact)
	if artifact.SchemaVersion != serialconsole.ArtifactSchemaVersion {
		t.Fatalf("SchemaVersion = %d, want %d", artifact.SchemaVersion, serialconsole.ArtifactSchemaVersion)
	}
	if artifact.Kind != serialconsole.ArtifactKind {
		t.Fatalf("Kind = %q, want %q", artifact.Kind, serialconsole.ArtifactKind)
	}
	if artifact.KernelParam != "console=ttyS1,115200n8" {
		t.Fatalf("KernelParam = %q", artifact.KernelParam)
	}
	if artifact.GettyUnit != "serial-getty@ttyS1.service" {
		t.Fatalf("GettyUnit = %q", artifact.GettyUnit)
	}
	if artifact.Resolution.SelectedBy != serialconsole.SourceACPISPCR {
		t.Fatalf("SelectedBy = %q", artifact.Resolution.SelectedBy)
	}
	if artifact.Resolution.Host.Vendor != "Lenovo" {
		t.Fatalf("Host.Vendor = %q, want Lenovo", artifact.Resolution.Host.Vendor)
	}
	if len(artifact.Resolution.Evidence) == 0 {
		t.Fatal("artifact carries no evidence trail")
	}
	if artifact.GeneratedAt == "" {
		t.Fatal("artifact has no generation timestamp")
	}

	runArtifact := filepath.Join(c.serialConsoleRunDir(), serialconsole.ArtifactFileName)
	if published := readSerialArtifact(t, runArtifact); published.KernelParam != artifact.KernelParam {
		t.Fatalf("run artifact %q differs from target artifact %q", published.KernelParam, artifact.KernelParam)
	}
}

func TestConfigureSerialConsolePersistsArtifactWhenAmbiguous(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS0", "0x3f8")
	host.addUART("ttyS1", "0x2f8")

	err := c.ConfigureSerialConsole(&config.MachineConfig{})
	if err == nil {
		t.Fatal("ConfigureSerialConsole() = nil error, want fail-closed")
	}

	artifact := readSerialArtifact(t,
		filepath.Join(c.rootDir, "var", "lib", "booty", serialconsole.ArtifactFileName))
	if artifact.Resolution.State != serialconsole.StateAmbiguous {
		t.Fatalf("State = %q, want %q", artifact.Resolution.State, serialconsole.StateAmbiguous)
	}
	if artifact.Error == "" {
		t.Fatal("ambiguous artifact must carry the fail-closed reason")
	}
	if artifact.KernelParam != "" || artifact.GettyUnit != "" {
		t.Fatalf("ambiguous artifact must not advertise a console: %+v", artifact)
	}

	wantsDir := filepath.Join(c.rootDir, "etc", "systemd", "system", "getty.target.wants")
	if _, err := os.Stat(wantsDir); !os.IsNotExist(err) {
		t.Fatal("no getty must be enabled when resolution fails closed")
	}
}

func TestConfigureSerialConsoleDisabledOverride(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS0", "0x3f8")
	host.addSPCR(0x3f8)

	cfg := &config.MachineConfig{}
	cfg.Provision.SerialConsole = "none"

	if err := c.ConfigureSerialConsole(cfg); err != nil {
		t.Fatalf("ConfigureSerialConsole: %v", err)
	}
	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
	if content := readGrubConfig(t, c); strings.Contains(content, "console=") {
		t.Fatalf("disabled console must not emit a kernel parameter: %s", content)
	}
	matches, err := filepath.Glob(filepath.Join(c.rootDir, "etc", "systemd", "system",
		"getty.target.wants", "serial-getty@*.service"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("disabled console must not enable a getty: %v", matches)
	}
}

func TestConfigureGRUBRejectsUnsupportedConsoleParams(t *testing.T) {
	tests := []struct {
		name  string
		extra string
	}{
		{name: "command injection", extra: "console=ttyS0;reboot"},
		{name: "device path traversal", extra: "console=../../dev/ttyS0"},
		{name: "unsupported baud", extra: "console=ttyS0,99"},
		{name: "nonexistent virtual terminal", extra: "console=tty99"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestConfigurator(t, newMockCommander())
			host := newSerialHostFixture(t, c)
			host.addUART("ttyS0", "0x3f8")
			host.addSPCR(0x3f8)

			cfg := &config.MachineConfig{}
			cfg.Provision.ExtraKernelParams = tc.extra

			if err := c.ConfigureGRUB(context.Background(), cfg); err == nil {
				t.Fatalf("ConfigureGRUB(%q) = nil error, want rejection", tc.extra)
			}
		})
	}
}

func TestConfigureGRUBAcceptsVirtualTerminalDevicePath(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS0", "0x3f8")
	host.addSPCR(0x3f8)

	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "console=/dev/tty0 ro"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
}

func TestConfigureGRUBKeepsVirtualTerminalConsoleOutOfSerialResolution(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS0", "0x3f8")
	host.addSPCR(0x3f8)

	cfg := &config.MachineConfig{}
	cfg.Provision.ExtraKernelParams = "console=tty0 ro"

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
	content := readGrubConfig(t, c)
	if got := countConsoleParams(t, content); got != 1 {
		t.Fatalf("grub config has %d console parameters, want exactly 1: %s", got, content)
	}
	if !strings.Contains(content, "console=ttyS0,115200n8") {
		t.Fatalf("firmware console lost: %s", content)
	}
}

func TestStripConsoleParams(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantKept    string
		wantDropped int
	}{
		{name: "no console", input: "quiet splash", wantKept: "quiet splash"},
		{name: "single console", input: "console=ttyS0 quiet", wantKept: "quiet", wantDropped: 1},
		{
			name:        "multiple consoles",
			input:       "console=tty0 console=ttyS1,115200n8 ro",
			wantKept:    "ro",
			wantDropped: 2,
		},
		{name: "empty", input: "", wantKept: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kept, dropped := stripConsoleParams(tc.input)
			if kept != tc.wantKept {
				t.Fatalf("kept = %q, want %q", kept, tc.wantKept)
			}
			if len(dropped) != tc.wantDropped {
				t.Fatalf("dropped = %v, want %d entries", dropped, tc.wantDropped)
			}
		})
	}
}

func TestIsSerialGettyInstance(t *testing.T) {
	valid := []string{"serial-getty@ttyS0.service", "serial-getty@ttyAMA0.service"}
	for _, name := range valid {
		if !isSerialGettyInstance(name) {
			t.Fatalf("isSerialGettyInstance(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"getty@tty1.service",
		"serial-getty@.service",
		"serial-getty@tty0.service",
		"serial-getty@ttyS0.socket",
		"notes.txt",
	}
	for _, name := range invalid {
		if isSerialGettyInstance(name) {
			t.Fatalf("isSerialGettyInstance(%q) = true, want false", name)
		}
	}
}

// TestConfigureSerialConsoleResolvesOncePerRun guards the kernel/getty
// agreement: both consumers must observe the identical cached resolution.
func TestConfigureSerialConsoleResolvesOncePerRun(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	host := newSerialHostFixture(t, c)
	host.addUART("ttyS1", "0x2f8")
	host.addSPCR(0x2f8)

	cfg := &config.MachineConfig{}
	if err := c.ConfigureSerialConsole(cfg); err != nil {
		t.Fatalf("ConfigureSerialConsole: %v", err)
	}

	// Mutate the host evidence after the first resolution: the cached value
	// must still drive the kernel command line.
	host.write(filepath.Join("sys", "firmware", "acpi", "tables", "SPCR"), []byte("junk"))

	if err := c.ConfigureGRUB(context.Background(), cfg); err != nil {
		t.Fatalf("ConfigureGRUB: %v", err)
	}
	if content := readGrubConfig(t, c); !strings.Contains(content, "console=ttyS1,115200n8") {
		t.Fatalf("kernel console diverged from the getty configuration: %s", content)
	}
}

func TestValidateConsoleKernelParamPreservesParseError(t *testing.T) {
	_, parseErr := serialconsole.ParseSpec("ttyS0,99")
	err := validateConsoleKernelParam("console=ttyS0,99")
	if parseErr == nil || err == nil || !strings.Contains(err.Error(), parseErr.Error()) {
		t.Fatalf("console validation error %v must preserve parse error %v", err, parseErr)
	}
}

func TestPruneSerialGettyUnitsRejectsAncestorSymlinkOutOfRoot(t *testing.T) {
	c := newTestConfigurator(t, newMockCommander())
	outside := t.TempDir()
	wants := filepath.Join(outside, "system", "getty.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wants, "serial-getty@ttyS1.service")
	if err := os.Symlink("/lib/systemd/system/serial-getty@.service", link); err != nil {
		t.Fatal(err)
	}
	dropIn := filepath.Join(outside, "system", "serial-getty@ttyS1.service.d", serialConsoleDropInName)
	if err := os.MkdirAll(filepath.Dir(dropIn), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropIn, []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A kept machine-file symlink: etc/systemd points out of the root.
	if err := os.MkdirAll(filepath.Join(c.rootDir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(c.rootDir, "etc", "systemd")); err != nil {
		t.Fatal(err)
	}

	if err := c.pruneSerialGettyLinks(""); err == nil {
		t.Fatal("pruneSerialGettyLinks() = nil, want error for an ancestor outside the root")
	}
	if err := c.pruneSerialGettyDropIns(""); err == nil {
		t.Fatal("pruneSerialGettyDropIns() = nil, want error for an ancestor outside the root")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("getty link outside the root was removed: %v", err)
	}
	if _, err := os.Lstat(dropIn); err != nil {
		t.Fatalf("drop-in outside the root was removed: %v", err)
	}
}
