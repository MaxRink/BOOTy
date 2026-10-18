package sriov

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// FirmwareBackend reads and writes the persistent settings of one NIC
// family. A PF port can have several backends; each setting belongs to the
// first one that reads a value for it.
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
	// Apply stores the settings that change sets. reboot is true when they
	// take effect only after a firmware reset or a reboot.
	Apply(ctx context.Context, pf *PF, change *FirmwareSettings) (reboot bool, err error)
}

// FirmwareRead is what a backend read from a NIC.
type FirmwareRead struct {
	// Values holds the stored values (next boot); a setting the backend
	// cannot read stays unset.
	Values FirmwareSettings
	// Active holds the values the firmware runs with now, when the backend
	// can tell them apart from the stored ones (mstconfig -e).
	Active FirmwareSettings
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

// backendsFor returns the backends that support the PF port and that the
// tool class allows, and why the others were left out.
func backendsFor(backends []FirmwareBackend, pf *PF, tools string) (use []FirmwareBackend, excluded []string) {
	for _, b := range backends {
		if !b.Supports(pf) {
			continue
		}
		if toolRank(b.Tool()) > toolRank(tools) {
			excluded = append(excluded, fmt.Sprintf("backend %s needs NIC_TOOLS=%s (now %s)", b.Name(), b.Tool(), tools))
			continue
		}
		use = append(use, b)
	}
	return use, excluded
}

// firmwareView merges the reads of the backends of one PF port.
type firmwareView struct {
	stored map[string]string
	active map[string]string
	owner  map[string]FirmwareBackend
	notes  map[string]string // why no backend has a value
}

// readFirmware reads every setting of want from the backends of the PF
// port. The first backend with a value owns the setting.
func readFirmware(ctx context.Context, backends []FirmwareBackend, pf *PF, want *FirmwareSettings, tools string) *firmwareView {
	v := &firmwareView{stored: map[string]string{}, active: map[string]string{},
		owner: map[string]FirmwareBackend{}, notes: map[string]string{}}
	use, excluded := backendsFor(backends, pf, tools)
	type result struct {
		b    FirmwareBackend
		read *FirmwareRead
		err  error
	}
	results := make([]result, 0, len(use))
	for _, b := range use {
		read, err := b.Read(ctx, pf, want)
		results = append(results, result{b, read, err})
	}
	for _, name := range settingNames(want) {
		var why []string
		for _, res := range results {
			if res.err != nil {
				why = append(why, fmt.Sprintf("%s read: %v", res.b.Name(), res.err))
				continue
			}
			if reason, ok := res.read.unsupported(name); ok {
				why = append(why, res.b.Name()+": "+reason)
				continue
			}
			if val := getSetting(&res.read.Values, name); val != "" {
				v.stored[name], v.owner[name] = val, res.b
				v.active[name] = getSetting(&res.read.Active, name)
				break
			}
			why = append(why, res.b.Name()+": no value")
		}
		if _, ok := v.owner[name]; !ok {
			why = append(why, excluded...)
			if len(why) == 0 {
				why = append(why, noBackendReason(pf.Driver, name))
			}
			v.notes[name] = strings.Join(why, "; ")
		}
	}
	return v
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
// the stored value from the backend and the active value from the kernel
// or the backend.
type firmwareItem struct {
	setting string
	want    string
	stored  string // "" when unknown
	active  string // "" when unknown
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
func firmwareItems(want *FirmwareSettings, pf *PF, v *firmwareView) []firmwareItem {
	names := settingNames(want)
	items := make([]firmwareItem, 0, len(names))
	for _, name := range names {
		it := firmwareItem{setting: name, want: getSetting(want, name), stored: v.stored[name], active: kernelActive(pf, name)}
		if it.active == "" {
			it.active = v.active[name]
		}
		items = append(items, it)
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
