//go:build linux

package inventory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/telekom/BOOTy/pkg/sriov"
)

// Paths can be overridden in tests.
var (
	ProcCPUInfoPath  = "/proc/cpuinfo"
	ProcMemInfoPath  = "/proc/meminfo"
	SysBlockPath     = "/sys/block"
	SysNetPath       = "/sys/class/net"
	SysDMIPath       = "/sys/class/dmi/id"
	SysPCIPath       = "/sys/bus/pci/devices"
	SysDMIMemoryPath = "/sys/firmware/dmi/entries/17-0"
	SysThermalPath   = "/sys/class/thermal"
	SysHwmonPath     = "/sys/class/hwmon"
	SysUSBPath       = "/sys/bus/usb/devices"
)

// Collect gathers a full hardware inventory from sysfs and procfs.
func Collect() (*HardwareInventory, error) {
	inv := &HardwareInventory{
		Timestamp: time.Now().UTC(),
	}

	inv.System = collectSystem()
	inv.CPUs = collectCPUs()
	inv.Memory = collectMemory()
	inv.Disks = collectDisks()
	inv.NICs = collectNICs()
	attachSRIOV(inv.NICs)
	inv.PCIDevices = collectPCI()
	inv.Accelerators = findAccelerators(inv.PCIDevices)

	return inv, nil
}

func collectSystem() SystemInfo {
	return SystemInfo{
		Vendor:      readSysFile(filepath.Join(SysDMIPath, "sys_vendor")),
		Product:     readSysFile(filepath.Join(SysDMIPath, "product_name")),
		UUID:        readSysFile(filepath.Join(SysDMIPath, "product_uuid")),
		BIOSVersion: readSysFile(filepath.Join(SysDMIPath, "bios_version")),
		// SerialNumber intentionally omitted from sysfs reads; may be populated by CAPRF.
	}
}

func collectCPUs() []CPUInfo {
	f, err := os.Open(ProcCPUInfoPath) //nolint:gosec // trusted sysfs path
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck // best-effort

	type cpuEntry struct {
		model     string
		cores     int
		socket    int
		freqMHz   int
		microcode string
	}

	var entries []cpuEntry
	var cur cpuEntry
	threadCounts := map[int]int{} // socket → thread count

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if cur.model != "" {
				threadCounts[cur.socket]++
				entries = append(entries, cur)
			}
			cur = cpuEntry{}
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		switch key {
		case "model name":
			cur.model = val
		case "cpu cores":
			cur.cores, _ = strconv.Atoi(val)
		case "physical id":
			cur.socket, _ = strconv.Atoi(val)
		case "cpu MHz":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				cur.freqMHz = int(f)
			}
		case "microcode":
			cur.microcode = val
		}
	}
	// Flush last entry.
	if cur.model != "" {
		threadCounts[cur.socket]++
		entries = append(entries, cur)
	}

	// Deduplicate by socket.
	seen := map[int]bool{}
	var cpus []CPUInfo
	for _, e := range entries {
		if seen[e.socket] {
			continue
		}
		seen[e.socket] = true
		cpus = append(cpus, CPUInfo{
			Model:     e.model,
			Cores:     e.cores,
			Threads:   threadCounts[e.socket],
			Socket:    e.socket,
			FreqMHz:   e.freqMHz,
			Microcode: e.microcode,
		})
	}
	return cpus
}

func collectMemory() MemoryInfo {
	f, err := os.Open(ProcMemInfoPath) //nolint:gosec // trusted sysfs path
	if err != nil {
		return MemoryInfo{}
	}
	defer f.Close() //nolint:errcheck // best-effort

	var mem MemoryInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, _ := strconv.ParseUint(fields[1], 10, 64)
				mem.TotalBytes = kb * 1024
			}
			break
		}
	}
	mem.DIMMs = collectDIMMs()
	return mem
}

// collectDIMMs reads DIMM info from DMI SMBIOS type-17 entries.
// Falls back gracefully if DMI data is unavailable.
func collectDIMMs() []DIMMInfo {
	// DMI type 17 = Memory Device. Search for all type-17 directories.
	parent := filepath.Dir(SysDMIMemoryPath)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}

	var dimms []DIMMInfo
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "17-") {
			continue
		}
		dir := filepath.Join(parent, e.Name())
		slot := readDMIField(dir, "locator")
		sizeStr := readDMIField(dir, "size")
		memType := readDMIField(dir, "type")
		speedStr := readDMIField(dir, "speed")

		if sizeStr == "" || sizeStr == "No Module Installed" || sizeStr == "0" {
			continue
		}

		var sizeGB int
		var sizeMB uint64
		if _, err := fmt.Sscanf(sizeStr, "%d", &sizeMB); err == nil {
			sizeGB = int(sizeMB / 1024) //nolint:gosec // trusted DMI value
		}

		var speedMHz int
		if speedStr != "" {
			_, _ = fmt.Sscanf(speedStr, "%d", &speedMHz)
		}

		dimms = append(dimms, DIMMInfo{
			Slot:     slot,
			SizeGB:   sizeGB,
			Type:     memType,
			SpeedMHz: speedMHz,
		})
	}
	return dimms
}

