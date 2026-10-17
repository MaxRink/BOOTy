//go:build linux

package inventory

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/sriov"
)

func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectSystem(t *testing.T) {
	dir := t.TempDir()
	origPath := SysDMIPath
	SysDMIPath = dir
	t.Cleanup(func() { SysDMIPath = origPath })

	writeFile(t, dir, "sys_vendor", "Dell Inc.\n")
	writeFile(t, dir, "product_name", "PowerEdge R750\n")
	writeFile(t, dir, "product_uuid", "4C4C4544-0042-4810-8035-C7C04F4A3533\n")
	writeFile(t, dir, "bios_version", "1.6.3\n")

	info := collectSystem()
	if info.Vendor != "Dell Inc." {
		t.Errorf("Vendor = %q, want %q", info.Vendor, "Dell Inc.")
	}
	if info.Product != "PowerEdge R750" {
		t.Errorf("Product = %q, want %q", info.Product, "PowerEdge R750")
	}
	if info.UUID != "4C4C4544-0042-4810-8035-C7C04F4A3533" {
		t.Errorf("UUID = %q", info.UUID)
	}
	if info.BIOSVersion != "1.6.3" {
		t.Errorf("BIOSVersion = %q, want %q", info.BIOSVersion, "1.6.3")
	}
}

func TestCollectSystemMissingFiles(t *testing.T) {
	dir := t.TempDir()
	origPath := SysDMIPath
	SysDMIPath = dir
	t.Cleanup(func() { SysDMIPath = origPath })

	info := collectSystem()
	if info.Vendor != "" || info.Product != "" {
		t.Errorf("expected empty system info, got vendor=%q product=%q", info.Vendor, info.Product)
	}
}

