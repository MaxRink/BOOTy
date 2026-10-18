package sriov

import "fmt"

// vendorTool is the documented way to change the firmware settings of a
// NIC family that has no BOOTy backend for them.
type vendorTool struct {
	drivers []string
	family  string
	tool    string
}

// vendorTools lists the NIC families and their firmware tools. BOOTy has
// no backend for these settings; docs/sriov.md has the same table.
var vendorTools = []vendorTool{
	{[]string{"mlx5_core", "mlx4_core"}, "NVIDIA ConnectX/BlueField", "mstconfig (mstflint, NIC_TOOLS=opensource) or mlxconfig (MFT, NIC_TOOLS=vendor)"},
	{[]string{"ice", "i40e", "ixgbe", "igb", "idpf"}, "Intel", "the NVM: nvmupdate64e (vendor flavor) or the Redfish NIC attributes of the BMC"},
	{[]string{"bnxt_en"}, "Broadcom NetXtreme-E", "niccli or bnxtnvm (vendor flavor) or the Redfish NIC attributes; devlink enable_sriov covers SR-IOV on/off"},
	{[]string{"qede", "qed"}, "Marvell/QLogic FastLinQ", "the vendor NVM tool (QConvergeConsole CLI) or the Redfish NIC attributes"},
	{[]string{"cxgb4"}, "Chelsio T5/T6", "the firmware configuration file and cxgbtool"},
	{[]string{"sfc", "sfc_ef100"}, "AMD Solarflare", "sfboot (vendor flavor)"},
	{[]string{"ionic"}, "AMD Pensando", "the vendor tooling of the DSC"},
}

// noBackendReason explains why no backend handles a setting of a driver
// and names the tool that can change it.
func noBackendReason(driver, setting string) string {
	for _, v := range vendorTools {
		for _, d := range v.drivers {
			if d == driver {
				return fmt.Sprintf("no BOOTy backend for %s on %s (%s): use %s", setting, driver, v.family, v.tool)
			}
		}
	}
	return fmt.Sprintf("no firmware backend for driver %q", driver)
}
