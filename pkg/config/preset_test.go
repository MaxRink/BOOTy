package config

import "testing"

func TestLifecycleOptionsAndLegacyPreset(t *testing.T) {
	tests := []struct {
		name         string
		cfg          Config
		wantPowerOff bool
		wantWait     bool
	}{
		{name: "defaults", cfg: Config{}},
		{name: "native poweroff", cfg: Config{SuccessAction: "PowerOff"}, wantPowerOff: true},
		{name: "native wait", cfg: Config{FailureAction: " wait "}, wantWait: true},
		{name: "native reboot", cfg: Config{SuccessAction: "reboot", FailureAction: "reboot"}},
		{name: "preset", cfg: Config{LegacyCompat: true}, wantPowerOff: true, wantWait: true},
		{name: "explicit success action wins over preset", cfg: Config{LegacyCompat: true, SuccessAction: "reboot"}, wantWait: true},
		{name: "explicit failure action wins over preset", cfg: Config{LegacyCompat: true, FailureAction: "reboot"}, wantPowerOff: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if got := cfg.PowerOffOnSuccess(); got != tt.wantPowerOff {
				t.Errorf("PowerOffOnSuccess() = %v, want %v", got, tt.wantPowerOff)
			}
			if got := cfg.KexecDisabled(); got != tt.wantPowerOff {
				t.Errorf("KexecDisabled() = %v, want %v", got, tt.wantPowerOff)
			}
			if got := cfg.WaitOnFailure(); got != tt.wantWait {
				t.Errorf("WaitOnFailure() = %v, want %v", got, tt.wantWait)
			}
		})
	}
}

func TestLifecycleOptionsRejectUnknownValues(t *testing.T) {
	for _, cfg := range []Config{{SuccessAction: "kexec"}, {FailureAction: "shell"}} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted an unknown action", cfg)
		}
	}
}
