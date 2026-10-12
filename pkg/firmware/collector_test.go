//go:build linux

package firmware

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupDMI creates a fake /sys/class/dmi/id tree in tmpDir.
func setupDMI(t *testing.T, tmpDir string, files map[string]string) {
	t.Helper()
	dmiDir := filepath.Join(tmpDir, "dmi", "id")
	if err := os.MkdirAll(dmiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dmiDir, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// setupNIC creates a fake /sys/class/net/<iface>/device tree.
func setupNIC(t *testing.T, tmpDir, iface string, files map[string]string) {
	t.Helper()
	deviceDir := filepath.Join(tmpDir, "net", iface, "device")
	if err := os.MkdirAll(deviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(deviceDir, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// setupSCSIHost creates a fake /sys/class/scsi_host/<host> tree.
func setupSCSIHost(t *testing.T, tmpDir, host string, files map[string]string) {
	t.Helper()
	hostDir := filepath.Join(tmpDir, "scsi_host", host)
	if err := os.MkdirAll(hostDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(hostDir, name), []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func withTestPaths(t *testing.T, tmpDir string) {
	t.Helper()
	origDMI := sysDMIPath
	origNet := sysNetPath
	origSCSI := sysSCSIHostPath
	origFW := nicFirmwareVersion
	nicFirmwareVersion = func(string) string { return "" }

	sysDMIPath = filepath.Join(tmpDir, "dmi", "id")
	sysNetPath = filepath.Join(tmpDir, "net")
	sysSCSIHostPath = filepath.Join(tmpDir, "scsi_host")

	t.Cleanup(func() {
		sysDMIPath = origDMI
		sysNetPath = origNet
		sysSCSIHostPath = origSCSI
		nicFirmwareVersion = origFW
	})
}

func TestCollectBIOS(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)

	setupDMI(t, tmpDir, map[string]string{
		"bios_version": "U46 v2.72",
		"bios_date":    "12/15/2023",
		"bios_vendor":  "HPE",
	})

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}

	if report.BIOS.Component != "BIOS" {
		t.Errorf("BIOS.Component = %q, want BIOS", report.BIOS.Component)
	}
	if report.BIOS.Version != "U46 v2.72" {
		t.Errorf("BIOS.Version = %q, want %q", report.BIOS.Version, "U46 v2.72")
	}
	if report.BIOS.Date != "12/15/2023" {
		t.Errorf("BIOS.Date = %q, want %q", report.BIOS.Date, "12/15/2023")
	}
	if report.BIOS.Vendor != "HPE" {
		t.Errorf("BIOS.Vendor = %q, want %q", report.BIOS.Vendor, "HPE")
	}
}

func TestCollectBMC(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)

	setupDMI(t, tmpDir, map[string]string{
		"board_vendor": "Lenovo",
	})

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}

	if report.BMC.Component != "BMC" {
		t.Errorf("BMC.Component = %q, want BMC", report.BMC.Component)
	}
	if report.BMC.Vendor != "Lenovo" {
		t.Errorf("BMC.Vendor = %q, want %q", report.BMC.Vendor, "Lenovo")
	}
	if report.BMC.Version != "" {
		t.Errorf("BMC.Version = %q, want empty when no BMC firmware source is collected", report.BMC.Version)
	}
}

func TestCollectNICFirmware(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)

	setupDMI(t, tmpDir, map[string]string{})

	setupNIC(t, tmpDir, "eth0", map[string]string{
		"firmware_version": "22.39.1002",
	})

	// Create a driver symlink.
	deviceDir := filepath.Join(tmpDir, "net", "eth0", "device")
	driverDir := filepath.Join(deviceDir, "driver_link_target", "i40e")
	if err := os.MkdirAll(driverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(driverDir, filepath.Join(deviceDir, "driver"))

	// Create lo — should be skipped.
	loDir := filepath.Join(tmpDir, "net", "lo")
	if err := os.MkdirAll(loDir, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}

	if len(report.NICs) != 1 {
		t.Fatalf("expected 1 NIC, got %d", len(report.NICs))
	}
	nic := report.NICs[0]
	if nic.Interface != "eth0" {
		t.Errorf("NIC.Interface = %q, want eth0", nic.Interface)
	}
	if nic.Version != "22.39.1002" {
		t.Errorf("NIC.Version = %q, want %q", nic.Version, "22.39.1002")
	}
	if nic.Driver != "i40e" {
		t.Errorf("NIC.Driver = %q, want i40e", nic.Driver)
	}
}

func TestCollectNICSkipsVirtual(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)
	setupDMI(t, tmpDir, map[string]string{})

	// veth should be skipped.
	vethDir := filepath.Join(tmpDir, "net", "veth1234")
	if err := os.MkdirAll(vethDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// docker0 should be skipped.
	dockerDir := filepath.Join(tmpDir, "net", "docker0")
	if err := os.MkdirAll(dockerDir, 0o755); err != nil {
		t.Fatal(err)
	}

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}

	if len(report.NICs) != 0 {
		t.Errorf("expected 0 NICs (all virtual), got %d", len(report.NICs))
	}
}

func TestCollectStorageFirmware(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)
	setupDMI(t, tmpDir, map[string]string{})

	setupSCSIHost(t, tmpDir, "host0", map[string]string{
		"firmware_rev": "4.6.0.27",
		"model_name":   "Adaptec SmartRAID",
	})

	// host1 has no firmware — should be skipped.
	setupSCSIHost(t, tmpDir, "host1", map[string]string{
		"model_name": "Virtual SCSI",
	})

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}

	if len(report.Storage) != 1 {
		t.Fatalf("expected 1 storage controller, got %d", len(report.Storage))
	}
	s := report.Storage[0]
	if s.Controller != "host0" {
		t.Errorf("Controller = %q, want host0", s.Controller)
	}
	if s.Version != "4.6.0.27" {
		t.Errorf("Version = %q, want %q", s.Version, "4.6.0.27")
	}
	if s.Model != "Adaptec SmartRAID" {
		t.Errorf("Model = %q, want %q", s.Model, "Adaptec SmartRAID")
	}
}

