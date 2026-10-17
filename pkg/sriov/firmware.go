package sriov

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// FirmwareBackend reads and writes the persistent settings of one NIC
// family. A stored setting takes effect after a firmware reset or a reboot.
type FirmwareBackend interface {
	// Name names the backend in reports, for example mstconfig.
	Name() string
	// Tool is the tool class the backend needs: kernel, opensource or
	// vendor (NIC_TOOLS).
	Tool() string
	// Supports reports whether the backend handles the PF port.
	Supports(pf *PF) bool
	// Read returns the stored values of the settings that want sets.
	Read(ctx context.Context, pf *PF, want *FirmwareSettings) (*FirmwareRead, error)
	// Apply stores the settings that change sets.
	Apply(ctx context.Context, pf *PF, change *FirmwareSettings) error
}

// FirmwareRead is what a backend read from a NIC.
type FirmwareRead struct {
	// Values holds the stored values; a setting the backend cannot read
	// stays unset.
	Values FirmwareSettings
	// Unsupported lists the settings the backend cannot read or write on
	// this NIC.
	Unsupported []Unsupported
}

// Unsupported is a setting that a backend cannot handle, and why.
type Unsupported struct {
	Setting string
	Reason  string
}

func (r *FirmwareRead) unsupported(setting string) (string, bool) {
	if r == nil {
		return "", false
	}
	for _, u := range r.Unsupported {
		if u.Setting == setting {
			return u.Reason, true
		}
	}
	return "", false
}

// toolRank orders the tool classes: a class allows the ones below it.
func toolRank(tool string) int {
	switch tool {
	case ToolsKernel:
		return 0
	case ToolsOpenSource:
		return 1
	case ToolsVendor:
		return 2
	}
	return 3
}

// backendFor returns the first backend that supports the PF port and that
// the allowed tool class permits. The reason names the backends that the
// tool class excluded.
func backendFor(backends []FirmwareBackend, pf *PF, tools string) (backend FirmwareBackend, reason string) {
	reason = fmt.Sprintf("no firmware backend for driver %q", pf.Driver)
	for _, b := range backends {
		if !b.Supports(pf) {
			continue
		}
		if toolRank(b.Tool()) > toolRank(tools) {
			reason = fmt.Sprintf("backend %s needs NIC_TOOLS=%s (now %s)", b.Name(), b.Tool(), tools)
			continue
		}
		return b, ""
	}
	return nil, reason
}

// Setting names in findings and Unsupported.
const (
	SettingSRIOVEnabled = "sriovEnabled"
	SettingMaxVFs       = "maxVFs"
	SettingLinkType     = "linkType"
	SettingFWLLDPAgent  = "fwLLDPAgent"
	settingNVPrefix     = "nvConfig."
	settingParamPrefix  = "devlinkParams."
)

// firmwareItem is one firmware setting of a profile: the wanted value,
// the stored value from the backend and the active value from the kernel.
type firmwareItem struct {
	setting string
	want    string
	stored  string // "" when unknown
	active  string // "" when the kernel does not show it
}

// settingNames lists the firmware settings that want sets, in a fixed
// order.
func settingNames(want *FirmwareSettings) []string {
	var names []string
	if want.SRIOVEnabled != nil {
		names = append(names, SettingSRIOVEnabled)
	}
	if want.MaxVFs != nil {
		names = append(names, SettingMaxVFs)
	}
	if want.LinkType != "" {
		names = append(names, SettingLinkType)
	}
	if want.FWLLDPAgent != nil {
		names = append(names, SettingFWLLDPAgent)
	}
	for _, s := range want.NVConfig {
		names = append(names, settingNVPrefix+s.Name)
	}
	for _, s := range want.DevlinkParams {
		names = append(names, settingParamPrefix+s.Name)
	}
	return names
}

// getSetting returns a setting as text, "" when it is unset.
func getSetting(f *FirmwareSettings, name string) string {
	switch {
	case name == SettingSRIOVEnabled:
		return boolString(f.SRIOVEnabled)
	case name == SettingMaxVFs:
		return intString(f.MaxVFs)
	case name == SettingLinkType:
		return f.LinkType
	case name == SettingFWLLDPAgent:
		return boolString(f.FWLLDPAgent)
	case strings.HasPrefix(name, settingNVPrefix):
		return settingValue(f.NVConfig, strings.TrimPrefix(name, settingNVPrefix))
	case strings.HasPrefix(name, settingParamPrefix):
		return settingValue(f.DevlinkParams, strings.TrimPrefix(name, settingParamPrefix))
	}
	return ""
}

// kernelActive returns the value the kernel shows for a setting, "" when
// the kernel does not show it.
func kernelActive(pf *PF, name string) string {
	switch name {
	case SettingSRIOVEnabled:
		return strconv.FormatBool(pf.SRIOVCapable)
	case SettingMaxVFs:
		return strconv.Itoa(pf.TotalVFs)
	case SettingLinkType:
		return pf.LinkType
	}
	return ""
}

// firmwareItems lists the firmware settings that want sets with their
// stored and active values.
func firmwareItems(want *FirmwareSettings, pf *PF, read *FirmwareRead) []firmwareItem {
	var got FirmwareSettings
	if read != nil {
		got = read.Values
	}
	names := settingNames(want)
	items := make([]firmwareItem, 0, len(names))
	for _, name := range names {
		items = append(items, firmwareItem{setting: name, want: getSetting(want, name),
			stored: getSetting(&got, name), active: kernelActive(pf, name)})
	}
	return items
}

func boolString(b *bool) string {
	if b == nil {
		return ""
	}
	return strconv.FormatBool(*b)
}

func intString(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

func settingValue(list []Setting, name string) string {
	for _, s := range list {
		if s.Name == name {
			return s.Value
		}
	}
	return ""
}

// changeFor builds the FirmwareSettings that set the drifted items.
func changeFor(want *FirmwareSettings, drifted []string) *FirmwareSettings {
	set := map[string]bool{}
	for _, s := range drifted {
		set[s] = true
	}
	c := &FirmwareSettings{}
	if set[SettingSRIOVEnabled] {
		c.SRIOVEnabled = want.SRIOVEnabled
	}
	if set[SettingMaxVFs] {
		c.MaxVFs = want.MaxVFs
		if want.SRIOVEnabled == nil && *want.MaxVFs > 0 {
			// A VF limit needs SR-IOV on, like mstconfig SRIOV_EN=1 NUM_OF_VFS=n.
			on := true
			c.SRIOVEnabled = &on
		}
	}
	if set[SettingLinkType] {
		c.LinkType = want.LinkType
	}
	if set[SettingFWLLDPAgent] {
		c.FWLLDPAgent = want.FWLLDPAgent
	}
	for _, s := range want.NVConfig {
		if set[settingNVPrefix+s.Name] {
			c.NVConfig = append(c.NVConfig, s)
		}
	}
	for _, s := range want.DevlinkParams {
		if set[settingParamPrefix+s.Name] {
			c.DevlinkParams = append(c.DevlinkParams, s)
		}
	}
	return c
}
