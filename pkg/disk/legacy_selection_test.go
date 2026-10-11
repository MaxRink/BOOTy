//go:build linux

package disk

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
)

// lsblkCommander answers the lsblk calls of the legacy CAPRF disk discovery
// by --include group.
type lsblkCommander struct {
	byMajors map[string]string
	fail     map[string]bool
}

func (c lsblkCommander) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	if name != "lsblk" {
		return nil, nil
	}
	for i, arg := range args {
		if arg == "--include" && i+1 < len(args) {
			if c.fail[args[i+1]] {
				return nil, errors.New("lsblk failed")
			}
			return []byte(c.byMajors[args[i+1]]), nil
		}
	}
	return nil, errors.New("missing --include")
}

// Legacy evidence: common.sh _discover_disks lists NVMe and virtio disks
// (lsblk --include 259,253) before SCSI/SATA disks (--include 8) and
// _select_single_disk takes the first one that meets MIN_DISK_SIZE_GB.
func TestDetectDiskLegacyCAPRF(t *testing.T) {
	const gib = int64(1) << 30
	size := func(n int64) string { return strconv.FormatInt(n*gib, 10) }
	tests := []struct {
		name      string
		byMajors  map[string]string
		fail      map[string]bool
		minSizeGB int
		want      string
		wantErr   bool
	}{
		{
			name: "first nvme wins over a larger second nvme",
			byMajors: map[string]string{
				"259,253": "/dev/nvme1n1 " + size(100) + "\n/dev/nvme0n1 " + size(900) + "\n",
				"8":       "/dev/sda " + size(2000) + "\n",
			},
			want: "/dev/nvme1n1",
		},
		{
			name: "virtio before sata",
			byMajors: map[string]string{
				"259,253": "/dev/vda " + size(10) + "\n",
				"8":       "/dev/sda " + size(500) + "\n",
			},
			want: "/dev/vda",
		},
		{
			name: "minimum size skips small nvme and falls back to sata",
			byMajors: map[string]string{
				"259,253": "/dev/nvme0n1 " + size(50) + "\n",
				"8":       "/dev/sdb " + size(99) + "\n/dev/sda " + size(100) + "\n",
			},
			minSizeGB: 100,
			want:      "/dev/sda",
		},
		{
			name:     "lsblk failure for one group is ignored",
			byMajors: map[string]string{"8": "/dev/sda " + size(100) + "\n"},
			fail:     map[string]bool{"259,253": true},
			want:     "/dev/sda",
		},
		{
			name:      "no disk meets the minimum",
			byMajors:  map[string]string{"8": "/dev/sda " + size(10) + "\n"},
			minSizeGB: 100,
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewManager(lsblkCommander{byMajors: tt.byMajors, fail: tt.fail})
			mgr.SetLegacyCAPRFSelection(true)
			got, err := mgr.DetectDisk(context.Background(), tt.minSizeGB)
			if (err != nil) != tt.wantErr {
				t.Fatalf("DetectDisk() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("DetectDisk() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetectDiskLegacyCAPRFStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr := NewManager(lsblkCommander{
		byMajors: map[string]string{"8": "/dev/sda 107374182400\n"},
		fail:     map[string]bool{"259,253": true},
	})
	mgr.SetLegacyCAPRFSelection(true)
	if _, err := mgr.DetectDisk(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("DetectDisk() = %v, want context.Canceled", err)
	}
}

func TestDetectDiskLegacyCAPRFRejectsOverflowingMinimum(t *testing.T) {
	mgr := NewManager(lsblkCommander{byMajors: map[string]string{"8": "/dev/sda 1024\n"}})
	mgr.SetLegacyCAPRFSelection(true)
	for _, minSizeGB := range []int{-1, math.MaxInt} {
		if _, err := mgr.DetectDisk(context.Background(), minSizeGB); err == nil {
			t.Fatalf("DetectDisk(min %d) selected a disk, want an error", minSizeGB)
		}
	}
}
