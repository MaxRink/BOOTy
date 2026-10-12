//go:build linux

package provision

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/disk"
)

// Legacy evidence: provision.sh is_root_partition_mount accepts a filesystem
// with /etc/os-release, or with /etc and /usr directories.
func TestLooksLikeRootFilesystem(t *testing.T) {
	tests := []struct {
		name  string
		dirs  []string
		files []string
		want  bool
	}{
		{name: "os-release", files: []string{"etc/os-release"}, want: true},
		{name: "etc and usr", dirs: []string{"etc", "usr"}, want: true},
		{name: "only etc", dirs: []string{"etc"}},
		{name: "data partition", files: []string{"data/file"}},
		{name: "os-release directory", dirs: []string{"etc/os-release"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, d := range tt.dirs {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range tt.files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := looksLikeRootFilesystem(dir); got != tt.want {
				t.Fatalf("looksLikeRootFilesystem() = %v, want %v", got, tt.want)
			}
		})
	}
}

// On resume, parse-partitions runs again while the root is mounted.
func TestProbeRootPartitionReusesMountedRoot(t *testing.T) {
	o := newTestOrchestrator(t, &config.MachineConfig{}, &mockProvider{})
	if err := os.MkdirAll(filepath.Join(o.config.rootDir, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.config.rootDir, "etc", "os-release"), []byte("ID=ubuntu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSource := mountedSource
	mountedSource = func(path string) (string, bool) {
		if path == o.config.rootDir {
			return "/dev/vda3", true
		}
		return "", false
	}
	t.Cleanup(func() { mountedSource = oldSource })

	parts := []disk.Partition{
		{Node: "/dev/vda2", Type: disk.LinuxFilesystemGUID},
		{Node: "/dev/vda3", Type: disk.LinuxFilesystemGUID},
	}
	root, err := o.probeRootPartition(context.Background(), parts, disk.ErrAmbiguousRootPartition)
	if err != nil || root.Node != "/dev/vda3" {
		t.Fatalf("probeRootPartition() = %v, %v; want the mounted /dev/vda3", root, err)
	}
}

// Symlinks out of the candidate must not make it look like a root.
func TestLooksLikeRootFilesystemIgnoresEscapingSymlinks(t *testing.T) {
	outside := t.TempDir()
	for _, d := range []string{"etc", "usr"} {
		if err := os.MkdirAll(filepath.Join(outside, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "etc", "os-release"), []byte("ID=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	for _, d := range []string{"etc", "usr"} {
		if err := os.Symlink(filepath.Join(outside, d), filepath.Join(data, d)); err != nil {
			t.Fatal(err)
		}
	}
	if looksLikeRootFilesystem(data) {
		t.Fatal("a data partition with etc and usr symlinks out of it was taken as root")
	}

	// A relative link inside the candidate still counts.
	rel := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rel, "usr", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rel, "usr", "lib", "os-release"), []byte("ID=x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rel, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../usr/lib/os-release", filepath.Join(rel, "etc", "os-release")); err != nil {
		t.Fatal(err)
	}
	if !looksLikeRootFilesystem(rel) {
		t.Fatal("a root with etc/os-release -> ../usr/lib/os-release was rejected")
	}
}