func TestCollectEmptySysfs(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}

	if report.BIOS.Version != "" {
		t.Errorf("expected empty BIOS version, got %q", report.BIOS.Version)
	}
	if len(report.NICs) != 0 {
		t.Errorf("expected 0 NICs, got %d", len(report.NICs))
	}
	if len(report.Storage) != 0 {
		t.Errorf("expected 0 storage, got %d", len(report.Storage))
	}
	if report.CollectedAt.IsZero() {
		t.Error("CollectedAt should be set")
	}
}

func TestValidateBIOSPass(t *testing.T) {
	report := &Report{
		BIOS: Version{Component: "BIOS", Version: "U50"},
	}
	policy := Policy{MinBIOSVersion: "U46"}

	results := Validate(report, policy)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "pass" {
		t.Errorf("expected pass, got %q: %s", results[0].Status, results[0].Message)
	}
}

func TestValidateBIOSFail(t *testing.T) {
	report := &Report{
		BIOS: Version{Component: "BIOS", Version: "U30"},
	}
	policy := Policy{MinBIOSVersion: "U46"}

	results := Validate(report, policy)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "fail" {
		t.Errorf("expected fail, got %q: %s", results[0].Status, results[0].Message)
	}
}

func TestValidateBIOSUnknown(t *testing.T) {
	report := &Report{
		BIOS: Version{Component: "BIOS", Version: ""},
	}
	policy := Policy{MinBIOSVersion: "U46"}

	results := Validate(report, policy)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "fail" {
		t.Errorf("expected fail for unknown version, got %q", results[0].Status)
	}
}

func TestValidateBMC(t *testing.T) {
	report := &Report{
		BMC: Version{Component: "BMC", Version: "2.80"},
	}
	policy := Policy{MinBMCVersion: "2.72"}

	results := Validate(report, policy)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "pass" {
		t.Errorf("expected pass, got %q", results[0].Status)
	}
}

