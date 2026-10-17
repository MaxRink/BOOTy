// Package buildinfo provides build metadata and size analysis utilities.
package buildinfo

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Flavor represents an initramfs build flavor.
type Flavor string

const (
	// FlavorFull includes FRR, tools, and all features (~80 MB).
	FlavorFull Flavor = "full"
	// FlavorGoBGP uses GoBGP instead of FRR (~40 MB).
	FlavorGoBGP Flavor = "gobgp"
	// FlavorSlim is DHCP-only with minimal tools (~15 MB).
	FlavorSlim Flavor = "slim"
	// FlavorMicro is pure Go with no external tools (~10 MB).
	FlavorMicro Flavor = "micro"
)

// Info holds build-time metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Flavor    Flavor `json:"flavor"`
}

// ldflags-set variables.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
	// commitTime is the committer time of the built commit (RFC 3339), set
	// by the build: -X .../pkg/buildinfo.commitTime=$(git log -1 --format=%cI).
	commitTime = ""
	flavor     = "full"
)

// validFlavors is the set of known build flavors.
var validFlavors = map[Flavor]bool{
	FlavorFull:  true,
	FlavorGoBGP: true,
	FlavorSlim:  true,
	FlavorMicro: true,
}

// CommitTime returns the committer time of the built commit, stamped at
// build time. ok is false when the build did not stamp a valid time.
func CommitTime() (t time.Time, ok bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(commitTime))
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Get returns the current build info.
func Get() *Info {
	f := Flavor(flavor)
	if !validFlavors[f] {
		f = FlavorFull
	}
	return &Info{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Flavor:    f,
	}
}

// DepInfo holds dependency analysis data.
type DepInfo struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Sum     string `json:"sum,omitempty"`
}

// Dependencies returns the list of compiled-in Go module dependencies.
// Returns an empty slice (never nil) when build info is unavailable.
func Dependencies() []DepInfo {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return []DepInfo{}
	}
	deps := make([]DepInfo, 0, len(bi.Deps))
	for _, d := range bi.Deps {
		mod := d
		if d.Replace != nil {
			mod = d.Replace
		}
		dep := DepInfo{
			Path:    mod.Path,
			Version: mod.Version,
		}
		if mod.Sum != "" {
			dep.Sum = mod.Sum
		}
		deps = append(deps, dep)
	}
	return deps
}

// SizeEstimate provides approximate binary size estimates per component.
type SizeEstimate struct {
	Component string  `json:"component"`
	SizeMB    float64 `json:"sizeMB"`
	Required  bool    `json:"required"`
	BuildTag  string  `json:"buildTag,omitempty"`
}

// EstimateComponents returns known size estimates for major dependencies.
func EstimateComponents() []SizeEstimate {
	return []SizeEstimate{
		{Component: "go-runtime", SizeMB: 5.0, Required: true},
		{Component: "gobgp-v3", SizeMB: 8.0, BuildTag: "!micro,!slim"},
		{Component: "go-containerregistry", SizeMB: 3.0, BuildTag: "!micro"},
		{Component: "cobra+viper", SizeMB: 1.0, Required: true},
		{Component: "crypto/tls", SizeMB: 1.5, Required: true},
	}
}

// TotalEstimate returns the approximate total binary size in MB.
func TotalEstimate(components []SizeEstimate) float64 {
	var total float64
	for _, c := range components {
		total += c.SizeMB
	}
	return total
}

// JSON returns the build info as JSON bytes.
func (i *Info) JSON() ([]byte, error) {
	data, err := json.Marshal(i)
	if err != nil {
		return nil, fmt.Errorf("marshal build info: %w", err)
	}
	return data, nil
}

// sanitizeLDFlagValue ensures a value is safe for use in -ldflags -X assignments.
// It rejects values containing whitespace, quotes, or shell metacharacters.
func sanitizeLDFlagValue(v string) string {
	for _, c := range v {
		if c == ' ' || c == '\t' || c == '"' || c == '\'' || c == '`' || c == '$' || c == '\\' {
			return ""
		}
	}
	return v
}

// LDFlags returns the recommended ldflags for a production build.
// Values are validated and empty/unsafe inputs are replaced with defaults.
func LDFlags(ver, sha, date, flv string) string {
	pkg := "github.com/telekom/BOOTy/pkg/buildinfo"

	if v := sanitizeLDFlagValue(ver); v != "" {
		ver = v
	} else {
		ver = "dev"
	}
	if v := sanitizeLDFlagValue(sha); v != "" {
		sha = v
	} else {
		sha = "unknown"
	}
	if v := sanitizeLDFlagValue(date); v != "" {
		date = v
	} else {
		date = "unknown"
	}
	if v := sanitizeLDFlagValue(flv); v != "" {
		flv = v
	} else {
		flv = "full"
	}

	parts := []string{
		"-s", "-w",
		fmt.Sprintf("-X %s.version=%s", pkg, ver),
		fmt.Sprintf("-X %s.commit=%s", pkg, sha),
		fmt.Sprintf("-X %s.buildDate=%s", pkg, date),
		fmt.Sprintf("-X %s.flavor=%s", pkg, flv),
	}
	return strings.Join(parts, " ")
}
