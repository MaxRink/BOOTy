//go:build linux

// Package oob writes a short out-of-band status of a BOOTy run: a fixed
// serial line on the active consoles at each phase change and at the end,
// and the BootyLastResult UEFI variable that the next run reads. The
// records hold only fixed tokens (phase, step, failure code, result,
// version), never URLs, addresses or secrets.
package oob

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/telekom/BOOTy/pkg/efi"
)

// Results of a status line.
const (
	ResultRunning = "running"
	ResultSuccess = "success"
	ResultFailed  = "failed"
)

const (
	// VarName is the UEFI variable with the result of the last run, under
	// the BOOTy vendor GUID.
	VarName = "BootyLastResult-f0821f58-5fcc-46a7-8fbd-47f665a67d7f"
	// varAttrs: non-volatile, boot service and runtime access.
	varAttrs = 0x7
	// MaxFieldLen caps every field of the line and the record.
	MaxFieldLen = 48
	// MaxRecordLen caps the encoded record. Seven fields of MaxFieldLen and
	// the JSON keys fit.
	MaxRecordLen = 512
	// MaxLineLen is the longest status line, newline included: the fixed
	// text and five fields of MaxFieldLen.
	MaxLineLen = len("BOOTY1 run= phase= code= step= result=\n") + 5*MaxFieldLen
)

// Status is one status line.
type Status struct {
	Run    string
	Phase  string
	Code   string
	Step   string
	Result string
}

// Line returns the fixed status line
// "BOOTY1 run=<id8> phase=<p> code=<c> step=<s> result=<r>\n". An empty
// field is "-"; other characters than letters, digits and "._-" become "_".
func Line(s *Status) string {
	return fmt.Sprintf("BOOTY1 run=%s phase=%s code=%s step=%s result=%s\n",
		token(s.Run), token(s.Phase), token(s.Code), token(s.Step), token(s.Result))
}

// token makes v a single safe word of at most MaxFieldLen bytes.
func token(v string) string {
	if v == "" {
		return "-"
	}
	if len(v) > MaxFieldLen {
		v = v[:MaxFieldLen]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, v)
}

// Record is the content of the BootyLastResult variable. It has no run ID
// and no time, so a run that ends like the previous one does not write
// NVRAM again (a host in a failure loop does not wear the flash).
type Record struct {
	V      int    `json:"v"`
	Mode   string `json:"mode"`
	Phase  string `json:"phase"`
	Step   string `json:"step"`
	Code   string `json:"code"`
	Result string `json:"result"`
	Booty  string `json:"booty"`
}

// EncodeRecord returns the variable content.
func EncodeRecord(in *Record) ([]byte, error) {
	r := *in
	r.V = 1
	for _, f := range []*string{&r.Mode, &r.Phase, &r.Step, &r.Code, &r.Result, &r.Booty} {
		*f = token(*f)
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode last result: %w", err)
	}
	if len(data) > MaxRecordLen {
		return nil, fmt.Errorf("last result is %d bytes, limit %d", len(data), MaxRecordLen)
	}
	return data, nil
}

// DecodeRecord parses a variable content written by EncodeRecord.
func DecodeRecord(data []byte) (Record, error) {
	var r Record
	if len(data) > MaxRecordLen {
		return r, fmt.Errorf("last result is %d bytes, limit %d", len(data), MaxRecordLen)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("decode last result: %w", err)
	}
	if r.V != 1 {
		return Record{}, fmt.Errorf("last result version %d, want 1", r.V)
	}
	return r, nil
}

// Options configure a Reporter.
type Options struct {
	// Serial writes the status line to Consoles.
	Serial bool
	// Consoles are the device paths of the status line.
	Consoles []string
	// Vars writes the BootyLastResult variable; nil disables it.
	Vars *efi.EFIVarReader
	// Mode and Version go into the variable.
	Mode, Version string
	// Run is the run ID of the lines; empty picks a new one (NewRunID).
	Run string
}

// Reporter writes the status of one run.
type Reporter struct {
	opts  Options
	run   string
	mu    sync.Mutex
	phase string
}

// NewRunID returns a random 8-character run ID.
func NewRunID() string {
	id := make([]byte, 4)
	_, _ = rand.Read(id) // crypto/rand.Read never returns an error
	return hex.EncodeToString(id)
}

// NewReporter returns a reporter for opts.
func NewReporter(opts *Options) *Reporter {
	run := opts.Run
	if run == "" {
		run = NewRunID()
	}
	return &Reporter{opts: *opts, run: run}
}