func TestCollectCPUs(t *testing.T) {
	dir := t.TempDir()
	cpuinfo := filepath.Join(dir, "cpuinfo")

	content := "processor\t: 0\nmodel name\t: Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz\ncpu cores\t: 32\nphysical id\t: 0\ncpu MHz\t\t: 2000.000\nmicrocode\t: 0xd0003b9\n\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz\ncpu cores\t: 32\nphysical id\t: 0\ncpu MHz\t\t: 2000.000\nmicrocode\t: 0xd0003b9\n\nprocessor\t: 2\nmodel name\t: Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz\ncpu cores\t: 32\nphysical id\t: 1\ncpu MHz\t\t: 2000.000\nmicrocode\t: 0xd0003b9\n"
	if err := os.WriteFile(cpuinfo, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	origPath := ProcCPUInfoPath
	ProcCPUInfoPath = cpuinfo
	t.Cleanup(func() { ProcCPUInfoPath = origPath })

	cpus := collectCPUs()
	if len(cpus) != 2 {
		t.Fatalf("got %d CPUs, want 2", len(cpus))
	}
	if cpus[0].Model != "Intel(R) Xeon(R) Gold 6338 CPU @ 2.00GHz" {
		t.Errorf("CPU[0].Model = %q", cpus[0].Model)
	}
	if cpus[0].Cores != 32 {
		t.Errorf("CPU[0].Cores = %d, want 32", cpus[0].Cores)
	}
	if cpus[0].Threads != 2 {
		t.Errorf("CPU[0].Threads = %d, want 2", cpus[0].Threads)
	}
	if cpus[1].Socket != 1 {
		t.Errorf("CPU[1].Socket = %d, want 1", cpus[1].Socket)
	}
	if cpus[0].FreqMHz != 2000 {
		t.Errorf("CPU[0].FreqMHz = %d, want 2000", cpus[0].FreqMHz)
	}
	if cpus[0].Microcode != "0xd0003b9" {
		t.Errorf("CPU[0].Microcode = %q", cpus[0].Microcode)
	}
}

func TestCollectCPUsMissingFile(t *testing.T) {
	origPath := ProcCPUInfoPath
	ProcCPUInfoPath = "/nonexistent/cpuinfo"
	t.Cleanup(func() { ProcCPUInfoPath = origPath })

	cpus := collectCPUs()
	if cpus != nil {
		t.Errorf("expected nil, got %d CPUs", len(cpus))
	}
}

func TestCollectMemory(t *testing.T) {
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	content := "MemTotal:       131891200 kB\nMemFree:         2048000 kB\nMemAvailable:    4096000 kB\n"
	if err := os.WriteFile(meminfo, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	origPath := ProcMemInfoPath
	ProcMemInfoPath = meminfo
	t.Cleanup(func() { ProcMemInfoPath = origPath })

	mem := collectMemory()
	want := uint64(131891200) * 1024
	if mem.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d", mem.TotalBytes, want)
	}
}

func TestCollectMemoryMissingFile(t *testing.T) {
	origPath := ProcMemInfoPath
	ProcMemInfoPath = "/nonexistent/meminfo"
	t.Cleanup(func() { ProcMemInfoPath = origPath })

	mem := collectMemory()
	if mem.TotalBytes != 0 {
		t.Errorf("TotalBytes = %d, want 0", mem.TotalBytes)
	}
}

func TestCollectDisks(t *testing.T) {
	dir := t.TempDir()
	origPath := SysBlockPath
	SysBlockPath = dir
	t.Cleanup(func() { SysBlockPath = origPath })

	writeFile(t, dir, "sda/device/model", "ST4000NM0035\n")
	writeFile(t, dir, "sda/device/serial", "ZC11AG20\n")
	writeFile(t, dir, "sda/device/firmware_rev", "TN03\n")
	writeFile(t, dir, "sda/size", "7814037168\n")
	writeFile(t, dir, "sda/queue/rotational", "1\n")

	writeFile(t, dir, "nvme0n1/device/model", "Samsung SSD 980 PRO\n")
	writeFile(t, dir, "nvme0n1/device/serial", "S5GXNG0R123456\n")
	writeFile(t, dir, "nvme0n1/device/firmware_rev", "5B2QGXA7\n")
	writeFile(t, dir, "nvme0n1/size", "1953525168\n")
	writeFile(t, dir, "nvme0n1/queue/rotational", "0\n")

	writeFile(t, dir, "loop0/size", "0\n")

	disks := collectDisks()
	if len(disks) != 2 {
		t.Fatalf("got %d disks, want 2", len(disks))
	}

	var sda, nvme DiskInfo
	for _, d := range disks {
		switch d.Name {
		case "sda":
			sda = d
		case "nvme0n1":
			nvme = d
		}
	}

	if sda.Model != "ST4000NM0035" {
		t.Errorf("sda.Model = %q", sda.Model)
	}
	if !sda.Rotational {
		t.Error("sda should be rotational")
	}
	if sda.Type != "HDD" {
		t.Errorf("sda.Type = %q, want HDD", sda.Type)
	}
	if sda.SizeBytes != 7814037168*512 {
		t.Errorf("sda.SizeBytes = %d", sda.SizeBytes)
	}
	if nvme.Type != "NVMe" {
		t.Errorf("nvme.Type = %q, want NVMe", nvme.Type)
	}
	if nvme.Transport != "NVMe" {
		t.Errorf("nvme.Transport = %q, want NVMe", nvme.Transport)
	}
	if nvme.Firmware != "5B2QGXA7" {
		t.Errorf("nvme.Firmware = %q", nvme.Firmware)
	}
}

func TestCollectDisksEmpty(t *testing.T) {
	dir := t.TempDir()
	origPath := SysBlockPath
	SysBlockPath = dir
	t.Cleanup(func() { SysBlockPath = origPath })

	disks := collectDisks()
	if len(disks) != 0 {
		t.Errorf("got %d disks, want 0", len(disks))
	}
}

func TestCollectNICs(t *testing.T) {
	dir := t.TempDir()
	origPath := SysNetPath
	SysNetPath = dir
	t.Cleanup(func() { SysNetPath = origPath })

	writeFile(t, dir, "eth0/address", "aa:bb:cc:dd:ee:ff\n")
	writeFile(t, dir, "eth0/speed", "25000\n")
	writeFile(t, dir, "eth0/device/firmware_version", "22.31.1014\n")
	writeFile(t, dir, "eth0/device/sriov_totalvfs", "128\n")
	writeFile(t, dir, "eth0/device/.present", "")

	writeFile(t, dir, "lo/address", "00:00:00:00:00:00\n")

	nics := collectNICs()
	if len(nics) != 1 {
		t.Fatalf("got %d NICs, want 1", len(nics))
	}
	if nics[0].Name != "eth0" {
		t.Errorf("Name = %q", nics[0].Name)
	}
	if nics[0].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("MAC = %q", nics[0].MAC)
	}
	if nics[0].Firmware != "22.31.1014" {
		t.Errorf("Firmware = %q", nics[0].Firmware)
	}
	if nics[0].SRIOVVFs != 128 {
		t.Errorf("SRIOVVFs = %d, want 128", nics[0].SRIOVVFs)
	}
}

