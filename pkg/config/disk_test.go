package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsRootPartitionSelectorConflict(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Disk.RootPartitionLabel = "ubuntu-root"
	cfg.Provision.Disk.RootPartitionNumber = 2

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected root partition selector conflict")
	}
	if got := err.Error(); !strings.Contains(got, "provision.disk.rootPartitionLabel and provision.disk.rootPartitionNumber are mutually exclusive") {
		t.Fatalf("Validate() error = %q", got)
	}
}

func TestValidateRejectsNegativeRootPartitionNumber(t *testing.T) {
	cfg := &Config{}
	cfg.Provision.Disk.RootPartitionNumber = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected negative root partition number error")
	}
	if got := err.Error(); !strings.Contains(got, "provision.disk.rootPartitionNumber must be non-negative") {
		t.Fatalf("Validate() error = %q", got)
	}
}

func TestValidateRAIDMinimumDeviceCount(t *testing.T) {
	tests := []struct {
		name    string
		level   int
		devices []string
		wantErr string
	}{
		{"raid0 with 1 device", 0, []string{"/dev/sda"}, "level 0 requires at least 2 devices, got 1"},
		{"raid0 with 2 devices", 0, []string{"/dev/sda", "/dev/sdb"}, ""},
		{"raid1 with 1 device", 1, []string{"/dev/sda"}, "level 1 requires at least 2 devices, got 1"},
		{"raid1 with 2 devices", 1, []string{"/dev/sda", "/dev/sdb"}, ""},
		{"raid5 with 2 devices", 5, []string{"/dev/sda", "/dev/sdb"}, "level 5 requires at least 3 devices, got 2"},
		{"raid5 with 3 devices", 5, []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}, ""},
		{"raid6 with 3 devices", 6, []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}, "level 6 requires at least 4 devices, got 3"},
		{"raid6 with 4 devices", 6, []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"}, ""},
		{"raid10 with 3 devices", 10, []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}, "level 10 requires at least 4 devices, got 3"},
		{"raid10 with 4 devices", 10, []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Provision.Disk.RAID = []RAIDConfig{{Name: "md0", Level: tt.level, Devices: tt.devices}}

			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