func TestValidateBMCUnknown(t *testing.T) {
	report := &Report{
		BMC: Version{Component: "BMC", Version: ""},
	}
	policy := Policy{MinBMCVersion: "2.72"}

	results := Validate(report, policy)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != "fail" {
		t.Errorf("expected fail for unknown BMC version, got %q", results[0].Status)
	}
	if !strings.Contains(results[0].Message, "version unknown") {
		t.Errorf("BMC validation message = %q, want unknown-version diagnostic", results[0].Message)
	}
}

func TestValidateNICFirmware(t *testing.T) {
	report := &Report{
		NICs: []NICFirmware{
			{Interface: "eth0", Driver: "i40e", Version: "9.20"},
			{Interface: "eth1", Driver: "mlx5_core", Version: "22.39.1002"},
		},
	}
	policy := Policy{
		MinNICVersions: map[string]string{
			"i40e": "9.00",
		},
	}

	results := Validate(report, policy)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Name != "firmware-nic-eth0-i40e" {
		t.Errorf("Name = %q", results[0].Name)
	}
	if results[0].Status != "pass" {
		t.Errorf("expected pass, got %q", results[0].Status)
	}
}

func TestValidateNoPolicy(t *testing.T) {
	report := &Report{
		BIOS: Version{Component: "BIOS", Version: "U50"},
	}
	policy := Policy{}

	results := Validate(report, policy)
	if len(results) != 0 {
		t.Errorf("expected 0 results with empty policy, got %d", len(results))
	}
}

func TestCollectTimestamp(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}

	if report.CollectedAt.IsZero() {
		t.Error("CollectedAt should not be zero")
	}
}

func TestReadSysFileNotExist(t *testing.T) {
	result := readSysFile("/nonexistent/path/to/file")
	if result != "" {
		t.Errorf("expected empty string, got %q", result)
	}
}

func TestDirExists(t *testing.T) {
	tmpDir := t.TempDir()
	if !dirExists(tmpDir) {
		t.Error("expected dir to exist")
	}
	if dirExists(filepath.Join(tmpDir, "nope")) {
		t.Error("expected dir to not exist")
	}

	f := filepath.Join(tmpDir, "file.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirExists(f) {
		t.Error("expected file to not be reported as dir")
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"2.72", "2.72", 0},
		{"2.80", "2.72", 1},
		{"2.50", "2.72", -1},
		{"U50", "U46", 1},
		{"U30", "U46", -1},
		{"", "1.0", -1},
	}
	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if (tt.want < 0 && got >= 0) || (tt.want > 0 && got <= 0) || (tt.want == 0 && got != 0) {
			t.Errorf("compareVersions(%q, %q) = %d, want sign %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// Most drivers have no firmware_version in sysfs; the version then comes from
// the ethtool driver info, like `ethtool -i` in the CAPRF legacy provision.sh.
func TestCollectNICFirmwareFromEthtool(t *testing.T) {
	tmpDir := t.TempDir()
	withTestPaths(t, tmpDir)
	setupDMI(t, tmpDir, map[string]string{})
	setupNIC(t, tmpDir, "eth0", map[string]string{})
	nicFirmwareVersion = func(iface string) string { return iface + "-fw 16.35.2000" }

	report, err := Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.NICs) != 1 || report.NICs[0].Version != "eth0-fw 16.35.2000" {
		t.Fatalf("NICs = %+v, want eth0 with the ethtool version", report.NICs)
	}
}

// The real ethtool helper returns "" for NICs without driver info, such as
// loopback, and for missing NICs. A real NIC is covered by the KVM test.
func TestEthtoolFirmwareVersionErrorPaths(t *testing.T) {
	for _, iface := range []string{"lo", "booty-missing0"} {
		if got := ethtoolFirmwareVersion(iface); got != "" {
			t.Errorf("ethtoolFirmwareVersion(%q) = %q, want empty", iface, got)
		}
	}
}
