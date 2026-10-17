package sriov

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// RuntimeOps changes the runtime SR-IOV state of PF ports. KernelRuntime
// is the implementation for the running kernel.
type RuntimeOps interface {
	// SetNumVFs writes sriov_numvfs of the device of the PF port.
	SetNumVFs(pf *PF, n int) error
	// SetEswitch sets the devlink eSwitch mode of the device.
	SetEswitch(pf *PF, mode string) error
	// SetVF sets the attributes of one VF through the PF netdev; mac is
	// empty to keep the MAC.
	SetVF(pf *PF, index int, s *VFSettings, mac string) error
	// InUse reports whether the ramdisk uses the PF port (an address, a
	// route or a master device), with the reason.
	InUse(pf *PF) (inUse bool, reason string)
	// Master returns the master device (bond, bridge, VRF) of the PF port,
	// "" without one.
	Master(pf *PF) string
}

// Runtime setting names in findings.
const (
	SettingNumVFs  = "numVFs"
	SettingEswitch = "eswitchMode"
	SettingHandoff = "handoff"
)

// runtimeDevice is one device (devlink handle) with runtime settings. The
// ports of a multi-port device share sriov_numvfs and the eSwitch.
type runtimeDevice struct {
	handle      string
	pf          *PF // the first port
	assignments []Assignment
	numVFs      *int
	eswitch     string
	origNumVFs  int
	origEswitch string
	cur         int  // sriov_numvfs now
	changed     bool // the VF count or the eSwitch changed
	eswitchSet  bool // the eSwitch changed
	keep        bool
	errs        map[string]string // setting: apply error
	refused     map[string]string // setting: why the in-use guard refused it
}

// portKey names a PF port by bus, PCI address and port, stable across a
// netdev re-register: phys_port_name, else the MAC.
func portKey(pf *PF) string {
	port := pf.PhysPortName
	if port == "" {
		port = pf.MAC
	}
	return pf.Bus + "/" + pf.PCIAddress + "/" + port
}

// deferDevices takes the devices of adapters with a pending firmware change
// out of the runtime pass and reports their settings as deferred.
func deferDevices(devs []*runtimeDevice, pendingFirmware map[string][]string) (keep []*runtimeDevice, deferred []Finding) {
	for _, d := range devs {
		settings := pendingFirmware[adapterKey(d.pf)]
		if len(settings) == 0 {
			keep = append(keep, d)
			continue
		}
		for _, a := range d.assignments {
			deferred = append(deferred, Finding{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelRuntime,
				Setting: "runtime", Status: StatusDeferred,
				Detail: "stored firmware change of " + strings.Join(settings, ", ") +
					" takes effect after the reboot; BOOTy boots the ramdisk again to check the runtime settings"})
		}
	}
	return keep, deferred
}

// runtimeAffecting reports the firmware settings that change what the
// runtime settings can do: the SR-IOV enable and the VF limit
// (sriov_totalvfs) and the link type. Other NV parameters, such as
// CQE_COMPRESSION, do not.
func runtimeAffecting(setting string) bool {
	return setting == SettingSRIOVEnabled || setting == SettingMaxVFs || setting == SettingLinkType
}

// needsReset reports a firmware finding that BOOTy stored and that takes
// effect only after a reset.
func needsReset(f *Finding) bool {
	return f.Level == LevelFirmware && f.Status == StatusApplied && f.Detail != detailActiveNow
}

func hasRuntimeSettings(rt *RuntimeSettings) bool {
	return rt.NumVFs != nil || rt.EswitchMode != ""
}

// runtimeDevices groups the assignments with runtime settings by device.
// Two ports of one device that want different values are a conflict.
func runtimeDevices(assignments []Assignment) (devs []*runtimeDevice, conflicts []Finding) {
	byHandle := map[string]*runtimeDevice{}
	for _, a := range assignments {
		rt := &a.Profile.Runtime
		if !hasRuntimeSettings(rt) {
			continue
		}
		h := a.PF.Bus + "/" + a.PF.PCIAddress
		d, ok := byHandle[h]
		if !ok {
			d = &runtimeDevice{handle: h, pf: a.PF, origNumVFs: a.PF.NumVFs, origEswitch: a.PF.Eswitch.Mode,
				numVFs: rt.NumVFs, eswitch: rt.EswitchMode, keep: rt.Handoff == HandoffKeep, errs: map[string]string{}, refused: map[string]string{}}
			byHandle[h] = d
			devs = append(devs, d)
		} else if intString(d.numVFs) != intString(rt.NumVFs) || d.eswitch != rt.EswitchMode {
			conflicts = append(conflicts, Finding{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelRuntime,
				Setting: SettingNumVFs, Status: StatusFailed,
				Detail: fmt.Sprintf("port %s of device %s wants other runtime settings", d.pf.Interface, h)})
			continue
		}
		d.assignments = append(d.assignments, a)
	}
	return devs, conflicts
}

