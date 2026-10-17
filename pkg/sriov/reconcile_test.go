package sriov

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// fakeBackend stores firmware values in memory, like NV config.
type fakeBackend struct {
	name, tool  string
	driver      string
	stored      FirmwareSettings
	unsupported []Unsupported
	readErr     error
	applyErr    error
	noPersist   bool // the set returns no error but the NIC keeps the old value
	applied     []*FirmwareSettings
}

func (b *fakeBackend) Name() string         { return b.name }
func (b *fakeBackend) Tool() string         { return b.tool }
func (b *fakeBackend) Supports(pf *PF) bool { return pf.Driver == b.driver }

func (b *fakeBackend) Read(_ context.Context, _ *PF, _ *FirmwareSettings) (*FirmwareRead, error) {
	if b.readErr != nil {
		return nil, b.readErr
	}
	return &FirmwareRead{Values: b.stored, Unsupported: b.unsupported}, nil
}

func (b *fakeBackend) Apply(_ context.Context, _ *PF, change *FirmwareSettings) error {
	if b.applyErr != nil {
		return b.applyErr
	}
	b.applied = append(b.applied, change)
	if !b.noPersist {
		storeChange(&b.stored, change)
	}
	return nil
}

// storeChange writes a change into stored values, like NV config does.
func storeChange(stored, change *FirmwareSettings) {
	if change.SRIOVEnabled != nil {
		stored.SRIOVEnabled = change.SRIOVEnabled
	}
	if change.MaxVFs != nil {
		stored.MaxVFs = change.MaxVFs
	}
	if change.LinkType != "" {
		stored.LinkType = change.LinkType
	}
	if change.FWLLDPAgent != nil {
		stored.FWLLDPAgent = change.FWLLDPAgent
	}
	stored.NVConfig = mergeSettings(stored.NVConfig, change.NVConfig)
	stored.DevlinkParams = mergeSettings(stored.DevlinkParams, change.DevlinkParams)
}

func mergeSettings(list, change []Setting) []Setting {
	out := append([]Setting(nil), list...)
	for _, c := range change {
		found := false
		for i := range out {
			if out[i].Name == c.Name {
				out[i].Value, found = c.Value, true
			}
		}
		if !found {
			out = append(out, c)
		}
	}
	return out
}

// cx6 is a ConnectX-6 Dx port with SR-IOV on and 64 VFs active.
func cx6() PF {
	return PF{Interface: "ens1f0np0", VendorID: "15b3", DeviceID: "101d", Driver: "mlx5_core",
		LinkType: "eth", SRIOVCapable: true, TotalVFs: 64}
}

func cx6Profile() Profile {
	return Profile{Name: "cx6", Match: Match{Driver: "mlx5_core"},
		Firmware: FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(64), LinkType: "eth",
			NVConfig: []Setting{{"CQE_COMPRESSION", "1"}}}}
}

func compliantBackend() *fakeBackend {
	return &fakeBackend{name: "mstconfig", tool: ToolsOpenSource, driver: "mlx5_core",
		stored: FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(64), LinkType: "eth",
			NVConfig: []Setting{{"CQE_COMPRESSION", "1"}}}}
}

func runReconcile(t *testing.T, c *Config, pfs []PF, backends ...FirmwareBackend) (res *Result, logs string, err error) {
	t.Helper()
	return runReconcileJournal(t, c, pfs, nil, backends...)
}

func runReconcileJournal(t *testing.T, c *Config, pfs []PF, j *Journal, backends ...FirmwareBackend) (res *Result, logs string, err error) {
	t.Helper()
	var buf bytes.Buffer
	r := &Reconciler{Config: c, Discover: func() ([]PF, error) { return pfs, nil }, Backends: backends, Journal: j,
		Log: slog.New(slog.NewTextHandler(&buf, nil))}
	res, err = r.Run(context.Background())
	return res, buf.String(), err
}

func statuses(res *Result) map[string]string {
	m := map[string]string{}
	for _, f := range res.Findings {
		m[f.Level+"."+f.Setting] = f.Status
	}
	return m
}

func TestReconcileCompliant(t *testing.T) {
	c := &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftFail}
	res, _, err := runReconcile(t, c, []PF{cx6()}, compliantBackend())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		if f.Status != StatusOK {
			t.Errorf("finding %s", f.String())
		}
	}
	if res.Reboot || len(res.Findings) != 5 {
		t.Fatalf("result %+v", res)
	}
}

