//go:build linux

package sriov

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/firmware/nic"
	"github.com/telekom/BOOTy/pkg/firmware/nic/ethtool"
	netethtool "github.com/telekom/BOOTy/pkg/network/ethtool"
)

// fakeParams holds devlink parameters like bnxt_en and ice report them.
type fakeParams struct {
	params    map[string]*netlink.DevlinkParam
	sets      []string
	setErr    error
	noPersist bool
}

func (f *fakeParams) backend() *DevlinkParams {
	return &DevlinkParams{
		get: func(_, _, name string) (*netlink.DevlinkParam, error) {
			if p, ok := f.params[name]; ok {
				return p, nil
			}
			return nil, fmt.Errorf("get: %w", unix.EINVAL)
		},
		set: func(bus, dev, name string, cmode uint8, value any) error {
			f.sets = append(f.sets, fmt.Sprintf("%s/%s %s cmode=%d %T=%v", bus, dev, name, cmode, value, value))
			if f.setErr != nil {
				return f.setErr
			}
			// The NIC stores the value, like the permanent mode does.
			if p, ok := f.params[name]; ok && !f.noPersist {
				for i := range p.Values {
					if p.Values[i].CMODE == cmode {
						p.Values[i].Data = value
					}
				}
			}
			return nil
		},
	}
}

func param(name string, typ uint8, values ...netlink.DevlinkParamValue) *netlink.DevlinkParam {
	return &netlink.DevlinkParam{Name: name, Type: typ, Values: values}
}

// bnxtParams are the devlink parameters of a BCM57508 port ("devlink dev
// param show pci/0000:41:00.0"): enable_sriov and msix_vec_per_pf_max in
// the permanent mode, enable_roce in driverinit.
func bnxtParams() *fakeParams {
	perm := func(v any) netlink.DevlinkParamValue {
		return netlink.DevlinkParamValue{Data: v, CMODE: nl.DEVLINK_PARAM_CMODE_PERMANENT}
	}
	return &fakeParams{params: map[string]*netlink.DevlinkParam{
		"enable_sriov":        param("enable_sriov", nl.DEVLINK_PARAM_TYPE_BOOL, perm(true)),
		"msix_vec_per_pf_max": param("msix_vec_per_pf_max", nl.DEVLINK_PARAM_TYPE_U32, perm(uint32(64))),
		"ignore_ari":          param("ignore_ari", nl.DEVLINK_PARAM_TYPE_BOOL, perm(false)),
		"enable_roce": param("enable_roce", nl.DEVLINK_PARAM_TYPE_BOOL,
			netlink.DevlinkParamValue{Data: true, CMODE: nl.DEVLINK_PARAM_CMODE_DRIVERINIT}),
		"tiny":  param("tiny", nl.DEVLINK_PARAM_TYPE_U8, perm(uint8(3))),
		"mid":   param("mid", nl.DEVLINK_PARAM_TYPE_U16, perm(uint16(300))),
		"label": param("label", nl.DEVLINK_PARAM_TYPE_STRING, perm("abc")),
	}}
}

func bnxtPF() *PF {
	return &PF{Interface: "ens3f0np0", Bus: "pci", PCIAddress: "0000:41:00.0", Driver: "bnxt_en", Devlink: &DevlinkInfo{}}
}