// DefaultUplinkVFDrivers are the drivers that create and remove VFs
// without resetting the PF, so BOOTy may change the VF count of a port the
// ramdisk network uses (Linux 6.12):
//
//   - mlx5_core: mlx5_core_sriov_configure enables the legacy eSwitch and
//     calls pci_enable_sriov; the PF netdev stays up.
//   - ice: ice_sriov_configure (ice_ena_vfs, ice_free_vfs) changes only the
//     VF resources.
//
// The others refuse such a change: igb (igb_sriov_reinit closes and opens
// the PF), ixgbe (ixgbe_sriov_reinit) and i40e (i40e_pci_sriov_configure
// resets and rebuilds the PF when it turns VEB mode on or off), and every
// driver not in the list. SRIOV_UPLINK_VF_DRIVERS replaces the list.
var DefaultUplinkVFDrivers = []string{"mlx5_core", "ice"}

// applyDevice brings one device to its runtime settings: VFs off before an
// eSwitch change or a VF count change, then the eSwitch mode, the VF count
// and the VF attributes. uplinkVFDrivers may change the VF count of a port
// in use.
func applyDevice(ops RuntimeOps, d *runtimeDevice, uplinkVFDrivers []string) {
	d.cur = d.origNumVFs
	d.guardInUse(ops, uplinkVFDrivers)
	if _, refused := d.refused[SettingEswitch]; !refused {
		d.applyEswitch(ops)
	}
	if _, refused := d.refused[SettingNumVFs]; refused {
		return
	}
	if d.numVFs != nil && *d.numVFs != d.cur {
		if d.cur == 0 || *d.numVFs == 0 || d.setVFs(ops, 0) {
			d.setVFs(ops, *d.numVFs)
		}
	}
	if _, failed := d.errs[SettingNumVFs]; !failed && d.numVFs != nil {
		d.applyVFs(ops)
	}
}

// guardInUse refuses the changes that would disrupt the ramdisk network
// on a port it uses: every eSwitch change (mlx5 replaces the PF netdev by
// the uplink representor in switchdev mode), and a VF count change on a
// driver that resets the PF for it.
func (d *runtimeDevice) guardInUse(ops RuntimeOps, uplinkVFDrivers []string) {
	eswitchChange := d.eswitch != "" && d.eswitch != d.origEswitch
	countChange := d.numVFs != nil && *d.numVFs != d.cur
	if !eswitchChange && !countChange {
		return
	}
	inUse, reason := ops.InUse(d.pf)
	if !inUse {
		return
	}
	if eswitchChange {
		d.refused[SettingEswitch] = "the eSwitch change would disrupt the ramdisk network: " + reason
	}
	if countChange && !slices.Contains(uplinkVFDrivers, d.pf.Driver) {
		d.refused[SettingNumVFs] = fmt.Sprintf("driver %s resets the PF for a VF count change, which would disrupt the ramdisk network: %s",
			d.pf.Driver, reason)
		return
	}
	// mlx5 with a bonded uplink: the first VFs enable the eSwitch, and
	// mlx5_eswitch_enable toggles the hardware LAG around it
	// (mlx5_lag_disable_change and mlx5_lag_enable_change, Linux 6.12), so
	// the bond flaps.
	if master := ops.Master(d.pf); countChange && d.pf.Driver == "mlx5_core" && master != "" {
		d.refused[SettingNumVFs] = fmt.Sprintf("mlx5 toggles the hardware LAG of the bonded uplink (master %s) for a VF count change, "+
			"which would disrupt the ramdisk network: %s", master, reason)
	}
}

// setVFs writes sriov_numvfs and records a failure.
func (d *runtimeDevice) setVFs(ops RuntimeOps, n int) bool {
	if err := ops.SetNumVFs(d.pf, n); err != nil {
		d.errs[SettingNumVFs] = fmt.Sprintf("sriov_numvfs %d: %v", n, err)
		return false
	}
	d.cur, d.changed = n, true
	return true
}

// applyEswitch changes the eSwitch mode with the VFs off.
func (d *runtimeDevice) applyEswitch(ops RuntimeOps) {
	if d.eswitch == "" || d.eswitch == d.origEswitch {
		return
	}
	if d.cur != 0 && !d.setVFs(ops, 0) {
		return
	}
	if err := ops.SetEswitch(d.pf, d.eswitch); err != nil {
		d.errs[SettingEswitch] = fmt.Sprintf("devlink eswitch %s: %v", d.eswitch, err)
		return
	}
	d.changed, d.eswitchSet = true, true
}

func (d *runtimeDevice) applyVFs(ops RuntimeOps) {
	for _, a := range d.assignments {
		for i := 0; i < *d.numVFs; i++ {
			s, mac := effectiveVF(&a.Profile.Runtime, i)
			if s == (VFSettings{}) && mac == "" {
				continue
			}
			if err := ops.SetVF(a.PF, i, &s, mac); err != nil {
				d.errs[vfSetting(i)] = fmt.Sprintf("VF %d: %v", i, err)
			}
		}
	}
}

func vfSetting(i int) string { return "vfs[" + strconv.Itoa(i) + "]" }

