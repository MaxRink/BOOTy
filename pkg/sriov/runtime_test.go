package sriov

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// fakeRuntime keeps the runtime state per port in memory and records the
// calls, like sysfs and devlink would apply them.
type fakeRuntime struct {
	pfs     map[string]*PF // by interface; ports of one device share state
	calls   []string
	errOn   map[string]error // call prefix: error
	inUse   map[string]string
	noApply map[string]bool // call prefix: accept but do not change the state
	// renameOnEswitch gives the PF netdev this name after an eSwitch
	// change, like a re-register without udev.
	renameOnEswitch string
	master          map[string]string // interface: master device
}

func newFakeRuntime(pfs ...PF) *fakeRuntime {
	f := &fakeRuntime{pfs: map[string]*PF{}, errOn: map[string]error{}, inUse: map[string]string{}, noApply: map[string]bool{}}
	for i := range pfs {
		pf := pfs[i]
		f.pfs[pf.Interface] = &pf
	}
	return f
}

func (f *fakeRuntime) call(c string) (error, bool) { //nolint:revive // test fake: error, skip
	f.calls = append(f.calls, c)
	for prefix, err := range f.errOn {
		if strings.HasPrefix(c, prefix) {
			return err, true
		}
	}
	for prefix := range f.noApply {
		if strings.HasPrefix(c, prefix) {
			return nil, true
		}
	}
	return nil, false
}