func TestDevlinkParamsRead(t *testing.T) {
	b := bnxtParams().backend()
	want := &FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(8), LinkType: "eth", FWLLDPAgent: ptr(true),
		NVConfig: []Setting{{"X", "1"}},
		DevlinkParams: []Setting{{"msix_vec_per_pf_max", "64"}, {"ignore_ari", "0"}, {"enable_roce", "true"},
			{"missing", "1"}, {"tiny", "3"}, {"mid", "300"}, {"label", "abc"}}}
	r, err := b.Read(context.Background(), bnxtPF(), want)
	if err != nil {
		t.Fatal(err)
	}
	if r.Values.SRIOVEnabled == nil || !*r.Values.SRIOVEnabled {
		t.Fatalf("enable_sriov %+v", r.Values)
	}
	got := map[string]string{}
	for _, s := range r.Values.DevlinkParams {
		got[s.Name] = s.Value
	}
	if got["msix_vec_per_pf_max"] != "64" || got["ignore_ari"] != "0" || got["tiny"] != "3" || got["mid"] != "300" || got["label"] != "abc" {
		t.Fatalf("params %v", got)
	}
	reasons := map[string]string{}
	for _, u := range r.Unsupported {
		reasons[u.Setting] = u.Reason
	}
	for setting, want := range map[string]string{
		SettingMaxVFs: "VF limit", SettingLinkType: "link type", SettingFWLLDPAgent: "private flag", "nvConfig.X": "NV config",
		"devlinkParams.enable_roce": "not stored in the NIC (modes [driverinit])",
		"devlinkParams.missing":     "driver bnxt_en has no devlink parameter missing",
	} {
		if !strings.Contains(reasons[setting], want) {
			t.Errorf("%s: %q, want %q", setting, reasons[setting], want)
		}
	}
	if b.Name() != "devlink" || b.Tool() != ToolsKernel || !b.Supports(bnxtPF()) || b.Supports(&PF{}) || NewDevlinkParams().get == nil {
		t.Fatal("identity")
	}
}

