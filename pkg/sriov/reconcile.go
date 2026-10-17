package sriov

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Reconciler checks, and with onDrift apply changes, the SR-IOV profiles
// of a host. It is the configure-sriov provisioning step.
type Reconciler struct {
	Config *Config
	// Discover reports the PF ports (Inspector.Discover).
	Discover func() ([]PF, error)
	// Backends are the firmware backends, tried in order.
	Backends []FirmwareBackend
	// Journal remembers the settings stored in this boot; nil forgets.
	Journal *Journal
	Log     *slog.Logger
}

// Result is the outcome of a reconcile.
type Result struct {
	Findings []Finding `json:"findings"`
	// Reboot is true when BOOTy stored firmware settings. They take effect
	// only after a reboot, so the handoff must reboot instead of kexec.
	Reboot bool `json:"reboot"`
}

// Run checks every profile. It returns an error when a profile with
// onDrift fail or apply is not compliant, or when the profiles are
// ambiguous; drift of an onDrift warn profile is only logged.
func (r *Reconciler) Run(ctx context.Context) (*Result, error) {
	res := &Result{}
	if len(r.Config.Profiles) == 0 {
		return res, nil
	}
	if r.Journal == nil {
		// Without a file the journal still covers the ports of this run.
		r.Journal = &Journal{}
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	pfs, err := r.Discover()
	if err != nil {
		return res, fmt.Errorf("discover SR-IOV PF ports: %w", err)
	}
	assignments, findings, err := Assign(r.Config.Profiles, pfs)
	if err != nil {
		return res, fmt.Errorf("SR-IOV profiles: %w", err)
	}
	res.Findings = findings
	conflicts := adapterConflicts(assignments)
	for _, a := range assignments {
		fw, reboot := r.firmware(ctx, a, conflicts)
		res.Findings = append(res.Findings, fw...)
		res.Reboot = res.Reboot || reboot
		res.Findings = append(res.Findings, r.runtime(a)...)
	}
	return res, r.enforce(log, res)
}

// firmware reads, checks and, with onDrift apply, stores the firmware
// settings of one PF port. The settings of an adapter are shared by its
// ports: after the first port stores them, the next port reads the stored
// value and the journal marks it as applied, so BOOTy writes once per
// adapter.
func (r *Reconciler) firmware(ctx context.Context, a Assignment, conflicts map[string]string) (findings []Finding, reboot bool) {
	want := &a.Profile.Firmware
	if !hasFirmwareSettings(want) {
		return nil, false
	}
	backend, note := backendFor(r.Backends, a.PF, r.Config.Tools())
	var read *FirmwareRead
	if backend != nil {
		var err error
		read, err = backend.Read(ctx, a.PF, want)
		if err != nil {
			read, note = nil, fmt.Sprintf("%s read: %v", backend.Name(), err)
		}
	}
	findings = checkFirmware(a, read, note, conflicts, r.Journal)
	for i := range findings {
		reboot = reboot || findings[i].Status == StatusApplied
	}
	if r.Config.DriftAction(a.Profile) != OnDriftApply {
		return findings, reboot
	}
	if backend == nil || read == nil {
		backend = nil
	}
	return findings, r.applyDrift(ctx, backend, note, a, findings) || reboot
}

// applyDrift stores the drifted settings and updates their findings. It
// reports whether a stored setting needs the reboot.
func (r *Reconciler) applyDrift(ctx context.Context, b FirmwareBackend, note string, a Assignment, findings []Finding) bool {
	var drifted []string
	for i := range findings {
		if findings[i].Status == StatusDrift {
			drifted = append(drifted, findings[i].Setting)
		}
	}
	if len(drifted) == 0 {
		return false
	}
	results := map[string][2]string{}
	if b == nil {
		for _, name := range drifted {
			results[name] = [2]string{StatusFailed, "cannot apply: " + note}
		}
	} else {
		results = r.store(ctx, b, a, drifted)
	}
	reboot := false
	for i := range findings {
		if res, ok := results[findings[i].Setting]; ok {
			findings[i].Status, findings[i].Detail = res[0], res[1]
			reboot = reboot || res[0] == StatusApplied
		}
	}
	return reboot
}

// store applies the drifted settings, reads them back and records the
// ones that the NIC kept. A set that does not persist fails instead of
// rebooting into the same drift again.
func (r *Reconciler) store(ctx context.Context, b FirmwareBackend, a Assignment, drifted []string) map[string][2]string {
	want := &a.Profile.Firmware
	out := map[string][2]string{}
	if err := b.Apply(ctx, a.PF, changeFor(want, drifted)); err != nil {
		for _, name := range drifted {
			out[name] = [2]string{StatusFailed, fmt.Sprintf("%s apply: %v", b.Name(), err)}
		}
		return out
	}
	after, err := b.Read(ctx, a.PF, want)
	for _, name := range drifted {
		wantValue := getSetting(want, name)
		switch {
		case err != nil:
			out[name] = [2]string{StatusFailed, fmt.Sprintf("%s read back: %v", b.Name(), err)}
		case getSetting(&after.Values, name) != wantValue:
			out[name] = [2]string{StatusFailed, fmt.Sprintf("%s set did not persist: stored %q after the set",
				b.Name(), getSetting(&after.Values, name))}
		default:
			if err := r.Journal.Record(scopeKey(a.PF, name), name, wantValue); err != nil {
				r.logger().Warn("SR-IOV journal", "error", err)
			}
			out[name] = [2]string{StatusApplied, "takes effect after the reboot"}
		}
	}
	return out
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

// runtime reports the runtime settings of a profile. This version does not
// apply them.
func (r *Reconciler) runtime(a Assignment) []Finding {
	rt := &a.Profile.Runtime
	if rt.NumVFs == nil && rt.EswitchMode == "" {
		return nil
	}
	return []Finding{{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelRuntime,
		Setting: "runtime", Status: StatusUnverified, Detail: "this BOOTy version does not apply runtime settings"}}
}

// enforce logs the findings and fails for the profiles that require it.
func (r *Reconciler) enforce(log *slog.Logger, res *Result) error {
	actions := map[string]string{}
	for i := range r.Config.Profiles {
		actions[r.Config.Profiles[i].Name] = r.Config.DriftAction(&r.Config.Profiles[i])
	}
	var failed []string
	compliant := 0
	for i := range res.Findings {
		f := &res.Findings[i]
		if f.Compliant() {
			compliant++
			continue
		}
		log.Warn("SR-IOV drift", "profile", f.Profile, "interface", f.Interface, "level", f.Level,
			"setting", f.Setting, "want", f.Want, "got", f.Got, "status", f.Status, "detail", f.Detail,
			"onDrift", actions[f.Profile])
		if actions[f.Profile] != OnDriftWarn {
			failed = append(failed, f.String())
		}
	}
	log.Info("SR-IOV compliance", "findings", len(res.Findings), "compliant", compliant,
		"failed", len(failed), "reboot", res.Reboot)
	if len(failed) > 0 {
		return fmt.Errorf("SR-IOV profiles not compliant: %s", strings.Join(failed, "; "))
	}
	return nil
}

func hasFirmwareSettings(f *FirmwareSettings) bool {
	return f.SRIOVEnabled != nil || f.MaxVFs != nil || f.LinkType != "" || f.FWLLDPAgent != nil ||
		len(f.NVConfig) > 0 || len(f.DevlinkParams) > 0
}
