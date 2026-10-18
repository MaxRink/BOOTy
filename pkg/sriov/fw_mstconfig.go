package sriov

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
)

// Mstconfig is the firmware backend of NVIDIA ConnectX and BlueField NICs.
// It reads and writes the NV configuration with mstconfig from mstflint
// (open source, in the default ramdisk) or with mlxconfig from MFT
// (proprietary, a vendor flavor); both take the same arguments.
type Mstconfig struct {
	binary string
	tool   string
	cmd    nic.Commander
	// lockdownPath is the kernel lockdown file; under Secure Boot the
	// kernel forbids the PCI config writes of mstconfig.
	lockdownPath string
}

// lockdownFile is the lockdown file of new backends; tests point it at a
// fixture.
var lockdownFile = DefaultLockdownPath

// NewMstconfig returns the mstflint backend (NIC_TOOLS=opensource).
func NewMstconfig(cmd nic.Commander) *Mstconfig {
	return &Mstconfig{binary: "mstconfig", tool: ToolsOpenSource, cmd: cmd, lockdownPath: lockdownFile}
}

// NewMlxconfig returns the MFT backend (NIC_TOOLS=vendor).
func NewMlxconfig(cmd nic.Commander) *Mstconfig {
	return &Mstconfig{binary: "mlxconfig", tool: ToolsVendor, cmd: cmd, lockdownPath: lockdownFile}
}

// Name implements FirmwareBackend.
func (m *Mstconfig) Name() string { return m.binary }

// Tool implements FirmwareBackend.
func (m *Mstconfig) Tool() string { return m.tool }

// Supports implements FirmwareBackend: PCI functions of NVIDIA (Mellanox)
// adapters with the mlx5 or mlx4 driver.
func (m *Mstconfig) Supports(pf *PF) bool {
	return pf.Bus == "pci" && pf.VendorID == "15b3" && (pf.Driver == "mlx5_core" || pf.Driver == "mlx4_core")
}

// nvParam is one line of "mstconfig -e query": the default, the current
// (active) and the next-boot (stored) value. Without -e only next is set.
type nvParam struct {
	name, def, current, next string
}