// device returns every port of the device of pf.
func (f *fakeRuntime) device(pf *PF) []*PF {
	var out []*PF
	for _, p := range f.pfs {
		if p.Bus == pf.Bus && p.PCIAddress == pf.PCIAddress {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeRuntime) SetNumVFs(pf *PF, n int) error {
	if err, skip := f.call(fmt.Sprintf("numvfs %s %d", pf.Interface, n)); skip {
		return err
	}
	for _, p := range f.device(pf) {
		if p.NumVFs != 0 && n != 0 {
			return errors.New("EBUSY: VFs already enabled")
		}
		if n > p.TotalVFs {
			return errors.New("ERANGE: above sriov_totalvfs")
		}
		p.NumVFs = n
		p.VFs = nil
		for i := 0; i < n; i++ {
			p.VFs = append(p.VFs, VF{Index: i, LinkState: "auto", SpoofChk: true})
		}
	}
	return nil
}

func (f *fakeRuntime) SetEswitch(pf *PF, mode string) error {
	if err, skip := f.call(fmt.Sprintf("eswitch %s %s", pf.Interface, mode)); skip {
		return err
	}
	for _, p := range f.device(pf) {
		if p.NumVFs != 0 {
			return errors.New("EBUSY: VFs enabled")
		}
		p.Eswitch.Mode = mode
		if f.renameOnEswitch != "" {
			p.Interface = f.renameOnEswitch
		}
	}
	return nil
}

func (f *fakeRuntime) SetVF(pf *PF, i int, s *VFSettings, mac string) error {
	if err, skip := f.call(fmt.Sprintf("vf %s %d", pf.Interface, i)); skip {
		return err
	}
	p := f.pfs[pf.Interface]
	vf := findVF(p.VFs, i)
	if vf == nil {
		return errors.New("EINVAL: no such VF")
	}
	if mac != "" {
		vf.MAC = normalizeMAC(mac)
	}
	if s.Trust != nil {
		vf.Trust = *s.Trust
	}
	if s.SpoofChk != nil {
		vf.SpoofChk = *s.SpoofChk
	}
	vf.VLAN, vf.QoS = derefOr(s.VLAN, vf.VLAN), derefOr(s.QoS, vf.QoS)
	if s.MinTxRate != nil {
		vf.MinTxRate = uint32(*s.MinTxRate)
	}
	if s.MaxTxRate != nil {
		vf.MaxTxRate = uint32(*s.MaxTxRate)
	}
	if s.LinkState != "" {
		vf.LinkState = s.LinkState
	}
	return nil
}

func derefOr(p *int, v int) int {
	if p == nil {
		return v
	}
	return *p
}

func (f *fakeRuntime) Master(pf *PF) string { return f.master[pf.Interface] }

func (f *fakeRuntime) InUse(pf *PF) (bool, string) {
	r, ok := f.inUse[pf.Interface]
	return ok, r
}

func (f *fakeRuntime) discover() ([]PF, error) {
	out := make([]PF, 0, len(f.pfs))
	for _, p := range f.pfs {
		c := *p
		c.VFs = append([]VF(nil), p.VFs...)
		out = append(out, c)
	}
	return out, nil
}

func rtPF(name, addr string) PF {
	return PF{Interface: name, Bus: "pci", PCIAddress: addr, Driver: "mlx5_core", SRIOVCapable: true, TotalVFs: 8,
		Eswitch: Eswitch{Capability: CapSupported, Mode: EswitchLegacy}}
}

func rtProfile(rt RuntimeSettings) Profile {
	return Profile{Name: "rt", Match: Match{Driver: "mlx5_core"}, Runtime: rt}
}

func runRuntime(t *testing.T, f *fakeRuntime, onDrift string, profiles ...Profile) (*Result, error) {
	t.Helper()
	r := &Reconciler{Config: &Config{Profiles: profiles, OnDrift: onDrift}, Discover: f.discover, Runtime: f,
		Log: slog.New(slog.DiscardHandler)}
	return r.Run(t.Context())
}

func findingsBy(res *Result, level string) map[string]Finding {
	m := map[string]Finding{}
	for _, f := range res.Findings {
		if f.Level == level {
			m[f.Interface+"/"+f.Setting] = f
		}
	}
	return m
}

func TestRuntimeApplyVerifyAndReset(t *testing.T) {
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	p := rtProfile(RuntimeSettings{NumVFs: ptr(2), EswitchMode: EswitchSwitchdev,
		VFDefaults: VFSettings{Trust: ptr(true), SpoofChk: ptr(false)},
		VFs: []VFOverride{{Index: 1, MAC: "02:00:00:00:00:01", VFSettings: VFSettings{VLAN: ptr(100), QoS: ptr(3),
			MinTxRate: ptr(10), MaxTxRate: ptr(100), LinkState: "enable", SpoofChk: ptr(true)}}}})
	res, err := runRuntime(t, f, OnDriftFail, p)
	if err != nil {
		t.Fatal(err)
	}
	got := findingsBy(res, LevelRuntime)
	for _, k := range []string{"p0/eswitchMode", "p0/numVFs", "p0/vfs[0]", "p0/vfs[1]", "p0/handoff"} {
		if got[k].Status != StatusOK {
			t.Errorf("%s = %+v", k, got[k])
		}
	}
	if w := got["p0/vfs[1]"].Want; w != "mac=02:00:00:00:00:01 trust=true spoofChk=true vlan=100 qos=3 minTxRate=10 maxTxRate=100 linkState=enable" {
		t.Errorf("vfs[1] want %q", w)
	}
	// eSwitch before the VFs; the reset turns the VFs off first and
	// restores the eSwitch.
	want := "eswitch p0 switchdev,numvfs p0 2,vf p0 0,vf p0 1,numvfs p0 0,eswitch p0 legacy"
	if strings.Join(f.calls, ",") != want {
		t.Fatalf("calls %v\nwant %s", f.calls, want)
	}
	if pf := f.pfs["p0"]; pf.NumVFs != 0 || pf.Eswitch.Mode != EswitchLegacy {
		t.Fatalf("not reset: %+v", pf)
	}
}

func TestRuntimeKeepAndChangeCount(t *testing.T) {
	pf := rtPF("p0", "0000:3b:00.0")
	pf.NumVFs = 2
	f := newFakeRuntime(pf)
	f.pfs["p0"].VFs = []VF{{Index: 0}, {Index: 1}}
	res, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{NumVFs: ptr(4), Handoff: HandoffKeep}))
	if err != nil {
		t.Fatal(err)
	}
	// A count change goes through 0; keep leaves the VFs.
	if strings.Join(f.calls, ",") != "numvfs p0 0,numvfs p0 4" || f.pfs["p0"].NumVFs != 4 {
		t.Fatalf("calls %v state %+v", f.calls, f.pfs["p0"])
	}
	if _, ok := findingsBy(res, LevelRuntime)["p0/handoff"]; ok {
		t.Fatal("keep must not reset")
	}
}

func TestRuntimeNoChangeNoReset(t *testing.T) {
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	res, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{NumVFs: ptr(0), EswitchMode: EswitchLegacy}))
	if err != nil || len(f.calls) != 0 {
		t.Fatalf("calls %v err %v", f.calls, err)
	}
	if len(findingsBy(res, LevelRuntime)) != 2 {
		t.Fatalf("findings %+v", res.Findings)
	}
}