func TestReconcileWithoutProfiles(t *testing.T) {
	r := &Reconciler{Config: &Config{}, Discover: func() ([]PF, error) { t.Fatal("discovered without profiles"); return nil, nil }}
	res, err := r.Run(context.Background())
	if err != nil || len(res.Findings) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestReconcileDiscoverAndAssignErrors(t *testing.T) {
	r := &Reconciler{Config: &Config{Profiles: []Profile{cx6Profile()}},
		Discover: func() ([]PF, error) { return nil, errors.New("no sysfs") }}
	if _, err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "no sysfs") {
		t.Fatalf("discover error: %v", err)
	}
	two := cx6Profile()
	two.Name = "cx6-other"
	c := &Config{Profiles: []Profile{cx6Profile(), two}}
	if _, _, err := runReconcile(t, c, []PF{cx6()}); err == nil || !strings.Contains(err.Error(), "matches the SR-IOV profiles cx6, cx6-other") {
		t.Fatalf("ambiguous profiles: %v", err)
	}
}

func TestReconcileCountsMatchesAndLogsSummary(t *testing.T) {
	p1 := cx6()
	p1.Interface = "ens1f1np1"
	res, logs, err := runReconcile(t, &Config{Profiles: []Profile{cx6Profile()}}, []PF{cx6(), p1}, compliantBackend())
	if err != nil {
		t.Fatal(err)
	}
	// 1 match finding plus 4 firmware findings on each of the two ports.
	if !strings.Contains(logs, "msg=\"SR-IOV compliance\" findings=9 compliant=9 failed=0") ||
		res.Findings[0].Setting != "pfPorts" || res.Findings[0].Got != "2" {
		t.Fatalf("findings %+v logs:\n%s", res.Findings, logs)
	}
}

func TestReconcileWarnLogsAndGoesOn(t *testing.T) {
	b := compliantBackend()
	b.stored.MaxVFs = ptr(32)
	c := &Config{Profiles: []Profile{cx6Profile()}}
	res, logs, err := runReconcile(t, c, []PF{cx6()}, b)
	if err != nil {
		t.Fatalf("warn must not fail: %v", err)
	}
	if statuses(res)["firmware.maxVFs"] != StatusDrift || len(b.applied) != 0 || res.Reboot {
		t.Fatalf("result %+v applied %v", res, b.applied)
	}
	if !strings.Contains(logs, "SR-IOV drift") || !strings.Contains(logs, "setting=maxVFs") {
		t.Fatalf("logs: %s", logs)
	}
}

func TestReconcileFailStops(t *testing.T) {
	b := compliantBackend()
	b.stored.NVConfig = []Setting{{"CQE_COMPRESSION", "0"}}
	c := &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftFail}
	_, _, err := runReconcile(t, c, []PF{cx6()}, b)
	if err == nil || !strings.Contains(err.Error(), "nvConfig.CQE_COMPRESSION: drift (want 1, got stored=0)") {
		t.Fatalf("fail: %v", err)
	}
}

func TestReconcileApplyStoresAndReboots(t *testing.T) {
	b := compliantBackend()
	b.stored = FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(8), LinkType: "eth",
		NVConfig: []Setting{{"CQE_COMPRESSION", "0"}}}
	pf := cx6()
	pf.TotalVFs = 8
	c := &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftApply}
	res, _, err := runReconcile(t, c, []PF{pf}, b)
	if err != nil {
		t.Fatal(err)
	}
	st := statuses(res)
	if st["firmware.maxVFs"] != StatusApplied || st["firmware.nvConfig.CQE_COMPRESSION"] != StatusApplied ||
		st["firmware.sriovEnabled"] != StatusOK || !res.Reboot {
		t.Fatalf("statuses %v reboot %v", st, res.Reboot)
	}
	if len(b.applied) != 1 {
		t.Fatalf("applied %d times", len(b.applied))
	}
	ch := b.applied[0]
	// Only the drifted settings are written.
	if *ch.MaxVFs != 64 || ch.SRIOVEnabled != nil || ch.LinkType != "" ||
		len(ch.NVConfig) != 1 || ch.NVConfig[0].Value != "1" {
		t.Fatalf("change %+v", ch)
	}
}

