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
		wantDepth     string
	}{
		{name: "defaults"},
		{name: "native", disk: DiskConfig{Selection: "LSBLK", WipeAllDisks: new(true), DiscardNVMe: new(true), WipeDepth: "Quick"}, wantSelection: "lsblk", wantWipeAll: true, wantDiscard: true, wantDepth: "quick"},
		{name: "preset", legacy: true, wantSelection: "lsblk", wantWipeAll: true, wantDiscard: true, wantDepth: "quick"},
		{
			name: "explicit options win over preset", legacy: true,
			disk:          DiskConfig{Selection: "prefer-nvme", WipeAllDisks: new(false), DiscardNVMe: new(false), WipeDepth: "thorough"},
			wantSelection: "prefer-nvme", wantDepth: "thorough",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{LegacyCompat: tt.legacy, Provision: ProvisionConfig{Disk: tt.disk}}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			d := cfg.Provision.Disk
			if d.WipeDepth != tt.wantDepth {
				t.Fatalf("wipeDepth = %q, want %q", d.WipeDepth, tt.wantDepth)
			}
			if d.Selection != tt.wantSelection || BoolOr(d.WipeAllDisks, false) != tt.wantWipeAll || BoolOr(d.DiscardNVMe, false) != tt.wantDiscard {
				t.Fatalf("disk = %q %v %v, want %q %v %v", d.Selection, BoolOr(d.WipeAllDisks, false), BoolOr(d.DiscardNVMe, false),
					tt.wantSelection, tt.wantWipeAll, tt.wantDiscard)
			}
		})
	}
	if err := (&Config{Provision: ProvisionConfig{Disk: DiskConfig{Selection: "largest"}}}).Validate(); err == nil {
		t.Fatal("Validate() accepted provision.disk.selection=largest")
	}
	if err := (&Config{Provision: ProvisionConfig{Disk: DiskConfig{WipeDepth: "deep"}}}).Validate(); err == nil {
		t.Fatal("Validate() accepted provision.disk.wipeDepth=deep")
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

func TestGRUBCmdlineModeAndLegacyPreset(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{name: "default"},
		{name: "native append", cfg: Config{Provision: ProvisionConfig{GRUBCmdlineMode: "Append"}}, want: true},
		{name: "preset", cfg: Config{LegacyCompat: true}, want: true},
		{name: "explicit resolved wins over preset", cfg: Config{LegacyCompat: true, Provision: ProvisionConfig{GRUBCmdlineMode: "resolved"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if got := cfg.LegacyGRUBCmdline(); got != tt.want {
				t.Fatalf("LegacyGRUBCmdline() = %v, want %v", got, tt.want)
			}
		})
	}
	if err := (&Config{Provision: ProvisionConfig{GRUBCmdlineMode: "replace"}}).Validate(); err == nil {
		t.Fatal("Validate() accepted provision.grubCmdlineMode=replace")
	}
}

func TestNICFirmwareOptionsAndLegacyPreset(t *testing.T) {
	for _, tt := range []struct {
		name      string
		cfg       Config
		wantLog   bool
		wantRaise bool
	}{
		{name: "default"},
		{name: "native", cfg: Config{Provision: ProvisionConfig{Firmware: FirmwareConfig{LogNICs: new(true), NumVFsMode: "Raise"}}}, wantLog: true, wantRaise: true},
		{name: "preset", cfg: Config{LegacyCompat: true}, wantLog: true, wantRaise: true},
		{name: "explicit options win over preset", cfg: Config{LegacyCompat: true, Provision: ProvisionConfig{Firmware: FirmwareConfig{LogNICs: new(false), NumVFsMode: "set"}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if got := BoolOr(cfg.Provision.Firmware.LogNICs, false); got != tt.wantLog {
				t.Errorf("LogNICs = %v, want %v", got, tt.wantLog)
			}
			if got := cfg.Provision.Firmware.NumVFsMode == NumVFsModeRaise; got != tt.wantRaise {
				t.Errorf("NumVFsMode = %q, want raise %v", cfg.Provision.Firmware.NumVFsMode, tt.wantRaise)
			}
		})
	}
	if err := (&Config{Provision: ProvisionConfig{Firmware: FirmwareConfig{NumVFsMode: "max"}}}).Validate(); err == nil {
		t.Fatal("Validate() accepted provision.firmware.numVFsMode=max")
	}
}

func TestImageChecksumScopeValidation(t *testing.T) {
	cfg := Config{Provision: ProvisionConfig{Image: ImageConfig{ChecksumScope: " Decompressed "}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if cfg.Provision.Image.ChecksumScope != ChecksumScopeDecompressed {
		t.Fatalf("ChecksumScope = %q, want %q", cfg.Provision.Image.ChecksumScope, ChecksumScopeDecompressed)
	}
	bad := Config{Provision: ProvisionConfig{Image: ImageConfig{ChecksumScope: "payload"}}}
	if err := bad.Validate(); err == nil {
		t.Fatal("Validate() accepted provision.image.checksumScope=payload")
	}
}

func TestGrowPartitionOptionAndLegacyPreset(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "default"},
		{name: "native", cfg: Config{Provision: ProvisionConfig{Disk: DiskConfig{GrowPartition: "Last-Linux"}}}, want: "last-linux"},
		{name: "preset", cfg: Config{LegacyCompat: true}, want: "last-linux"},
		{name: "explicit root wins over preset", cfg: Config{LegacyCompat: true, Provision: ProvisionConfig{Disk: DiskConfig{GrowPartition: "root"}}}, want: "root"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if got := cfg.Provision.Disk.GrowPartition; got != tt.want {
				t.Fatalf("GrowPartition = %q, want %q", got, tt.want)
			}
		})
	}
	if err := (&Config{Provision: ProvisionConfig{Disk: DiskConfig{GrowPartition: "all"}}}).Validate(); err == nil {
		t.Fatal("Validate() accepted provision.disk.growPartition=all")
	}
}

func TestNetworkWaitModeAndLegacyPreset(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "default"},
		{name: "native", cfg: Config{Network: NetworkConfig{WaitMode: "Rounds"}}, want: NetworkWaitRounds},
		{name: "preset", cfg: Config{LegacyCompat: true}, want: NetworkWaitRounds},
		{name: "explicit default wins over preset", cfg: Config{LegacyCompat: true, Network: NetworkConfig{WaitMode: "default"}}, want: NetworkWaitDefault},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if got := cfg.Network.WaitMode; got != tt.want {
				t.Fatalf("WaitMode = %q, want %q", got, tt.want)
			}
		})
	}
	if err := (&Config{Network: NetworkConfig{WaitMode: "forever"}}).Validate(); err == nil {
		t.Fatal("Validate() accepted network.waitMode=forever")
	}
}

func TestDiskSettleTimeoutBounds(t *testing.T) {
	for v, ok := range map[int]bool{-1: false, 0: true, 600: true, 601: false} {
		cfg := Config{Provision: ProvisionConfig{Disk: DiskConfig{SettleTimeoutSec: new(v)}}}
		if err := cfg.Validate(); (err == nil) != ok {
			t.Errorf("settleTimeoutSec=%d: Validate() = %v, want ok %v", v, err, ok)
		}
	}
}
