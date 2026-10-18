//go:build linux

package sriov

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
	"github.com/telekom/BOOTy/pkg/firmware/nic/ethtool"
)

// IntelFlags is the firmware backend of the Intel private flags: the
// firmware LLDP and DCBX agent of ice (fw-lldp-agent) and i40e
// (disable-fw-lldp). Intel documents the flag as kept in the NVM; it takes
// effect at once. It reuses the ethtool manager of pkg/firmware/nic; the
// netlink private-flag code of the cable check can replace it.
type IntelFlags struct {
	m *ethtool.Manager
}

// NewIntelFlags returns the backend with the ethtool binary of the ramdisk.
func NewIntelFlags(log *slog.Logger, cmd nic.Commander) *IntelFlags {
	return &IntelFlags{m: ethtool.NewWithCommander(nic.VendorIntel, "0x8086", log, cmd)}
}

// Name implements FirmwareBackend.
func (b *IntelFlags) Name() string { return "ethtool" }

// Tool implements FirmwareBackend.
func (b *IntelFlags) Tool() string { return ToolsOpenSource }

// Supports implements FirmwareBackend.
func (b *IntelFlags) Supports(pf *PF) bool { return pf.Driver == "ice" || pf.Driver == "i40e" }

// lldpFlag returns the private flag of the firmware LLDP agent and whether
// "on" means the agent runs.
func lldpFlag(driver string) (flag string, onIsAgent bool) {
	if driver == "ice" {
		return "fw-lldp-agent", true
	}
	return "disable-fw-lldp", false
}

const intelNVM = "Intel stores it in the NVM: use nvmupdate64e (vendor flavor) or the Redfish NIC attributes"

// Read implements FirmwareBackend.
func (b *IntelFlags) Read(ctx context.Context, pf *PF, want *FirmwareSettings) (*FirmwareRead, error) {
	r := &FirmwareRead{}
	for _, s := range []struct {
		set     bool
		setting string
	}{
		{want.SRIOVEnabled != nil, SettingSRIOVEnabled},
		{want.MaxVFs != nil, SettingMaxVFs},
		{want.LinkType != "", SettingLinkType},
	} {
		if s.set {
			r.Unsupported = append(r.Unsupported, Unsupported{s.setting, intelNVM})
		}
	}
	for _, s := range want.NVConfig {
		r.Unsupported = append(r.Unsupported, Unsupported{settingNVPrefix + s.Name, "NVIDIA NV config"})
	}
	for _, s := range want.DevlinkParams {
		r.Unsupported = append(r.Unsupported, Unsupported{settingParamPrefix + s.Name, "a devlink parameter"})
	}
	if want.FWLLDPAgent == nil {
		return r, nil
	}
	state := &nic.FirmwareState{Parameters: map[string]nic.Parameter{}}
	if err := b.m.CaptureViaEthtool(ctx, pf.Interface, state); err != nil {
		return nil, fmt.Errorf("ethtool %s: %w", pf.Interface, err)
	}
	flag, onIsAgent := lldpFlag(pf.Driver)
	p, ok := state.Parameters[flag]
	switch {
	case !ok:
		r.Unsupported = append(r.Unsupported, Unsupported{SettingFWLLDPAgent, fmt.Sprintf("%s has no private flag %s", pf.Interface, flag)})
	case p.Current == "on" || p.Current == "off":
		agent := (p.Current == "on") == onIsAgent
		r.Values.FWLLDPAgent = &agent
	default:
		r.Unsupported = append(r.Unsupported, Unsupported{SettingFWLLDPAgent, fmt.Sprintf("private flag %s is %q", flag, p.Current)})
	}
	return r, nil
}

// Apply implements FirmwareBackend. The flag takes effect at once, so no
// reboot is needed.
func (b *IntelFlags) Apply(ctx context.Context, pf *PF, change *FirmwareSettings) (bool, error) {
	if change.FWLLDPAgent == nil {
		return false, nil
	}
	flag, onIsAgent := lldpFlag(pf.Driver)
	value := "off"
	if *change.FWLLDPAgent == onIsAgent {
		value = "on"
	}
	if err := b.m.ApplyViaEthtool(ctx, pf.Interface, nic.FlagChange{Name: flag, Value: value}); err != nil {
		return false, fmt.Errorf("set %s %s: %w", flag, value, err)
	}
	return false, nil
}