func TestDevlinkParamsReadError(t *testing.T) {
	b := &DevlinkParams{get: func(_, _, _ string) (*netlink.DevlinkParam, error) { return nil, errors.New("EBUSY") }}
	r, err := b.Read(context.Background(), bnxtPF(), &FirmwareSettings{SRIOVEnabled: ptr(true)})
	if err != nil || len(r.Unsupported) != 1 || !strings.Contains(r.Unsupported[0].Reason, "devlink param enable_sriov: EBUSY") {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestDevlinkParamsApply(t *testing.T) {
	f := bnxtParams()
	reboot, err := f.backend().Apply(context.Background(), bnxtPF(), &FirmwareSettings{SRIOVEnabled: ptr(false),
		DevlinkParams: []Setting{{"msix_vec_per_pf_max", "128"}, {"tiny", "4"}, {"mid", "9"}, {"label", "x"}}})
	if err != nil || !reboot {
		t.Fatalf("apply %v %v", reboot, err)
	}
	want := "pci/0000:41:00.0 enable_sriov cmode=2 bool=false|pci/0000:41:00.0 msix_vec_per_pf_max cmode=2 uint32=128|" +
		"pci/0000:41:00.0 tiny cmode=2 uint8=4|pci/0000:41:00.0 mid cmode=2 uint16=9|pci/0000:41:00.0 label cmode=2 string=x"
	if got := strings.Join(f.sets, "|"); got != want {
		t.Fatalf("sets\n%s\nwant\n%s", got, want)
	}
	for name, c := range map[string]*FirmwareSettings{
		"not a number": {DevlinkParams: []Setting{{"msix_vec_per_pf_max", "x"}}},
		"too big":      {DevlinkParams: []Setting{{"tiny", "300"}}},
		"not a bool":   {DevlinkParams: []Setting{{"ignore_ari", "maybe"}}},
		"missing":      {DevlinkParams: []Setting{{"missing", "1"}}},
	} {
		if _, err := f.backend().Apply(context.Background(), bnxtPF(), c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	f.setErr = errors.New("EPERM")
	if _, err := f.backend().Apply(context.Background(), bnxtPF(), &FirmwareSettings{SRIOVEnabled: ptr(true)}); err == nil ||
		!strings.Contains(err.Error(), "EPERM") {
		t.Fatalf("set error: %v", err)
	}
	if reboot, err := f.backend().Apply(context.Background(), bnxtPF(), &FirmwareSettings{}); reboot || err != nil {
		t.Fatal("empty change")
	}
	if _, err := paramValue(99, "1"); err == nil {
		t.Fatal("unknown type")
	}
	if paramString(int64(5)) != "5" || cmodeName(nl.DEVLINK_PARAM_CMODE_RUNTIME) != "runtime" || cmodeName(7) != "cmode-7" {
		t.Fatal("formatting")
	}
	if paramSpell("1", "true") != "1" || paramSpell("1", "false") != "false" || paramSpell("64", "64") != "64" {
		t.Fatal("paramSpell")
	}
}

// TestBroadcomReconcile turns SR-IOV on in the NVM of a bnxt_en port
// through devlink and reports the VF limit as a vendor-tool setting.
func TestBroadcomReconcile(t *testing.T) {
	f := bnxtParams()
	f.params["enable_sriov"].Values[0].Data = false
	p := Profile{Name: "p5", Match: Match{Driver: "bnxt_en"}, OnDrift: OnDriftApply, Firmware: FirmwareSettings{SRIOVEnabled: ptr(true)}}
	pf := *bnxtPF()
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{pf}, f.backend())
	if err != nil || !res.Reboot || len(f.sets) != 1 || !strings.Contains(f.sets[0], "enable_sriov cmode=2 bool=true") {
		t.Fatalf("%v reboot %v sets %v", err, res.Reboot, f.sets)
	}
	p.Firmware = FirmwareSettings{MaxVFs: ptr(64)}
	_, _, err = runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{pf}, f.backend())
	if err == nil || !strings.Contains(err.Error(), "no generic devlink parameter for the VF limit") {
		t.Fatalf("maxVFs: %v", err)
	}
}

// ethtoolPrivFlags is the "ethtool --show-priv-flags" output of an E810
// port (ice) and an X710 port (i40e).
const (
	iceFlags = `Private flags for ens2f0:
link-down-on-close     : off
fw-lldp-agent          : on
vf-true-promisc-support: off
mdd-auto-reset-vf      : off
vf-vlan-pruning        : off
legacy-rx              : off
`
	i40eFlags = `Private flags for ens4f0:
MFP                   : off
total-port-shutdown   : off
LinkPolling           : off
flow-director-atr     : on
veb-stats             : off
hw-atr-eviction       : off
link-down-on-close    : off
legacy-rx             : off
disable-source-pruning: off
disable-fw-lldp       : on
rs-fec                : off
base-r-fec            : off
vf-vlan-pruning       : off
`
)

// cmdPrivFlags serves the private flags from the fake "ethtool
// --show-priv-flags" output and records a set as "ethtool --set-priv-flags",
// so the tests keep the flag lists of real NICs.
func cmdPrivFlags(cmd nic.Commander) ethtool.PrivFlagIO {
	return ethtool.PrivFlagIO{
		Get: func(iface string) ([]netethtool.Flag, error) {
			out, err := cmd.CombinedOutput(context.Background(), "ethtool", "--show-priv-flags", iface)
			if err != nil {
				return nil, err
			}
			var flags []netethtool.Flag
			for _, line := range strings.Split(string(out), "\n") {
				name, val, ok := strings.Cut(line, ":")
				if !ok || strings.HasPrefix(line, "Private flags") {
					continue
				}
				flags = append(flags, netethtool.Flag{Name: strings.TrimSpace(name), On: strings.TrimSpace(val) == "on"})
			}
			return flags, nil
		},
		Set: func(iface, name string, on bool) error {
			v := "off"
			if on {
				v = "on"
			}
			_, err := cmd.CombinedOutput(context.Background(), "ethtool", "--set-priv-flags", iface, name, v)
			return err
		},
	}
}

func useCmdPrivFlags(t *testing.T) {
	t.Helper()
	orig := intelPrivFlags
	t.Cleanup(func() { intelPrivFlags = orig })
	intelPrivFlags = cmdPrivFlags
}

func intelCmd(iface, flags string) *fakeCmd {
	return &fakeCmd{out: map[string]string{
		"ethtool -i " + iface:                "driver: ice\nfirmware-version: 4.40 0x8001c967 1.3534.0\n",
		"ethtool --show-priv-flags " + iface: flags,
	}}
}

func TestIntelFlags(t *testing.T) {
	useCmdPrivFlags(t)
	ctx := context.Background()
	want := &FirmwareSettings{FWLLDPAgent: ptr(false), MaxVFs: ptr(64), SRIOVEnabled: ptr(true), LinkType: "eth",
		NVConfig: []Setting{{"X", "1"}}, DevlinkParams: []Setting{{"y", "1"}}}

	ice := &PF{Interface: "ens2f0", Driver: "ice"}
	b := NewIntelFlags(slog.Default(), intelCmd("ens2f0", iceFlags))
	r, err := b.Read(ctx, ice, want)
	if err != nil || r.Values.FWLLDPAgent == nil || !*r.Values.FWLLDPAgent || len(r.Unsupported) != 5 {
		t.Fatalf("ice read %+v %v", r, err)
	}
	if !strings.Contains(r.Unsupported[1].Reason, "nvmupdate64e") {
		t.Fatalf("maxVFs reason %q", r.Unsupported[1].Reason)
	}

	// i40e inverts the flag: disable-fw-lldp on means no firmware agent.
	i40e := &PF{Interface: "ens4f0", Driver: "i40e"}
	r, err = NewIntelFlags(slog.Default(), intelCmd("ens4f0", i40eFlags)).Read(ctx, i40e, &FirmwareSettings{FWLLDPAgent: ptr(false)})
	if err != nil || r.Values.FWLLDPAgent == nil || *r.Values.FWLLDPAgent {
		t.Fatalf("i40e read %+v %v", r, err)
	}

	for name, flags := range map[string]string{"no flag": "Private flags for ens2f0:\nlegacy-rx: off\n"} {
		r, err := NewIntelFlags(slog.Default(), intelCmd("ens2f0", flags)).Read(ctx, ice, &FirmwareSettings{FWLLDPAgent: ptr(true)})
		if err != nil || len(r.Unsupported) != 1 {
			t.Errorf("%s: %+v %v", name, r, err)
		}
	}
	bad := intelCmd("ens2f0", iceFlags)
	bad.err = map[string]error{"ethtool -i ens2f0": errors.New("no such device")}
	if _, err := NewIntelFlags(slog.Default(), bad).Read(ctx, ice, &FirmwareSettings{FWLLDPAgent: ptr(true)}); err == nil {
		t.Fatal("ethtool error must fail the read")
	}
	if b.Name() != "ethtool" || b.Tool() != ToolsOpenSource || !b.Supports(i40e) || b.Supports(&PF{Driver: "ixgbe"}) {
		t.Fatal("identity")
	}
}

func TestIntelFlagsApply(t *testing.T) {
	useCmdPrivFlags(t)
	ctx := context.Background()
	for _, c := range []struct {
		driver, iface string
		agent         bool
		want          string
	}{
		{"ice", "ens2f0", false, "ethtool --set-priv-flags ens2f0 fw-lldp-agent off"},
		{"ice", "ens2f0", true, "ethtool --set-priv-flags ens2f0 fw-lldp-agent on"},
		{"i40e", "ens4f0", false, "ethtool --set-priv-flags ens4f0 disable-fw-lldp on"},
		{"i40e", "ens4f0", true, "ethtool --set-priv-flags ens4f0 disable-fw-lldp off"},
	} {
		cmd := &fakeCmd{}
		reboot, err := NewIntelFlags(slog.Default(), cmd).Apply(ctx, &PF{Interface: c.iface, Driver: c.driver}, &FirmwareSettings{FWLLDPAgent: &c.agent})
		if err != nil || reboot || len(cmd.calls) != 1 || cmd.calls[0] != c.want {
			t.Errorf("%s agent=%v: %v %v %v", c.driver, c.agent, reboot, err, cmd.calls)
		}
	}
	cmd := &fakeCmd{err: map[string]error{"ethtool --set-priv-flags ens2f0 fw-lldp-agent off": errors.New("EOPNOTSUPP")}}
	if _, err := NewIntelFlags(slog.Default(), cmd).Apply(ctx, &PF{Interface: "ens2f0", Driver: "ice"}, &FirmwareSettings{FWLLDPAgent: ptr(false)}); err == nil {
		t.Fatal("apply error")
	}
	if reboot, err := NewIntelFlags(slog.Default(), cmd).Apply(ctx, &PF{}, &FirmwareSettings{}); reboot || err != nil {
		t.Fatal("empty change")
	}
}

func TestDefaultBackends(t *testing.T) {
	backends := DefaultBackends(slog.Default())
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name())
	}
	if strings.Join(names, ",") != "devlink,mstconfig,mlxconfig,ethtool" {
		t.Fatalf("backends %v", names)
	}
}
