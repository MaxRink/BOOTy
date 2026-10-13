package config

import (
	"strings"
	"testing"
)

func TestABConfigResolvedTargetSlot(t *testing.T) {
	tests := []struct {
		name string
		cfg  ABConfig
		want string
	}{
		{name: "empty defaults to slot a", cfg: ABConfig{}, want: ABSlotA},
		{name: "inactive from active a selects b", cfg: ABConfig{ActiveSlot: ABSlotA, TargetSlot: ABTargetInactive}, want: ABSlotB},
		{name: "inactive from active b selects a", cfg: ABConfig{ActiveSlot: ABSlotB, TargetSlot: ABTargetInactive}, want: ABSlotA},
		{name: "explicit target wins", cfg: ABConfig{ActiveSlot: ABSlotA, TargetSlot: ABSlotA}, want: ABSlotA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.ResolvedTargetSlot()
			if err != nil {
				t.Fatalf("ResolvedTargetSlot() error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolvedTargetSlot() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestABConfigPartitionLayoutTargetsInactiveSlot(t *testing.T) {
	layout, err := (&ABConfig{ActiveSlot: ABSlotA, TargetSlot: ABTargetInactive, RootSizeMB: 4096}).PartitionLayout("/dev/sda")
	if err != nil {
		t.Fatalf("PartitionLayout() error: %v", err)
	}
	if layout.Device != "/dev/sda" {
		t.Fatalf("Device = %q, want /dev/sda", layout.Device)
	}
	if len(layout.Partitions) != 4 {
		t.Fatalf("partitions = %d, want 4", len(layout.Partitions))
	}
	if layout.Partitions[1].Mountpoint != "" {
		t.Fatalf("slot A mountpoint = %q, want empty", layout.Partitions[1].Mountpoint)
	}
	if layout.Partitions[2].Mountpoint != "/" {
		t.Fatalf("slot B mountpoint = %q, want /", layout.Partitions[2].Mountpoint)
	}
	if layout.Partitions[2].MountOptions != "" {
		t.Fatalf("dual-root slot B mount options = %q, want empty", layout.Partitions[2].MountOptions)
	}
	if layout.Partitions[3].Mountpoint != "/var/lib/booty" {
		t.Fatalf("state mountpoint = %q", layout.Partitions[3].Mountpoint)
	}
}

func TestABConfigSystemABPartitionLayoutDefaultsToSharedVar(t *testing.T) {
	layout, err := (&ABConfig{
		Scheme:     ABSchemeSystemAB,
		ActiveSlot: ABSlotA,
		TargetSlot: ABTargetInactive,
		RootSizeMB: 4096,
	}).PartitionLayout("/dev/sda")
	if err != nil {
		t.Fatalf("PartitionLayout() error: %v", err)
	}
	if len(layout.Partitions) != 4 {
		t.Fatalf("partitions = %d, want 4", len(layout.Partitions))
	}
	if layout.Partitions[1].MountOptions != "" {
		t.Fatalf("inactive slot A mount options = %q, want empty", layout.Partitions[1].MountOptions)
	}
	if layout.Partitions[2].MountOptions != "ro" {
		t.Fatalf("active system-ab root mount options = %q, want ro", layout.Partitions[2].MountOptions)
	}
	data := layout.Partitions[3]
	if data.Label != "BOOTY-DATA" || data.Mountpoint != "/var" || data.Filesystem != "ext4" || data.SizeMB != 0 {
		t.Fatalf("data partition = %+v, want BOOTY-DATA ext4 /var fill remaining", data)
	}
}

func TestABConfigSystemABPartitionLayoutUsesConfiguredDataPartitions(t *testing.T) {
	layout, err := (&ABConfig{
		Scheme:     ABSchemeSystemAB,
		TargetSlot: ABSlotA,
		RootSizeMB: 4096,
		DataPartitions: []ABDataPartition{
			{Label: "BOOTY-VAR", SizeMB: 8192, Mountpoint: "/var"},
			{Label: "BOOTY-HOME", Mountpoint: "/home"},
		},
	}).PartitionLayout("/dev/vda")
	if err != nil {
		t.Fatalf("PartitionLayout() error: %v", err)
	}
	if len(layout.Partitions) != 5 {
		t.Fatalf("partitions = %d, want 5", len(layout.Partitions))
	}
	if layout.Partitions[3].Label != "BOOTY-VAR" || layout.Partitions[3].SizeMB != 8192 || layout.Partitions[3].Mountpoint != "/var" {
		t.Fatalf("var partition = %+v", layout.Partitions[3])
	}
	if layout.Partitions[4].Label != "BOOTY-HOME" || layout.Partitions[4].SizeMB != 0 || layout.Partitions[4].Mountpoint != "/home" {
		t.Fatalf("home partition = %+v", layout.Partitions[4])
	}
}

func TestValidateAcceptsABImageMode(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.ActiveSlot = ABSlotB
	cfg.Provision.AB.TargetSlot = ABTargetInactive
	cfg.Provision.AB.RootSizeMB = 8192

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
	if cfg.Provision.AB.TargetSlot != ABTargetInactive {
		t.Fatalf("target slot normalized to %q, want inactive", cfg.Provision.AB.TargetSlot)
	}
}

func TestValidateAcceptsSystemABImageMode(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.Scheme = ABSchemeSystemAB
	cfg.Provision.AB.RootSizeMB = 8192

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
	if cfg.Provision.AB.Scheme != ABSchemeSystemAB {
		t.Fatalf("scheme normalized to %q, want %q", cfg.Provision.AB.Scheme, ABSchemeSystemAB)
	}
}

func TestValidateRejectsInvalidSystemABDataPartition(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.Scheme = ABSchemeSystemAB
	cfg.Provision.AB.RootSizeMB = 8192
	cfg.Provision.AB.DataPartitions = []ABDataPartition{
		{Label: "BOOTY-DATA", SizeMB: 1024, Mountpoint: "/var"},
		{Label: "BOOTY-DATA", Mountpoint: "/home"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected duplicate data partition label validation error")
	}
	if got := err.Error(); !strings.Contains(got, `duplicate label "BOOTY-DATA"`) {
		t.Fatalf("Validate() error = %q", got)
	}
}

func TestValidateRejectsDataPartitionsOutsideSystemABMode(t *testing.T) {
	tests := []struct {
		name      string
		imageMode string
		scheme    string
	}{
		{name: "whole disk image mode", imageMode: ImageModeWholeDisk, scheme: ABSchemeSystemAB},
		{name: "dual root scheme", imageMode: ImageModeAB, scheme: ABSchemeDualRoot},
		{name: "implicit dual root scheme", imageMode: ImageModeAB},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Provision.Image.Mode = tt.imageMode
			cfg.Provision.AB.Scheme = tt.scheme
			cfg.Provision.AB.RootSizeMB = 8192
			cfg.Provision.AB.DataPartitions = []ABDataPartition{
				{Label: "BOOTY-DATA", SizeMB: 1024, Mountpoint: "/var"},
			}

			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected dataPartitions mode validation error")
			}
			if got := err.Error(); !strings.Contains(got, "provision.ab.dataPartitions requires provision.image.mode=ab and provision.ab.scheme=system-ab") {
				t.Fatalf("Validate() error = %q", got)
			}
		})
	}
}

func TestValidateRejectsVFATSystemABDataPartition(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.Scheme = ABSchemeSystemAB
	cfg.Provision.AB.RootSizeMB = 8192
	cfg.Provision.AB.DataPartitions = []ABDataPartition{
		{Label: "BOOTY-DATA", SizeMB: 1024, Filesystem: "VFAT", Mountpoint: "/var"},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected vfat data partition validation error")
	}
	if got := err.Error(); !strings.Contains(got, "provision.ab.dataPartitions[0].filesystem must not be vfat") {
		t.Fatalf("Validate() error = %q", got)
	}
}

func TestValidateRejectsSystemABDataPartitionWithoutUsableMountpoint(t *testing.T) {
	tests := []struct {
		name string
		part ABDataPartition
		want string
	}{
		{
			name: "missing mountpoint",
			part: ABDataPartition{Label: "BOOTY-CACHE", SizeMB: 1024},
			want: "provision.ab.dataPartitions[0].mountpoint is required",
		},
		{
			name: "relative mountpoint",
			part: ABDataPartition{Label: "BOOTY-CACHE", SizeMB: 1024, Mountpoint: "var/cache"},
			want: `provision.ab.dataPartitions[0].mountpoint "var/cache" must be an absolute path`,
		},
		{
			name: "root mountpoint",
			part: ABDataPartition{Label: "BOOTY-CACHE", SizeMB: 1024, Mountpoint: "/"},
			want: `provision.ab.dataPartitions[0].mountpoint must not be "/"`,
		},
		{
			name: "efi mountpoint",
			part: ABDataPartition{Label: "BOOTY-CACHE", SizeMB: 1024, Mountpoint: "/boot/efi"},
			want: `provision.ab.dataPartitions[0].mountpoint must not be "/boot/efi"`,
		},
		{
			name: "efi mountpoint with trailing slash",
			part: ABDataPartition{Label: "BOOTY-CACHE", SizeMB: 1024, Mountpoint: "/boot/efi/"},
			want: `provision.ab.dataPartitions[0].mountpoint must not be "/boot/efi"`,
		},
		{
			name: "swap filesystem",
			part: ABDataPartition{Label: "BOOTY-CACHE", SizeMB: 1024, Filesystem: "swap", Mountpoint: "/var/cache"},
			want: "provision.ab.dataPartitions[0].filesystem must not be swap",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Provision.Image.Mode = ImageModeAB
			cfg.Provision.AB.Scheme = ABSchemeSystemAB
			cfg.Provision.AB.RootSizeMB = 8192
			cfg.Provision.AB.DataPartitions = []ABDataPartition{tt.part}

			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected data partition validation error")
			}
			if got := err.Error(); !strings.Contains(got, tt.want) {
				t.Fatalf("Validate() error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateRejectsABPreserveOutsideABMode(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeWholeDisk
	cfg.Provision.AB.PreserveExisting = true

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsABPreserveInactiveWithoutActiveSlot(t *testing.T) {
	tests := []struct {
		name       string
		targetSlot string
	}{
		{name: "default inactive target"},
		{name: "explicit inactive target", targetSlot: ABTargetInactive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Provision.Image.Mode = ImageModeAB
			cfg.Provision.AB.PreserveExisting = true
			cfg.Provision.AB.TargetSlot = tt.targetSlot
			cfg.Provision.AB.RootSizeMB = 8192

			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if got := err.Error(); !strings.Contains(got, "provision.ab.activeSlot is required") {
				t.Fatalf("Validate() error = %q", got)
			}
		})
	}
}

func TestValidateRejectsABPreserveExplicitTargetWithoutActiveSlot(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.TargetSlot = ABSlotB
	cfg.Provision.AB.RootSizeMB = 8192

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected activeSlot validation error")
	}
}

func TestValidateRejectsABPreserveTargetEqualsActive(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.ActiveSlot = ABSlotA
	cfg.Provision.AB.TargetSlot = ABSlotA
	cfg.Provision.AB.RootSizeMB = 8192

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected target=active validation error")
	}
	if got := err.Error(); !strings.Contains(got, "must not equal") {
		t.Fatalf("Validate() error = %q", got)
	}
}

func TestValidateRejectsABPreserveWithDisableKexec(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.DisableKexec = true
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.ActiveSlot = ABSlotA
	cfg.Provision.AB.TargetSlot = ABTargetInactive
	cfg.Provision.AB.RootSizeMB = 8192

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected disableKexec validation error")
	}
	if got := err.Error(); !strings.Contains(got, "disableKexec must be false") {
		t.Fatalf("Validate() error = %q", got)
	}
}

// SUCCESS_ACTION=poweroff, also through the CAPRF_LEGACY_COMPAT preset,
// never kexecs, so it cannot complete an A/B preserve-existing upgrade.
func TestValidateRejectsABPreserveWithPowerOff(t *testing.T) {
	for _, base := range []Config{{SuccessAction: "poweroff"}, {LegacyCompat: true}} {
		cfg := base
		cfg.Provision.Image.Mode = ImageModeAB
		cfg.Provision.AB.PreserveExisting = true
		cfg.Provision.AB.ActiveSlot = ABSlotA
		cfg.Provision.AB.TargetSlot = ABTargetInactive
		cfg.Provision.AB.RootSizeMB = 8192

		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "successAction poweroff") {
			t.Fatalf("Validate(%+v) = %v, want successAction poweroff incompatibility error", base, err)
		}
		if strings.Contains(err.Error(), "disableKexec must be false") {
			t.Fatalf("Validate() = %v, must not blame disableKexec when it is false", err)
		}
	}
}

// An explicit SUCCESS_ACTION=reboot wins over the preset, so the legacy
// preset can still run an A/B preserve-existing upgrade.
func TestValidateAcceptsABPreserveWithLegacyCompatAndExplicitReboot(t *testing.T) {
	cfg := &Config{LegacyCompat: true, SuccessAction: "reboot"}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.ActiveSlot = ABSlotA
	cfg.Provision.AB.TargetSlot = ABTargetInactive
	cfg.Provision.AB.RootSizeMB = 8192
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if !cfg.WaitOnFailure() {
		t.Fatal("the preset must still set FAILURE_ACTION=wait")
	}
}

func TestValidateRejectsABPreserveWithSecureBootReEnable(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.SecureBoot.ReEnable = true
	cfg.Provision.AB.PreserveExisting = true
	cfg.Provision.AB.ActiveSlot = ABSlotA
	cfg.Provision.AB.TargetSlot = ABTargetInactive
	cfg.Provision.AB.RootSizeMB = 8192

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected secure boot re-enable validation error")
	}
	if got := err.Error(); !strings.Contains(got, "provision.secureBoot.reEnable requires a hard reboot") {
		t.Fatalf("Validate() error = %q", got)
	}
}

func TestValidateRejectsABSourceRootSelectorConflict(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.RootSizeMB = 8192
	cfg.Provision.AB.SourceRootLabel = "rootfs"
	cfg.Provision.AB.SourceRootPartition = 2

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected source-root selector conflict")
	}
	if got := err.Error(); !strings.Contains(got, "sourceRootLabel and provision.ab.sourceRootPartition are mutually exclusive") {
		t.Fatalf("Validate() error = %q", got)
	}
}

func TestValidateTrimsABSourceRootSelector(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.RootSizeMB = 8192
	cfg.Provision.AB.SourceRootLabel = " rootfs "

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Provision.AB.SourceRootLabel != "rootfs" {
		t.Fatalf("sourceRootLabel = %q, want rootfs", cfg.Provision.AB.SourceRootLabel)
	}
}

func TestValidateRejectsBlankABSourceRootSelector(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Image.Mode = ImageModeAB
	cfg.Provision.AB.RootSizeMB = 8192
	cfg.Provision.AB.SourceRootLabel = "   "

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected blank source-root selector")
	}
	if got := err.Error(); !strings.Contains(got, "provision.ab.sourceRootLabel must not be blank") {
		t.Fatalf("Validate() error = %q", got)
	}
}