// Run returns the run ID of the status lines.
func (r *Reporter) Run() string { return r.run }

// Phase writes a line with result=running when the phase changes.
func (r *Reporter) Phase(phase string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if phase == r.phase {
		return
	}
	r.phase = phase
	r.writeLine(&Status{Run: r.run, Phase: phase, Result: ResultRunning})
}

// Fail writes the failure line and the variable.
func (r *Reporter) Fail(step, code string) { r.end(step, code, ResultFailed) }

// Done writes the success line and the variable.
func (r *Reporter) Done() { r.end("", "", ResultSuccess) }

func (r *Reporter) end(step, code, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeLine(&Status{Run: r.run, Phase: r.phase, Code: code, Step: step, Result: result})
	if r.opts.Vars == nil {
		return
	}
	rec := Record{Mode: r.opts.Mode, Phase: r.phase, Step: step, Code: code, Result: result, Booty: r.opts.Version}
	if err := writeRecord(r.opts.Vars, &rec); err != nil {
		slog.Warn("writing the BootyLastResult UEFI variable failed", "error", err)
	}
}

func (r *Reporter) writeLine(s *Status) {
	if !r.opts.Serial {
		return
	}
	line := []byte(Line(s))
	for _, dev := range r.opts.Consoles {
		if err := writeDevice(dev, line); err != nil {
			slog.Debug("writing the status line failed", "device", dev, "error", err)
		}
	}
}

// writeDevice writes with plain non-blocking syscalls, so a stuck console
// never stops PID 1 (os.File would wait in the poller).
func writeDevice(path string, data []byte) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_APPEND|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	_, werr := unix.Write(fd, data)
	if cerr := unix.Close(fd); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("write %s: %w", path, werr)
	}
	return nil
}

// writeRecord writes the variable only when its content changes.
func writeRecord(vars *efi.EFIVarReader, rec *Record) error {
	data, err := EncodeRecord(rec)
	if err != nil {
		return err
	}
	if old, err := vars.ReadVar(VarName); err == nil && bytes.Equal(old, data) {
		slog.Debug("BootyLastResult UEFI variable unchanged, not writing")
		return nil
	}
	if err := vars.WriteVar(VarName, varAttrs, data); err != nil {
		return fmt.Errorf("write %s: %w", VarName, err)
	}
	slog.Info("wrote the BootyLastResult UEFI variable", "phase", rec.Phase, "code", rec.Code, "result", rec.Result)
	return nil
}

// LogLastResult logs the result that the previous run stored.
func LogLastResult(vars *efi.EFIVarReader) {
	data, err := vars.ReadVar(VarName)
	if errors.Is(err, os.ErrNotExist) {
		slog.Info("no BootyLastResult UEFI variable from an earlier run")
		return
	}
	if err != nil {
		slog.Warn("reading the BootyLastResult UEFI variable failed", "error", err)
		return
	}
	rec, err := DecodeRecord(data)
	if err != nil {
		slog.Warn("the BootyLastResult UEFI variable is invalid", "error", err)
		return
	}
	slog.Info("previous BOOTy run result", "mode", rec.Mode, "phase", rec.Phase, "step", rec.Step,
		"code", rec.Code, "result", rec.Result, "booty", rec.Booty)
}

// ActiveConsoles returns the device paths of the active kernel consoles
// from /sys/class/tty/console/active, or /dev/console.
func ActiveConsoles(activeFile string) []string {
	data, err := os.ReadFile(activeFile) //nolint:gosec // sysfs path
	var out []string
	if err == nil {
		for _, name := range strings.Fields(string(data)) {
			if token(name) == name { // a plain tty name, no path
				out = append(out, "/dev/"+name)
			}
		}
	}
	if len(out) == 0 {
		return []string{"/dev/console"}
	}
	return out
}

var std atomic.Pointer[Reporter]

// Install makes r the reporter of Phase, Fail and Done. nil removes it.
func Install(r *Reporter) { std.Store(r) }

// Phase reports a phase change to the installed reporter, if any.
func Phase(phase string) {
	if r := std.Load(); r != nil {
		r.Phase(phase)
	}
}

// Fail reports a failed run to the installed reporter, if any.
func Fail(step, code string) {
	if r := std.Load(); r != nil {
		r.Fail(step, code)
	}
}

// Done reports a successful run to the installed reporter, if any.
func Done() {
	if r := std.Load(); r != nil {
		r.Done()
	}
}