func TestRuntimeEswitchOnPortInUse(t *testing.T) {
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	f.inUse["p0"] = "a route goes through the port"
	res, err := runRuntime(t, f, OnDriftWarn, rtProfile(RuntimeSettings{EswitchMode: EswitchSwitchdev, NumVFs: ptr(1)}))
	if err != nil {
		t.Fatalf("warn: %v", err)
	}
	got := findingsBy(res, LevelRuntime)["p0/eswitchMode"]
	if got.Status != StatusRefusedInUse || !strings.Contains(got.Detail, "disrupt the ramdisk network: a route goes through the port") {
		t.Fatalf("finding %+v", got)
	}
	if strings.Contains(strings.Join(f.calls, ","), "eswitch") {
		t.Fatalf("eSwitch changed on a port in use: %v", f.calls)
	}
	if _, err := runRuntime(t, newFakeRuntimeInUse(), OnDriftFail, rtProfile(RuntimeSettings{EswitchMode: EswitchSwitchdev})); err == nil {
		t.Fatal("fail must stop on a refused eSwitch change")
	}
}

func newFakeRuntimeInUse() *fakeRuntime {
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	f.inUse["p0"] = "the port has a master device"
	return f
}

func TestRuntimeFailures(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(f *fakeRuntime)
		rt      RuntimeSettings
		setting string
		detail  string
	}{
		{"numvfs above total", nil, RuntimeSettings{NumVFs: ptr(9)}, "numVFs", "ERANGE"},
		{"numvfs error", func(f *fakeRuntime) { f.errOn["numvfs p0 2"] = errors.New("EIO") }, RuntimeSettings{NumVFs: ptr(2)}, "numVFs", "sriov_numvfs 2: EIO"},
		{"eswitch error", func(f *fakeRuntime) { f.errOn["eswitch p0 switchdev"] = errors.New("EOPNOTSUPP") },
			RuntimeSettings{EswitchMode: EswitchSwitchdev}, "eswitchMode", "devlink eswitch switchdev: EOPNOTSUPP"},
		{"vf error", func(f *fakeRuntime) { f.errOn["vf p0 1"] = errors.New("EINVAL") },
			RuntimeSettings{NumVFs: ptr(2), VFDefaults: VFSettings{Trust: ptr(true)}}, "vfs[1]", "VF 1: EINVAL"},
		{"vf ignored", func(f *fakeRuntime) { f.noApply["vf p0 0"] = true },
			RuntimeSettings{NumVFs: ptr(1), VFDefaults: VFSettings{VLAN: ptr(5)}}, "vfs[0]", ""},
		{"numvfs ignored", func(f *fakeRuntime) { f.noApply["numvfs p0 3"] = true }, RuntimeSettings{NumVFs: ptr(3)}, "numVFs", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
			if tc.setup != nil {
				tc.setup(f)
			}
			res, err := runRuntime(t, f, OnDriftFail, rtProfile(tc.rt))
			got := findingsBy(res, LevelRuntime)["p0/"+tc.setting]
			if err == nil || got.Status != StatusFailed || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("err %v finding %+v", err, got)
			}
		})
	}
}

func TestRuntimeResetFailure(t *testing.T) {
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	f.errOn["numvfs p0 0"] = errors.New("EBUSY")
	res, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{NumVFs: ptr(1)}))
	got := findingsBy(res, LevelRuntime)["p0/handoff"]
	if err == nil || got.Status != StatusFailed || !strings.Contains(got.Detail, "sriov_numvfs 0: EBUSY") || got.Got != "numVFs=1 eswitch=legacy" {
		t.Fatalf("err %v finding %+v", err, got)
	}
}

func TestRuntimeRestoresEarlierVFs(t *testing.T) {
	pf := rtPF("p0", "0000:3b:00.0")
	pf.NumVFs = 1
	f := newFakeRuntime(pf)
	if _, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{NumVFs: ptr(3)})); err != nil {
		t.Fatal(err)
	}
	if f.pfs["p0"].NumVFs != 1 {
		t.Fatalf("handoff reset must restore the earlier VF count: %+v", f.pfs["p0"])
	}
}

