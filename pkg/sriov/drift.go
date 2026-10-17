package sriov

import (
	"fmt"
	"strings"
)

// Finding statuses.
const (
	// StatusOK means the setting has the wanted value.
	StatusOK = "ok"
	// StatusDrift means the setting differs; apply can change it.
	StatusDrift = "drift"
	// StatusPendingReset means the stored firmware value is right but the
	// active one differs: the reset that applies it has not happened, or
	// the BIOS blocks it (for example SR-IOV off in the BIOS).
	StatusPendingReset = "pending-reset"
	// StatusUnverified means BOOTy could not read the setting.
	StatusUnverified = "unverified"
	// StatusApplied means BOOTy stored the value; it takes effect after
	// the reboot that follows.
	StatusApplied = "applied"
	// StatusFailed means the apply failed or is not possible.
	StatusFailed = "failed"
	// StatusRefusedInUse means BOOTy did not make a runtime change because
	// the port carries the ramdisk network and the change would disrupt
	// it. The target is not met, so it fails with onDrift fail or apply.
	StatusRefusedInUse = "refused-in-use"
	// StatusDeferred means a runtime setting waits for the reboot that
	// activates a stored firmware change of the adapter.
	StatusDeferred = "deferred"
)

// Finding levels.
const (
	LevelMatch    = "match"
	LevelFirmware = "firmware"
	LevelRuntime  = "runtime"
)

// Finding is the result for one setting of a profile on one PF port.
type Finding struct {
	Profile   string `json:"profile"`
	Interface string `json:"interface,omitempty"`
	Level     string `json:"level"`
	Setting   string `json:"setting"`
	Want      string `json:"want,omitempty"`
	Got       string `json:"got,omitempty"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
}

// Compliant reports whether the finding meets the profile: ok, or applied
// (it takes effect after the reboot that BOOTy does next).
func (f *Finding) Compliant() bool {
	return f.Status == StatusOK || f.Status == StatusApplied || f.Status == StatusDeferred
}

// String formats a finding for logs and errors.
func (f *Finding) String() string {
	s := fmt.Sprintf("%s/%s %s.%s: %s", f.Profile, f.Interface, f.Level, f.Setting, f.Status)
	if f.Want != "" || f.Got != "" {
		s += fmt.Sprintf(" (want %s, got %s)", f.Want, f.Got)
	}
	if f.Detail != "" {
		s += ": " + f.Detail
	}
	return s
}

// Assignment is a PF port and the profile that matches it.
type Assignment struct {
	Profile *Profile
	PF      *PF
}

// Assign matches the PF ports to the profiles. A PF port that matches
// two profiles is an error: the result would depend on the order. A
// profile that matches no PF port and is not optional is a match finding.
func Assign(profiles []Profile, pfs []PF) ([]Assignment, []Finding, error) {
	var (
		out      []Assignment
		findings []Finding
		errs     []string
	)
	count := make([]int, len(profiles))
	for i := range pfs {
		var match []int
		for j := range profiles {
			if profiles[j].Match.Matches(&pfs[i]) {
				match = append(match, j)
			}
		}
		switch len(match) {
		case 0:
			continue
		case 1:
			count[match[0]]++
			out = append(out, Assignment{Profile: &profiles[match[0]], PF: &pfs[i]})
		default:
			names := make([]string, 0, len(match))
			for _, j := range match {
				names = append(names, profiles[j].Name)
			}
			errs = append(errs, fmt.Sprintf("PF port %s matches the SR-IOV profiles %s", pfs[i].Interface, strings.Join(names, ", ")))
		}
	}
	if len(errs) > 0 {
		return nil, nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	for j := range profiles {
		f := Finding{Profile: profiles[j].Name, Level: LevelMatch, Setting: "pfPorts",
			Want: ">= 1", Got: fmt.Sprint(count[j]), Status: StatusOK}
		if count[j] == 0 {
			if profiles[j].Optional {
				f.Detail = "optional profile without a PF port"
			} else {
				f.Status = StatusDrift
				f.Detail = "no PF port matches the profile"
			}
		}
		findings = append(findings, f)
	}
	return out, findings, nil
}

// Detail of an applied setting that is active at once, without a reset.
const detailActiveNow = "active now"

// Detail of a setting that BOOTy stored earlier in this boot.
const detailStoredThisBoot = "stored in this boot; takes effect after the reboot"

// checkFirmware compares the firmware settings of a profile with the
// stored values (read, from a backend) and the active values (the kernel).
// note explains a missing read. A setting in conflicts fails; a setting
// that the journal shows as stored in this boot is applied, not pending.
func checkFirmware(a Assignment, read *FirmwareRead, note string, conflicts map[string]string, journal *Journal) []Finding {
	items := firmwareItems(&a.Profile.Firmware, a.PF, read)
	findings := make([]Finding, 0, len(items))
	for _, it := range items {
		f := Finding{Profile: a.Profile.Name, Interface: a.PF.Interface, Level: LevelFirmware,
			Setting: it.setting, Want: it.want, Got: gotString(it)}
		scope := scopeKey(a.PF, it.setting)
		if c := conflicts[scope+"|"+it.setting]; c != "" {
			f.Status, f.Detail = StatusFailed, c
		} else {
			f.Status, f.Detail = itemStatus(it, journal.Has(scope, it.setting, it.want), note)
			if f.Status == StatusUnverified {
				if reason, ok := read.unsupported(it.setting); ok {
					f.Detail = reason
				}
			}
		}
		findings = append(findings, f)
	}
	return findings
}

// itemStatus decides the status of one setting from the wanted, stored and
// active values; storedThisBoot is true when the journal has the value.
func itemStatus(it firmwareItem, storedThisBoot bool, note string) (status, detail string) {
	switch {
	case it.stored != "" && it.stored != it.want:
		return StatusDrift, ""
	case it.active != "" && it.active != it.want && it.stored == it.want && storedThisBoot:
		return StatusApplied, detailStoredThisBoot
	case it.active != "" && it.active != it.want && it.stored == it.want:
		return StatusPendingReset, "the stored value is not active: a firmware reset or power cycle is pending, or the BIOS blocks it"
	case it.active != "" && it.active != it.want:
		return StatusDrift, note
	case it.stored == "" && it.active == "":
		return StatusUnverified, note
	}
	return StatusOK, ""
}

func gotString(it firmwareItem) string {
	var parts []string
	if it.stored != "" {
		parts = append(parts, "stored="+it.stored)
	}
	if it.active != "" {
		parts = append(parts, "active="+it.active)
	}
	if len(parts) == 0 {
		return "unknown"
	}
	return strings.Join(parts, " ")
}
