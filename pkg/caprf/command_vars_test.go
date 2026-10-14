package caprf

import (
	"slices"
	"strings"
	"testing"
)

const commandBaseVars = `export MODE="standby"
export HOSTNAME="t1"
export TOKEN="tok"
export MACHINE_EXTRA_KERNEL_PARAMS="intel_iommu=on"
export EXTRA_KERNEL_PARAMS="fromvars=t1"
export HEARTBEAT_URL="https://caprf.example.com/agent/heartbeat"
export COMMANDS_URL="https://caprf.example.com/agent/commands"
export STATIC_IP="10.0.0.2/24"
`

func TestMergeCommandVarsPayloadWins(t *testing.T) {
	cfg, keys, err := MergeCommandVars([]byte(commandBaseVars),
		[]byte("export HOSTNAME=\"t1-payload\"\nEXTRA_KERNEL_PARAMS=frompayload=t1\n# comment\nHOSTNAME=t1-payload\n"))
	if err != nil {
		t.Fatalf("MergeCommandVars: %v", err)
	}
	if cfg.Hostname != "t1-payload" {
		t.Fatalf("hostname = %q, want the payload value", cfg.Hostname)
	}
	// EXTRA_KERNEL_PARAMS of the payload replaces the base entry and still
	// goes after MACHINE_EXTRA_KERNEL_PARAMS, like in a vars file.
	if cfg.Provision.ExtraKernelParams != "intel_iommu=on frompayload=t1" {
		t.Fatalf("extra kernel params = %q", cfg.Provision.ExtraKernelParams)
	}
	if !slices.Equal(keys, []string{"EXTRA_KERNEL_PARAMS", "HOSTNAME"}) {
		t.Fatalf("keys = %v", keys)
	}
	if cfg.Transport.Token != "tok" || cfg.Mode != "standby" {
		t.Fatalf("base values lost: token %q mode %q", cfg.Transport.Token, cfg.Mode)
	}
}

// An empty payload value clears the base value, like `source vars`.
func TestMergeCommandVarsEmptyIntegerClearsBase(t *testing.T) {
	cfg, keys, err := MergeCommandVars([]byte(commandBaseVars+"export NUM_VFS=\"8\"\n"), []byte("NUM_VFS=\n"))
	if err != nil {
		t.Fatalf("MergeCommandVars: %v", err)
	}
	if cfg.Provision.Disk.NumVFs != 0 {
		t.Fatalf("NumVFs = %d, want 0 after an empty payload value", cfg.Provision.Disk.NumVFs)
	}
	if !slices.Equal(keys, []string{"NUM_VFS"}) {
		t.Fatalf("keys = %v", keys)
	}
}

func TestMergeCommandVarsRejectsRuntimeSettings(t *testing.T) {
	for payload, want := range map[string]string{
		"TOKEN=other":                                "transport",
		"INIT_URL=https://x.example.com/":            "transport",
		"SKIP_TLS_VERIFY=true":                       "transport",
		"COMMANDS_URL=https://x/agent/c":             "agent",
		"STATIC_IP=10.0.0.9/24":                      "network",
		"MODE=provision":                             "MODE",
		"DRY_RUN=true":                               "DRY_RUN",
		"CAPRF_LEGACY_COMPAT=true":                   "CAPRF_LEGACY_COMPAT",
		"STANDBY_DEPROVISION_SUCCESS_ACTION=standby": "agent",
		"no assignment":                              "line 1 is not KEY=VALUE",
		"HOSTNAME=x\nno assignment":                  "line 2 is not KEY=VALUE",
		"HOSTNMAE=typo\nHOSTNAME=x":                  "unknown payload keys HOSTNMAE",
		"HOSTNAME =x":                                "unknown payload keys HOSTNAME ",
		"INVENTORY_URL=https://x.example.com/inv":    "INVENTORY_URL",
		"FIRMWARE_URL=https://x.example.com/fw":      "FIRMWARE_URL",
		"HEALTH_CHECK_URL=https://x.example.com/h":   "HEALTH_CHECK_URL",
		"METRICS_URL=https://x.example.com/m":        "telemetry",
		"CRASH_ARTIFACTS_UPLOAD_URL=https://x/u":     "crash artifact",
		"LOG_URL=https://x.example.com/log":          "transport",
		"SUCCESS_ACTION=later":                       "payload keys SUCCESS_ACTION do not parse or validate",
		"LUKS_DEVICE=/dev/sda":                       "LUKS_DEVICE",
	} {
		_, _, err := MergeCommandVars([]byte(commandBaseVars), []byte(payload))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("payload %q: error = %v, want it to name %q", payload, err, want)
		}
	}
}

// The payload sees the presets of the merged file: SUCCESS_ACTION in the
// payload wins over the CAPRF_LEGACY_COMPAT preset of the base.
func TestMergeCommandVarsKeepsPresetRules(t *testing.T) {
	base := commandBaseVars + "CAPRF_LEGACY_COMPAT=true\n"
	cfg, _, err := MergeCommandVars([]byte(base), []byte("SUCCESS_ACTION=reboot"))
	if err != nil {
		t.Fatalf("MergeCommandVars: %v", err)
	}
	if cfg.SuccessAction != "reboot" || cfg.FailureAction != "wait" {
		t.Fatalf("success %q failure %q, want the payload value and the preset", cfg.SuccessAction, cfg.FailureAction)
	}
}

// Blank and comment lines are no payload: no keys, no config.
func TestMergeCommandVarsEmptyPayload(t *testing.T) {
	for _, payload := range []string{"", "  \n\t\n", "# only a comment\n\n"} {
		cfg, keys, err := MergeCommandVars([]byte(commandBaseVars), []byte(payload))
		if err != nil || cfg != nil || len(keys) != 0 {
			t.Errorf("payload %q: cfg %v keys %v err %v, want no payload", payload, cfg, keys, err)
		}
	}
}

// A rejection names keys, never values: the error goes into the ACK.
func TestMergeCommandVarsErrorsHaveNoValues(t *testing.T) {
	for _, payload := range []string{"SUCCESS_ACTION=s3cr3t", "MIN_DISK_SIZE_GB=s3cr3t", "SYSEXT_LAYERS=s3cr3t"} {
		_, _, err := MergeCommandVars([]byte(commandBaseVars), []byte(payload))
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("payload %q: error %v, want an error without the value", payload, err)
		}
	}
}

// Only the key that fails is named.
func TestMergeCommandVarsNamesOnlyTheBadKey(t *testing.T) {
	_, _, err := MergeCommandVars([]byte(commandBaseVars), []byte("HOSTNAME=ok\nSUCCESS_ACTION=s3cr3t\n"))
	if err == nil || err.Error() != "payload keys SUCCESS_ACTION do not parse or validate" {
		t.Fatalf("error = %v", err)
	}
}