func TestRuntimeMultiPortDevice(t *testing.T) {
	a, b := rtPF("n0", "netdevsim1"), rtPF("n1", "netdevsim1")
	a.Bus, b.Bus = "netdevsim", "netdevsim"
	f := newFakeRuntime(a, b)
	p := rtProfile(RuntimeSettings{NumVFs: ptr(2)})
	res, err := runRuntime(t, f, OnDriftFail, p)
	if err != nil {
		t.Fatal(err)
	}
	// One write per device, two verified ports.
	if strings.Count(strings.Join(f.calls, ","), "numvfs") != 2 || len(findingsBy(res, LevelRuntime)) != 3 {
		t.Fatalf("calls %v findings %+v", f.calls, res.Findings)
	}

	f = newFakeRuntime(a, b)
	p0 := Profile{Name: "a", Match: Match{Interface: "n0"}, Runtime: RuntimeSettings{NumVFs: ptr(2)}}
	p1 := Profile{Name: "b", Match: Match{Interface: "n1"}, Runtime: RuntimeSettings{NumVFs: ptr(4)}}
	res, err = runRuntime(t, f, OnDriftFail, p0, p1)
	if err == nil || !strings.Contains(err.Error(), "wants other runtime settings") {
		t.Fatalf("conflict: %v %+v", err, res.Findings)
	}
}

func TestRuntimePortMissingAfterApply(t *testing.T) {
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	calls := 0
	r := &Reconciler{Config: &Config{Profiles: []Profile{rtProfile(RuntimeSettings{NumVFs: ptr(1)})}}, Runtime: f,
		Log: slog.New(slog.DiscardHandler),
		Discover: func() ([]PF, error) {
			calls++
			if calls > 1 {
				return nil, errors.New("sysfs gone")
			}
			return f.discover()
		}}
	res, err := r.Run(t.Context())
	if err != nil {
		t.Fatalf("warn: %v", err)
	}
	got := findingsBy(res, LevelRuntime)
	if got["p0/runtime"].Status != StatusFailed || got["p0/handoff"].Status != StatusFailed {
		t.Fatalf("findings %+v", got)
	}
}

func TestEffectiveVF(t *testing.T) {
	rt := &RuntimeSettings{VFDefaults: VFSettings{Trust: ptr(false), VLAN: ptr(1)},
		VFs: []VFOverride{{Index: 2, MAC: "02:00:00:00:00:02", VFSettings: VFSettings{Trust: ptr(true), LinkState: "disable"}}}}
	s, mac := effectiveVF(rt, 2)
	if *s.Trust != true || *s.VLAN != 1 || s.LinkState != "disable" || mac != "02:00:00:00:00:02" {
		t.Fatalf("vf 2: %+v %s", s, mac)
	}
	s, mac = effectiveVF(rt, 0)
	if *s.Trust || mac != "" || s.LinkState != "" {
		t.Fatalf("vf 0: %+v %s", s, mac)
	}
	if w, g := vfCompare(&VFSettings{Trust: ptr(true)}, "", nil); w != "trust=true" || g != "trust=false" {
		t.Fatalf("missing VF: %s / %s", w, g)
	}
}

func TestFindVF(t *testing.T) {
	vfs := []VF{{Index: 3, MAC: "c"}, {Index: 1, MAC: "a"}, {Index: 2, MAC: "b"}}
	if v := findVF(vfs, 1); v == nil || v.MAC != "a" {
		t.Fatalf("findVF(1) = %+v", v)
	}
	if v := findVF(vfs, 3); v == nil || v.MAC != "c" {
		t.Fatalf("findVF(3) = %+v", v)
	}
	if findVF(vfs, 0) != nil {
		t.Fatal("findVF(0) must be nil")
	}
}