func TestChangeForCarriesSRIOVEnable(t *testing.T) {
	// A VF limit without sriovEnabled in the profile turns SR-IOV on, like
	// mstconfig SRIOV_EN=1 NUM_OF_VFS=n of setup-mellanox.
	ch := changeFor(&FirmwareSettings{MaxVFs: ptr(16)}, []string{SettingMaxVFs})
	if ch.SRIOVEnabled == nil || !*ch.SRIOVEnabled || *ch.MaxVFs != 16 {
		t.Fatalf("change %+v", ch)
	}
	if ch := changeFor(&FirmwareSettings{MaxVFs: ptr(0)}, []string{SettingMaxVFs}); ch.SRIOVEnabled != nil {
		t.Fatalf("maxVFs 0 must not turn SR-IOV on: %+v", ch)
	}
	all := changeFor(&FirmwareSettings{LinkType: "ib", FWLLDPAgent: ptr(true), SRIOVEnabled: ptr(false),
		NVConfig: []Setting{{"A", "1"}, {"B", "2"}}, DevlinkParams: []Setting{{"enable_sriov", "true"}}},
		[]string{SettingLinkType, SettingFWLLDPAgent, SettingSRIOVEnabled, "nvConfig.B", "devlinkParams.enable_sriov"})
	if all.LinkType != "ib" || !*all.FWLLDPAgent || *all.SRIOVEnabled || len(all.NVConfig) != 1 ||
		all.NVConfig[0].Name != "B" || len(all.DevlinkParams) != 1 {
		t.Fatalf("change %+v", all)
	}
}

func TestReconcileApplyFailures(t *testing.T) {
	pf := cx6()
	pf.TotalVFs = 8
	c := &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftApply}

	b := compliantBackend()
	b.stored.MaxVFs = ptr(8)
	b.applyErr = errors.New("mstconfig: device busy")
	if _, _, err := runReconcile(t, c, []PF{pf}, b); err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("apply error: %v", err)
	}

	// No backend: drift of a kernel-visible value cannot be applied.
	res, _, err := runReconcile(t, c, []PF{pf})
	if err == nil || !strings.Contains(err.Error(), `no firmware backend for driver "mlx5_core"`) || res.Reboot {
		t.Fatalf("no backend: %v", err)
	}

	// The tool class excludes the backend.
	k := &Config{Profiles: c.Profiles, OnDrift: OnDriftApply, NICTools: ToolsKernel}
	if _, _, err := runReconcile(t, k, []PF{pf}, compliantBackend()); err == nil || !strings.Contains(err.Error(), "needs NIC_TOOLS=opensource (now kernel)") {
		t.Fatalf("tool class: %v", err)
	}

	// A read error leaves only the kernel view.
	r := compliantBackend()
	r.readErr = errors.New("no mst device")
	res, _, err = runReconcile(t, c, []PF{pf}, r)
	if err == nil || !strings.Contains(err.Error(), "mstconfig read: no mst device") || statuses(res)["firmware.nvConfig.CQE_COMPRESSION"] != StatusUnverified {
		t.Fatalf("read error: %v %v", err, statuses(res))
	}
}

func TestReconcilePendingResetIsNotApplied(t *testing.T) {
	// Stored 64 but 8 active: the reset is pending. Writing again does not
	// help, so apply fails instead of rebooting in a loop.
	pf := cx6()
	pf.TotalVFs = 8
	b := compliantBackend()
	c := &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftApply}
	res, _, err := runReconcile(t, c, []PF{pf}, b)
	if err == nil || statuses(res)["firmware.maxVFs"] != StatusPendingReset || len(b.applied) != 0 || res.Reboot {
		t.Fatalf("pending reset: %v %v applied %d", err, statuses(res), len(b.applied))
	}
}

func TestReconcileUnsupportedSettingIsUnverified(t *testing.T) {
	p := Profile{Name: "e810", Match: Match{Driver: "ice"}, Firmware: FirmwareSettings{FWLLDPAgent: ptr(false)}}
	b := &fakeBackend{name: "intel", tool: ToolsKernel, driver: "ice",
		unsupported: []Unsupported{{SettingFWLLDPAgent, "ice firmware without the fw-lldp-agent flag"}}}
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{{Interface: "ens2f0", Driver: "ice"}}, b)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Findings {
		if f.Setting == SettingFWLLDPAgent && (f.Status != StatusUnverified || f.Detail != "ice firmware without the fw-lldp-agent flag") {
			t.Fatalf("finding %s", f.String())
		}
	}
}

func TestReconcileMatchFindings(t *testing.T) {
	missing := Profile{Name: "e810", Match: Match{Driver: "ice"}, Firmware: FirmwareSettings{MaxVFs: ptr(64)}}
	optional := Profile{Name: "bnxt", Match: Match{Driver: "bnxt_en"}, Optional: true}
	c := &Config{Profiles: []Profile{missing, optional}, OnDrift: OnDriftFail}
	res, _, err := runReconcile(t, c, []PF{cx6()})
	if err == nil || !strings.Contains(err.Error(), "e810/ match.pfPorts: drift") || strings.Contains(err.Error(), "bnxt") {
		t.Fatalf("match: %v", err)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("findings %+v", res.Findings)
	}
}