func TestCollectNICsNoDevice(t *testing.T) {
	dir := t.TempDir()
	origPath := SysNetPath
	SysNetPath = dir
	t.Cleanup(func() { SysNetPath = origPath })

	writeFile(t, dir, "veth12345/address", "aa:bb:cc:dd:ee:ff\n")

	nics := collectNICs()
	if len(nics) != 0 {
		t.Errorf("got %d NICs, want 0", len(nics))
	}
}

func TestCollectPCI(t *testing.T) {
	dir := t.TempDir()
	origPath := SysPCIPath
	SysPCIPath = dir
	t.Cleanup(func() { SysPCIPath = origPath })

	writeFile(t, dir, "0000:00:1f.0/vendor", "0x8086\n")
	writeFile(t, dir, "0000:00:1f.0/device", "0xa382\n")
	writeFile(t, dir, "0000:00:1f.0/class", "0x060100\n")
	writeFile(t, dir, "0000:00:1f.0/revision", "0x10\n")

	devs := collectPCI()
	if len(devs) != 1 {
		t.Fatalf("got %d PCI devices, want 1", len(devs))
	}
	if devs[0].Address != "0000:00:1f.0" {
		t.Errorf("Address = %q", devs[0].Address)
	}
	if devs[0].Vendor != "0x8086" {
		t.Errorf("Vendor = %q", devs[0].Vendor)
	}
}

func TestFindAccelerators(t *testing.T) {
	devices := []PCIDevice{
		{Address: "0000:01:00.0", Vendor: "0x10de", Device: "0x2204", Class: "0x030000", Driver: "nvidia"},
		{Address: "0000:00:1f.0", Vendor: "0x8086", Device: "0xa382", Class: "0x060100"},
		{Address: "0000:02:00.0", Vendor: "0x8086", Device: "0x0d5d", Class: "0x120000", Driver: "idxd"},
	}

	accels := findAccelerators(devices)
	if len(accels) != 2 {
		t.Fatalf("got %d accelerators, want 2", len(accels))
	}
	if accels[0].PCIAddr != "0000:01:00.0" {
		t.Errorf("accels[0].PCIAddr = %q", accels[0].PCIAddr)
	}
	if accels[0].Driver != "nvidia" {
		t.Errorf("accels[0].Driver = %q", accels[0].Driver)
	}
	if accels[1].PCIAddr != "0000:02:00.0" {
		t.Errorf("accels[1].PCIAddr = %q", accels[1].PCIAddr)
	}
}

