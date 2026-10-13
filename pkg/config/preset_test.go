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

func TestDiskOptionsAndLegacyPreset(t *testing.T) {
	tests := []struct {
		name          string
		disk          DiskConfig
		legacy        bool
		wantSelection string
		wantWipeAll   bool
		wantDiscard   bool
	}{
		{name: "defaults"},
		{name: "native", disk: DiskConfig{Selection: "LSBLK", WipeAllDisks: new(true), DiscardNVMe: new(true)}, wantSelection: "lsblk", wantWipeAll: true, wantDiscard: true},
		{name: "preset", legacy: true, wantSelection: "lsblk", wantWipeAll: true, wantDiscard: true},
		{
			name: "explicit options win over preset", legacy: true,
			disk:          DiskConfig{Selection: "prefer-nvme", WipeAllDisks: new(false), DiscardNVMe: new(false)},
			wantSelection: "prefer-nvme",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{LegacyCompat: tt.legacy, Provision: ProvisionConfig{Disk: tt.disk}}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			d := cfg.Provision.Disk
			if d.Selection != tt.wantSelection || BoolOr(d.WipeAllDisks, false) != tt.wantWipeAll || BoolOr(d.DiscardNVMe, false) != tt.wantDiscard {
				t.Fatalf("disk = %q %v %v, want %q %v %v", d.Selection, BoolOr(d.WipeAllDisks, false), BoolOr(d.DiscardNVMe, false),
					tt.wantSelection, tt.wantWipeAll, tt.wantDiscard)
			}
		})
	}
	if err := (&Config{Provision: ProvisionConfig{Disk: DiskConfig{Selection: "largest"}}}).Validate(); err == nil {
		t.Fatal("Validate() accepted provision.disk.selection=largest")
	}
}

func TestFilesOptionsAndLegacyPreset(t *testing.T) {
	tests := []struct {
		name         string
		cfg          Config
		wantRamdisk  bool
		wantResolv   bool
		wantHostname bool
	}{
		{name: "defaults", wantResolv: true},
		{
			name:        "native",
			cfg:         Config{ProvisionerFilesTarget: "Ramdisk", RamdiskHostname: new(true), Provision: ProvisionConfig{WriteResolvConf: new(false)}},
			wantRamdisk: true, wantHostname: true,
		},
		{name: "preset", cfg: Config{LegacyCompat: true}, wantRamdisk: true, wantHostname: true},
		{
			name:       "explicit options win over preset",
			cfg:        Config{LegacyCompat: true, ProvisionerFilesTarget: "target", RamdiskHostname: new(false), Provision: ProvisionConfig{WriteResolvConf: new(true)}},
			wantResolv: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if got := cfg.ProvisionerFilesInRamdisk(); got != tt.wantRamdisk {
				t.Errorf("ProvisionerFilesInRamdisk() = %v, want %v", got, tt.wantRamdisk)
			}
			if got := BoolOr(cfg.Provision.WriteResolvConf, true); got != tt.wantResolv {
				t.Errorf("WriteResolvConf = %v, want %v", got, tt.wantResolv)
			}
			if got := BoolOr(cfg.RamdiskHostname, false); got != tt.wantHostname {
				t.Errorf("RamdiskHostname = %v, want %v", got, tt.wantHostname)
			}
		})
	}
	if err := (&Config{ProvisionerFilesTarget: "both"}).Validate(); err == nil {
		t.Fatal("Validate() accepted provisionerFilesTarget=both")
	}
}
