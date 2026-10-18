// Package inventory collects hardware inventory from sysfs and procfs.
package inventory

import (
	"time"

	"github.com/telekom/BOOTy/pkg/network/lldp"
	"github.com/telekom/BOOTy/pkg/sriov"
)

// HardwareInventory is the full hardware report collected during provisioning.
type HardwareInventory struct {
	Timestamp    time.Time     `json:"timestamp"`
	System       SystemInfo    `json:"system"`
	CPUs         []CPUInfo     `json:"cpus"`
	Memory       MemoryInfo    `json:"memory"`
	Disks        []DiskInfo    `json:"disks"`
	NICs         []NICInfo     `json:"nics"`
	PCIDevices   []PCIDevice   `json:"pciDevices,omitempty"`
	Accelerators []Accelerator `json:"accelerators,omitempty"`
	// LLDPNeighbors are the switches seen on the physical NICs.
	LLDPNeighbors []lldp.Neighbor `json:"lldpNeighbors,omitempty"`
}

// SystemInfo holds DMI system identification data.
type SystemInfo struct {
	Vendor       string `json:"vendor"`
	Product      string `json:"product"`
	SerialNumber string `json:"serialNumber"`
	UUID         string `json:"uuid"`
	BIOSVersion  string `json:"biosVersion"`
}

// CPUInfo describes a single CPU socket.
type CPUInfo struct {
	Model     string `json:"model"`
	Cores     int    `json:"cores"`
	Threads   int    `json:"threads"`
	Socket    int    `json:"socket"`
	FreqMHz   int    `json:"freqMHz"`
	Microcode string `json:"microcode"`
}

// MemoryInfo holds total memory and optional DIMM details.
type MemoryInfo struct {
	TotalBytes uint64     `json:"totalBytes"`
	DIMMs      []DIMMInfo `json:"dimms,omitempty"`
}

// DIMMInfo describes one memory DIMM slot.
type DIMMInfo struct {
	Slot     string `json:"slot"`
	SizeGB   int    `json:"sizeGB"`
	Type     string `json:"type"`
	SpeedMHz int    `json:"speedMHz"`
	ECC      bool   `json:"ecc"`
}

// DiskInfo describes a block device.
type DiskInfo struct {
	Name       string `json:"name"`
	Model      string `json:"model"`
	Serial     string `json:"serial"`
	SizeBytes  uint64 `json:"sizeBytes"`
	Type       string `json:"type"`
	Transport  string `json:"transport"`
	Firmware   string `json:"firmware"`
	Rotational bool   `json:"rotational"`
}

// NICInfo describes a network interface.
type NICInfo struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	MAC      string `json:"mac"`
	PCIAddr  string `json:"pciAddr"`
	Speed    string `json:"speed"`
	SRIOVVFs int    `json:"sriovVfs"`
	Firmware string `json:"firmware"`
	// SRIOV is the SR-IOV report of a PF port (docs/sriov.md). VFs and
	// VF representors have none.
	SRIOV *sriov.PF `json:"sriov,omitempty"`
}

// PCIDevice describes a PCI device from /sys/bus/pci/devices.
type PCIDevice struct {
	Address  string `json:"address"`
	Vendor   string `json:"vendor"`
	Device   string `json:"device"`
	Class    string `json:"class"`
	Driver   string `json:"driver,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// Accelerator describes a GPU or other accelerator card.
type Accelerator struct {
	Name    string `json:"name"`
	Vendor  string `json:"vendor"`
	PCIAddr string `json:"pciAddr"`
	Driver  string `json:"driver,omitempty"`
}
