package sriov

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The other tests must not depend on the Secure Boot state of the test
// host: new backends read a lockdown file that does not exist.
func init() { lockdownFile = filepath.Join(os.TempDir(), "booty-sriov-no-lockdown") }

func lockdownFixture(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "lockdown")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestKernelLockdown(t *testing.T) {
	for content, want := range map[string]string{
		"[none] integrity confidentiality\n": "",
		"none [integrity] confidentiality\n": "integrity",
		"none integrity [confidentiality]\n": "confidentiality",
		"":                                   "",
	} {
		if got := kernelLockdown(lockdownFixture(t, content)); got != want {
			t.Errorf("%q: %q, want %q", content, got, want)
		}
	}
	if kernelLockdown(filepath.Join(t.TempDir(), "missing")) != "" {
		t.Fatal("a missing file is no lockdown")
	}
}

// TestMstconfigUnderLockdown: Secure Boot locks the kernel down, so
// mstconfig cannot write (and usually not read) the NV config. BOOTy
// reports the coded reason instead of a raw EPERM and runs no set.
func TestMstconfigUnderLockdown(t *testing.T) {
	q := queryCmd("mstconfig", "0000:3b:00.0")
	cmd := &fakeCmd{
		out: map[string]string{q: "-E- Failed to open the device: Operation not permitted\n"},
		err: map[string]error{q: errors.New("exit status 3")},
	}
	m := NewMstconfig(cmd)
	m.lockdownPath = lockdownFixture(t, "none [integrity] confidentiality\n")
	pf := mlxPF("0000:3b:00.0", "p0")
	want := &FirmwareSettings{SRIOVEnabled: ptr(true), MaxVFs: ptr(64), NVConfig: []Setting{{"CQE_COMPRESSION", "1"}},
		DevlinkParams: []Setting{{"x", "1"}}}
	r, err := m.Read(context.Background(), pf, want)
	if err != nil || len(r.Unsupported) != 3 {
		t.Fatalf("read %+v %v", r, err)
	}
	for _, u := range r.Unsupported {
		if !strings.Contains(u.Reason, "kernel lockdown integrity: nic-firmware-lockdown") {
			t.Fatalf("reason %q", u.Reason)
		}
	}
	if _, err := m.Apply(context.Background(), pf, &FirmwareSettings{MaxVFs: ptr(64)}); !errors.Is(err, ErrFirmwareLockdown) {
		t.Fatalf("apply: %v", err)
	}
	for _, c := range cmd.calls {
		if strings.Contains(c, " set ") {
			t.Fatalf("mstconfig set ran under lockdown: %v", cmd.calls)
		}
	}

	// Without the lockdown file, an EPERM from the tool gets the same code.
	m.lockdownPath = filepath.Join(t.TempDir(), "missing")
	if r, err := m.Read(context.Background(), pf, want); err != nil || len(r.Unsupported) != 3 ||
		strings.Contains(r.Unsupported[0].Reason, "kernel lockdown integrity") {
		t.Fatalf("EPERM read %+v %v", r, err)
	}
	set := "mstconfig -y -d 0000:3b:00.0 set NUM_OF_VFS=64"
	cmd.out[set], cmd.err[set] = "-E- Operation not permitted\n", errors.New("exit status 1")
	if _, err := m.Apply(context.Background(), pf, &FirmwareSettings{MaxVFs: ptr(64)}); !errors.Is(err, ErrFirmwareLockdown) {
		t.Fatalf("EPERM apply: %v", err)
	}
}

// TestReconcileUnderLockdown: with a VF limit drift on a ConnectX under
// Secure Boot, the finding names the coded reason, nothing is written and
// the host does not reboot. The kernel still shows the active value, so a
// compliant limit stays ok.
func TestReconcileUnderLockdown(t *testing.T) {
	q := queryCmd("mstconfig", "0000:3b:00.0")
	cmd := &fakeCmd{out: map[string]string{q: "-E- Operation not permitted\n"}, err: map[string]error{q: errors.New("exit status 3")}}
	m := NewMstconfig(cmd)
	m.lockdownPath = lockdownFixture(t, "none [integrity] confidentiality\n")
	pf := *mlxPF("0000:3b:00.0", "p0")
	pf.SRIOVCapable, pf.TotalVFs = true, 8
	p := Profile{Name: "cx6", Match: Match{VendorID: "15b3"}, OnDrift: OnDriftApply, Firmware: FirmwareSettings{MaxVFs: ptr(64)}}
	res, _, err := runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{pf}, m)
	if err == nil || !strings.Contains(err.Error(), "nic-firmware-lockdown: NIC firmware change needs Secure Boot off or an out-of-band path") ||
		res.Reboot {
		t.Fatalf("%v reboot %v", err, res.Reboot)
	}
	pf.TotalVFs = 64
	if res, _, err := runReconcile(t, &Config{Profiles: []Profile{p}}, []PF{pf}, m); err != nil || statuses(res)["firmware.maxVFs"] != StatusOK {
		t.Fatalf("compliant under lockdown: %v %v", err, statuses(res))
	}
}

func TestPermissionDenied(t *testing.T) {
	if !permissionDenied(nil, syscall.EPERM) || !permissionDenied([]byte("-E- Operation not permitted"), errors.New("x")) ||
		permissionDenied([]byte("-E- no device"), errors.New("x")) {
		t.Fatal("EPERM")
	}
	// EACCES is a file permission problem, not lockdown.
	if permissionDenied([]byte("open /dev/mst: Permission denied"), syscall.EACCES) {
		t.Fatal("EACCES reported as lockdown")
	}
}