// TestRuntimeDeferredAfterFirmwareApply is review finding 3: after BOOTy
// stores a new VF limit, sriov_totalvfs is still the old one, so the
// runtime settings of that adapter wait for the reboot instead of failing
// with ERANGE.
func TestRuntimeDeferredAfterFirmwareApply(t *testing.T) {
	pf := rtPF("ens1f0np0", "0000:3b:00.0")
	f := newFakeRuntime(pf)
	b := &fakeBackend{name: "mstconfig", tool: ToolsOpenSource, driver: "mlx5_core", stored: FirmwareSettings{MaxVFs: ptr(8)}}
	p := rtProfile(RuntimeSettings{NumVFs: ptr(32)})
	p.Firmware.MaxVFs = ptr(64)
	r := &Reconciler{Config: &Config{Profiles: []Profile{p}, OnDrift: OnDriftApply}, Discover: f.discover, Runtime: f,
		Backends: []FirmwareBackend{b}, Log: slog.New(slog.DiscardHandler)}
	res, err := r.Run(t.Context())
	if err != nil || !res.Reboot {
		t.Fatalf("%v reboot %v", err, res.Reboot)
	}
	got := findingsBy(res, LevelRuntime)["ens1f0np0/runtime"]
	if got.Status != StatusDeferred || len(f.calls) != 0 {
		t.Fatalf("runtime %+v calls %v", got, f.calls)
	}
	if !res.VerifyReboot || res.VerifyDetail != "adapters=0000:3b:00 settings=maxVFs" {
		t.Fatalf("verify reboot %v %q", res.VerifyReboot, res.VerifyDetail)
	}
	// Another adapter in the same run is not deferred.
	other := rtPF("ens2f0np0", "0000:5e:00.0")
	f = newFakeRuntime(pf, other)
	b.stored.MaxVFs = ptr(8)
	q := rtProfile(RuntimeSettings{NumVFs: ptr(2)})
	q.Name, q.Match = "other", Match{Interface: "ens2f0np0"}
	p.Match = Match{Interface: "ens1f0np0"}
	r = &Reconciler{Config: &Config{Profiles: []Profile{p, q}, OnDrift: OnDriftApply}, Discover: f.discover, Runtime: f,
		Backends: []FirmwareBackend{b}, Log: slog.New(slog.DiscardHandler)}
	res, err = r.Run(t.Context())
	if err != nil || findingsBy(res, LevelRuntime)["ens2f0np0/numVFs"].Status != StatusOK {
		t.Fatalf("other adapter: %v %+v", err, res.Findings)
	}
}

// TestRuntimeVFCountOnPortInUse is review finding 5: igb, ixgbe and i40e
// reset the PF when VFs come or go, so the VF count of a port the ramdisk
// uses stays as it is.
func TestRuntimeVFCountOnPortInUse(t *testing.T) {
	inUse := "TCP session to fe80::1 is bound to the port (BGP over IPv6 link-local)"
	// Drivers that reset the PF for a VF count change: refused, the VF
	// findings too, and it fails with onDrift fail.
	for _, driver := range []string{"igb", "ixgbe", "i40e", "bnxt_en"} {
		pf := rtPF("p0", "0000:3b:00.0")
		pf.Driver = driver
		f := newFakeRuntime(pf)
		f.inUse["p0"] = inUse
		p := rtProfile(RuntimeSettings{NumVFs: ptr(2), VFDefaults: VFSettings{Trust: ptr(true)}})
		p.Match = Match{Driver: driver}
		res, err := runRuntime(t, f, OnDriftFail, p)
		got := findingsBy(res, LevelRuntime)
		if err == nil || got["p0/numVFs"].Status != StatusRefusedInUse || got["p0/vfs[0]"].Status != StatusRefusedInUse ||
			!strings.Contains(got["p0/numVFs"].Detail, "driver "+driver+" resets the PF") || len(f.calls) != 0 {
			t.Errorf("%s: err %v findings %+v calls %v", driver, err, got, f.calls)
		}
	}
	// mlx5 and ice keep the PF up (the T-CaaS uplinks): allowed on a port in use.
	for _, driver := range []string{"mlx5_core", "ice"} {
		pf := rtPF("p0", "0000:3b:00.0")
		pf.Driver = driver
		f := newFakeRuntime(pf)
		f.inUse["p0"] = inUse
		p := rtProfile(RuntimeSettings{NumVFs: ptr(2)})
		p.Match = Match{Driver: driver}
		res, err := runRuntime(t, f, OnDriftFail, p)
		if err != nil || findingsBy(res, LevelRuntime)["p0/numVFs"].Status != StatusOK {
			t.Errorf("%s: %v %+v", driver, err, res.Findings)
		}
	}
	// The eSwitch change stays refused on a port in use, also on mlx5.
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	f.inUse["p0"] = inUse
	res, _ := runRuntime(t, f, OnDriftWarn, rtProfile(RuntimeSettings{NumVFs: ptr(2), EswitchMode: EswitchSwitchdev}))
	got := findingsBy(res, LevelRuntime)
	if got["p0/eswitchMode"].Status != StatusRefusedInUse || got["p0/numVFs"].Status != StatusOK ||
		strings.Contains(strings.Join(f.calls, ","), "eswitch") {
		t.Fatalf("mlx5 eSwitch: %+v calls %v", got, f.calls)
	}
	// Without a change the port is not touched and the check passes.
	pf := rtPF("p0", "0000:3b:00.0")
	pf.Driver = "igb"
	f = newFakeRuntime(pf)
	f.inUse["p0"] = "busy"
	p := rtProfile(RuntimeSettings{NumVFs: ptr(0)})
	p.Match = Match{Driver: "igb"}
	if _, err := runRuntime(t, f, OnDriftFail, p); err != nil {
		t.Fatalf("no change on a busy port: %v", err)
	}
}

