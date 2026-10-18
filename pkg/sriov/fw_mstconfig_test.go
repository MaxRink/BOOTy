package sriov

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures in testdata/mstconfig follow the output of mstconfig 4.31
// (mstflint in Debian trixie): the "Configurations:" table with Default,
// Current and Next Boot columns for -e, only Next Boot without it, a '*'
// before a changed parameter and the closing note. The column layout comes
// from the format string in the mstconfig binary; the values are from
// vendor documentation. Add lab captures of real adapters next to them.

// fakeCmd answers commands from a table and records the calls.
type fakeCmd struct {
	out   map[string]string // joined command: output
	err   map[string]error
	calls []string
}

func (f *fakeCmd) CombinedOutput(_ context.Context, name string, args ...string) ([]byte, error) {
	c := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, c)
	if f.err[c] == nil && len(args) > 4 && args[0] == "-y" && args[3] == "set" {
		f.storeNV(name, args[2], args[4:])
	}
	return []byte(f.out[c]), f.err[c]
}

// storeNV writes "mstconfig set" values into the Next Boot column of the
// query output of the device, like the NIC stores them.
func (f *fakeCmd) storeNV(bin, addr string, sets []string) {
	q := bin + " -e -d " + addr + " query"
	out, ok := f.out[q]
	if !ok {
		return
	}
	lines := strings.Split(out, "\n")
	for _, kv := range sets {
		k, v, _ := strings.Cut(kv, "=")
		for i, l := range lines {
			fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "*"))
			if len(fields) >= 2 && fields[0] == k {
				fields[len(fields)-1] = v
				lines[i] = "*       " + strings.Join(fields, "    ")
			}
		}
	}
	f.out[q] = strings.Join(lines, "\n")
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "mstconfig", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mlxPF(addr, port string) *PF {
	return &PF{Interface: "ens1f0np0", Bus: "pci", PCIAddress: addr, VendorID: "15b3", Driver: "mlx5_core", PhysPortName: port}
}

func queryCmd(bin, addr string) string { return bin + " -e -d " + addr + " query" }

func TestParseMstconfigQuery(t *testing.T) {
	params := parseMstconfigQuery(fixture(t, "cx6dx-e.txt"))
	if len(params) != 17 {
		t.Fatalf("got %d parameters: %+v", len(params), params)
	}
	vfs := findNV(params, "NUM_OF_VFS")
	if vfs == nil || vfs.def != "8" || vfs.current != "32" || vfs.next != "64" {
		t.Fatalf("NUM_OF_VFS = %+v", vfs)
	}
	if p := findNV(params, "ESWITCH_HAIRPIN_DESCRIPTORS[0]"); p == nil || p.next != "128" {
		t.Fatalf("array parameter = %+v", p)
	}
	q := parseMstconfigQuery(fixture(t, "cx5-vpi-q.txt"))
	if p := findNV(q, "LINK_TYPE_P1"); len(q) != 5 || p == nil || p.next != "IB(1)" || p.current != "" {
		t.Fatalf("query without -e: %+v", q)
	}
	if len(parseMstconfigQuery(fixture(t, "open-error.txt"))) != 0 || len(parseMstconfigQuery("SRIOV_EN True(1)\n")) != 0 {
		t.Fatal("lines outside the table must not parse")
	}
}

func TestNVValues(t *testing.T) {
	for _, c := range []struct{ raw, label, num string }{{"True(1)", "True", "1"}, {"ETH(2)", "ETH", "2"}, {"64", "64", "64"}, {"_256KB(1)", "_256KB", "1"}} {
		if l, n := nvValue(c.raw); l != c.label || n != c.num {
			t.Errorf("nvValue(%q) = %q %q", c.raw, l, n)
		}
	}
	if !nvMatches("aggressive", "AGGRESSIVE(1)") || !nvMatches("1", "AGGRESSIVE(1)") || nvMatches("0", "AGGRESSIVE(1)") {
		t.Fatal("nvMatches")
	}
	if nvBool("True(1)") != "true" || nvBool("False(0)") != "false" || nvBool("1") != "true" || nvBool("") != "" {
		t.Fatal("nvBool")
	}
	if nvLinkType("IB(1)") != "ib" || nvLinkType("ETH(2)") != "eth" || nvLinkType("VPI(3)") != "vpi" || nvLinkType("X") != "X" || nvLinkType("") != "" {
		t.Fatal("nvLinkType")
	}
	for pf, want := range map[*PF]int{
		{PhysPortName: "p1"}: 2, {PhysPortName: "p0", PCIAddress: "0000:3b:00.1"}: 1,
		{PCIAddress: "0000:3b:00.1"}: 2, {PCIAddress: "netdevsim1"}: 1, {}: 1,
	} {
		if got := portNumber(pf); got != want {
			t.Errorf("portNumber(%+v) = %d, want %d", pf, got, want)
		}
	}
}

