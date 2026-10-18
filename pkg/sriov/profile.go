package sriov

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Drift actions (SRIOV_ON_DRIFT, profile onDrift).
const (
	// OnDriftWarn reports drift and goes on.
	OnDriftWarn = "warn"
	// OnDriftFail stops provisioning on drift.
	OnDriftFail = "fail"
	// OnDriftApply changes the settings; a firmware change reboots the
	// host instead of kexec.
	OnDriftApply = "apply"
)

// Tool classes that BOOTy may use for firmware settings (NIC_TOOLS).
const (
	// ToolsKernel allows only sysfs, devlink and rtnetlink.
	ToolsKernel = "kernel"
	// ToolsOpenSource also allows the open-source tools of the default
	// ramdisk (mstconfig from mstflint, ethtool).
	ToolsOpenSource = "opensource"
	// ToolsVendor also allows proprietary vendor tools of a site flavor.
	ToolsVendor = "vendor"
)

// Handoff modes of the runtime settings.
const (
	// HandoffReset sets sriov_numvfs to 0 and the eSwitch to legacy before
	// the handoff, so the OS starts like after a cold boot.
	HandoffReset = "reset"
	// HandoffKeep leaves the runtime settings, for modes in which the
	// ramdisk keeps running (standby, agent).
	HandoffKeep = "keep"
)

// Limits of the settings.
const (
	maxVFCount = 65535 // PCI SR-IOV TotalVFs is 16 bits
	maxVLAN    = 4095
	maxQoS     = 7
)

// Config is the SR-IOV part of the machine config (provision.sriov).
type Config struct {
	// Profiles are the desired SR-IOV states (SRIOV_PROFILES).
	Profiles []Profile `yaml:"profiles,omitempty" json:"profiles,omitempty"`
	// OnDrift is the default drift action of the profiles
	// (SRIOV_ON_DRIFT): warn (default), fail or apply.
	OnDrift string `yaml:"onDrift,omitempty" json:"onDrift,omitempty"`
	// NICTools are the tool classes BOOTy may use (NIC_TOOLS): kernel,
	// opensource (default) or vendor.
	NICTools string `yaml:"nicTools,omitempty" json:"nicTools,omitempty"`
	// VerifyReboot is the verify reboot after stored firmware changes that
	// the runtime settings depend on (SRIOV_VERIFY_REBOOT): on (default)
	// boots the ramdisk again to check them and needs REBOOT_URL; off goes
	// on to the handoff without that check.
	VerifyReboot string `yaml:"verifyReboot,omitempty" json:"verifyReboot,omitempty"`
	// UplinkVFDrivers are the drivers that may change the VF count of a
	// port the ramdisk network uses (SRIOV_UPLINK_VF_DRIVERS); empty uses
	// DefaultUplinkVFDrivers (mlx5_core, ice).
	UplinkVFDrivers []string `yaml:"uplinkVFDrivers,omitempty" json:"uplinkVFDrivers,omitempty"`
}