func TestCollectIntegration(t *testing.T) {
	dir := t.TempDir()

	origDMI := SysDMIPath
	origCPU := ProcCPUInfoPath
	origMem := ProcMemInfoPath
	origBlock := SysBlockPath
	origNet := SysNetPath
	origPCI := SysPCIPath
	t.Cleanup(func() {
		SysDMIPath = origDMI
		ProcCPUInfoPath = origCPU
		ProcMemInfoPath = origMem
		SysBlockPath = origBlock
		SysNetPath = origNet
		SysPCIPath = origPCI
	})

	dmiDir := filepath.Join(dir, "dmi")
	SysDMIPath = dmiDir
	writeFile(t, dmiDir, "sys_vendor", "TestVendor\n")
	writeFile(t, dmiDir, "product_name", "TestProduct\n")

	cpuFile := filepath.Join(dir, "cpuinfo")
	if err := os.WriteFile(cpuFile, []byte("processor\t: 0\nmodel name\t: TestCPU\ncpu cores\t: 4\nphysical id\t: 0\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ProcCPUInfoPath = cpuFile

	memFile := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(memFile, []byte("MemTotal:       16384000 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ProcMemInfoPath = memFile

	blockDir := filepath.Join(dir, "block")
	SysBlockPath = blockDir
	if err := os.MkdirAll(blockDir, 0o755); err != nil {
		t.Fatal(err)
	}

	netDir := filepath.Join(dir, "net")
	SysNetPath = netDir
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatal(err)
	}

	pciDir := filepath.Join(dir, "pci")
	SysPCIPath = pciDir
	if err := os.MkdirAll(pciDir, 0o755); err != nil {
		t.Fatal(err)
	}

	inv, err := Collect()
	if err != nil {
		t.Fatalf("Collect() error: %v", err)
	}
	if inv.Timestamp.IsZero() {
		t.Error("Timestamp should not be zero")
	}
	if inv.System.Vendor != "TestVendor" {
		t.Errorf("System.Vendor = %q", inv.System.Vendor)
	}
	if len(inv.CPUs) != 1 {
		t.Errorf("got %d CPUs, want 1", len(inv.CPUs))
	}
	if inv.Memory.TotalBytes != 16384000*1024 {
		t.Errorf("Memory.TotalBytes = %d", inv.Memory.TotalBytes)
	}
}

func TestReadSysFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readSysFile(path)
	if got != "hello" {
		t.Errorf("readSysFile = %q, want %q", got, "hello")
	}

	got = readSysFile(filepath.Join(dir, "nonexistent"))
	if got != "" {
		t.Errorf("readSysFile(nonexistent) = %q, want empty", got)
	}
}

func TestCollectNICsAttachesSRIOV(t *testing.T) {
	dir := t.TempDir()
	origPath, origInspector := SysNetPath, sriovInspector
	SysNetPath = dir
	sriovInspector = func() *sriov.Inspector { return &sriov.Inspector{SysNet: SysNetPath} }
	t.Cleanup(func() { SysNetPath, sriovInspector = origPath, origInspector })

	writeFile(t, dir, "ens1f0np0/address", "aa:bb:cc:dd:ee:ff\n")
	writeFile(t, dir, "ens1f0np0/type", "1\n")
	writeFile(t, dir, "ens1f0np0/phys_port_name", "p0\n")
	writeFile(t, dir, "ens1f0np0/device/sriov_totalvfs", "64\n")
	writeFile(t, dir, "ens1f0np0/device/sriov_numvfs", "4\n")
	writeFile(t, dir, "pf0vf0/phys_port_name", "pf0vf0\n")
	writeFile(t, dir, "pf0vf0/device/.present", "")

	nics := collectNICs()
	attachSRIOV(nics)
	if len(nics) != 2 {
		t.Fatalf("got %d NICs, want 2", len(nics))
	}
	for _, n := range nics {
		switch n.Name {
		case "ens1f0np0":
			if n.SRIOV == nil || !n.SRIOV.SRIOVCapable || n.SRIOV.TotalVFs != 64 || n.SRIOV.NumVFs != 4 ||
				n.SRIOV.LinkType != sriov.LinkTypeEthernet || n.SRIOV.PhysPortName != "p0" {
				t.Errorf("ens1f0np0 SR-IOV report = %+v", n.SRIOV)
			}
		case "pf0vf0":
			if n.SRIOV != nil {
				t.Errorf("representor pf0vf0 has an SR-IOV report: %+v", n.SRIOV)
			}
		}
	}
}

func TestAttachSRIOVWithoutSysNet(t *testing.T) {
	origInspector := sriovInspector
	sriovInspector = func() *sriov.Inspector { return &sriov.Inspector{SysNet: filepath.Join(t.TempDir(), "missing")} }
	t.Cleanup(func() { sriovInspector = origInspector })
	nics := []NICInfo{{Name: "eth0"}}
	attachSRIOV(nics)
	if nics[0].SRIOV != nil {
		t.Fatalf("SR-IOV report without sysfs: %+v", nics[0].SRIOV)
	}
}