func TestMstconfigRead(t *testing.T) {
	cmd := &fakeCmd{out: map[string]string{queryCmd("mstconfig", "0000:3b:00.0"): fixture(t, "cx6dx-e.txt")}}
	m := NewMstconfig(cmd)
	want := &FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(64), LinkType: "eth", FWLLDPAgent: ptr(false),
		NVConfig:      []Setting{{"CQE_COMPRESSION", "aggressive"}, {"NUM_VF_MSIX", "11"}, {"NO_SUCH_PARAM", "1"}},
		DevlinkParams: []Setting{{"enable_sriov", "true"}}}
	r, err := m.Read(context.Background(), mlxPF("0000:3b:00.0", "p0"), want)
	if err != nil {
		t.Fatal(err)
	}
	v, a := r.Values, r.Active
	if !*v.SRIOVEnabled || *v.MaxVFs != 64 || *a.MaxVFs != 32 || v.LinkType != "eth" || a.LinkType != "eth" {
		t.Fatalf("values %+v active %+v", v, a)
	}
	if settingValue(v.NVConfig, "CQE_COMPRESSION") != "aggressive" || settingValue(a.NVConfig, "CQE_COMPRESSION") != "BALANCED(0)" ||
		settingValue(v.NVConfig, "NUM_VF_MSIX") != "11" {
		t.Fatalf("nv %+v active %+v", v.NVConfig, a.NVConfig)
	}
	unsupported := make([]string, 0, len(r.Unsupported))
	for _, u := range r.Unsupported {
		unsupported = append(unsupported, u.Setting)
	}
	if got := strings.Join(unsupported, ","); got != "fwLLDPAgent,nvConfig.NO_SUCH_PARAM,devlinkParams.enable_sriov" {
		t.Fatalf("unsupported %s", got)
	}
	if m.Name() != "mstconfig" || m.Tool() != ToolsOpenSource || NewMlxconfig(cmd).Tool() != ToolsVendor || NewMlxconfig(cmd).Name() != "mlxconfig" {
		t.Fatal("names")
	}
}

func TestMstconfigReadPortsAndCards(t *testing.T) {
	cmd := &fakeCmd{out: map[string]string{
		queryCmd("mstconfig", "0000:af:00.1"): fixture(t, "cx5-vpi-q.txt"),
		queryCmd("mstconfig", "0000:5e:00.0"): fixture(t, "cx4lx-eth-e.txt"),
		queryCmd("mstconfig", "0000:03:00.0"): fixture(t, "bf3-nic-mode-e.txt"),
	}}
	m := NewMstconfig(cmd)
	ctx := context.Background()
	lt := &FirmwareSettings{LinkType: "eth"}
	// Port 2 of a VPI card is Ethernet, port 1 InfiniBand.
	if r, _ := m.Read(ctx, mlxPF("0000:af:00.1", ""), lt); r.Values.LinkType != "eth" || r.Active.LinkType != "" {
		t.Fatalf("port 2: %+v", r)
	}
	if r, _ := m.Read(ctx, mlxPF("0000:af:00.1", "p0"), lt); r.Values.LinkType != "ib" {
		t.Fatalf("port 1: %+v", r)
	}
	// An Ethernet-only card has no LINK_TYPE parameter.
	if r, _ := m.Read(ctx, mlxPF("0000:5e:00.0", ""), lt); len(r.Unsupported) != 1 || r.Unsupported[0].Setting != SettingLinkType {
		t.Fatalf("cx4 lx: %+v", r)
	}
	// BlueField-3 in NIC mode: the DPU mode parameter is plain NV config.
	r, err := m.Read(ctx, mlxPF("0000:03:00.0", "p0"), &FirmwareSettings{NVConfig: []Setting{{"INTERNAL_CPU_OFFLOAD_ENGINE", "DISABLED"}}})
	if err != nil || settingValue(r.Values.NVConfig, "INTERNAL_CPU_OFFLOAD_ENGINE") != "DISABLED" ||
		settingValue(r.Active.NVConfig, "INTERNAL_CPU_OFFLOAD_ENGINE") != "DISABLED" {
		t.Fatalf("bf3: %+v %v", r, err)
	}
}

