package caprf

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/telekom/BOOTy/pkg/config"
)

func TestReportStatusRebootRequired(t *testing.T) {
	var path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, body = r.URL.Path, string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg, err := ParseVars(strings.NewReader("REBOOT_URL=" + srv.URL + "/status/reboot-required\n"))
	if err != nil || cfg.Transport.RebootURL != srv.URL+"/status/reboot-required" {
		t.Fatalf("REBOOT_URL: %v %q", err, cfg.Transport.RebootURL)
	}
	msg := "reboot-required reason=sriov-firmware adapters=0000:3b:00 settings=maxVFs"
	if err := NewFromConfig(cfg).ReportStatus(context.Background(), config.StatusRebootRequired, msg); err != nil {
		t.Fatal(err)
	}
	if path != "/status/reboot-required" || body != msg {
		t.Fatalf("posted %q %q", path, body)
	}
	// Without REBOOT_URL the status is skipped, not an error.
	if err := NewFromConfig(&config.MachineConfig{}).ReportStatus(context.Background(), config.StatusRebootRequired, msg); err != nil {
		t.Fatal(err)
	}
}