// effectiveVF merges the VF defaults and the override of VF i.
func effectiveVF(rt *RuntimeSettings, i int) (s VFSettings, mac string) {
	s = rt.VFDefaults
	for _, o := range rt.VFs {
		if o.Index != i {
			continue
		}
		mergeVF(&s, &o.VFSettings)
		return s, o.MAC
	}
	return s, ""
}

func mergeVF(dst, src *VFSettings) {
	if src.Trust != nil {
		dst.Trust = src.Trust
	}
	if src.SpoofChk != nil {
		dst.SpoofChk = src.SpoofChk
	}
	if src.VLAN != nil {
		dst.VLAN = src.VLAN
	}
	if src.QoS != nil {
		dst.QoS = src.QoS
	}
	if src.MinTxRate != nil {
		dst.MinTxRate = src.MinTxRate
	}
	if src.MaxTxRate != nil {
		dst.MaxTxRate = src.MaxTxRate
	}
	if src.LinkState != "" {
		dst.LinkState = src.LinkState
	}
}

// verifyRuntime compares the PF port after the apply with the profile.
func verifyRuntime(a Assignment, now *PF, d *runtimeDevice) []Finding {
	rt := &a.Profile.Runtime
	finding := func(setting, want, got string) Finding {
		f := Finding{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelRuntime,
			Setting: setting, Want: want, Got: got, Status: StatusOK}
		if want != got {
			f.Status = StatusFailed
		}
		if e, ok := d.errs[setting]; ok {
			f.Status, f.Detail = StatusFailed, e
		}
		refusal, refused := d.refused[setting]
		if !refused && strings.HasPrefix(setting, "vfs[") {
			// The VFs were not created because the VF count was refused.
			refusal, refused = d.refused[SettingNumVFs]
		}
		if refused && f.Status != StatusOK {
			f.Status, f.Detail = StatusRefusedInUse, refusal
		}
		return f
	}
	var out []Finding
	if rt.EswitchMode != "" {
		out = append(out, finding(SettingEswitch, rt.EswitchMode, now.Eswitch.Mode))
	}
	if rt.NumVFs == nil {
		return out
	}
	out = append(out, finding(SettingNumVFs, strconv.Itoa(*rt.NumVFs), strconv.Itoa(now.NumVFs)))
	for i := 0; i < *rt.NumVFs; i++ {
		s, mac := effectiveVF(rt, i)
		if s == (VFSettings{}) && mac == "" {
			continue
		}
		want, got := vfCompare(&s, mac, findVF(now.VFs, i))
		out = append(out, finding(vfSetting(i), want, got))
	}
	return out
}

func findVF(vfs []VF, i int) *VF {
	for j := range vfs {
		if vfs[j].Index == i {
			return &vfs[j]
		}
	}
	return nil
}

// vfCompare formats the wanted VF attributes and the same attributes of the
// VF, so a mismatch shows in the finding.
func vfCompare(s *VFSettings, mac string, vf *VF) (want, got string) {
	if vf == nil {
		vf = &VF{LinkState: "missing"}
	}
	var w, g []string
	add := func(name, wantV, gotV string) {
		w = append(w, name+"="+wantV)
		g = append(g, name+"="+gotV)
	}
	if mac != "" {
		add("mac", normalizeMAC(mac), vf.MAC)
	}
	if s.Trust != nil {
		add("trust", strconv.FormatBool(*s.Trust), strconv.FormatBool(vf.Trust))
	}
	if s.SpoofChk != nil {
		add("spoofChk", strconv.FormatBool(*s.SpoofChk), strconv.FormatBool(vf.SpoofChk))
	}
	if s.VLAN != nil {
		add("vlan", strconv.Itoa(*s.VLAN), strconv.Itoa(vf.VLAN))
	}
	if s.QoS != nil {
		add("qos", strconv.Itoa(*s.QoS), strconv.Itoa(vf.QoS))
	}
	if s.MinTxRate != nil {
		add("minTxRate", strconv.Itoa(*s.MinTxRate), strconv.FormatUint(uint64(vf.MinTxRate), 10))
	}
	if s.MaxTxRate != nil {
		add("maxTxRate", strconv.Itoa(*s.MaxTxRate), strconv.FormatUint(uint64(vf.MaxTxRate), 10))
	}
	if s.LinkState != "" {
		add("linkState", s.LinkState, vf.LinkState)
	}
	return strings.Join(w, " "), strings.Join(g, " ")
}

// resetDevice restores the VF count and the eSwitch mode from before the
// apply: VFs off, the old eSwitch mode, the old VF count.
func resetDevice(ops RuntimeOps, d *runtimeDevice) error {
	var errs []string
	if err := ops.SetNumVFs(d.pf, 0); err != nil {
		errs = append(errs, fmt.Sprintf("sriov_numvfs 0: %v", err))
	}
	if d.eswitchSet {
		if err := ops.SetEswitch(d.pf, d.origEswitch); err != nil {
			errs = append(errs, fmt.Sprintf("devlink eswitch %s: %v", d.origEswitch, err))
		}
	}
	if d.origNumVFs > 0 {
		if err := ops.SetNumVFs(d.pf, d.origNumVFs); err != nil {
			errs = append(errs, fmt.Sprintf("sriov_numvfs %d: %v", d.origNumVFs, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
