package sriov

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Journal remembers the firmware settings that BOOTy stored in this boot.
// The file lives in the ramdisk, so it is gone after the reboot that makes
// the settings active. A second run in the same boot (rescue retry, a
// resumed step, the second port of an adapter) then reports such a
// setting as applied instead of pending-reset.
type Journal struct {
	// Path is the journal file; empty keeps the journal in memory only.
	Path    string
	entries []JournalEntry
	loaded  bool
}

// JournalEntry is one stored setting: the scope (adapter or port), the
// setting and the stored value.
type JournalEntry struct {
	Scope   string `json:"scope"`
	Setting string `json:"setting"`
	Value   string `json:"value"`
}

// DefaultJournalPath is the journal in the ramdisk, next to the provision
// checkpoint.
const DefaultJournalPath = "/tmp/booty-sriov-stored.json"

func (j *Journal) load() {
	if j.loaded || j.Path == "" {
		j.loaded = true
		return
	}
	j.loaded = true
	data, err := os.ReadFile(j.Path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &j.entries) // a broken journal only forgets
}

// Has reports whether BOOTy stored value for the setting in this boot.
func (j *Journal) Has(scope, setting, value string) bool {
	if j == nil {
		return false
	}
	j.load()
	for _, e := range j.entries {
		if e.Scope == scope && e.Setting == setting && e.Value == value {
			return true
		}
	}
	return false
}

// Record remembers a stored setting and writes the journal file.
func (j *Journal) Record(scope, setting, value string) error {
	if j == nil {
		return nil
	}
	j.load()
	kept := j.entries[:0]
	for _, e := range j.entries {
		if e.Scope != scope || e.Setting != setting {
			kept = append(kept, e)
		}
	}
	kept = append(kept, JournalEntry{scope, setting, value})
	j.entries = kept
	if j.Path == "" {
		return nil
	}
	data, err := json.Marshal(j.entries)
	if err != nil {
		return fmt.Errorf("encode SR-IOV journal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(j.Path), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("SR-IOV journal directory: %w", err)
	}
	if err := os.WriteFile(j.Path, data, 0o600); err != nil {
		return fmt.Errorf("write SR-IOV journal: %w", err)
	}
	return nil
}

// adapterScoped reports whether a firmware setting belongs to the whole
// adapter: the NV configuration of a ConnectX (SR-IOV enable, VF limit and
// the other NV parameters) and the SR-IOV enable in a Broadcom NVM. The
// link type, the Intel private flags and devlink parameters are per port.
func adapterScoped(setting string) bool {
	return setting == SettingSRIOVEnabled || setting == SettingMaxVFs || strings.HasPrefix(setting, settingNVPrefix)
}

// scopeKey names the adapter or the port that a setting belongs to.
func scopeKey(pf *PF, setting string) string {
	if adapterScoped(setting) {
		return "adapter " + adapterKey(pf)
	}
	return "port " + portKey(pf)
}

// adapterConflicts finds the adapter-wide settings that profiles of ports
// of one adapter want with different values. Applying both would change
// the setting back and forth on every provision, a reboot loop. The key is
// scope and setting; the value explains the conflict.
func adapterConflicts(assignments []Assignment) map[string]string {
	type want struct{ profile, value string }
	seen := map[string]want{}
	out := map[string]string{}
	for _, a := range assignments {
		for _, name := range settingNames(&a.Profile.Firmware) {
			if !adapterScoped(name) {
				continue
			}
			key := scopeKey(a.PF, name) + "|" + name
			v := getSetting(&a.Profile.Firmware, name)
			if prev, ok := seen[key]; ok && prev.value != v {
				out[key] = fmt.Sprintf("ports of %s want %s=%s (profile %s) and %s (profile %s); the setting belongs to the whole adapter",
					strings.TrimPrefix(scopeKey(a.PF, name), "adapter "), name, prev.value, prev.profile, v, a.Profile.Name)
				continue
			}
			if _, ok := seen[key]; !ok {
				seen[key] = want{a.Profile.Name, v}
			}
		}
	}
	return out
}
