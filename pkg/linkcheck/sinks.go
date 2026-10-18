package linkcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/health"
)

// Paths of the file sinks; test seams.
var (
	ResultPath     = "/run/booty/check-result.json"
	BM4XReportPath = "/run/booty/bm4x-connection-report.json"
)

// Reporter sends results to CAPRF or another HTTP endpoint; the CAPRF
// client implements it.
type Reporter interface {
	ReportCheckResult(ctx context.Context, data []byte) error
	ReportHealthChecks(ctx context.Context, results []health.CheckResult) error
}

// BM4XReport is the bm4x ConnectionReport.
type BM4XReport struct {
	Diff []ConnectionEntry `json:"diff"`
}

// Deliver sends the document to the log and to the configured sinks. A
// failing sink is logged; the others still run. r may be nil.
func Deliver(ctx context.Context, doc *Document, cfg *config.CheckConfig, r Reporter, log *slog.Logger) {
	for _, c := range doc.Checks {
		log.Info("check result", "check", c.Name, "status", c.Status, "severity", c.Severity,
			"message", c.Message, "details", c.Details)
	}
	log.Info("check run done", "status", doc.Status, "checks", len(doc.Checks), "links", len(doc.Links), "diff", len(doc.Diff))
	data, err := json.Marshal(doc)
	if err != nil {
		log.Error("check result not encoded", "error", err)
		return
	}
	for _, s := range cfg.Sinks {
		if err := deliver(ctx, s, doc, data, r, log); err != nil {
			log.Warn("check result sink failed", "sink", s, "error", err)
		}
	}
}

func deliver(ctx context.Context, sink string, doc *Document, data []byte, r Reporter, log *slog.Logger) error {
	switch sink {
	case config.CheckSinkFile:
		return writeFile(ResultPath, data)
	case config.CheckSinkBM4XReport:
		report, err := json.Marshal(BM4XReport{Diff: append([]ConnectionEntry{}, doc.Diff...)})
		if err != nil {
			return fmt.Errorf("encode bm4x report: %w", err)
		}
		log.Info("bm4x connection report", "report", string(report))
		return writeFile(BM4XReportPath, report)
	case config.CheckSinkHTTP, config.CheckSinkCAPRF:
		if r == nil {
			return fmt.Errorf("no reporter")
		}
		if sink == config.CheckSinkHTTP {
			return r.ReportCheckResult(ctx, data) //nolint:wrapcheck // the reporter wraps its errors
		}
		return r.ReportHealthChecks(ctx, doc.Checks) //nolint:wrapcheck // the reporter wraps its errors
	}
	return fmt.Errorf("unknown sink %q", sink)
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // readable result
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // readable result
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
