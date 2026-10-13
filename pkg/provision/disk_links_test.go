//go:build linux

package provision

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestCreateDiskLinksStep(t *testing.T) {
	for _, tt := range []struct {
		option   *bool
		wantLink bool
	}{
		{option: nil, wantLink: true},
		{option: new(false), wantLink: false},
	} {
		cfg := &config.MachineConfig{}
		cfg.Provision.DiskLinks = tt.option
		o, cmd := newTestOrchestratorWithCommander(t, cfg, &mockProvider{})
		cmd.setResult("blkid -c", []byte("DEVNAME=/dev/vda2\nUUID=1234-abcd\nTYPE=ext4\n"), nil)
		if err := o.createDiskLinks(context.Background()); err != nil {
			t.Fatalf("DiskLinks=%v: createDiskLinks() = %v", tt.option, err)
		}
		_, err := os.Lstat(filepath.Join(diskLinksDevDir, "disk", "by-uuid", "1234-abcd"))
		if (err == nil) != tt.wantLink {
			t.Errorf("DiskLinks=%v: by-uuid link exists = %v, want %v", tt.option, err == nil, tt.wantLink)
		}
	}
}
