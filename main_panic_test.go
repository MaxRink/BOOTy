//go:build linux

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var regexpPanic = regexp.MustCompile(`(?m)^cmdline="[^"]*\bpanic=` + panicRebootSeconds + `\b`)

func TestSetPanicReboot(t *testing.T) {
	origCmdline, origPanic, origPid := procCmdlinePath, kernelPanicPath, panicGetpid
	t.Cleanup(func() { procCmdlinePath, kernelPanicPath, panicGetpid = origCmdline, origPanic, origPid })
	panicGetpid = func() int { return 1 }
	for _, tc := range []struct{ cmdline, want string }{
		{"console=ttyS0,115200n8 --", panicRebootSeconds},
		{"console=ttyS0 panic=0", "unset"},
		{"console=ttyS0 panic_on_warn=1", panicRebootSeconds},
		{"console=ttyS0 -- panic=0", panicRebootSeconds},
	} {
		dir := t.TempDir()
		procCmdlinePath = filepath.Join(dir, "cmdline")
		kernelPanicPath = filepath.Join(dir, "panic")
		if err := os.WriteFile(procCmdlinePath, []byte(tc.cmdline+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(kernelPanicPath, []byte("unset"), 0o600); err != nil {
			t.Fatal(err)
		}
		setPanicReboot()
		got, _ := os.ReadFile(kernelPanicPath)
		if string(got) != tc.want {
			t.Errorf("cmdline %q: kernel.panic = %q, want %q", tc.cmdline, got, tc.want)
		}
	}
}

// The ISOs that BOOTy builds set panic= on the kernel command line, so a
// panic before BOOTy starts reboots too.
func TestMakeISOCmdlineRebootsOnPanic(t *testing.T) {
	b, err := os.ReadFile("hack/make-iso.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !regexpPanic.Match(b) {
		t.Fatal("hack/make-iso.sh: the kernel command line has no panic=" + panicRebootSeconds)
	}
}

// Not PID 1 (the containerlab e2e runs BOOTy under sh): kernel.panic is
// global for the host and stays untouched.
func TestSetPanicRebootOnlyAsPID1(t *testing.T) {
	origCmdline, origPanic, origPid := procCmdlinePath, kernelPanicPath, panicGetpid
	t.Cleanup(func() { procCmdlinePath, kernelPanicPath, panicGetpid = origCmdline, origPanic, origPid })
	panicGetpid = func() int { return 4242 }
	dir := t.TempDir()
	procCmdlinePath = filepath.Join(dir, "cmdline")
	kernelPanicPath = filepath.Join(dir, "panic")
	if err := os.WriteFile(procCmdlinePath, []byte("console=ttyS0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kernelPanicPath, []byte("unset"), 0o600); err != nil {
		t.Fatal(err)
	}
	setPanicReboot()
	if got, _ := os.ReadFile(kernelPanicPath); string(got) != "unset" {
		t.Fatalf("kernel.panic = %q outside PID 1, want it untouched", got)
	}
}

// main sets kernel.panic after the mounts, before the provisioning starts.
func TestMainSetsPanicReboot(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	mounts, panicCall, run := strings.Index(src, "setupMountsAndDevices()"), strings.Index(src, "setPanicReboot()"), strings.Index(src, "runCAPRF(ctx)")
	if panicCall < 0 || mounts < 0 || run < 0 || panicCall < mounts || panicCall > run {
		t.Fatal("main.go: setPanicReboot() must run after setupMountsAndDevices() and before runCAPRF")
	}
}
