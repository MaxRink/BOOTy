//go:build linux

package runmode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
)

// A payload command runs while the remote log handler ships lines and reads
// the client config. With the real caprf client and LOG_URL set this test
// finds a data race under -race when the command writes the shared config.
func TestStandbyPayloadCommandWithLogShipping(t *testing.T) {
	var mu sync.Mutex
	served, acks := false, map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/log", func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	mux.HandleFunc("/status/", func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	mux.HandleFunc("/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
	mux.HandleFunc("GET /agent/commands", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if served {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		served = true
		_ = json.NewEncoder(w).Encode([]config.Command{
			{ID: "d1", Type: "deprovision", Payload: []byte("HOSTNAME=t1-payload\n")},
			{ID: "r1", Type: "reboot"},
		})
	})
	mux.HandleFunc("POST /agent/commands/ack", func(w http.ResponseWriter, r *http.Request) {
		var ack struct{ ID, Status string }
		_ = json.NewDecoder(r.Body).Decode(&ack)
		mu.Lock()
		acks[ack.ID] = ack.Status
		mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	vars := filepath.Join(t.TempDir(), "vars")
	if err := os.WriteFile(vars, []byte("MODE=standby\nHOSTNAME=t1\nTOKEN=tok\n"+
		"LOG_URL="+srv.URL+"/log\nINIT_URL="+srv.URL+"/status/init\n"+
		"HEARTBEAT_URL="+srv.URL+"/agent/heartbeat\nCOMMANDS_URL="+srv.URL+"/agent/commands\n"+
		"STANDBY_DEPROVISION_SUCCESS_ACTION=standby\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := caprf.New(vars)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := client.GetConfig(context.Background())
	handler := caprf.NewRemoteHandler(client, slog.NewTextHandler(io.Discard, nil), slog.LevelInfo, 256)
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev); handler.Close() })

	var gotHostname string
	mode := &StandbyMode{deps: Deps{Cfg: cfg, Client: client}}
	mode.newDeprovisioner = func(c *config.MachineConfig) standbyDeprovisioner {
		return standbyDeprovisionerFunc(func(context.Context) error {
			gotHostname = c.Hostname
			for range 50 {
				slog.Info("deprovision step", "host", c.Hostname)
			}
			return nil
		})
	}

	err = mode.Run(context.Background())
	var rebootErr *RebootRequestedError
	if !errors.As(err, &rebootErr) {
		t.Fatalf("Run() = %v, want the reboot command after the deprovision", err)
	}
	if gotHostname != "t1-payload" || cfg.Hostname != "t1" {
		t.Fatalf("command hostname %q, shared hostname %q; want t1-payload and t1", gotHostname, cfg.Hostname)
	}
	mu.Lock()
	defer mu.Unlock()
	if acks["d1"] != "completed" || acks["r1"] != "completed" {
		t.Fatalf("acks = %v", acks)
	}
}
