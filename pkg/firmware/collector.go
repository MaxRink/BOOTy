//go:build linux

package firmware

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Sysfs paths (variables for testability).
var (
	sysDMIPath      = "/sys/class/dmi/id"
	sysNetPath      = "/sys/class/net"
	sysSCSIHostPath = "/sys/class/scsi_host"

	// nicFirmwareVersion reads the firmware version that `ethtool -i`
	// prints. Most NIC drivers do not expose it in sysfs.
	nicFirmwareVersion = ethtoolFirmwareVersion
)

func ethtoolFirmwareVersion(iface string) string {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return ""
	}
	defer unix.Close(fd) //nolint:errcheck // socket only used for the ioctl
	info, err := unix.IoctlGetEthtoolDrvinfo(fd, iface)
	if err != nil {
		return ""
	}
	return unix.ByteSliceToString(info.Fw_version[:])
}

// Collect gathers firmware version data from sysfs.
func Collect() (*Report, error) {
	r := &Report{CollectedAt: time.Now().UTC()}

	r.BIOS = collectBIOS()
	r.BMC = collectBMC()
	r.NICs = collectNICFirmware()
	r.Storage = collectStorageFirmware()

	return r, nil
}

func collectBIOS() Version {
	v := Version{Component: "BIOS"}
	v.Version = readSysFile(filepath.Join(sysDMIPath, "bios_version"))
	v.Date = readSysFile(filepath.Join(sysDMIPath, "bios_date"))
	v.Vendor = readSysFile(filepath.Join(sysDMIPath, "bios_vendor"))
	return v
}

func collectBMC() Version {
	// BMC firmware is not exposed via standard DMI sysfs. Keep the board
	// vendor for correlation, but leave Version empty unless a real BMC source
	// is added so MinBMC checks fail closed instead of comparing baseboard data.
	v := Version{Component: "BMC"}
	v.Vendor = readSysFile(filepath.Join(sysDMIPath, "board_vendor"))
	return v
}

func collectNICFirmware() []NICFirmware {
	entries, err := os.ReadDir(sysNetPath)
	if err != nil {
		return nil
	}

	var nics []NICFirmware
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}

		// Only include physical NICs that have a PCI device backing.
		devicePath := filepath.Join(sysNetPath, name, "device")
		if !dirExists(devicePath) {
			continue
		}

		nic := NICFirmware{Interface: name}
		nic.Version = readSysFile(filepath.Join(devicePath, "firmware_version"))
		if nic.Version == "" {
			nic.Version = nicFirmwareVersion(name)
		}
		nic.Driver = readNICDriver(devicePath)
		nic.PCIAddr = readPCIAddr(devicePath)

		nics = append(nics, nic)
	}
	return nics
}

func readNICDriver(devicePath string) string {
	target, err := os.Readlink(filepath.Join(devicePath, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

func readPCIAddr(devicePath string) string {
	target, err := os.Readlink(devicePath)
	if err != nil {
		return ""
	}
	// The symlink target ends with the PCI address, e.g.
	// ../../../0000:3b:00.0
	return filepath.Base(target)
}

func collectStorageFirmware() []StorageFirmware {
	entries, err := os.ReadDir(sysSCSIHostPath)
	if err != nil {
		return nil
	}

	var storage []StorageFirmware
	for _, e := range entries {
		fw := readSysFile(filepath.Join(sysSCSIHostPath, e.Name(), "firmware_rev"))
		if fw == "" {
			continue
		}
		model := readSysFile(filepath.Join(sysSCSIHostPath, e.Name(), "model_name"))
		storage = append(storage, StorageFirmware{
			Controller: e.Name(),
			Model:      model,
			Version:    fw,
		})
	}
	return storage
}

// Validate checks firmware versions against a policy and returns results.
func Validate(report *Report, policy Policy) []ValidationResult {
	var results []ValidationResult

	if policy.MinBIOSVersion != "" {
		results = append(results, checkVersion(
			"firmware-bios",
			report.BIOS.Version,
			policy.MinBIOSVersion,
		))
	}

	if policy.MinBMCVersion != "" {
		results = append(results, checkVersion(
			"firmware-bmc",
			report.BMC.Version,
			policy.MinBMCVersion,
		))
	}

	for driver, minVer := range policy.MinNICVersions {
		for _, nic := range report.NICs {
			if nic.Driver != driver {
				continue
			}
			results = append(results, checkVersion(
				fmt.Sprintf("firmware-nic-%s-%s", nic.Interface, driver),
				nic.Version,
				minVer,
			))
		}
	}

	return results
}

func checkVersion(name, actual, minimum string) ValidationResult {
	if actual == "" {
		return ValidationResult{
			Name:    name,
			Status:  "fail",
			Message: fmt.Sprintf("version unknown, minimum required: %s", minimum),
		}
	}
	if compareVersions(actual, minimum) < 0 {
		return ValidationResult{
			Name:    name,
			Status:  "fail",
			Message: fmt.Sprintf("%s < minimum %s", actual, minimum),
		}
	}
	return ValidationResult{
		Name:    name,
		Status:  "pass",
		Message: fmt.Sprintf("%s >= %s", actual, minimum),
	}
}

// compareVersions compares version strings segment by segment.
// Segments are split on ".". Each segment is compared numerically if both
// parse as integers; otherwise they are compared lexicographically.
func compareVersions(a, b string) int {
	sa := strings.Split(a, ".")
	sb := strings.Split(b, ".")
	for i := 0; i < len(sa) || i < len(sb); i++ {
		var va, vb string
		if i < len(sa) {
			va = sa[i]
		}
		if i < len(sb) {
			vb = sb[i]
		}
		na, errA := strconv.Atoi(va)
		nb, errB := strconv.Atoi(vb)
		switch {
		case errA == nil && errB == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		default:
			if c := strings.Compare(va, vb); c != 0 {
				return c
			}
		}
	}
	return 0
}

func readSysFile(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // sysfs paths constructed internally
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
