package sriov

import "strings"

// adapterKey groups the PF ports of one adapter: PCI functions of the same
// slot (domain:bus:device), or the ports of one non-PCI device.
func adapterKey(pf *PF) string {
	if pf.Bus == "pci" {
		if i := strings.LastIndexByte(pf.PCIAddress, '.'); i > 0 {
			return pf.PCIAddress[:i]
		}
	}
	return pf.Bus + "/" + pf.PCIAddress
}

// groupPorts sets PortCount and the VF LAG capability of every PF.
func groupPorts(pfs []PF) {
	count := map[string]int{}
	for i := range pfs {
		count[adapterKey(&pfs[i])]++
	}
	for i := range pfs {
		pfs[i].PortCount = count[adapterKey(&pfs[i])]
		pfs[i].VFLAG, pfs[i].VFLAGReason = vfLAG(&pfs[i])
	}
}

// vfLAG decides the VF LAG capability from the driver, the port count and
// the eSwitch support. The kernel has no query for it, so only a driver
// with VF LAG and two ports on the adapter can be "supported", and the
// drivers that depend on firmware or DDP features stay "unknown".
func vfLAG(pf *PF) (capability Capability, reason string) {
	switch pf.Driver {
	case "mlx5_core":
		switch {
		case pf.LinkType != "" && pf.LinkType != LinkTypeEthernet:
			return CapUnsupported, "mlx5 VF LAG needs Ethernet ports"
		case pf.PortCount < 2:
			return CapUnsupported, "one PF port on the adapter"
		case pf.Eswitch.Capability != CapSupported:
			return CapUnsupported, "no devlink eSwitch on the device"
		}
		return CapSupported, "mlx5 VF LAG: bond both PF ports of the adapter (switchdev, or legacy mode with the LAG bond before the VFs)"
	case "ice":
		if pf.PortCount < 2 {
			return CapUnsupported, "one PF port on the adapter"
		}
		return CapUnknown, "ice SR-IOV LAG depends on the kernel version, the NVM and the DDP package"
	case "bnxt_en":
		if pf.PortCount < 2 || pf.Eswitch.Capability != CapSupported {
			return CapUnsupported, "needs two PF ports and a devlink eSwitch"
		}
		return CapUnknown, "bnxt_en VF LAG depends on TruFlow firmware support"
	case "":
		return CapUnknown, "no driver bound"
	}
	return CapUnsupported, "driver " + pf.Driver + " has no VF LAG"
}
