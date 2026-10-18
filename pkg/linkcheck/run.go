package linkcheck

import (
	"cmp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/telekom/BOOTy/pkg/buildinfo"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
)

// sysDMI is /sys/class/dmi/id; a test seam.
var sysDMI = "/sys/class/dmi/id"

// NewDocument starts the result document of a check run.
func NewDocument(cfg *config.Config, started time.Time) *Document {
	profiles := cfg.Check.Profiles
	if len(profiles) == 0 {
		profiles = []string{config.CheckProfileHealth}
	}
	host, _ := os.Hostname()
	return &Document{
		Schema: Schema, Mode: cmp.Or(cfg.Mode, "check"), Profiles: profiles, StartedAt: started.UTC(),
		Host: Host{
			Hostname: cmp.Or(cfg.Hostname, host), Product: readDMI("product_name"), UUID: readDMI("product_uuid"),
			BootyVersion: buildinfo.Get().Version,
		},
	}
}

func readDMI(name string) string {
	b, err := os.ReadFile(filepath.Join(sysDMI, name)) //nolint:gosec // sysfs path
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Cabling collects the NICs and runs the cabling checks into doc.
func Cabling(doc *Document, cfg *config.CheckConfig, nics []string) {
	doc.Links = collect(nics)
	expected := append([]config.ExpectedLink(nil), cfg.ExpectedCabling...)
	unmatched := matchExpected(doc.Links, expected)
	doc.Diff = Diff(doc.Host.Hostname, doc.Links, unmatched)
	doc.Checks = append(doc.Checks, cablingChecks(doc.Links, doc.Diff, len(expected) > 0, &cfg.Thresholds)...)
	doc.Checks = append(doc.Checks, linkChecks(doc.Links, &cfg.Thresholds)...)
	doc.Checks = append(doc.Checks, opticsChecks(doc.Links, &cfg.Thresholds)...)
}

// Finish sets the end time and the status: fail when a critical check
// failed, warn when another check failed, else pass. It reports whether
// the run failed.
func (d *Document) Finish(now time.Time) bool {
	d.FinishedAt = now.UTC()
	d.Status = "pass"
	for _, r := range d.Checks {
		switch {
		case r.Status != health.StatusFail:
		case r.Severity == health.SeverityCritical:
			d.Status = "fail"
		case d.Status == "pass":
			d.Status = "warn"
		}
	}
	return d.Status == "fail"
}
