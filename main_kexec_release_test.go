//go:build linux

package main

import (
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

func TestReleaseKexecTargetHonorsOption(t *testing.T) {
	for _, tt := range []struct {
		option *bool
		want   bool
	}{
		{option: nil, want: true},
		{option: new(true), want: true},
		{option: new(false), want: false},
	} {
		cfg := &config.MachineConfig{}
		cfg.Provision.KexecUnmountTarget = tt.option
		called := false
		releaseKexecTarget(cfg, func() (disk.ReleaseReport, error) {
			called = true
			return disk.ReleaseReport{Unmounted: []string{installedRootPath}}, nil
		})
		if called != tt.want {
			t.Errorf("KexecUnmountTarget=%v: release called = %v, want %v", tt.option, called, tt.want)
		}
	}
}