func TestMstconfigReadErrors(t *testing.T) {
	cmd := &fakeCmd{
		out: map[string]string{queryCmd("mstconfig", "0000:3b:00.0"): fixture(t, "open-error.txt"), queryCmd("mstconfig", "0000:3b:00.1"): "Device #1:\n"},
		err: map[string]error{queryCmd("mstconfig", "0000:3b:00.0"): errors.New("exit status 3")},
	}
	m := NewMstconfig(cmd)
	if _, err := m.Read(context.Background(), mlxPF("0000:3b:00.0", ""), &FirmwareSettings{}); err == nil ||
		!strings.Contains(err.Error(), "-E- Failed to open the device") {
		t.Fatalf("open error: %v", err)
	}
	if _, err := m.Read(context.Background(), mlxPF("0000:3b:00.1", ""), &FirmwareSettings{}); err == nil ||
		!strings.Contains(err.Error(), "no configuration table") {
		t.Fatalf("empty table: %v", err)
	}
}

func TestMstconfigSupports(t *testing.T) {
	m := NewMstconfig(&fakeCmd{})
	for pf, want := range map[*PF]bool{
		mlxPF("0000:3b:00.0", ""):                                 true,
		{Bus: "pci", VendorID: "15b3", Driver: "mlx4_core"}:       true,
		{Bus: "pci", VendorID: "8086", Driver: "ice"}:             false,
		{Bus: "netdevsim", VendorID: "15b3", Driver: "mlx5_core"}: false,
		{Bus: "pci", VendorID: "15b3", Driver: "vfio-pci"}:        false,
	} {
		if m.Supports(pf) != want {
			t.Errorf("Supports(%+v) != %v", pf, want)
		}
	}
}

func TestMstconfigApply(t *testing.T) {
	cmd := &fakeCmd{}
	m := NewMstconfig(cmd)
	reboot, err := m.Apply(context.Background(), mlxPF("0000:3b:00.1", ""), &FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(64),
		LinkType: "eth", NVConfig: []Setting{{"CQE_COMPRESSION", "1"}}})
	if err != nil || !reboot {
		t.Fatalf("apply %v %v", reboot, err)
	}
	if want := "mstconfig -y -d 0000:3b:00.1 set SRIOV_EN=True NUM_OF_VFS=64 LINK_TYPE_P2=ETH CQE_COMPRESSION=1"; cmd.calls[0] != want {
		t.Fatalf("call %q\nwant %q", cmd.calls[0], want)
	}
	if reboot, err := m.Apply(context.Background(), mlxPF("0000:3b:00.1", ""), &FirmwareSettings{}); reboot || err != nil || len(cmd.calls) != 1 {
		t.Fatal("an empty change must not run mstconfig")
	}
	set := "mstconfig -y -d 0000:3b:00.0 set SRIOV_EN=False"
	cmd.err = map[string]error{set: errors.New("exit status 1")}
	cmd.out = map[string]string{set: "-E- The Device doesn't support SRIOV_EN parameter\n"}
	if _, err := m.Apply(context.Background(), mlxPF("0000:3b:00.0", ""), &FirmwareSettings{SRIOVEnabled: ptr(false)}); err == nil ||
		!strings.Contains(err.Error(), "doesn't support SRIOV_EN") {
		t.Fatalf("apply error: %v", err)
	}
}

// TestMstconfigReconcile runs the step with the backend: drift in the
// stored values goes into one mstconfig set and asks for a reboot.
func TestMstconfigReconcile(t *testing.T) {
	cmd := &fakeCmd{out: map[string]string{queryCmd("mstconfig", "0000:3b:00.0"): fixture(t, "cx6dx-e.txt")}}
	p := Profile{Name: "cx6", Match: Match{VendorID: "15b3"}, OnDrift: OnDriftApply,
		Firmware: FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(128), NVConfig: []Setting{{"CQE_COMPRESSION", "BALANCED"}}}}
	pf := *mlxPF("0000:3b:00.0", "p0")
	pf.SRIOVCapable, pf.TotalVFs, pf.LinkType = true, 64, "eth"
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{pf}, NewMstconfig(cmd))
	if err != nil || !res.Reboot {
		t.Fatalf("reconcile %v reboot %v", err, res.Reboot)
	}
	// query, one set, and the read back.
	want := []string{queryCmd("mstconfig", "0000:3b:00.0"), "mstconfig -y -d 0000:3b:00.0 set NUM_OF_VFS=128 CQE_COMPRESSION=BALANCED",
		queryCmd("mstconfig", "0000:3b:00.0")}
	if strings.Join(cmd.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q", cmd.calls)
	}
}

