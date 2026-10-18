//go:build linux

package sriov

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

// paramEnableSRIOV is the generic devlink parameter for SR-IOV in the NIC
// (bnxt_en stores it in the NVM).
const paramEnableSRIOV = "enable_sriov"

// DevlinkParams is the firmware backend of the devlink parameters that the
// NIC stores (configuration mode permanent): the generic enable_sriov of
// bnxt_en and any driver parameter of firmware.devlinkParams.
type DevlinkParams struct {
	get func(bus, device, name string) (*netlink.DevlinkParam, error)
	set func(bus, device, name string, cmode uint8, value any) error
}

// NewDevlinkParams returns the backend for the running kernel.
func NewDevlinkParams() *DevlinkParams {
	return &DevlinkParams{get: netlink.DevlinkGetDeviceParamByName, set: netlink.DevlinkSetDeviceParam}
}

// Name implements FirmwareBackend.
func (d *DevlinkParams) Name() string { return "devlink" }

// Tool implements FirmwareBackend.
func (d *DevlinkParams) Tool() string { return ToolsKernel }

// Supports implements FirmwareBackend: every port with a devlink instance.
func (d *DevlinkParams) Supports(pf *PF) bool { return pf.Devlink != nil }

// Read implements FirmwareBackend.
func (d *DevlinkParams) Read(_ context.Context, pf *PF, want *FirmwareSettings) (*FirmwareRead, error) {
	r := &FirmwareRead{}
	unsupported := func(setting, reason string) {
		r.Unsupported = append(r.Unsupported, Unsupported{setting, reason})
	}
	if want.SRIOVEnabled != nil {
		if v, reason := d.permanent(pf, paramEnableSRIOV); reason != "" {
			unsupported(SettingSRIOVEnabled, reason)
		} else {
			r.Values.SRIOVEnabled = boolPtr(v)
		}
	}
	if want.MaxVFs != nil {
		unsupported(SettingMaxVFs, "no generic devlink parameter for the VF limit")
	}
	if want.LinkType != "" {
		unsupported(SettingLinkType, "no generic devlink parameter for the link type")
	}
	if want.FWLLDPAgent != nil {
		unsupported(SettingFWLLDPAgent, "a private flag, not a devlink parameter")
	}
	for _, s := range want.NVConfig {
		unsupported(settingNVPrefix+s.Name, "NVIDIA NV config, not a devlink parameter")
	}
	for _, s := range want.DevlinkParams {
		if v, reason := d.permanent(pf, s.Name); reason != "" {
			unsupported(settingParamPrefix+s.Name, reason)
		} else {
			r.Values.DevlinkParams = append(r.Values.DevlinkParams, Setting{s.Name, paramSpell(s.Value, v)})
		}
	}
	return r, nil
}

// permanent returns the value of a parameter in the permanent mode, or why
// there is none.
func (d *DevlinkParams) permanent(pf *PF, name string) (value, reason string) {
	p, err := d.get(pf.Bus, pf.PCIAddress, name)
	if err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) || errors.Is(err, unix.EOPNOTSUPP) {
			return "", fmt.Sprintf("driver %s has no devlink parameter %s", pf.Driver, name)
		}
		return "", fmt.Sprintf("devlink param %s: %v", name, err)
	}
	var cmodes []string
	for _, v := range p.Values {
		if v.CMODE == nl.DEVLINK_PARAM_CMODE_PERMANENT {
			return paramString(v.Data), ""
		}
		cmodes = append(cmodes, cmodeName(v.CMODE))
	}
	return "", fmt.Sprintf("devlink parameter %s is not stored in the NIC (modes %v)", name, cmodes)
}

func cmodeName(c uint8) string {
	switch c {
	case nl.DEVLINK_PARAM_CMODE_RUNTIME:
		return "runtime"
	case nl.DEVLINK_PARAM_CMODE_DRIVERINIT:
		return "driverinit"
	}
	return "cmode-" + strconv.Itoa(int(c))
}

func paramString(data any) string {
	switch v := data.(type) {
	case bool:
		return strconv.FormatBool(v)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case string:
		return v
	}
	return fmt.Sprint(data)
}

// paramSpell returns the wanted spelling of an equal boolean (1, true), so
// the drift check compares equal values as equal.
func paramSpell(want, got string) string {
	w, errW := strconv.ParseBool(want)
	g, errG := strconv.ParseBool(got)
	if errW == nil && errG == nil && w == g {
		return want
	}
	return got
}

// Apply implements FirmwareBackend. Permanent parameters take effect after
// a reboot or a devlink reload.
func (d *DevlinkParams) Apply(_ context.Context, pf *PF, change *FirmwareSettings) (bool, error) {
	var params []Setting
	if change.SRIOVEnabled != nil {
		params = append(params, Setting{paramEnableSRIOV, strconv.FormatBool(*change.SRIOVEnabled)})
	}
	params = append(params, change.DevlinkParams...)
	for _, s := range params {
		p, err := d.get(pf.Bus, pf.PCIAddress, s.Name)
		if err != nil {
			return false, fmt.Errorf("devlink param %s: %w", s.Name, err)
		}
		value, err := paramValue(p.Type, s.Value)
		if err != nil {
			return false, fmt.Errorf("devlink param %s: %w", s.Name, err)
		}
		if err := d.set(pf.Bus, pf.PCIAddress, s.Name, nl.DEVLINK_PARAM_CMODE_PERMANENT, value); err != nil {
			return false, fmt.Errorf("set devlink param %s=%s: %w", s.Name, s.Value, err)
		}
	}
	return len(params) > 0, nil
}

// paramValue converts a value to the Go type that netlink sends for the
// parameter type.
func paramValue(typ uint8, s string) (any, error) {
	var bits int
	switch typ {
	case nl.DEVLINK_PARAM_TYPE_BOOL:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("value %q is not a boolean: %w", s, err)
		}
		return b, nil
	case nl.DEVLINK_PARAM_TYPE_STRING:
		return s, nil
	case nl.DEVLINK_PARAM_TYPE_U8:
		bits = 8
	case nl.DEVLINK_PARAM_TYPE_U16:
		bits = 16
	case nl.DEVLINK_PARAM_TYPE_U32:
		bits = 32
	default:
		return nil, fmt.Errorf("parameter type %d is not supported", typ)
	}
	n, err := strconv.ParseUint(s, 10, bits)
	if err != nil {
		return nil, fmt.Errorf("value %q is not a %d-bit number: %w", s, bits, err)
	}
	switch bits {
	case 8:
		return uint8(n), nil
	case 16:
		return uint16(n), nil
	}
	return uint32(n), nil
}