// TestUplinkVFDriversOverride replaces the driver table.
func TestUplinkVFDriversOverride(t *testing.T) {
	pf := rtPF("p0", "0000:3b:00.0")
	pf.Driver = "bnxt_en"
	f := newFakeRuntime(pf)
	f.inUse["p0"] = "busy"
	p := rtProfile(RuntimeSettings{NumVFs: ptr(2)})
	p.Match = Match{Driver: "bnxt_en"}
	r := &Reconciler{Config: &Config{Profiles: []Profile{p}, OnDrift: OnDriftFail, UplinkVFDrivers: []string{"bnxt_en"}},
		Discover: f.discover, Runtime: f, Log: slog.New(slog.DiscardHandler)}
	if res, err := r.Run(t.Context()); err != nil || findingsBy(res, LevelRuntime)["p0/numVFs"].Status != StatusOK {
		t.Fatalf("override: %v %+v", err, res.Findings)
	}
	c := &Config{UplinkVFDrivers: []string{"mlx5 core"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SRIOV_UPLINK_VF_DRIVERS") {
		t.Fatalf("validation: %v", err)
	}
	if got := (&Config{}).UplinkVFDriverList(); strings.Join(got, ",") != "mlx5_core,ice" {
		t.Fatalf("default table %v", got)
	}
}

// TestRuntimeEswitchWithVFsPresent is review finding 7: VFs already exist,
// so the eSwitch change must turn them off first (the kernel answers EBUSY
// otherwise) and the VFs come back after it.
func TestRuntimeEswitchWithVFsPresent(t *testing.T) {
	pf := rtPF("p0", "0000:3b:00.0")
	pf.NumVFs = 2
	f := newFakeRuntime(pf)
	f.pfs["p0"].VFs = []VF{{Index: 0}, {Index: 1}}
	res, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{NumVFs: ptr(2), EswitchMode: EswitchSwitchdev, Handoff: HandoffKeep}))
	if err != nil {
		t.Fatalf("%v %+v", err, res.Findings)
	}
	if got := strings.Join(f.calls, ","); got != "numvfs p0 0,eswitch p0 switchdev,numvfs p0 2" {
		t.Fatalf("calls %s", got)
	}
}

// TestRuntimeFindsRenamedPort is review finding 6: after a switchdev change
// the PF netdev can come back with another name; the check finds it by
// its bus, PCI address and port.
func TestRuntimeFindsRenamedPort(t *testing.T) {
	pf := rtPF("ens1f0np0", "0000:3b:00.0")
	pf.PhysPortName = "p0"
	f := newFakeRuntime(pf)
	f.renameOnEswitch = "eth5"
	res, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{EswitchMode: EswitchSwitchdev}))
	if err != nil || findingsBy(res, LevelRuntime)["ens1f0np0/eswitchMode"].Status != StatusOK {
		t.Fatalf("%v %+v", err, res.Findings)
	}
}

func TestPortKey(t *testing.T) {
	pf := PF{Bus: "pci", PCIAddress: "0000:3b:00.0", PhysPortName: "p0", MAC: "b8:ce:f6:00:00:01", Interface: "eth5"}
	if got := portKey(&pf); got != "pci/0000:3b:00.0/p0" {
		t.Fatalf("portKey = %q", got)
	}
	pf.PhysPortName = ""
	if got := portKey(&pf); got != "pci/0000:3b:00.0/b8:ce:f6:00:00:01" {
		t.Fatalf("portKey without phys_port_name = %q", got)
	}
}