// TestMstconfigDualPortWritesOnce is review findings 1 and 4 with the real
// backend: both ports of a ConnectX-6 Dx share the NV configuration. Port
// 0 writes it and reads it back; port 1 reads the stored value and is
// applied through the journal, without a second set.
func TestMstconfigDualPortWritesOnce(t *testing.T) {
	out := fixture(t, "cx6dx-e.txt")
	cmd := &fakeCmd{out: map[string]string{queryCmd("mstconfig", "0000:3b:00.0"): out, queryCmd("mstconfig", "0000:3b:00.1"): out}}
	// Both functions show the same NV configuration.
	shared := &sharedNV{fakeCmd: cmd}
	p := Profile{Name: "cx6", Match: Match{VendorID: "15b3"}, OnDrift: OnDriftApply,
		Firmware: FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(128)}}
	p0, p1 := *mlxPF("0000:3b:00.0", "p0"), *mlxPF("0000:3b:00.1", "p1")
	p1.Interface = "ens1f1np1"
	for _, pf := range []*PF{&p0, &p1} {
		pf.SRIOVCapable, pf.TotalVFs, pf.LinkType = true, 64, "eth"
	}
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{p0, p1}, NewMstconfig(shared))
	if err != nil || !res.Reboot {
		t.Fatalf("reconcile %v reboot %v\n%+v", err, res.Reboot, res.Findings)
	}
	sets := 0
	for _, c := range cmd.calls {
		if strings.Contains(c, " set ") {
			sets++
		}
	}
	if sets != 1 {
		t.Fatalf("NV sets = %d, want 1: %q", sets, cmd.calls)
	}
}

// sharedNV mirrors a set on one PCI function to the query of the other.
type sharedNV struct{ *fakeCmd }

func (s *sharedNV) CombinedOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := s.fakeCmd.CombinedOutput(ctx, name, args...)
	if len(args) > 3 && args[3] == "set" {
		q := s.out[queryCmd(name, args[2])]
		for k := range s.out {
			if strings.HasSuffix(k, " query") {
				s.out[k] = q
			}
		}
	}
	return out, err
}

// TestMstconfigFixtureMutations removes or changes one parameter line of
// the ConnectX-6 Dx fixture per case: exactly that setting must leave ok.
func TestMstconfigFixtureMutations(t *testing.T) {
	base := fixture(t, "cx6dx-e.txt")
	p := Profile{Name: "cx6", Match: Match{VendorID: "15b3"},
		Firmware: FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(64), LinkType: "eth", NVConfig: []Setting{{"CQE_COMPRESSION", "AGGRESSIVE"}}}}
	pf := *mlxPF("0000:3b:00.0", "p0")
	pf.SRIOVCapable, pf.TotalVFs, pf.LinkType = true, 64, "eth"
	cases := []struct {
		name, old, new, setting, status string
	}{
		{"SRIOV_EN off", "True(1)             True(1)  ", "True(1)             False(0) ", SettingSRIOVEnabled, StatusDrift},
		{"SRIOV_EN missing", "*       SRIOV_EN", "", SettingSRIOVEnabled, StatusOK}, // the kernel still shows it
		{"NUM_OF_VFS", "32                  64 ", "32                  16 ", SettingMaxVFs, StatusDrift},
		{"LINK_TYPE_P1", "LINK_TYPE_P1                                ETH(2)              ETH(2)              ETH(2)",
			"LINK_TYPE_P1                                ETH(2)              ETH(2)              IB(1)", SettingLinkType, StatusDrift},
		{"CQE value", "BALANCED(0)         AGGRESSIVE(1)", "BALANCED(0)         BALANCED(0)  ", "nvConfig.CQE_COMPRESSION", StatusDrift},
		{"CQE current", "BALANCED(0)         BALANCED(0)         AGGRESSIVE(1)", "BALANCED(0)         AGGRESSIVE(1)       AGGRESSIVE(1)",
			"nvConfig.CQE_COMPRESSION", StatusOK},
		{"CQE missing", "*       CQE_COMPRESSION", "", "nvConfig.CQE_COMPRESSION", StatusUnverified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out string
			if tc.new == "" {
				out = dropLine(t, base, tc.old)
			} else {
				if !strings.Contains(base, tc.old) {
					t.Fatalf("fixture has no %q", tc.old)
				}
				out = strings.Replace(base, tc.old, tc.new, 1)
			}
			cmd := &fakeCmd{out: map[string]string{queryCmd("mstconfig", "0000:3b:00.0"): out}}
			res, _, _ := runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{pf}, NewMstconfig(cmd))
			for _, f := range res.Findings {
				want := StatusOK
				if f.Setting == tc.setting {
					want = tc.status
				}
				// The unchanged fixture has CQE stored AGGRESSIVE but
				// active BALANCED: a pending reset.
				if f.Setting == "nvConfig.CQE_COMPRESSION" && want == StatusOK && tc.name != "CQE current" {
					want = StatusPendingReset
				}
				if f.Status != want {
					t.Errorf("%s = %s, want %s", f.Setting, f.Status, want)
				}
			}
		})
	}
}

func dropLine(t *testing.T, text, prefix string) string {
	t.Helper()
	var out []string
	found := false
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			found = true
			continue
		}
		out = append(out, l)
	}
	if !found {
		t.Fatalf("fixture has no line %q", prefix)
	}
	return strings.Join(out, "\n")
}
