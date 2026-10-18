package sriov

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
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
	// Runtime applies the runtime settings; nil reports them unverified.
	Runtime RuntimeOps
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
	// VerifyReboot is true when runtime settings wait for stored firmware
	// changes (SR-IOV enable, VF limit, link type). The task must not go on
	// to the handoff: BOOTy boots the ramdisk again, where the firmware
	// values are active and the runtime settings are checked for real.
	VerifyReboot bool `json:"verifyReboot"`
	// VerifyDetail names the adapters and settings of VerifyReboot as
	// "adapters=<a,b> settings=<x,y>", without spaces in the values.
	VerifyDetail string `json:"verifyDetail,omitempty"`
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
	pending := map[string][]string{} // adapter: stored runtime-affecting settings
	for _, a := range assignments {
		fw, reboot := r.firmware(ctx, a, conflicts)
		res.Findings = append(res.Findings, fw...)
		res.Reboot = res.Reboot || reboot
		for i := range fw {
			if needsReset(&fw[i]) && runtimeAffecting(fw[i].Setting) {
				key := adapterKey(a.PF)
				pending[key] = appendOnce(pending[key], fw[i].Setting)
			}
		}
	}
	rt := r.runtime(assignments, pending)
	res.Findings = append(res.Findings, rt...)
	res.VerifyReboot, res.VerifyDetail = verifyReboot(log, rt, pending)
	return res, r.enforce(log, res)
}

// verifyReboot reports whether runtime settings were deferred, logs every
// deferral as a warning, and names the adapters and settings.
func verifyReboot(log *slog.Logger, runtime []Finding, pending map[string][]string) (needed bool, detail string) {
	for i := range runtime {
		f := &runtime[i]
		if f.Status != StatusDeferred {
			continue
		}
		needed = true
		log.Warn("SR-IOV runtime check deferred to a verify reboot", "profile", f.Profile, "interface", f.Interface,
			"detail", f.Detail)
	}
	if !needed {
		return false, ""
	}
	adapters := make([]string, 0, len(pending))
	var settings []string
	for k, list := range pending {
		adapters = append(adapters, k)
		for _, s := range list {
			settings = appendOnce(settings, s)
		}
	}
	sort.Strings(adapters)
	sort.Strings(settings)
	return true, "adapters=" + strings.Join(adapters, ",") + " settings=" + strings.Join(settings, ",")
}

