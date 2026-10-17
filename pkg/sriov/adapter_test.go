package sriov

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dualPort returns both ports of one ConnectX-6 Dx with 8 VFs active.
func dualPort() (p0, p1 PF) {
	p0, p1 = cx6(), cx6()
	p0.Bus, p0.PCIAddress, p0.PhysPortName = "pci", "0000:3b:00.0", "p0"
	p1.Bus, p1.PCIAddress, p1.PhysPortName, p1.Interface = "pci", "0000:3b:00.1", "p1", "ens1f1np1"
	p0.TotalVFs, p1.TotalVFs = 8, 8
	return p0, p1
}

// TestFirmwareAdapterAppliedOnce is review finding 1: SRIOV_EN and
// NUM_OF_VFS belong to the whole adapter. Port 0 stores them; port 1 then
// reads the stored value with the old active one and must report applied,
// not pending-reset. A second run in the same boot (rescue retry) reads
// the journal and reports applied again, and reboots.
func TestFirmwareAdapterAppliedOnce(t *testing.T) {
	b := compliantBackend()
	b.stored.MaxVFs = ptr(8)
	p0, p1 := dualPort()
	journalPath := filepath.Join(t.TempDir(), "journal.json")
	c := &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftApply}

	res, _, err := runReconcileJournal(t, c, []PF{p0, p1}, &Journal{Path: journalPath}, b)
	if err != nil || !res.Reboot {
		t.Fatalf("first run: %v reboot %v\n%v", err, res.Reboot, res.Findings)
	}
	if len(b.applied) != 1 {
		t.Fatalf("the adapter must be written once, got %d sets", len(b.applied))
	}
	for _, f := range res.Findings {
		if f.Setting == SettingMaxVFs && f.Status != StatusApplied {
			t.Fatalf("%s", f.String())
		}
	}

	// Same boot, new reconciler (new process state, same ramdisk file).
	res, _, err = runReconcileJournal(t, c, []PF{p0}, &Journal{Path: journalPath}, b)
	if err != nil || !res.Reboot || len(b.applied) != 1 {
		t.Fatalf("rerun: %v reboot %v sets %d\n%v", err, res.Reboot, len(b.applied), res.Findings)
	}
	for _, f := range res.Findings {
		if f.Setting == SettingMaxVFs && (f.Status != StatusApplied || f.Detail != detailStoredThisBoot) {
			t.Fatalf("rerun %s", f.String())
		}
	}

	// After the reboot the journal is gone: a stored value that is still
	// not active is a pending reset again.
	res, _, err = runReconcileJournal(t, c, []PF{p0}, &Journal{Path: filepath.Join(t.TempDir(), "new.json")}, b)
	if err == nil || res.Reboot || statuses(res)["firmware.maxVFs"] != StatusPendingReset {
		t.Fatalf("after reboot: %v %v", err, statuses(res))
	}
}

// TestFirmwareAdapterConflict is review finding 2: two profiles that want
// different values for an adapter-wide setting on ports of one adapter
// must fail without a write, instead of rebooting back and forth.
func TestFirmwareAdapterConflict(t *testing.T) {
	b := compliantBackend()
	p0, p1 := dualPort()
	a, bb := cx6Profile(), cx6Profile()
	a.Name, a.Match = "port0", Match{Interface: "ens1f0np0"}
	bb.Name, bb.Match = "port1", Match{Interface: "ens1f1np1"}
	bb.Firmware.MaxVFs = ptr(32)
	c := &Config{Profiles: []Profile{a, bb}, OnDrift: OnDriftApply}
	for run := range 3 {
		res, _, err := runReconcile(t, c, []PF{p0, p1}, b)
		if err == nil || !strings.Contains(err.Error(), "the setting belongs to the whole adapter") {
			t.Fatalf("run %d: %v", run, err)
		}
		if res.Reboot || len(b.applied) != 0 {
			t.Fatalf("run %d: reboot %v sets %d", run, res.Reboot, len(b.applied))
		}
	}
	// The link type is per port: different values on the two ports are fine.
	a.Firmware, bb.Firmware = FirmwareSettings{LinkType: "eth"}, FirmwareSettings{LinkType: "ib"}
	if got := adapterConflicts([]Assignment{{Profile: &a, PF: &p0}, {Profile: &bb, PF: &p1}}); len(got) != 0 {
		t.Fatalf("per-port settings conflict: %v", got)
	}
}