func readDMIField(dir, field string) string {
	data, err := os.ReadFile(filepath.Join(dir, field)) //nolint:gosec // trusted sysfs path
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func collectDisks() []DiskInfo {
	entries, err := os.ReadDir(SysBlockPath)
	if err != nil {
		return nil
	}

	var disks []DiskInfo
	for _, entry := range entries {
		name := entry.Name()
		// Skip virtual devices.
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "sr") {
			continue
		}

		d := DiskInfo{Name: name}
		devPath := filepath.Join(SysBlockPath, name, "device")

		d.Model = readSysFile(filepath.Join(devPath, "model"))
		d.Serial = readSysFile(filepath.Join(devPath, "serial"))
		d.Firmware = readSysFile(filepath.Join(devPath, "firmware_rev"))

		// Size: /sys/block/<dev>/size is in 512-byte sectors.
		if sizeStr := readSysFile(filepath.Join(SysBlockPath, name, "size")); sizeStr != "" {
			sectors, _ := strconv.ParseUint(sizeStr, 10, 64)
			d.SizeBytes = sectors * 512
		}

		// Rotational: 0 = SSD/NVMe, 1 = HDD.
		rotStr := readSysFile(filepath.Join(SysBlockPath, name, "queue", "rotational"))
		d.Rotational = rotStr == "1"
		switch {
		case d.Rotational:
			d.Type = "HDD"
		case strings.HasPrefix(name, "nvme"):
			d.Type = "NVMe"
		default:
			d.Type = "SSD"
		}

		// Transport heuristic.
		switch {
		case strings.HasPrefix(name, "nvme"):
			d.Transport = "NVMe"
		case fileExists(filepath.Join(devPath, "sas_address")):
			d.Transport = "SAS"
		default:
			d.Transport = "SATA"
		}

		disks = append(disks, d)
	}
	return disks
}

func collectNICs() []NICInfo {
	entries, err := os.ReadDir(SysNetPath)
	if err != nil {
		return nil
	}

	var nics []NICInfo
	for _, entry := range entries {
		name := entry.Name()
		// Skip virtual interfaces.
		if name == "lo" || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "virbr") || strings.HasPrefix(name, "bond") {
			continue
		}

		devPath := filepath.Join(SysNetPath, name, "device")
		if !fileExists(devPath) {
			continue // not a physical device
		}

		nic := NICInfo{
			Name:     name,
			MAC:      readSysFile(filepath.Join(SysNetPath, name, "address")),
			Speed:    readSysFile(filepath.Join(SysNetPath, name, "speed")),
			Firmware: readSysFile(filepath.Join(devPath, "firmware_version")),
		}

		// Driver: resolve symlink /sys/class/net/<name>/device/driver → basename.
		if driverLink, err := os.Readlink(filepath.Join(devPath, "driver")); err == nil {
			nic.Driver = filepath.Base(driverLink)
		}

		// PCI address: resolve symlink /sys/class/net/<name>/device → basename.
		if devLink, err := os.Readlink(devPath); err == nil {
			nic.PCIAddr = filepath.Base(devLink)
		}

		// SR-IOV total VFs.
		if vfStr := readSysFile(filepath.Join(devPath, "sriov_totalvfs")); vfStr != "" {
			nic.SRIOVVFs, _ = strconv.Atoi(vfStr)
		}

		nics = append(nics, nic)
	}
	return nics
}

// sriovInspector returns the SR-IOV inspector for SysNetPath. Tests replace
// it to keep devlink and rtnetlink of the test host out.
var sriovInspector = func() *sriov.Inspector {
	in := sriov.NewInspector()
	in.SysNet = SysNetPath
	return in
}

// attachSRIOV adds the SR-IOV report to every NIC that is a PF port. A
// failed discovery leaves the NICs without it.
func attachSRIOV(nics []NICInfo) {
	pfs, err := sriovInspector().Discover()
	if err != nil {
		return
	}
	byName := make(map[string]*sriov.PF, len(pfs))
	for i := range pfs {
		byName[pfs[i].Interface] = &pfs[i]
	}
	for i := range nics {
		nics[i].SRIOV = byName[nics[i].Name]
	}
}

func collectPCI() []PCIDevice {
	entries, err := os.ReadDir(SysPCIPath)
	if err != nil {
		return nil
	}

	var devs []PCIDevice
	for _, entry := range entries {
		addr := entry.Name()
		base := filepath.Join(SysPCIPath, addr)
		dev := PCIDevice{
			Address:  addr,
			Vendor:   readSysFile(filepath.Join(base, "vendor")),
			Device:   readSysFile(filepath.Join(base, "device")),
			Class:    readSysFile(filepath.Join(base, "class")),
			Revision: readSysFile(filepath.Join(base, "revision")),
		}
		if driverLink, err := os.Readlink(filepath.Join(base, "driver")); err == nil {
			dev.Driver = filepath.Base(driverLink)
		}
		devs = append(devs, dev)
	}
	return devs
}

// findAccelerators identifies GPU/accelerator PCI devices by class code.
// PCI class 0x03 = display controller, 0x12 = processing accelerator.
func findAccelerators(devices []PCIDevice) []Accelerator {
	var accels []Accelerator
	for _, d := range devices {
		classPrefix := strings.TrimPrefix(d.Class, "0x")
		if len(classPrefix) < 2 {
			continue
		}
		// Check first two hex digits for display (03) or accelerator (12).
		switch classPrefix[:2] {
		case "03", "12":
			accels = append(accels, Accelerator{
				Name:    fmt.Sprintf("%s:%s", d.Vendor, d.Device),
				Vendor:  d.Vendor,
				PCIAddr: d.Address,
				Driver:  d.Driver,
			})
		}
	}
	return accels
}

// readSysFile reads a single-line sysfs file, returning empty string on error.
func readSysFile(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // trusted sysfs path
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readSysfs reads a named attribute from a sysfs device directory.
func readSysfs(devPath, attr string) string {
	return readSysFile(filepath.Join(devPath, attr))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