// TestRuntimeNotDeferredForOtherNVChanges: a stored CQE_COMPRESSION alone
// needs the reboot at the handoff, but not the verify reboot: the runtime
// settings run and are checked in this boot.
func TestRuntimeNotDeferredForOtherNVChanges(t *testing.T) {
	pf := rtPF("ens1f0np0", "0000:3b:00.0")
	f := newFakeRuntime(pf)
	b := &fakeBackend{name: "mstconfig", tool: ToolsOpenSource, driver: "mlx5_core",
		stored: FirmwareSettings{NVConfig: []Setting{{"CQE_COMPRESSION", "0"}}}}
	p := rtProfile(RuntimeSettings{NumVFs: ptr(2)})
	p.Firmware.NVConfig = []Setting{{"CQE_COMPRESSION", "1"}}
	r := &Reconciler{Config: &Config{Profiles: []Profile{p}, OnDrift: OnDriftApply}, Discover: f.discover, Runtime: f,
		Backends: []FirmwareBackend{b}, Log: slog.New(slog.DiscardHandler)}
	res, err := r.Run(t.Context())
	if err != nil || !res.Reboot || res.VerifyReboot {
		t.Fatalf("%v reboot %v verify %v", err, res.Reboot, res.VerifyReboot)
	}
	if findingsBy(res, LevelRuntime)["ens1f0np0/numVFs"].Status != StatusOK {
		t.Fatalf("runtime not checked: %+v", res.Findings)
	}
}

func TestVerifyRebootDetail(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	rt := []Finding{{Profile: "a", Interface: "p0", Status: StatusDeferred, Detail: "x"}, {Status: StatusOK}}
	needed, detail := verifyReboot(log, rt, map[string][]string{
		"0000:5e:00": {SettingLinkType}, "0000:3b:00": {SettingMaxVFs, SettingSRIOVEnabled},
	})
	if !needed || detail != "adapters=0000:3b:00,0000:5e:00 settings=linkType,maxVFs,sriovEnabled" ||
		!strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "deferred to a verify reboot") {
		t.Fatalf("%v %q\n%s", needed, detail, buf.String())
	}
	if needed, _ := verifyReboot(log, []Finding{{Status: StatusOK}}, nil); needed {
		t.Fatal("no deferral")
	}
	if runtimeAffecting("nvConfig.CQE_COMPRESSION") || !runtimeAffecting(SettingLinkType) {
		t.Fatal("runtimeAffecting")
	}
	if needsReset(&Finding{Level: LevelFirmware, Status: StatusApplied, Detail: detailActiveNow}) ||
		!needsReset(&Finding{Level: LevelFirmware, Status: StatusApplied, Detail: detailStoredThisBoot}) {
		t.Fatal("needsReset")
	}
}

// TestRuntimeVFCountOnBondedMlx5Uplink: on an mlx5 port with a master
// (hardware LAG bond) the first VFs toggle the LAG, so the VF count change
// is refused; without a master it is allowed.
func TestRuntimeVFCountOnBondedMlx5Uplink(t *testing.T) {
	for _, bonded := range []bool{true, false} {
		f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
		f.inUse["p0"] = "the port has a master device (bond, bridge, VRF)"
		f.master = map[string]string{}
		if bonded {
			f.master["p0"] = "bond0"
		}
		res, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{NumVFs: ptr(2)}))
		got := findingsBy(res, LevelRuntime)["p0/numVFs"]
		if bonded && (err == nil || got.Status != StatusRefusedInUse || !strings.Contains(got.Detail, "hardware LAG") || len(f.calls) != 0) {
			t.Fatalf("bonded: %v %+v calls %v", err, got, f.calls)
		}
		if !bonded && (err != nil || got.Status != StatusOK) {
			t.Fatalf("not bonded: %v %+v", err, got)
		}
	}
	// Not in use: the master check does not run (no change refused).
	f := newFakeRuntime(rtPF("p0", "0000:3b:00.0"))
	f.master = map[string]string{"p0": "bond0"}
	if _, err := runRuntime(t, f, OnDriftFail, rtProfile(RuntimeSettings{NumVFs: ptr(2)})); err != nil {
		t.Fatalf("not in use: %v", err)
	}
}
