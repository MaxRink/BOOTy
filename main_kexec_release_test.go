//go:build linux

package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
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

func TestReleaseKexecTargetMountsUnknown(t *testing.T) {
	var logs bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	releaseKexecTarget(&config.MachineConfig{}, func() (disk.ReleaseReport, error) {
		return disk.ReleaseReport{}, fmt.Errorf("%w: read mounts: permission denied", disk.ErrMountsUnknown)
	})
	if strings.Contains(logs.String(), "released target filesystems") || !strings.Contains(logs.String(), "target filesystems unknown") {
		t.Fatalf("log = %s, want the unknown warning and no release line", logs.String())
	}
}
