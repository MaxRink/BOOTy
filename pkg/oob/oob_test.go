//go:build linux

package oob

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/efi"
)

func TestLine(t *testing.T) {
	got := Line(&Status{Run: "0a1b2c3d", Phase: "network", Code: "BOOTY-NET-002", Step: "network-setup", Result: ResultFailed})
	want := "BOOTY1 run=0a1b2c3d phase=network code=BOOTY-NET-002 step=network-setup result=failed\n"
	if got != want {
		t.Fatalf("Line() = %q, want %q", got, want)
	}
	if got := Line(&Status{Run: "0a1b2c3d", Phase: "provision", Result: ResultRunning}); got !=
		"BOOTY1 run=0a1b2c3d phase=provision code=- step=- result=running\n" {
		t.Fatalf("empty fields: %q", got)
	}
}

// A field can never add a key, a line or a URL to the line.
func TestLineSanitizesAndLimits(t *testing.T) {
	long := strings.Repeat("x", 3*MaxFieldLen)
	got := Line(&Status{Run: long, Phase: long, Code: "a b=c\nBOOTY1 x", Step: "https://u:p@h/x?token=1", Result: long})
	if len(got) > MaxLineLen {
		t.Fatalf("line is %d bytes, limit %d", len(got), MaxLineLen)
	}
	if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("line %q must be one line", got)
	}
	if strings.Count(got, "=") != 5 {
		t.Fatalf("line %q has extra keys", got)
	}
	for _, bad := range []string{":", "/", "@", "?", " BOOTY1 x"} {
		if strings.Contains(got, bad) {
			t.Fatalf("line %q contains %q", got, bad)
		}
	}
	if !strings.Contains(got, "run="+strings.Repeat("x", MaxFieldLen)+" ") {
		t.Fatalf("field not truncated to %d: %q", MaxFieldLen, got)
	}
}

// MaxLineLen is the real bound: five fields of maximum length reach it.
func TestLineMaxLength(t *testing.T) {
	long := strings.Repeat("z", 2*MaxFieldLen)
	got := Line(&Status{Run: long, Phase: long, Code: long, Step: long, Result: long})
	if len(got) != MaxLineLen {
		t.Fatalf("longest line is %d bytes, MaxLineLen %d", len(got), MaxLineLen)
	}
	if r := NewReporter(&Options{Run: "0a1b2c3d"}); r.Run() != "0a1b2c3d" {
		t.Fatalf("Run() = %q, want the given run ID", r.Run())
	}
}

