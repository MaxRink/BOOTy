package buildinfo

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", info.GoVersion, runtime.Version())
	}
	if info.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", info.OS, runtime.GOOS)
	}
	if info.Arch != runtime.GOARCH {
		t.Errorf("Arch = %q, want %q", info.Arch, runtime.GOARCH)
	}
}

func TestInfo_JSON(t *testing.T) {
	info := Get()
	data, err := info.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded Info
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Version != info.Version {
		t.Errorf("Version = %q, want %q", decoded.Version, info.Version)
	}
	if decoded.Commit != info.Commit {
		t.Errorf("Commit = %q, want %q", decoded.Commit, info.Commit)
	}
	if decoded.BuildDate != info.BuildDate {
		t.Errorf("BuildDate = %q, want %q", decoded.BuildDate, info.BuildDate)
	}
	if decoded.GoVersion != info.GoVersion {
		t.Errorf("GoVersion = %q, want %q", decoded.GoVersion, info.GoVersion)
	}
	if decoded.OS != info.OS {
		t.Errorf("OS = %q, want %q", decoded.OS, info.OS)
	}
	if decoded.Arch != info.Arch {
		t.Errorf("Arch = %q, want %q", decoded.Arch, info.Arch)
	}
	if decoded.Flavor != info.Flavor {
		t.Errorf("Flavor = %q, want %q", decoded.Flavor, info.Flavor)
	}
}

func TestDependencies(t *testing.T) {
	deps := Dependencies()
	// Dependencies always returns non-nil (empty slice, not nil).
	if deps == nil {
		t.Fatal("Dependencies() returned nil, want non-nil slice")
	}
	if len(deps) == 0 {
		t.Log("test binary has no external module dependencies")
	}
	for i, d := range deps {
		if d.Path == "" {
			t.Errorf("deps[%d].Path is empty", i)
		}
		if d.Version == "" {
			t.Errorf("deps[%d].Version is empty for %s", i, d.Path)
		}
	}
}

func TestEstimateComponents(t *testing.T) {
	components := EstimateComponents()
	if len(components) == 0 {
		t.Error("no components")
	}

	hasRequired := false
	for _, c := range components {
		if c.Required {
			hasRequired = true
		}
		if c.SizeMB <= 0 {
			t.Errorf("component %q has zero size", c.Component)
		}
	}
	if !hasRequired {
		t.Error("no required components")
	}
}

func TestTotalEstimate(t *testing.T) {
	components := EstimateComponents()
	total := TotalEstimate(components)
	if total <= 0 {
		t.Errorf("total = %f", total)
	}
}

func TestFlavorConstants(t *testing.T) {
	if got := string(FlavorFull); got != "full" {
		t.Errorf("FlavorFull = %q, want %q", got, "full")
	}
	if got := string(FlavorGoBGP); got != "gobgp" {
		t.Errorf("FlavorGoBGP = %q, want %q", got, "gobgp")
	}
	if got := string(FlavorSlim); got != "slim" {
		t.Errorf("FlavorSlim = %q, want %q", got, "slim")
	}
	if got := string(FlavorMicro); got != "micro" {
		t.Errorf("FlavorMicro = %q, want %q", got, "micro")
	}
}

func TestLDFlags(t *testing.T) {
	flags := LDFlags("v1.0.0", "abc123", "2025-01-01", "gobgp")
	if !strings.Contains(flags, "-s -w") {
		t.Error("missing strip flags")
	}
	if !strings.Contains(flags, "version=v1.0.0") {
		t.Error("missing version")
	}
	if !strings.Contains(flags, "commit=abc123") {
		t.Error("missing commit")
	}
	if !strings.Contains(flags, "flavor=gobgp") {
		t.Error("missing flavor")
	}
}

func TestTotalEstimate_Empty(t *testing.T) {
	total := TotalEstimate(nil)
	if total != 0 {
		t.Errorf("total = %f, want 0", total)
	}
}

func TestGet_Defaults(t *testing.T) {
	info := Get()
	if info.Version != "dev" {
		t.Errorf("default Version = %q, want %q", info.Version, "dev")
	}
	if info.Commit != "unknown" {
		t.Errorf("default Commit = %q, want %q", info.Commit, "unknown")
	}
	if info.BuildDate != "unknown" {
		t.Errorf("default BuildDate = %q, want %q", info.BuildDate, "unknown")
	}
	if info.Flavor != FlavorFull {
		t.Errorf("default Flavor = %q, want %q", info.Flavor, FlavorFull)
	}
}

func TestLDFlags_AllFields(t *testing.T) {
	flags := LDFlags("v2.0.0", "def456", "2026-01-15", "micro")
	expected := []string{
		"version=v2.0.0",
		"commit=def456",
		"buildDate=2026-01-15",
		"flavor=micro",
		"-s -w",
	}
	for _, want := range expected {
		if !strings.Contains(flags, want) {
			t.Errorf("LDFlags missing %q in %q", want, flags)
		}
	}
}

func TestLDFlags_Sanitization(t *testing.T) {
	tests := []struct {
		name    string
		ver     string
		sha     string
		date    string
		flv     string
		wantVer string
		wantFlv string
	}{
		{"clean values", "v1.0.0", "abc", "2026-01-01", "gobgp", "v1.0.0", "gobgp"},
		{"space in version", "v1 0", "abc", "2026-01-01", "full", "dev", "full"},
		{"quote in sha", "v1.0.0", `ab"c`, "2026-01-01", "full", "v1.0.0", "full"},
		{"dollar in date", "v1.0.0", "abc", "$HOME", "full", "v1.0.0", "full"},
		{"empty inputs", "", "", "", "", "dev", "full"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags := LDFlags(tt.ver, tt.sha, tt.date, tt.flv)
			if !strings.Contains(flags, "version="+tt.wantVer) {
				t.Errorf("expected version=%s in %q", tt.wantVer, flags)
			}
			if !strings.Contains(flags, "flavor="+tt.wantFlv) {
				t.Errorf("expected flavor=%s in %q", tt.wantFlv, flags)
			}
		})
	}
}

func TestGet_InvalidFlavor(t *testing.T) {
	// The package-level var is set via ldflags; in tests it's "full" by default.
	// We verify that Get() returns a valid flavor.
	info := Get()
	if !validFlavors[info.Flavor] {
		t.Errorf("Get() returned invalid flavor %q", info.Flavor)
	}
}

func TestEstimateComponents_Fields(t *testing.T) {
	for _, c := range EstimateComponents() {
		if c.Component == "" {
			t.Error("component has empty name")
		}
		if c.SizeMB <= 0 {
			t.Errorf("component %q has non-positive size: %f", c.Component, c.SizeMB)
		}
	}
}

func TestCommitTime(t *testing.T) {
	orig := commitTime
	t.Cleanup(func() { commitTime = orig })
	commitTime = "2026-10-01T08:30:00+02:00"
	if got, ok := CommitTime(); !ok || !got.Equal(time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)) {
		t.Fatalf("CommitTime() = %v, %v", got, ok)
	}
	for _, bad := range []string{"", "unknown", "2026-10-01"} {
		commitTime = bad
		if _, ok := CommitTime(); ok {
			t.Errorf("CommitTime() with %q reports ok", bad)
		}
	}
}