// Profile is the desired SR-IOV state of the PF ports it matches. A host
// class gets a list of profiles, one per NIC kind.
type Profile struct {
	// Name identifies the profile in reports and logs.
	Name string `yaml:"name" json:"name"`
	// Match selects the PF ports; every set field must match.
	Match Match `yaml:"match" json:"match"`
	// Firmware are the persistent settings in the NIC firmware; a change
	// takes effect after a firmware reset or reboot.
	Firmware FirmwareSettings `yaml:"firmware,omitempty" json:"firmware,omitempty"`
	// Runtime are the kernel settings, which last until the next reboot.
	Runtime RuntimeSettings `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	// OnDrift overrides Config.OnDrift for this profile.
	OnDrift string `yaml:"onDrift,omitempty" json:"onDrift,omitempty"`
	// Optional lets the profile match no PF port; otherwise that is drift.
	Optional bool `yaml:"optional,omitempty" json:"optional,omitempty"`
}

// Match selects PF ports. Every set field must match.
type Match struct {
	// VendorID and DeviceID are PCI IDs as four hex digits, for example
	// 15b3 and 101d.
	VendorID string `yaml:"vendorId,omitempty" json:"vendorId,omitempty"`
	DeviceID string `yaml:"deviceId,omitempty" json:"deviceId,omitempty"`
	// Driver is the kernel driver, for example mlx5_core.
	Driver string `yaml:"driver,omitempty" json:"driver,omitempty"`
	// MAC is the MAC address of the PF port.
	MAC string `yaml:"mac,omitempty" json:"mac,omitempty"`
	// Interface is the netdev name or a shell pattern such as ens1f*.
	Interface string `yaml:"interface,omitempty" json:"interface,omitempty"`
}

// FirmwareSettings are persistent NIC settings. Which ones a NIC supports
// depends on the firmware backend of its driver (docs/sriov.md).
type FirmwareSettings struct {
	// SRIOVEnabled is the SR-IOV capability in the NIC firmware.
	SRIOVEnabled *bool `yaml:"sriovEnabled,omitempty" json:"sriovEnabled,omitempty"`
	// MaxVFs is the VF limit per PF, sriov_totalvfs after the reset.
	MaxVFs *int `yaml:"maxVFs,omitempty" json:"maxVFs,omitempty"`
	// LinkType is eth or ib for the port (VPI adapters).
	LinkType string `yaml:"linkType,omitempty" json:"linkType,omitempty"`
	// FWLLDPAgent is the firmware LLDP and DCBX agent of Intel NICs; false
	// lets the host run LLDP and DCBX.
	FWLLDPAgent *bool `yaml:"fwLLDPAgent,omitempty" json:"fwLLDPAgent,omitempty"`
	// NVConfig are more NVIDIA/Mellanox NV parameters (mstconfig names,
	// for example CQE_COMPRESSION). SRIOV_EN, NUM_OF_VFS and LINK_TYPE_P*
	// are set through the fields above.
	NVConfig []Setting `yaml:"nvConfig,omitempty" json:"nvConfig,omitempty"`
	// DevlinkParams are devlink parameters in the permanent configuration
	// mode (stored in the NIC), for example enable_sriov on bnxt_en.
	DevlinkParams []Setting `yaml:"devlinkParams,omitempty" json:"devlinkParams,omitempty"`
}

// Setting is one named firmware parameter.
type Setting struct {
	Name  string `yaml:"name" json:"name"`
	Value string `yaml:"value" json:"value"`
}

// RuntimeSettings are kernel settings. They do not survive a kexec or a
// reboot; BOOTy applies them to verify that the PF can run them.
type RuntimeSettings struct {
	// NumVFs is sriov_numvfs.
	NumVFs *int `yaml:"numVFs,omitempty" json:"numVFs,omitempty"`
	// EswitchMode is legacy or switchdev.
	EswitchMode string `yaml:"eswitchMode,omitempty" json:"eswitchMode,omitempty"`
	// VFDefaults apply to every VF.
	VFDefaults VFSettings `yaml:"vfDefaults,omitempty" json:"vfDefaults,omitempty"`
	// VFs override VFDefaults per VF index.
	VFs []VFOverride `yaml:"vfs,omitempty" json:"vfs,omitempty"`
	// Handoff is reset (default) or keep.
	Handoff string `yaml:"handoff,omitempty" json:"handoff,omitempty"`
}

// VFSettings are the rtnetlink VF attributes. Unset fields stay as the
// driver sets them.
type VFSettings struct {
	Trust    *bool `yaml:"trust,omitempty" json:"trust,omitempty"`
	SpoofChk *bool `yaml:"spoofChk,omitempty" json:"spoofChk,omitempty"`
	// VLAN 0 removes the VLAN; QoS needs a VLAN.
	VLAN *int `yaml:"vlan,omitempty" json:"vlan,omitempty"`
	QoS  *int `yaml:"qos,omitempty" json:"qos,omitempty"`
	// MinTxRate and MaxTxRate are in Mbit/s; 0 is unlimited.
	MinTxRate *int `yaml:"minTxRate,omitempty" json:"minTxRate,omitempty"`
	MaxTxRate *int `yaml:"maxTxRate,omitempty" json:"maxTxRate,omitempty"`
	// LinkState is auto, enable or disable.
	LinkState string `yaml:"linkState,omitempty" json:"linkState,omitempty"`
}

// VFOverride sets the attributes of one VF.
type VFOverride struct {
	Index int `yaml:"index" json:"index"`
	// MAC is the VF MAC address; only a single VF can have it.
	MAC        string `yaml:"mac,omitempty" json:"mac,omitempty"`
	VFSettings `yaml:",inline"`
}

var (
	profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	pciIDRe     = regexp.MustCompile(`^[0-9a-f]{4}$`)
	driverRe    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	nvNameRe    = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	paramNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	valueRe     = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)
	// nvNamed are the NV parameters that have their own field.
	nvNamed = regexp.MustCompile(`^(SRIOV_EN|NUM_OF_VFS|LINK_TYPE_P\d+)$`)
)

// Validate checks the SR-IOV config strictly: an unknown value, a value out
// of range or a contradiction fails, so a typo never applies a default.
func (c *Config) Validate() error {
	var errs []string
	if err := oneOf("provision.sriov.onDrift (SRIOV_ON_DRIFT)", c.OnDrift, OnDriftWarn, OnDriftFail, OnDriftApply); err != "" {
		errs = append(errs, err)
	}
	if err := oneOf("provision.sriov.nicTools (NIC_TOOLS)", c.NICTools, ToolsKernel, ToolsOpenSource, ToolsVendor); err != "" {
		errs = append(errs, err)
	}
	if err := oneOf("provision.sriov.verifyReboot (SRIOV_VERIFY_REBOOT)", c.VerifyReboot, VerifyRebootOn, VerifyRebootOff); err != "" {
		errs = append(errs, err)
	}
	for _, d := range c.UplinkVFDrivers {
		if !driverRe.MatchString(d) {
			errs = append(errs, fmt.Sprintf("provision.sriov.uplinkVFDrivers (SRIOV_UPLINK_VF_DRIVERS) %q is not a driver name", d))
		}
	}
	seen := map[string]bool{}
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if seen[p.Name] {
			errs = append(errs, fmt.Sprintf("SR-IOV profile %q is defined twice", p.Name))
		}
		seen[p.Name] = true
		for _, e := range p.validate() {
			errs = append(errs, fmt.Sprintf("SR-IOV profile %q: %s", p.Name, e))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// DriftAction returns the drift action of a profile.
func (c *Config) DriftAction(p *Profile) string {
	if p.OnDrift != "" {
		return p.OnDrift
	}
	if c.OnDrift != "" {
		return c.OnDrift
	}
	return OnDriftWarn
}

// UplinkVFDriverList returns the drivers that may change the VF count of
// a port in use.
func (c *Config) UplinkVFDriverList() []string {
	if len(c.UplinkVFDrivers) > 0 {
		return c.UplinkVFDrivers
	}
	return DefaultUplinkVFDrivers
}

// VerifyRebootEnabled reports whether the verify reboot is on (the
// default).
func (c *Config) VerifyRebootEnabled() bool {
	return c.VerifyReboot != VerifyRebootOff
}

// Values of Config.VerifyReboot.
const (
	VerifyRebootOn  = "on"
	VerifyRebootOff = "off"
)

// Tools returns the allowed tool class.
func (c *Config) Tools() string {
	if c.NICTools == "" {
		return ToolsOpenSource
	}
	return c.NICTools
}

func (p *Profile) validate() []string {
	var errs []string
	if !profileName.MatchString(p.Name) {
		errs = append(errs, "name must be 1 to 63 letters, digits, '.', '_' or '-'")
	}
	if e := oneOf("onDrift", p.OnDrift, OnDriftWarn, OnDriftFail, OnDriftApply); e != "" {
		errs = append(errs, e)
	}
	errs = append(errs, p.Match.validate()...)
	errs = append(errs, p.Firmware.validate()...)
	errs = append(errs, p.Runtime.validate()...)
	if p.Firmware.MaxVFs != nil && p.Runtime.NumVFs != nil && *p.Runtime.NumVFs > *p.Firmware.MaxVFs {
		errs = append(errs, fmt.Sprintf("runtime.numVFs %d is above firmware.maxVFs %d", *p.Runtime.NumVFs, *p.Firmware.MaxVFs))
	}
	if p.Firmware.SRIOVEnabled != nil && !*p.Firmware.SRIOVEnabled && p.Runtime.NumVFs != nil && *p.Runtime.NumVFs > 0 {
		errs = append(errs, "runtime.numVFs needs firmware.sriovEnabled")
	}
	return errs
}

func (m *Match) validate() []string {
	var errs []string
	if *m == (Match{}) {
		return []string{"match needs at least one of vendorId, deviceId, driver, mac, interface"}
	}
	if m.VendorID != "" && !pciIDRe.MatchString(m.VendorID) {
		errs = append(errs, fmt.Sprintf("match.vendorId %q must be four lower-case hex digits", m.VendorID))
	}
	if m.DeviceID != "" && !pciIDRe.MatchString(m.DeviceID) {
		errs = append(errs, fmt.Sprintf("match.deviceId %q must be four lower-case hex digits", m.DeviceID))
	}
	if m.Driver != "" && !driverRe.MatchString(m.Driver) {
		errs = append(errs, fmt.Sprintf("match.driver %q is not a driver name", m.Driver))
	}
	if m.MAC != "" {
		if hw, err := net.ParseMAC(m.MAC); err != nil || len(hw) != 6 {
			errs = append(errs, fmt.Sprintf("match.mac %q is not a 48-bit MAC", m.MAC))
		}
	}
	if m.Interface != "" {
		if _, err := path.Match(m.Interface, ""); err != nil || strings.Contains(m.Interface, "/") {
			errs = append(errs, fmt.Sprintf("match.interface %q is not a name or pattern", m.Interface))
		}
	}
	return errs
}

func (f *FirmwareSettings) validate() []string {
	var errs []string
	if f.MaxVFs != nil && (*f.MaxVFs < 0 || *f.MaxVFs > maxVFCount) {
		errs = append(errs, fmt.Sprintf("firmware.maxVFs %d is out of 0 to %d", *f.MaxVFs, maxVFCount))
	}
	if f.SRIOVEnabled != nil && !*f.SRIOVEnabled && f.MaxVFs != nil && *f.MaxVFs > 0 {
		errs = append(errs, "firmware.maxVFs > 0 needs firmware.sriovEnabled")
	}
	if f.SRIOVEnabled != nil && *f.SRIOVEnabled && f.MaxVFs != nil && *f.MaxVFs == 0 {
		errs = append(errs, "firmware.sriovEnabled needs firmware.maxVFs > 0")
	}
	if e := oneOf("firmware.linkType", f.LinkType, LinkTypeEthernet, LinkTypeInfiniband); e != "" {
		errs = append(errs, e)
	}
	errs = append(errs, validateSettings("firmware.nvConfig", f.NVConfig, nvNameRe)...)
	for _, s := range f.NVConfig {
		if nvNamed.MatchString(s.Name) {
			errs = append(errs, fmt.Sprintf("firmware.nvConfig %s: use sriovEnabled, maxVFs or linkType", s.Name))
		}
	}
	errs = append(errs, validateSettings("firmware.devlinkParams", f.DevlinkParams, paramNameRe)...)
	return errs
}

func validateSettings(field string, list []Setting, name *regexp.Regexp) []string {
	var errs []string
	seen := map[string]bool{}
	for _, s := range list {
		if !name.MatchString(s.Name) {
			errs = append(errs, fmt.Sprintf("%s name %q is invalid", field, s.Name))
		}
		if !valueRe.MatchString(s.Value) {
			errs = append(errs, fmt.Sprintf("%s %s value %q is invalid", field, s.Name, s.Value))
		}
		if seen[s.Name] {
			errs = append(errs, fmt.Sprintf("%s %s is set twice", field, s.Name))
		}
		seen[s.Name] = true
	}
	return errs
}

func (r *RuntimeSettings) validate() []string {
	var errs []string
	if e := intRange("runtime.numVFs", r.NumVFs, 0, maxVFCount); e != "" {
		errs = append(errs, e)
	}
	if e := oneOf("runtime.eswitchMode", r.EswitchMode, EswitchLegacy, EswitchSwitchdev); e != "" {
		errs = append(errs, e)
	}
	if e := oneOf("runtime.handoff", r.Handoff, HandoffReset, HandoffKeep); e != "" {
		errs = append(errs, e)
	}
	errs = append(errs, r.VFDefaults.validate("runtime.vfDefaults")...)
	vfSettings := r.VFDefaults != (VFSettings{}) || len(r.VFs) > 0
	if vfSettings && (r.NumVFs == nil || *r.NumVFs == 0) {
		errs = append(errs, "VF settings need runtime.numVFs > 0")
	}
	return append(errs, r.validateVFs()...)
}

func (r *RuntimeSettings) validateVFs() []string {
	var errs []string
	seen := map[int]bool{}
	for _, vf := range r.VFs {
		field := fmt.Sprintf("runtime.vfs[%d]", vf.Index)
		if vf.Index < 0 || (r.NumVFs != nil && vf.Index >= *r.NumVFs) {
			errs = append(errs, field+" index is out of the VF range")
		}
		if seen[vf.Index] {
			errs = append(errs, field+" is set twice")
		}
		seen[vf.Index] = true
		if vf.MAC != "" {
			if hw, err := net.ParseMAC(vf.MAC); err != nil || len(hw) != 6 || hw[0]&1 == 1 {
				errs = append(errs, fmt.Sprintf("%s.mac %q is not a unicast 48-bit MAC", field, vf.MAC))
			}
		}
		errs = append(errs, vf.validate(field)...)
	}
	return errs
}

func (v *VFSettings) validate(field string) []string {
	var errs []string
	for _, e := range []string{
		intRange(field+".vlan", v.VLAN, 0, maxVLAN),
		intRange(field+".qos", v.QoS, 0, maxQoS),
		intRange(field+".minTxRate", v.MinTxRate, 0, math.MaxInt32),
		intRange(field+".maxTxRate", v.MaxTxRate, 0, math.MaxInt32),
		oneOf(field+".linkState", v.LinkState, "auto", "enable", "disable"),
	} {
		if e != "" {
			errs = append(errs, e)
		}
	}
	if v.QoS != nil && *v.QoS > 0 && (v.VLAN == nil || *v.VLAN == 0) {
		errs = append(errs, field+".qos needs a vlan")
	}
	if v.MinTxRate != nil && v.MaxTxRate != nil && *v.MaxTxRate > 0 && *v.MinTxRate > *v.MaxTxRate {
		errs = append(errs, field+".minTxRate is above maxTxRate")
	}
	return errs
}

// intRange returns an error text when v is set and out of lo to hi.
func intRange(field string, v *int, lo, hi int) string {
	if v == nil || (*v >= lo && *v <= hi) {
		return ""
	}
	return fmt.Sprintf("%s %d is out of %d to %d", field, *v, lo, hi)
}

// oneOf returns an error text when value is set and not one of allowed.
func oneOf(field, value string, allowed ...string) string {
	if value == "" {
		return ""
	}
	for _, a := range allowed {
		if value == a {
			return ""
		}
	}
	return fmt.Sprintf("%s %q is not one of %s", field, value, strings.Join(allowed, ", "))
}

// Matches reports whether a PF port matches m.
func (m *Match) Matches(pf *PF) bool {
	if m.VendorID != "" && m.VendorID != pf.VendorID {
		return false
	}
	if m.DeviceID != "" && m.DeviceID != pf.DeviceID {
		return false
	}
	if m.Driver != "" && m.Driver != pf.Driver {
		return false
	}
	if m.MAC != "" && !strings.EqualFold(normalizeMAC(m.MAC), pf.MAC) {
		return false
	}
	if m.Interface != "" {
		if ok, err := path.Match(m.Interface, pf.Interface); err != nil || !ok {
			return false
		}
	}
	return true
}

func normalizeMAC(s string) string {
	if hw, err := net.ParseMAC(s); err == nil {
		return hw.String()
	}
	return s
}

// OwnsMellanoxVFs reports whether a profile sets the SR-IOV enable or the
// VF limit and can match a ConnectX port: then the profiles own
// NUM_OF_VFS and setup-mellanox (NUM_VFS) must not write it too, or the two
// would change it back and forth on every provision.
func (c *Config) OwnsMellanoxVFs() bool {
	for i := range c.Profiles {
		p := &c.Profiles[i]
		if p.Firmware.SRIOVEnabled == nil && p.Firmware.MaxVFs == nil {
			continue
		}
		vendorOK := p.Match.VendorID == "" || p.Match.VendorID == "15b3"
		driverOK := p.Match.Driver == "" || p.Match.Driver == "mlx5_core" || p.Match.Driver == "mlx4_core"
		if vendorOK && driverOK {
			return true
		}
	}
	return false
}

// configFields is Config without its methods, for the strict decoders.
type configFields Config

// UnmarshalYAML decodes provision.sriov strictly: an unknown key fails
// even when the machine config is not loaded strictly, so a typo never
// drops a firmware setting.
func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	b, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("provision.sriov: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var f configFields
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("provision.sriov: %w", err)
	}
	*c = Config(f)
	return nil
}

// UnmarshalJSON decodes provision.sriov strictly, like UnmarshalYAML.
func (c *Config) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f configFields
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("provision.sriov: %w", err)
	}
	*c = Config(f)
	return nil
}