func TestRecordRoundTripAndLimits(t *testing.T) {
	rec := Record{Mode: "provision", Phase: "network", Step: "network-setup", Code: "BOOTY-NET-002", Result: ResultFailed, Booty: "v0.2.0"}
	data, err := EncodeRecord(&rec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRecord(data)
	rec.V = 1
	if err != nil || got != rec {
		t.Fatalf("DecodeRecord() = %+v, %v, want %+v", got, err, rec)
	}

	long := strings.Repeat("y", 10*MaxFieldLen)
	data, err = EncodeRecord(&Record{Mode: long, Phase: long, Step: long, Code: long, Result: long, Booty: "https://u:secret@h/"})
	if err != nil {
		t.Fatalf("EncodeRecord(max fields) = %v", err)
	}
	if len(data) > MaxRecordLen {
		t.Fatalf("record is %d bytes, limit %d", len(data), MaxRecordLen)
	}
	if strings.Contains(string(data), "secret@") || strings.Contains(string(data), "://") {
		t.Fatalf("record %s keeps URL parts", data)
	}
}

func TestDecodeRecordRejects(t *testing.T) {
	for name, data := range map[string]string{
		"garbage":       "\x00\x01",
		"unknown field": `{"v":1,"token":"x"}`,
		"version":       `{"v":2}`,
		"too long":      `{"v":1,"mode":"` + strings.Repeat("a", MaxRecordLen) + `"}`,
	} {
		if _, err := DecodeRecord([]byte(data)); err == nil {
			t.Errorf("%s: DecodeRecord() accepted %q", name, data)
		}
	}
}

func TestActiveConsoles(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "active")
	if err := os.WriteFile(active, []byte("tty0 ttyS1 ../evil\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ActiveConsoles(active), ","); got != "/dev/tty0,/dev/ttyS1" {
		t.Fatalf("ActiveConsoles() = %s", got)
	}
	if got := strings.Join(ActiveConsoles(filepath.Join(dir, "missing")), ","); got != "/dev/console" {
		t.Fatalf("ActiveConsoles(missing) = %s, want /dev/console", got)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestReporterWritesLinesToEveryConsoleAndVarOnChange(t *testing.T) {
	dir := t.TempDir()
	consoles := []string{filepath.Join(dir, "tty0"), filepath.Join(dir, "ttyS0")}
	for _, c := range consoles {
		if err := os.WriteFile(c, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	varsDir := filepath.Join(dir, "efivars")
	if err := os.Mkdir(varsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	vars := efi.NewEFIVarReader(varsDir)
	opts := Options{Serial: true, Consoles: consoles, Vars: vars, Mode: "provision", Version: "v1"}

	r := NewReporter(&opts)
	if len(r.Run()) != 8 {
		t.Fatalf("run ID %q, want 8 characters", r.Run())
	}
	r.Phase("network")
	r.Phase("network") // no change, no line
	r.Fail("network-setup", "BOOTY-NET-002")
	want := "BOOTY1 run=" + r.Run() + " phase=network code=- step=- result=running\n" +
		"BOOTY1 run=" + r.Run() + " phase=network code=BOOTY-NET-002 step=network-setup result=failed\n"
	for _, c := range consoles {
		if got := readFile(t, c); got != want {
			t.Fatalf("%s = %q, want %q", c, got, want)
		}
	}
	varPath := filepath.Join(varsDir, VarName)
	stored := readFile(t, varPath)
	if stored[:4] != "\x07\x00\x00\x00" || !strings.Contains(stored, `"code":"BOOTY-NET-002"`) {
		t.Fatalf("variable = %q, want attributes 7 and the failure code", stored)
	}

	// The next run fails the same way: the variable is not written again.
	old := time.Unix(1, 0)
	if err := os.Chtimes(varPath, old, old); err != nil {
		t.Fatal(err)
	}
	next := NewReporter(&opts)
	next.Phase("network")
	next.Fail("network-setup", "BOOTY-NET-002")
	if st, _ := os.Stat(varPath); !st.ModTime().Equal(old) {
		t.Fatal("an unchanged result was written to NVRAM again")
	}
	LogLastResult(vars)

	// A different result is written.
	next.Phase("provision")
	next.Done()
	if st, _ := os.Stat(varPath); st.ModTime().Equal(old) || !strings.Contains(readFile(t, varPath), `"result":"success"`) {
		t.Fatalf("changed result not written: %q", readFile(t, varPath))
	}
}

func TestReporterSerialOffAndNoVars(t *testing.T) {
	console := filepath.Join(t.TempDir(), "ttyS0")
	if err := os.WriteFile(console, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewReporter(&Options{Consoles: []string{console}})
	r.Phase("provision")
	r.Fail("detect-disk", "BOOTY-DSK-006")
	if got := readFile(t, console); got != "" {
		t.Fatalf("OOB_STATUS=off wrote %q", got)
	}
}

func TestPackageFunctionsUseInstalledReporter(t *testing.T) {
	console := filepath.Join(t.TempDir(), "ttyS0")
	if err := os.WriteFile(console, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	Phase("provision") // nothing installed: no-op
	r := NewReporter(&Options{Serial: true, Consoles: []string{console}})
	Install(r)
	t.Cleanup(func() { Install(nil) })
	Phase("provision")
	Fail("detect-disk", "BOOTY-DSK-006")
	Done()
	got := readFile(t, console)
	for _, want := range []string{"result=running", "code=BOOTY-DSK-006 step=detect-disk result=failed", "code=- step=- result=success"} {
		if !strings.Contains(got, want) {
			t.Errorf("console %q lacks %q", got, want)
		}
	}
}
