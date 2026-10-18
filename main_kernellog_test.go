//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
)

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	return &buf
}

// The kernel's Secure Boot and lockdown lines, hidden from the console by
// loglevel=4, appear in one BOOTy line with the texts the ISO tests read.
func TestLogSecureBootState(t *testing.T) {
	klog := "<5>[    0.000000] Linux version 6.12.48+deb13-amd64\n" +
		"<6>[    0.004000] secureboot: Secure boot enabled\n" +
		"<5>[    0.004100] Kernel is locked down from EFI Secure Boot mode; see man kernel_lockdown.7\n" +
		"<6>[    0.200000] pci 0000:00:01.0: [8086:7000] type 00 class 0x060100\n"
	buf := captureLog(t)
	logSecureBootState(klog)
	out := buf.String()
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("want one line, got:\n%s", out)
	}
	for _, want := range []string{"Secure boot enabled", "Kernel is locked down from EFI Secure Boot"} {
		if !strings.Contains(out, want) {
			t.Errorf("line lacks %q: %s", want, out)
		}
	}
	if strings.Contains(out, "<6>") || strings.Contains(out, "[    0.") || strings.Contains(out, "pci 0000") {
		t.Errorf("line carries prefixes or other kernel lines: %s", out)
	}

	buf.Reset()
	logSecureBootState("<5>[    0.000000] Linux version 6.12\n")
	if !strings.Contains(buf.String(), "not reported") {
		t.Errorf("BIOS boot: %s", buf.String())
	}
}

func TestKernelLogLine(t *testing.T) {
	for in, want := range map[string]string{
		"<6>[    0.004000] secureboot: Secure boot disabled": "secureboot: Secure boot disabled",
		"[    0.004000] no level":                            "no level",
		"plain":                                              "plain",
	} {
		if got := kernelLogLine(in); got != want {
			t.Errorf("kernelLogLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// The kernel log goes to DEBUG_URL only. Nothing that reaches LOG_URL may
// trip CAPRF's classifyLogMessage image error rule ("error" together with
// "image", "stream" or "download"), which the kernel log does.
func TestShipKernelLogOnlyToDebugURL(t *testing.T) {
	klog := "<5>[    0.000000] Linux version 6.12\n" +
		"<5>[    0.000000] Command line: BOOT_IMAGE=/boot/vmlinuz console=ttyS0 loglevel=4\n" +
		"<6>[    3.472801] RAS: Correctable Errors collector initialized.\n" +
		"<6>[    3.700593] Freeing unused kernel image (initmem) memory: 4172K\n"
	orig := readKernelLogFn
	t.Cleanup(func() { readKernelLogFn = orig })
	readKernelLogFn = func() string { return klog }

	for _, withDebug := range []bool{true, false} {
		var mu sync.Mutex
		bodies := map[string][]string{}
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies[r.URL.Path] = append(bodies[r.URL.Path], string(b))
			mu.Unlock()
		}))
		cfg := &config.MachineConfig{Transport: config.TransportConfig{Token: "t", LogURL: srv.URL + "/log"}}
		if withDebug {
			cfg.Transport.DebugURL = srv.URL + "/debug"
		}
		client := caprf.NewFromConfig(cfg)
		remote := caprf.NewRemoteHandler(client, slog.NewTextHandler(io.Discard, nil), slog.LevelInfo, 100)
		origLogger := slog.Default()
		slog.SetDefault(slog.New(remote))

		shipKernelLog(context.Background(), client, cfg.Transport.DebugURL)
		logSecureBootState(klog)

		remote.Close()
		slog.SetDefault(origLogger)
		srv.Close()

		for _, b := range bodies["/log"] {
			lower := strings.ToLower(b)
			if strings.Contains(lower, "error") && (strings.Contains(lower, "image") ||
				strings.Contains(lower, "stream") || strings.Contains(lower, "download")) {
				t.Errorf("debug=%v: /log got %q, which CAPRF classifies as an image error", withDebug, b)
			}
			if strings.Contains(b, "Linux version") || strings.Contains(b, "Freeing unused") {
				t.Errorf("debug=%v: /log got kernel log lines: %q", withDebug, b)
			}
		}
		switch {
		case withDebug && (len(bodies["/debug"]) != 1 || !strings.Contains(bodies["/debug"][0], "RAS: Correctable Errors")):
			t.Errorf("/debug got %q, want the whole kernel log once", bodies["/debug"])
		case !withDebug && len(bodies["/debug"]) != 0:
			t.Errorf("without DEBUG_URL /debug got %q", bodies["/debug"])
		}
	}
}