func TestReconcileRuntimeIsUnverified(t *testing.T) {
	p := Profile{Name: "rt", Match: Match{Driver: "mlx5_core"}, Runtime: RuntimeSettings{NumVFs: ptr(4)}}
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{p}, OnDrift: OnDriftFail}, []PF{cx6()})
	if err == nil || statuses(res)["runtime.runtime"] != StatusUnverified {
		t.Fatalf("runtime: %v %v", err, statuses(res))
	}
}

// TestFirmwareDriftMutations starts from a compliant port and changes one
// observable value per case. Every change must turn exactly the matching
// setting from ok into the wanted status.
func TestFirmwareDriftMutations(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(pf *PF, b *fakeBackend)
		setting string
		status  string
	}{
		{"stored SR-IOV off", func(_ *PF, b *fakeBackend) { b.stored.SRIOVEnabled = ptr(false) }, SettingSRIOVEnabled, StatusDrift},
		{"active SR-IOV off", func(pf *PF, _ *fakeBackend) { pf.SRIOVCapable = false }, SettingSRIOVEnabled, StatusPendingReset},
		{"stored VFs", func(_ *PF, b *fakeBackend) { b.stored.MaxVFs = ptr(16) }, SettingMaxVFs, StatusDrift},
		{"active VFs", func(pf *PF, _ *fakeBackend) { pf.TotalVFs = 16 }, SettingMaxVFs, StatusPendingReset},
		{"stored link type", func(_ *PF, b *fakeBackend) { b.stored.LinkType = "ib" }, SettingLinkType, StatusDrift},
		{"active link type", func(pf *PF, _ *fakeBackend) { pf.LinkType = "ib" }, SettingLinkType, StatusPendingReset},
		{"nv value", func(_ *PF, b *fakeBackend) { b.stored.NVConfig = []Setting{{"CQE_COMPRESSION", "0"}} }, "nvConfig.CQE_COMPRESSION", StatusDrift},
		{"nv missing", func(_ *PF, b *fakeBackend) { b.stored.NVConfig = nil }, "nvConfig.CQE_COMPRESSION", StatusUnverified},
		{"no backend value", func(_ *PF, b *fakeBackend) { b.stored.MaxVFs = nil }, SettingMaxVFs, StatusOK},
		{"no backend value, active differs", func(pf *PF, b *fakeBackend) { b.stored.MaxVFs = nil; pf.TotalVFs = 0 }, SettingMaxVFs, StatusDrift},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf, b := cx6(), compliantBackend()
			tc.mutate(&pf, b)
			res, _, _ := runReconcile(t, &Config{Profiles: []Profile{cx6Profile()}}, []PF{pf}, b)
			for _, f := range res.Findings {
				want := StatusOK
				if f.Setting == tc.setting {
					want = tc.status
				}
				if f.Status != want {
					t.Errorf("%s = %s, want %s", f.Setting, f.Status, want)
				}
			}
		})
	}
}

func TestFindingString(t *testing.T) {
	f := Finding{Profile: "p", Interface: "i", Level: "firmware", Setting: "maxVFs", Want: "64", Got: "stored=8", Status: StatusDrift, Detail: "d"}
	if got := f.String(); got != "p/i firmware.maxVFs: drift (want 64, got stored=8): d" {
		t.Fatal(got)
	}
	if got := (&Finding{Profile: "p", Level: "match", Setting: "x", Status: StatusOK}).String(); got != "p/ match.x: ok" {
		t.Fatal(got)
	}
	if gotString(firmwareItem{}) != "unknown" {
		t.Fatal("empty got")
	}
}

func TestBackendForOrder(t *testing.T) {
	pf := &PF{Driver: "mlx5_core"}
	vendor := &fakeBackend{name: "mlxconfig", tool: ToolsVendor, driver: "mlx5_core"}
	open := &fakeBackend{name: "mstconfig", tool: ToolsOpenSource, driver: "mlx5_core"}
	if b, _ := backendFor([]FirmwareBackend{vendor, open}, pf, ToolsOpenSource); b != open {
		t.Fatalf("got %v, want mstconfig", b)
	}
	if b, _ := backendFor([]FirmwareBackend{vendor, open}, pf, ToolsVendor); b != vendor {
		t.Fatalf("got %v, want mlxconfig", b)
	}
	if toolRank("bogus") <= toolRank(ToolsVendor) {
		t.Fatal("unknown tool class must rank above vendor")
	}
}
