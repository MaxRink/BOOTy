// Package sriov reports, checks and configures SR-IOV on the physical NIC
// functions (PFs) of a host. It reads the kernel interfaces that every
// SR-IOV driver implements (sysfs, devlink and rtnetlink) and does not need
// vendor tools. docs/sriov.md describes the report and the handoff contract
// to the Kubernetes SR-IOV stack.
package sriov

import "errors"

// eSwitch modes as devlink reports them.
const (
	EswitchLegacy    = "legacy"
	EswitchSwitchdev = "switchdev"
)

// Link types of a PF netdev, from /sys/class/net/<if>/type.
const (
	LinkTypeEthernet   = "eth"
	LinkTypeInfiniband = "ib"
)

// Capability is a feature that BOOTy can confirm, rule out or not decide.
type Capability string

// Values of Capability.
const (
	// CapSupported means the kernel and the driver confirmed the feature.
	CapSupported Capability = "supported"
	// CapUnsupported means the kernel or the driver rejected the query
	// (for example EOPNOTSUPP), or the driver has no such feature.
	CapUnsupported Capability = "unsupported"
	// CapUnknown means BOOTy could not decide, for example because the
	// kernel has no devlink or the query failed for another reason.
	CapUnknown Capability = "unknown"
)

// ErrNotSupported is returned by a Devlink implementation when the driver
// or the kernel does not implement the request (EOPNOTSUPP), or when the
// device has no devlink instance.
var ErrNotSupported = errors.New("not supported by the driver or kernel")

// PF is the SR-IOV report of one physical function port. Fields that BOOTy
// could not read stay empty, and Warnings says why.
type PF struct {
	// Interface is the netdev name of the PF port.
	Interface string `json:"interface"`
	// MAC is the permanent or current MAC address of the PF port.
	MAC string `json:"mac,omitempty"`
	// Bus is the bus of the parent device, "pci" on real hardware and
	// "netdevsim" in tests. It is the devlink bus name.
	Bus string `json:"bus,omitempty"`
	// PCIAddress is the device name on Bus, for example 0000:3b:00.0. It
	// is the devlink device name.
	PCIAddress string `json:"pciAddress,omitempty"`
	// VendorID and DeviceID are the PCI IDs in lower-case hex without 0x.
	VendorID string `json:"vendorId,omitempty"`
	DeviceID string `json:"deviceId,omitempty"`
	// Driver is the kernel driver of the parent device.
	Driver string `json:"driver,omitempty"`
	// Firmware is the running firmware version (ethtool drvinfo, or the
	// devlink running version fw or fw.version).
	Firmware string `json:"firmware,omitempty"`
	// NUMANode is the NUMA node of the parent device, -1 for none.
	NUMANode int `json:"numaNode"`
	// LinkType is "eth", "ib", or "arphrd-<n>" for other link types.
	LinkType string `json:"linkType,omitempty"`

	// SRIOVCapable is true when the device exposes sriov_totalvfs > 0,
	// which needs SR-IOV enabled in the NIC firmware and in the BIOS.
	SRIOVCapable bool `json:"sriovCapable"`
	// TotalVFs is sriov_totalvfs, the maximum VF count after the last
	// firmware reset.
	TotalVFs int `json:"sriovTotalVFs"`
	// NumVFs is sriov_numvfs, the VF count now.
	NumVFs int `json:"sriovNumVFs"`

	// Eswitch is the devlink eSwitch state of the device.
	Eswitch Eswitch `json:"eswitch"`
	// PhysSwitchID is /sys/class/net/<if>/phys_switch_id. The DRA SR-IOV
	// driver detects an eSwitch by it. mlx5 sets it only in switchdev mode.
	PhysSwitchID string `json:"physSwitchId,omitempty"`
	// PhysPortName is /sys/class/net/<if>/phys_port_name, for example p0.
	PhysPortName string `json:"physPortName,omitempty"`
	// PortCount is the number of PF ports on the same adapter (PCI slot).
	PortCount int `json:"portCount"`
	// VFLAG is the VF LAG capability and VFLAGReason explains it.
	VFLAG       Capability `json:"vfLag"`
	VFLAGReason string     `json:"vfLagReason,omitempty"`

	// Devlink holds the devlink info versions, when the driver has devlink.
	Devlink *DevlinkInfo `json:"devlink,omitempty"`
	// VFs are the VFs that exist now, from rtnetlink.
	VFs []VF `json:"vfs,omitempty"`
	// Warnings list the reads that failed.
	Warnings []string `json:"warnings,omitempty"`
}

// Eswitch is the devlink eSwitch state of a device.
type Eswitch struct {
	// Capability is supported when devlink answered an eSwitch query.
	Capability Capability `json:"capability"`
	// Mode is "legacy" or "switchdev" when Capability is supported.
	Mode string `json:"mode,omitempty"`
	// InlineMode and EncapMode are reported when the driver sets them.
	InlineMode string `json:"inlineMode,omitempty"`
	EncapMode  string `json:"encapMode,omitempty"`
}

// DevlinkInfo is the devlink dev info of a device.
type DevlinkInfo struct {
	// Handle is the devlink handle, for example pci/0000:3b:00.0.
	Handle string `json:"handle"`
	// Driver and SerialNumber as devlink reports them.
	Driver       string `json:"driver,omitempty"`
	SerialNumber string `json:"serialNumber,omitempty"`
	// Fixed, Running and Stored are the version lists. A stored version
	// that differs from the running one waits for a firmware reset.
	Fixed   []Version `json:"fixed,omitempty"`
	Running []Version `json:"running,omitempty"`
	Stored  []Version `json:"stored,omitempty"`
}

// Version is one devlink info version.
type Version struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// PendingReset reports whether a stored firmware version differs from the
// running one, so a firmware reset or reboot is pending.
func (d *DevlinkInfo) PendingReset() bool {
	if d == nil {
		return false
	}
	for _, s := range d.Stored {
		for _, r := range d.Running {
			if r.Name == s.Name && r.Value != s.Value {
				return true
			}
		}
	}
	return false
}

// VF is one virtual function of a PF, as rtnetlink reports it.
type VF struct {
	// Index is the VF number on the PF.
	Index int `json:"index"`
	// PCIAddress is the device name of the VF (virtfn<Index> link).
	PCIAddress string `json:"pciAddress,omitempty"`
	MAC        string `json:"mac,omitempty"`
	VLAN       int    `json:"vlan,omitempty"`
	QoS        int    `json:"qos,omitempty"`
	// MinTxRate and MaxTxRate are in Mbit/s; 0 is unlimited.
	MinTxRate uint32 `json:"minTxRate,omitempty"`
	MaxTxRate uint32 `json:"maxTxRate,omitempty"`
	SpoofChk  bool   `json:"spoofChk"`
	Trust     bool   `json:"trust"`
	// LinkState is "auto", "enable" or "disable".
	LinkState string `json:"linkState,omitempty"`
}
