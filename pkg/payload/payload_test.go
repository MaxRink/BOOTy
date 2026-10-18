package payload

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func useDir(t *testing.T, files map[string]string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "payloads")
	if files != nil {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig := Dir
	t.Cleanup(func() { Dir = orig })
	Dir = dir
}

func TestRequire(t *testing.T) {
	for _, tt := range []struct {
		name    string
		files   map[string]string
		missing bool
	}{
		{name: "no manifests (default flavor)"},
		{name: "not split off", files: map[string]string{"split": "qemu-img ipmitool\n"}},
		{name: "split off, loaded", files: map[string]string{"split": "frr qemu-img\n", "frr": "build=b1\n"}},
		{name: "split off, not loaded", files: map[string]string{"split": "frr qemu-img\n", "qemu-img": "build=b1\n"}, missing: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useDir(t, tt.files)
			err := Require(FRR)
			if got := errors.Is(err, ErrMissing); got != tt.missing {
				t.Fatalf("Require(frr) = %v, want missing %v", err, tt.missing)
			}
		})
	}
}

func TestCheck(t *testing.T) {
	for _, tt := range []struct {
		name     string
		files    map[string]string
		build    string
		mismatch bool
	}{
		{name: "no manifests", build: "b1"},
		{name: "same build", files: map[string]string{"split": "frr", "frr": "name=frr\nbuild=b1\n"}, build: "b1"},
		{name: "other build", files: map[string]string{"frr": "build=b0\n"}, build: "b1", mismatch: true},
		{name: "no build line", files: map[string]string{"frr": "name=frr\n"}, build: "b1", mismatch: true},
		{name: "development binary", files: map[string]string{"frr": "build=b0\n"}, build: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			useDir(t, tt.files)
			err := Check(tt.build)
			if got := errors.Is(err, ErrBuildMismatch); got != tt.mismatch {
				t.Fatalf("Check = %v, want mismatch %v", err, tt.mismatch)
			}
			if !tt.mismatch && err != nil {
				t.Fatalf("Check = %v", err)
			}
		})
	}
}
