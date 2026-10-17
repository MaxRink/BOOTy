//go:build e2e

package kvm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProductionTemplateKernelRelease(t *testing.T) {
	modules := t.TempDir()
	for _, release := range []string{"6.1.0-test", "6.2.0-test"} {
		if err := os.Mkdir(filepath.Join(modules, release), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pathKernelRoot := filepath.Join(t.TempDir(), "usr", "lib", "modules", "6.1.0-test")
	if err := os.MkdirAll(pathKernelRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathKernelRoot, "vmlinuz"), []byte("compressed image"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		kernel   string
		contents string
		want     string
		wantErr  bool
	}{
		{name: "versioned image", kernel: "vmlinuz-6.1.0-test", want: "6.1.0-test"},
		{name: "compressed image path", kernel: filepath.Join(pathKernelRoot, "vmlinuz"), want: "6.1.0-test"},
		{name: "embedded image", kernel: "vmlinuz", contents: "header Linux version 6.2.0-test SMP", want: "6.2.0-test"},
		{name: "mismatched modules", kernel: "vmlinuz", contents: "Linux version 6.3.0-test", wantErr: true},
		{name: "missing modules", kernel: "vmlinuz-6.4.0-test", wantErr: true},
		{name: "missing embedded release", kernel: "vmlinuz", contents: "not a Linux image", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kernel := tt.kernel
			if !filepath.IsAbs(kernel) {
				kernel = filepath.Join(t.TempDir(), kernel)
			}
			if err := os.WriteFile(kernel, []byte(tt.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := resolveProductionTemplateKernelRelease(kernel, modules, "")
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("release = %q, want %q", got, tt.want)
			}
		})
	}
}