func appendOnce(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// firmware reads, checks and, with onDrift apply, stores the firmware
// settings of one PF port. Each drifted setting goes to the backend that
// read it. The settings of an adapter are shared by its ports: after the
// first port stores them, the next port reads the stored value and the
// journal marks it as applied, so BOOTy writes once per adapter.
func (r *Reconciler) firmware(ctx context.Context, a Assignment, conflicts map[string]string) (findings []Finding, reboot bool) {
	want := &a.Profile.Firmware
	if !hasFirmwareSettings(want) {
		return nil, false
	}
	view := readFirmware(ctx, r.Backends, a.PF, want, r.Config.Tools())
	findings = checkFirmware(a, view, conflicts, r.Journal)
	for i := range findings {
		reboot = reboot || findings[i].Status == StatusApplied
	}
	if r.Config.DriftAction(a.Profile) != OnDriftApply {
		return findings, reboot
	}
	byOwner := map[FirmwareBackend][]string{}
	var owners []FirmwareBackend
	for i := range findings {
		f := &findings[i]
		if f.Status != StatusDrift {
			continue
		}
		b, ok := view.owner[f.Setting]
		if !ok {
			f.Status, f.Detail = StatusFailed, "cannot apply: "+view.notes[f.Setting]
			continue
		}
		if _, seen := byOwner[b]; !seen {
			owners = append(owners, b)
		}
		byOwner[b] = append(byOwner[b], f.Setting)
	}
	for _, b := range owners {
		results, needReboot := r.store(ctx, b, a, byOwner[b])
		reboot = reboot || needReboot
		for i := range findings {
			if res, ok := results[findings[i].Setting]; ok {
				findings[i].Status, findings[i].Detail = res[0], res[1]
			}
		}
	}
	return findings, reboot
}

// store applies the drifted settings of one backend, reads them back and
// records the ones that the NIC kept. A set that does not persist fails
// instead of rebooting into the same drift again.
func (r *Reconciler) store(ctx context.Context, b FirmwareBackend, a Assignment, drifted []string) (results map[string][2]string, reboot bool) {
	want := &a.Profile.Firmware
	results = map[string][2]string{}
	needReboot, err := b.Apply(ctx, a.PF, changeFor(want, drifted))
	if err != nil {
		for _, name := range drifted {
			results[name] = [2]string{StatusFailed, fmt.Sprintf("%s apply: %v", b.Name(), err)}
		}
		return results, false
	}
	after, err := b.Read(ctx, a.PF, want)
	detail := "active now"
	if needReboot {
		detail = "takes effect after the reboot"
	}
	for _, name := range drifted {
		wantValue := getSetting(want, name)
		switch {
		case err != nil:
			results[name] = [2]string{StatusFailed, fmt.Sprintf("%s read back: %v", b.Name(), err)}
		case getSetting(&after.Values, name) != wantValue:
			results[name] = [2]string{StatusFailed, fmt.Sprintf("%s set did not persist: stored %q after the set",
				b.Name(), getSetting(&after.Values, name))}
		default:
			if needReboot {
				if jerr := r.Journal.Record(scopeKey(a.PF, name), name, wantValue); jerr != nil {
					r.logger().Warn("SR-IOV journal", "error", jerr)
				}
				reboot = true
			}
			results[name] = [2]string{StatusApplied, detail}
		}
	}
	return results, reboot
}

func (r *Reconciler) logger() *slog.Logger {
	if r.Log == nil {
		return slog.Default()
	}
	return r.Log
}

// runtime applies the runtime settings, checks the result, and with
// handoff reset (the default) restores the state from before the apply.
// Runtime settings do not survive the handoff; BOOTy applies them to prove
// that the PF ports can run them.
//
// The runtime settings of an adapter with a stored firmware change wait
// for the reboot: sriov_totalvfs and the link type are still the old ones,
// so creating the VFs now would fail.
func (r *Reconciler) runtime(assignments []Assignment, pendingFirmware map[string][]string) []Finding {
	if r.Runtime == nil {
		var out []Finding
		for _, a := range assignments {
			if hasRuntimeSettings(&a.Profile.Runtime) {
				out = append(out, Finding{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelRuntime,
					Setting: "runtime", Status: StatusUnverified, Detail: "no runtime access"})
			}
		}
		return out
	}
	devs, findings := runtimeDevices(assignments)
	devs, deferred := deferDevices(devs, pendingFirmware)
	findings = append(findings, deferred...)
	if len(devs) == 0 {
		return findings
	}
	for _, d := range devs {
		applyDevice(r.Runtime, d, r.Config.UplinkVFDriverList())
	}
	now := r.discoverByPort()
	for _, d := range devs {
		for _, a := range d.assignments {
			pf, ok := now[portKey(a.PF)]
			if !ok {
				findings = append(findings, Finding{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelRuntime,
					Setting: "runtime", Status: StatusFailed, Detail: "PF port missing after the apply"})
				continue
			}
			findings = append(findings, verifyRuntime(a, pf, d)...)
		}
	}
	return append(findings, r.handoff(devs)...)
}

// handoff restores every changed device that does not keep its runtime
// state, then checks the VF count and the eSwitch mode again.
func (r *Reconciler) handoff(devs []*runtimeDevice) []Finding {
	var reset []*runtimeDevice
	errs := map[*runtimeDevice]error{}
	for _, d := range devs {
		if d.changed && !d.keep {
			errs[d] = resetDevice(r.Runtime, d)
			reset = append(reset, d)
		}
	}
	if len(reset) == 0 {
		return nil
	}
	now := r.discoverByPort()
	var out []Finding
	for _, d := range reset {
		a := d.assignments[0]
		want := fmt.Sprintf("numVFs=%d eswitch=%s", d.origNumVFs, d.origEswitch)
		f := Finding{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelRuntime,
			Setting: SettingHandoff, Want: want, Status: StatusOK}
		if pf, ok := now[portKey(a.PF)]; ok {
			f.Got = fmt.Sprintf("numVFs=%d eswitch=%s", pf.NumVFs, pf.Eswitch.Mode)
		}
		if f.Got != f.Want {
			f.Status = StatusFailed
		}
		if errs[d] != nil {
			f.Status, f.Detail = StatusFailed, errs[d].Error()
		}
		out = append(out, f)
	}
	return out
}

// discoverByPort reports the PF ports by port key (bus, PCI address and
// port), not by interface: a switchdev change re-registers the PF netdev,
// and without udev it can come back with another name. A failed discovery
// is an empty map, so every check after it fails.
func (r *Reconciler) discoverByPort() map[string]*PF {
	pfs, err := r.Discover()
	out := make(map[string]*PF, len(pfs))
	if err != nil {
		return out
	}
	for i := range pfs {
		out[portKey(&pfs[i])] = &pfs[i]
	}
	return out
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