var (
	nvLineName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*(\[[0-9.]+\])?$`)
	nvValueRe  = regexp.MustCompile(`^(.*)\((\d+)\)$`)
	mlxPortRe  = regexp.MustCompile(`^p(\d+)$`)
)

// parseMstconfigQuery parses the "Configurations:" table of mstconfig or
// mlxconfig query output, with or without -e.
func parseMstconfigQuery(out string) []nvParam {
	var params []nvParam
	inTable := false
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "Configurations:" {
			inTable = true
			continue
		}
		if !inTable || len(fields) == 0 {
			continue
		}
		if fields[0] == "*" {
			fields = fields[1:]
		}
		if len(fields) == 0 || !nvLineName.MatchString(fields[0]) {
			continue
		}
		switch len(fields) {
		case 2:
			params = append(params, nvParam{name: fields[0], next: fields[1]})
		case 4:
			params = append(params, nvParam{name: fields[0], def: fields[1], current: fields[2], next: fields[3]})
		}
	}
	return params
}

func findNV(params []nvParam, name string) *nvParam {
	for i := range params {
		if params[i].name == name {
			return &params[i]
		}
	}
	return nil
}

// nvValue splits a value such as True(1) or ETH(2) into the label and the
// number; a plain number is both.
func nvValue(raw string) (label, num string) {
	if m := nvValueRe.FindStringSubmatch(raw); m != nil {
		return m[1], m[2]
	}
	return raw, raw
}

// nvMatches reports whether a wanted value (label or number) names raw.
func nvMatches(want, raw string) bool {
	label, num := nvValue(raw)
	return strings.EqualFold(want, label) || want == num
}

func nvBool(raw string) string {
	if raw == "" {
		return ""
	}
	label, num := nvValue(raw)
	return strconv.FormatBool(num == "1" || strings.EqualFold(label, "true"))
}

func nvLinkType(raw string) string {
	if raw == "" {
		return ""
	}
	switch _, num := nvValue(raw); num {
	case "1":
		return LinkTypeInfiniband
	case "2":
		return LinkTypeEthernet
	case "3":
		return "vpi"
	}
	return raw
}

// portNumber is the 1-based port of the PF on the adapter: from
// phys_port_name (p0, p1), else from the PCI function.
func portNumber(pf *PF) int {
	if m := mlxPortRe.FindStringSubmatch(pf.PhysPortName); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n + 1
	}
	if i := strings.LastIndexByte(pf.PCIAddress, '.'); i >= 0 {
		if n, err := strconv.Atoi(pf.PCIAddress[i+1:]); err == nil {
			return n + 1
		}
	}
	return 1
}

// Read implements FirmwareBackend.
func (m *Mstconfig) Read(ctx context.Context, pf *PF, want *FirmwareSettings) (*FirmwareRead, error) {
	params, locked, err := m.query(ctx, pf, want)
	if locked != nil || err != nil {
		return locked, err
	}
	r := &FirmwareRead{}
	nv := func(setting, name string) *nvParam {
		p := findNV(params, name)
		if p == nil {
			r.Unsupported = append(r.Unsupported, Unsupported{setting, "the firmware has no NV parameter " + name})
		}
		return p
	}
	if want.SRIOVEnabled != nil {
		if p := nv(SettingSRIOVEnabled, "SRIOV_EN"); p != nil {
			r.Values.SRIOVEnabled, r.Active.SRIOVEnabled = boolPtr(nvBool(p.next)), boolPtr(nvBool(p.current))
		}
	}
	if want.MaxVFs != nil {
		if p := nv(SettingMaxVFs, "NUM_OF_VFS"); p != nil {
			r.Values.MaxVFs, r.Active.MaxVFs = intPtr(p.next), intPtr(p.current)
		}
	}
	if want.LinkType != "" {
		if p := nv(SettingLinkType, fmt.Sprintf("LINK_TYPE_P%d", portNumber(pf))); p != nil {
			r.Values.LinkType, r.Active.LinkType = nvLinkType(p.next), nvLinkType(p.current)
		}
	}
	if want.FWLLDPAgent != nil {
		r.Unsupported = append(r.Unsupported, Unsupported{SettingFWLLDPAgent, "an Intel setting"})
	}
	for _, s := range want.NVConfig {
		if p := nv(settingNVPrefix+s.Name, s.Name); p != nil {
			r.Values.NVConfig = append(r.Values.NVConfig, Setting{s.Name, nvSpell(s.Value, p.next)})
			if p.current != "" {
				r.Active.NVConfig = append(r.Active.NVConfig, Setting{s.Name, nvSpell(s.Value, p.current)})
			}
		}
	}
	for _, s := range want.DevlinkParams {
		r.Unsupported = append(r.Unsupported, Unsupported{settingParamPrefix + s.Name, "a devlink parameter, not NV config"})
	}
	return r, nil
}

// nvSpell returns the wanted spelling when it names raw, so the drift
// check compares equal values as equal; otherwise raw.
func nvSpell(want, raw string) string {
	if nvMatches(want, raw) {
		return want
	}
	return raw
}

func boolPtr(s string) *bool {
	if s == "" {
		return nil
	}
	b := s == "true"
	return &b
}

func intPtr(s string) *int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &n
}

// Apply implements FirmwareBackend. All changes go in one mstconfig set;
// they take effect after a firmware reset, so the host reboots.
func (m *Mstconfig) Apply(ctx context.Context, pf *PF, change *FirmwareSettings) (bool, error) {
	args := []string{"-y", "-d", pf.PCIAddress, "set"}
	if change.SRIOVEnabled != nil {
		args = append(args, "SRIOV_EN="+map[bool]string{true: "True", false: "False"}[*change.SRIOVEnabled])
	}
	if change.MaxVFs != nil {
		args = append(args, fmt.Sprintf("NUM_OF_VFS=%d", *change.MaxVFs))
	}
	if change.LinkType != "" {
		args = append(args, fmt.Sprintf("LINK_TYPE_P%d=%s", portNumber(pf), strings.ToUpper(change.LinkType)))
	}
	for _, s := range change.NVConfig {
		args = append(args, s.Name+"="+s.Value)
	}
	if len(args) == 4 {
		return false, nil
	}
	if mode := kernelLockdown(m.lockdownPath); mode != "" {
		return false, fmt.Errorf("kernel lockdown %s: %w", mode, ErrFirmwareLockdown)
	}
	if out, err := m.cmd.CombinedOutput(ctx, m.binary, args...); err != nil {
		if permissionDenied(out, err) {
			return false, fmt.Errorf("%s set: %s: %w", m.binary, lastLine(out), ErrFirmwareLockdown)
		}
		return false, fmt.Errorf("%s %s: %w: %s", m.binary, strings.Join(args, " "), err, lastLine(out))
	}
	return true, nil
}

// query runs "mstconfig -e query". Under kernel lockdown, or when the
// kernel refuses the access, it returns the coded lockdown read instead of
// a raw EPERM, so the findings say what to do.
func (m *Mstconfig) query(ctx context.Context, pf *PF, want *FirmwareSettings) ([]nvParam, *FirmwareRead, error) {
	out, err := m.cmd.CombinedOutput(ctx, m.binary, "-e", "-d", pf.PCIAddress, "query")
	if err != nil {
		if mode := kernelLockdown(m.lockdownPath); mode != "" || permissionDenied(out, err) {
			return nil, lockedRead(want, mode), nil
		}
		return nil, nil, fmt.Errorf("%s -e -d %s query: %w: %s", m.binary, pf.PCIAddress, err, lastLine(out))
	}
	params := parseMstconfigQuery(string(out))
	if len(params) == 0 {
		return nil, nil, fmt.Errorf("%s query of %s has no configuration table: %s", m.binary, pf.PCIAddress, lastLine(out))
	}
	return params, nil, nil
}

// lockedRead reports every NV setting of want as unsupported with the
// lockdown reason.
func lockedRead(want *FirmwareSettings, mode string) *FirmwareRead {
	reason := ErrFirmwareLockdown.Error()
	if mode != "" {
		reason = "kernel lockdown " + mode + ": " + reason
	}
	r := &FirmwareRead{}
	for _, name := range settingNames(want) {
		if name == SettingFWLLDPAgent || strings.HasPrefix(name, settingParamPrefix) {
			continue
		}
		r.Unsupported = append(r.Unsupported, Unsupported{name, reason})
	}
	return r
}

// lastLine returns the last non-empty output line, where mstconfig puts
// its error.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