// TestFirmwareSetDoesNotPersist is review finding 4: a set that returns no
// error but leaves the old value must fail, not reboot into the same
// drift again.
func TestFirmwareSetDoesNotPersist(t *testing.T) {
	b := compliantBackend()
	b.stored.MaxVFs = ptr(8)
	b.noPersist = true
	pf := cx6()
	pf.TotalVFs = 8
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftApply}, []PF{pf}, b)
	if err == nil || !strings.Contains(err.Error(), `set did not persist: stored "8"`) || res.Reboot {
		t.Fatalf("%v reboot %v", err, res.Reboot)
	}
	b.noPersist, b.readErr = false, nil
	failRead := &readBackFails{fakeBackend: b}
	b.stored.MaxVFs = ptr(8)
	if _, _, err := runReconcile(t, &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftApply}, []PF{pf}, failRead); err == nil ||
		!strings.Contains(err.Error(), "read back") {
		t.Fatalf("read back error: %v", err)
	}
}

// readBackFails reads once, then fails every read.
type readBackFails struct {
	*fakeBackend
	reads int
}

func (r *readBackFails) Read(ctx context.Context, pf *PF, want *FirmwareSettings) (*FirmwareRead, error) {
	r.reads++
	if r.reads > 1 {
		return nil, os.ErrDeadlineExceeded
	}
	return r.fakeBackend.Read(ctx, pf, want)
}

func TestJournal(t *testing.T) {
	var nilJournal *Journal
	if nilJournal.Has("a", "b", "c") || nilJournal.Record("a", "b", "c") != nil {
		t.Fatal("nil journal")
	}
	path := filepath.Join(t.TempDir(), "sub", "j.json")
	j := &Journal{Path: path}
	if err := j.Record("adapter x", SettingMaxVFs, "64"); err != nil {
		t.Fatal(err)
	}
	if err := j.Record("adapter x", SettingMaxVFs, "32"); err != nil {
		t.Fatal(err)
	}
	k := &Journal{Path: path}
	if k.Has("adapter x", SettingMaxVFs, "64") || !k.Has("adapter x", SettingMaxVFs, "32") || k.Has("adapter y", SettingMaxVFs, "32") {
		t.Fatalf("reload: %+v", k.entries)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if (&Journal{Path: path}).Has("adapter x", SettingMaxVFs, "32") {
		t.Fatal("a broken journal must forget")
	}
	mem := &Journal{}
	if mem.Record("s", "x", "1") != nil || !mem.Has("s", "x", "1") {
		t.Fatal("memory journal")
	}
	bad := &Journal{Path: filepath.Join(path, "file-is-a-dir")}
	if err := os.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if bad.Record("s", "x", "1") == nil {
		t.Fatal("a journal below a file must fail to write")
	}
}

func TestScopeKey(t *testing.T) {
	p0, p1 := dualPort()
	if scopeKey(&p0, SettingMaxVFs) != scopeKey(&p1, SettingMaxVFs) || scopeKey(&p0, "nvConfig.X") != "adapter 0000:3b:00" {
		t.Fatal("adapter scope")
	}
	if scopeKey(&p0, SettingLinkType) == scopeKey(&p1, SettingLinkType) {
		t.Fatal("link type is per port")
	}
	p0.PhysPortName = ""
	if scopeKey(&p0, SettingFWLLDPAgent) != "port pci/0000:3b:00.0/"+p0.MAC {
		t.Fatalf("MAC fallback %q", scopeKey(&p0, SettingFWLLDPAgent))
	}
}

// TestFirmwareAdapterWithoutJournalFile keeps the adapter memory within one
// run when the reconciler has no journal file.
func TestFirmwareAdapterWithoutJournalFile(t *testing.T) {
	b := compliantBackend()
	b.stored.MaxVFs = ptr(8)
	p0, p1 := dualPort()
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{cx6Profile()}, OnDrift: OnDriftApply}, []PF{p0, p1}, b)
	if err != nil || !res.Reboot || len(b.applied) != 1 {
		t.Fatalf("%v reboot %v sets %d", err, res.Reboot, len(b.applied))
	}
}
