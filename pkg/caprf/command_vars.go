package caprf

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"

	"github.com/telekom/BOOTy/pkg/config"
)

// MergeCommandVars returns the config for one standby command: the base vars
// file with the payload lines appended, so a payload entry wins over the
// same key in the base file. The payload uses the vars file format. It
// returns the payload keys, sorted.
//
// The payload can not change the settings that the running ramdisk already
// uses: the transport (tokens, callback URLs, TLS), the agent endpoints, the
// network, MODE, DRY_RUN and CAPRF_LEGACY_COMPAT. It can not change a URL
// that BOOTy posts to with the bearer token (inventory, firmware, health,
// telemetry, crash artifacts). Unknown keys are rejected too. A payload
// with only blank and comment lines returns no keys.
//
// The base is the vars file that BOOTy read at start; MergeCommandVars does
// not read /deploy/vars again.
func MergeCommandVars(base, payload []byte) (cfg *config.MachineConfig, keys []string, err error) {
	keys, err = payloadKeys(payload)
	if err != nil || len(keys) == 0 {
		return nil, nil, err
	}
	var unknown []string
	for _, k := range keys {
		if !knownVarKey(k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return nil, nil, fmt.Errorf("unknown payload keys %s", strings.Join(unknown, ","))
	}
	ref, err := ParseVars(bytes.NewReader(base))
	if err != nil {
		return nil, nil, fmt.Errorf("parse base vars: %w", err)
	}
	merged := slices.Concat(base, []byte("\n"), payload)
	cfg, err = ParseVars(bytes.NewReader(merged))
	if err != nil {
		// The parse and validation errors can quote values. The error goes
		// into the command ACK and the log goes to LOG_URL, so both name
		// only the keys.
		bad := strings.Join(badPayloadKeys(base, payload, keys), ",")
		slog.Warn("command payload does not parse or validate", "keys", bad)
		return nil, nil, fmt.Errorf("payload keys %s do not parse or validate", bad)
	}
	var fixed []string
	for _, s := range []struct {
		name      string
		ref, cand any
	}{
		{"transport", ref.Transport, cfg.Transport},
		{"agent", ref.Agent, cfg.Agent},
		{"network", ref.Network, cfg.Network},
		{"MODE", ref.Mode, cfg.Mode},
		{"DRY_RUN", ref.DryRun, cfg.DryRun},
		{"CAPRF_LEGACY_COMPAT", ref.LegacyCompat, cfg.LegacyCompat},
		// URLs that receive bearer-token POSTs.
		{"INVENTORY_URL", ref.Provision.Inventory.URL, cfg.Provision.Inventory.URL},
		{"FIRMWARE_URL", ref.Provision.Firmware.URL, cfg.Provision.Firmware.URL},
		{"HEALTH_CHECK_URL", ref.Health.ReportURL, cfg.Health.ReportURL},
		{"telemetry", []string{ref.Telemetry.URL, ref.Telemetry.MetricsURL, ref.Telemetry.EventURL},
			[]string{cfg.Telemetry.URL, cfg.Telemetry.MetricsURL, cfg.Telemetry.EventURL}},
		{"crash artifact", []string{ref.Provision.CrashArtifacts.PrepareURL, ref.Provision.CrashArtifacts.UploadURL},
			[]string{cfg.Provision.CrashArtifacts.PrepareURL, cfg.Provision.CrashArtifacts.UploadURL}},
	} {
		if !reflect.DeepEqual(s.ref, s.cand) {
			fixed = append(fixed, s.name)
		}
	}
	if len(fixed) > 0 {
		return nil, nil, fmt.Errorf("payload changes %s settings, which can not change in a running ramdisk",
			strings.Join(fixed, ", "))
	}
	return cfg, keys, nil
}

// payloadKeys returns the sorted, unique keys of the payload lines, read
// like ParseVars reads them. Blank and comment lines are skipped; any other
// line without "=" is an error.
func payloadKeys(payload []byte) ([]string, error) {
	var keys []string
	scanner := bufio.NewScanner(bytes.NewReader(payload))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		key, _, ok := parseVarLine(line)
		switch {
		case ok:
			keys = append(keys, key)
		case line != "" && !strings.HasPrefix(line, "#"):
			return nil, fmt.Errorf("payload line %d is not KEY=VALUE", n)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan payload: %w", err)
	}
	slices.Sort(keys)
	return slices.Compact(keys), nil
}

// badPayloadKeys returns the payload keys whose lines alone, appended to
// base, do not parse or validate. When each key passes alone, the keys only
// fail together and all are returned.
func badPayloadKeys(base, payload []byte, keys []string) []string {
	var bad []string
	for _, k := range keys {
		var lines []byte
		for line := range strings.Lines(string(payload)) {
			if key, _, ok := parseVarLine(line); ok && key == k {
				lines = append(lines, line...)
				lines = append(lines, '\n')
			}
		}
		if _, err := ParseVars(bytes.NewReader(slices.Concat(base, []byte("\n"), lines))); err != nil {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return keys
	}
	return bad
}
