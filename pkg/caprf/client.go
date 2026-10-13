// Package caprf implements the CAPRF provisioning server client.
package caprf

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/telekom/t-caas-go-library/pkg/redact"

	"github.com/telekom/BOOTy/pkg/auth"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/crash"
	"github.com/telekom/BOOTy/pkg/health"
)

// Client communicates with the CAPRF provisioning server.
type Client struct {
	httpClient   *http.Client
	cfg          *config.MachineConfig
	log          *slog.Logger
	tokenManager *auth.TokenManager
	insecureWarn sync.Map
}

var errInsecureTransport = errors.New("insecure transport")

// New creates a CAPRF client by parsing the vars file at the given path.
func New(varsPath string) (*Client, error) {
	f, err := os.Open(varsPath) //nolint:gosec // trusted path from ISO
	if err != nil {
		return nil, fmt.Errorf("open vars file: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort close

	cfg, err := ParseVars(f)
	if err != nil {
		return nil, fmt.Errorf("parse vars: %w", err)
	}

	if config.BoolOr(cfg.Transport.ExportVars, false) {
		// Like `source vars` in the legacy scripts: export every entry.
		// Proxy settings such as https_proxy then apply to all requests.
		if err := exportVars(varsPath); err != nil {
			return nil, fmt.Errorf("export vars: %w", err)
		}
	}

	c := &Client{
		httpClient: newHTTPClient(30*time.Second, config.BoolOr(cfg.Transport.SkipTLSVerify, false)),
		cfg:        cfg,
		log:        slog.Default().With("component", "caprf"),
	}
	c.warnTransport()
	return c, nil
}

func (c *Client) warnTransport() {
	if config.BoolOr(c.cfg.Transport.Insecure, false) {
		c.log.Warn("insecure transport enabled: bearer tokens will be sent over plain HTTP")
	}
	if config.BoolOr(c.cfg.Transport.SkipTLSVerify, false) {
		c.log.Warn("tls certificate verification disabled, like curl --insecure in the legacy CAPRF provisioner")
	}
}

// exportVars sets every variable of the vars file in the process
// environment, like `source vars` with `export` lines.
func exportVars(varsPath string) error {
	f, err := os.Open(varsPath) //nolint:gosec // trusted path from ISO
	if err != nil {
		return fmt.Errorf("open vars file: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort close

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, value, ok := parseVarLine(scanner.Text())
		if !ok || !isShellIdentifier(key) {
			continue
		}
		value, err := unquoteVarValue(value)
		if err != nil {
			return fmt.Errorf("parse var %s: %w", key, err)
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("export %s: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan vars: %w", err)
	}
	return nil
}

// parseVarLine splits one vars file line into key and raw value.
func parseVarLine(line string) (key, value string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	return strings.Cut(line, "=")
}

func isShellIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// FailureConfig reads only CAPRF_LEGACY_COMPAT and FAILURE_ACTION from the
// vars file and returns a config with the resulting FailureAction. It also
// works when the rest of the file does not parse, so BOOTy can still wait
// instead of rebooting after a vars file error.
func FailureConfig(varsPath string) *config.MachineConfig {
	cfg := &config.MachineConfig{}
	f, err := os.Open(varsPath) //nolint:gosec // trusted path from ISO
	if err != nil {
		return cfg
	}
	defer f.Close() //nolint:errcheck // read-only file

	// The last assignment wins, like in ParseVars. bufio.Reader has no line
	// length limit, so a long earlier entry cannot hide the setting.
	reader := bufio.NewReader(f)
	for {
		raw, err := reader.ReadString('\n')
		line := strings.TrimPrefix(strings.TrimSpace(raw), "export ")
		if key, value, ok := strings.Cut(line, "="); ok {
			value, unquoteErr := unquoteVarValue(value)
			switch key {
			case "CAPRF_LEGACY_COMPAT":
				cfg.LegacyCompat = unquoteErr == nil && parseBoolVar(value)
			case "FAILURE_ACTION":
				if unquoteErr != nil {
					value = ""
				}
				cfg.FailureAction = strings.ToLower(strings.TrimSpace(value))
			}
		}
		if err != nil {
			break
		}
	}
	cfg.ApplyPresets()
	return cfg
}

// NewFromConfig creates a CAPRF client from an already-parsed config.
func NewFromConfig(cfg *config.MachineConfig) *Client {
	c := &Client{
		httpClient: newHTTPClient(30*time.Second, config.BoolOr(cfg.Transport.SkipTLSVerify, false)),
		cfg:        cfg,
		log:        slog.Default().With("component", "caprf"),
	}
	c.warnTransport()
	return c
}

// AcquireToken exchanges the bootstrap token for a JWT if a token URL
// is configured. The acquired JWT replaces the bootstrap token for all
// subsequent API calls. The TokenManager is retained for background renewal.
func (c *Client) AcquireToken(ctx context.Context) error {
	if c.cfg.Transport.TokenURL == "" {
		return nil
	}
	if c.cfg.Transport.Token == "" {
		return fmt.Errorf("token URL configured but no bootstrap token")
	}
	if strings.TrimSpace(c.cfg.Hostname) == "" {
		return fmt.Errorf("token URL configured but hostname is empty")
	}

	tm, err := auth.NewTokenManager(c.cfg.Transport.TokenURL, c.cfg.Transport.Token, c.log)
	if err != nil {
		return fmt.Errorf("initialize token manager: %w", err)
	}
	if c.cfg.Transport.TokenAlgorithm != "" {
		switch c.cfg.Transport.TokenAlgorithm {
		case "RS256", "ES256":
			// Valid algorithms.
		default:
			return fmt.Errorf("unsupported token algorithm %q, must be RS256 or ES256", c.cfg.Transport.TokenAlgorithm)
		}
		tm.SetAlgorithm(c.cfg.Transport.TokenAlgorithm)
	}
	// bmcMAC is intentionally empty — the token endpoint identifies the
	// machine by serial (hostname). BMC MAC is only required for PXE
	// bootstrap flows that are not yet implemented.
	if err := tm.Acquire(ctx, c.cfg.Hostname, ""); err != nil {
		return fmt.Errorf("acquire jwt: %w", err)
	}
	// Snapshot the initial JWT into cfg.Transport.Token for backward compatibility with
	// GetConfig callers. After renewal, CurrentToken() is the authoritative
	// source — cfg.Transport.Token will hold the first-acquired JWT, not the latest.
	c.cfg.Transport.Token = tm.Token()
	c.tokenManager = tm
	c.log.Info("jwt token acquired, using for subsequent API calls")
	return nil
}

// SetTokenRenewalFatalHandler sets the callback invoked when token renewal
// is permanently exhausted.
func (c *Client) SetTokenRenewalFatalHandler(fn func()) {
	if c.tokenManager == nil {
		return
	}
	c.tokenManager.SetOnFatal(fn)
}

// StartTokenRenewal begins background JWT renewal if a token was acquired.
func (c *Client) StartTokenRenewal(ctx context.Context) error {
	if c.tokenManager == nil {
		return nil
	}
	return c.tokenManager.StartRenewal(ctx)
}

// CurrentToken returns the latest token, preferring the token manager if active.
func (c *Client) CurrentToken() string {
	if c.tokenManager != nil {
		return c.tokenManager.Token()
	}
	return c.cfg.Transport.Token
}

// GetConfig returns the parsed machine configuration.
func (c *Client) GetConfig(_ context.Context) (*config.MachineConfig, error) {
	return c.cfg, nil
}

// ReportStatus sends a provisioning status to the CAPRF server.
func (c *Client) ReportStatus(ctx context.Context, status config.Status, message string) error {
	var url string
	switch status {
	case config.StatusInit:
		url = c.cfg.Transport.InitURL
	case config.StatusSuccess:
		url = c.cfg.Transport.SuccessURL
	case config.StatusError:
		url = c.cfg.Transport.ErrorURL
	default:
		return fmt.Errorf("unknown status: %s", status)
	}

	if url == "" {
		c.log.Warn("no URL configured for status, skipping", "status", status)
		return nil
	}

	return c.postWithAuth(ctx, url, message)
}

// ShipLog sends a log line to the CAPRF /log endpoint.
func (c *Client) ShipLog(ctx context.Context, message string) error {
	if c.cfg.Transport.LogURL == "" {
		return nil
	}
	return c.postWithAuth(ctx, c.cfg.Transport.LogURL, message)
}

// ShipDebug sends a debug message to the CAPRF /debug endpoint.
func (c *Client) ShipDebug(ctx context.Context, message string) error {
	if c.cfg.Transport.DebugURL == "" {
		return nil
	}
	return c.postWithAuth(ctx, c.cfg.Transport.DebugURL, message)
}

// ReportHealthChecks sends health check results to the CAPRF server.
func (c *Client) ReportHealthChecks(ctx context.Context, results []health.CheckResult) error {
	if c.cfg.Health.ReportURL == "" {
		c.log.Warn("no health check URL configured, skipping report")
		return nil
	}

	data, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("marshal health results: %w", err)
	}

	return c.postJSONWithAuth(ctx, c.cfg.Health.ReportURL, data)
}

// Heartbeat sends a keepalive to the CAPRF server.
// Returns nil if no heartbeat URL is configured (non-standby mode).
func (c *Client) Heartbeat(ctx context.Context) error {
	if c.cfg.Agent.HeartbeatURL == "" {
		return nil
	}
	return c.postWithAuth(ctx, c.cfg.Agent.HeartbeatURL, "heartbeat")
}

// ReportFirmware sends a JSON firmware report to the CAPRF server.
func (c *Client) ReportFirmware(ctx context.Context, data []byte) error {
	if c.cfg.Provision.Firmware.URL == "" {
		c.log.Debug("no firmware URL configured, skipping report")
		return nil
	}
	return c.postJSONWithAuth(ctx, c.cfg.Provision.Firmware.URL, data)
}

// FetchCommands polls the CAPRF server for pending commands.
// Returns nil if no commands URL is configured.
func (c *Client) FetchCommands(ctx context.Context) ([]config.Command, error) {
	if c.cfg.Agent.CommandsURL == "" {
		return nil, nil
	}
	if err := c.requireSecureEndpoint(c.cfg.Agent.CommandsURL, "commands polling"); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Agent.CommandsURL, http.NoBody)
	if err != nil {
		redacted := redactedURL(c.cfg.Agent.CommandsURL)
		return nil, fmt.Errorf("create commands request %s: %w", redacted, redactedURLError(c.cfg.Agent.CommandsURL, err))
	}
	if err := c.setAuth(req); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req) //nolint:gosec // URL from trusted config
	if err != nil {
		return nil, fmt.Errorf("fetch commands: %w", redactedURLError(req.URL.String(), err))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch commands: status %d", resp.StatusCode)
	}

	var cmds []config.Command
	// Limit response body to 1 MiB to prevent OOM from an oversized response.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cmds); err != nil {
		return nil, fmt.Errorf("decode commands: %w", err)
	}
	return cmds, nil
}

// AcknowledgeCommand reports command execution result back to the CAPRF server.
func (c *Client) AcknowledgeCommand(ctx context.Context, cmdID, status, message string) error {
	if c.cfg.Agent.CommandsURL == "" {
		return nil
	}
	if err := c.requireSecureEndpoint(c.cfg.Agent.CommandsURL, "commands acknowledgement"); err != nil {
		return err
	}
	ack := struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}{
		ID:      cmdID,
		Status:  status,
		Message: message,
	}
	data, err := json.Marshal(ack)
	if err != nil {
		return fmt.Errorf("marshal command ack: %w", err)
	}
	ackURL, err := neturl.JoinPath(c.cfg.Agent.CommandsURL, "ack")
	if err != nil {
		redacted := redactedURL(c.cfg.Agent.CommandsURL)
		return fmt.Errorf("build command ack URL %s: %w", redacted, redactedURLError(c.cfg.Agent.CommandsURL, err))
	}
	return c.postJSONWithAuth(ctx, ackURL, data)
}

// ReportInventory posts a hardware inventory JSON payload to the CAPRF server.
func (c *Client) ReportInventory(ctx context.Context, data []byte) error {
	if c.cfg.Provision.Inventory.URL == "" {
		c.log.Warn("no inventory URL configured, skipping inventory report")
		return nil
	}
	return c.postJSONWithAuth(ctx, c.cfg.Provision.Inventory.URL, data)
}

// ReportMetrics posts provisioning metrics to the CAPRF server.
// Requires TelemetryEnabled. Uses MetricsURL, falling back to TelemetryURL.
func (c *Client) ReportMetrics(ctx context.Context, data []byte) error {
	if !c.cfg.Telemetry.Enabled {
		c.log.Debug("telemetry disabled, skipping metrics")
		return nil
	}
	url := c.cfg.Telemetry.MetricsURL
	if url == "" {
		url = c.cfg.Telemetry.URL
	}
	if url == "" {
		c.log.Debug("no metrics URL configured, skipping")
		return nil
	}
	return c.postJSONWithAuth(ctx, url, data)
}

// SendEvent posts a single provisioning event to the CAPRF server.
func (c *Client) SendEvent(ctx context.Context, data []byte) error {
	if !c.cfg.Telemetry.Enabled || c.cfg.Telemetry.EventURL == "" {
		return nil
	}
	return c.postJSONWithAuth(ctx, c.cfg.Telemetry.EventURL, data)
}

// ReportCrashArtifacts uploads a crash artifact archive using CAPRF-provided instructions.
func (c *Client) ReportCrashArtifacts(ctx context.Context, req *crash.PrepareRequest, archivePath string) error {
	if c.cfg.Provision.CrashArtifacts.PrepareURL == "" && c.cfg.Provision.CrashArtifacts.UploadURL == "" {
		c.log.Debug("no crash artifact URL configured, skipping")
		return crash.ErrNoUploadURL
	}
	if req == nil {
		return fmt.Errorf("crash artifact prepare request is nil")
	}
	if archivePath == "" {
		return fmt.Errorf("crash artifact archive path is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, c.crashUploadTimeout())
	defer cancel()

	if c.cfg.Provision.CrashArtifacts.PrepareURL != "" {
		instructions, err := c.prepareCrashArtifactUpload(ctx, req)
		if err != nil {
			return err
		}
		return c.uploadCrashArchive(ctx, instructions, archivePath)
	}

	return c.uploadCrashProxyMultipart(ctx, c.cfg.Provision.CrashArtifacts.UploadURL, req, archivePath)
}

func (c *Client) crashUploadTimeout() time.Duration {
	seconds := c.cfg.Provision.CrashArtifacts.UploadTimeoutSec
	if seconds <= 0 {
		seconds = config.DefaultCrashArtifactsUploadTimeoutSec
	}
	return time.Duration(seconds) * time.Second
}

func (c *Client) prepareCrashArtifactUpload(ctx context.Context, req *crash.PrepareRequest) (*crash.PrepareResponse, error) {
	if err := c.requireSecureEndpoint(c.cfg.Provision.CrashArtifacts.PrepareURL, "crash artifact prepare"); err != nil {
		return nil, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal crash artifact prepare request: %w", err)
	}
	redacted := redactedURL(c.cfg.Provision.CrashArtifacts.PrepareURL)
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			if !sleepForRetry(ctx, attempt, c.log, redacted) {
				return nil, fmt.Errorf("crash artifact prepare retry canceled: %w", ctx.Err())
			}
		}
		resp, retry, err := c.doPrepareCrashArtifactUpload(ctx, data)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retry || errors.Is(err, errInsecureTransport) {
			return nil, err
		}
		c.log.Warn("crash artifact prepare failed", "url", redacted, "attempt", attempt+1, "error", err)
	}
	return nil, fmt.Errorf("crash artifact prepare failed after 3 attempts to %s: %w", redacted, lastErr)
}

func (c *Client) doPrepareCrashArtifactUpload(ctx context.Context, data []byte) (*crash.PrepareResponse, bool, error) {
	rawURL := c.cfg.Provision.CrashArtifacts.PrepareURL
	redacted := redactedURL(rawURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, false, fmt.Errorf("create crash artifact prepare request %s: %w", redacted, redactedURLError(rawURL, err))
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.setAuth(req); err != nil {
		return nil, false, err
	}
	resp, err := c.httpClient.Do(req) //nolint:gosec // URL from trusted config
	if err != nil {
		return nil, true, fmt.Errorf("post %s: %w", redacted, redactedURLError(req.URL.String(), err))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, false, fmt.Errorf("post %s: status %d", redacted, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, true, fmt.Errorf("post %s: status %d", redacted, resp.StatusCode)
	}
	var instructions crash.PrepareResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&instructions); err != nil {
		return nil, false, fmt.Errorf("decode crash artifact prepare response: %w", err)
	}
	return &instructions, false, nil
}

func sleepForRetry(ctx context.Context, attempt int, log *slog.Logger, redacted string) bool {
	backoff := time.Duration(1<<(attempt-1)) * time.Second
	log.Info("retrying crash artifact request", "url", redacted, "attempt", attempt+1, "backoff", backoff)
	select {
	case <-time.After(backoff):
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) uploadCrashArchive(ctx context.Context, instructions *crash.PrepareResponse, archivePath string) error {
	if instructions == nil {
		return fmt.Errorf("crash artifact upload instructions are nil")
	}
	if instructions.UploadURL == "" {
		return fmt.Errorf("crash artifact upload URL is empty")
	}
	if err := c.requireSecureEndpoint(instructions.UploadURL, "crash artifact upload"); err != nil {
		return err
	}
	if instructions.MaxBytes > 0 {
		info, err := os.Stat(archivePath)
		if err != nil {
			return fmt.Errorf("stat crash artifact archive: %w", err)
		}
		if info.Size() > instructions.MaxBytes {
			return fmt.Errorf("crash artifact archive exceeds upload limit: %d > %d", info.Size(), instructions.MaxBytes)
		}
	}
	mode := instructions.UploadMode
	if mode == "" {
		mode = crash.UploadModePresignedPUT
		if len(instructions.FormFields) > 0 {
			mode = crash.UploadModePresignedPOST
		}
	}
	switch mode {
	case crash.UploadModePresignedPUT, crash.UploadModeCAPRFProxy:
		return c.uploadCrashRaw(ctx, instructions, archivePath)
	case crash.UploadModePresignedPOST:
		return c.uploadCrashPresignedForm(ctx, instructions, archivePath)
	default:
		return fmt.Errorf("unsupported crash artifact upload mode %q", mode)
	}
}

func (c *Client) uploadCrashRaw(ctx context.Context, instructions *crash.PrepareResponse, archivePath string) error {
	method := instructions.Method
	if method == "" {
		method = http.MethodPut
	}
	redacted := redactedURL(instructions.UploadURL)
	return c.uploadCrashWithRetry(ctx, redacted, func() error {
		body, err := os.Open(archivePath) //nolint:gosec // archive path created by BOOTy
		if err != nil {
			return fmt.Errorf("open crash artifact archive: %w", err)
		}
		defer body.Close() //nolint:errcheck // best-effort close
		req, err := http.NewRequestWithContext(ctx, method, instructions.UploadURL, body)
		if err != nil {
			return fmt.Errorf("create crash artifact upload request %s: %w", redacted, redactedURLError(instructions.UploadURL, err))
		}
		req.Header.Set("Content-Type", "application/gzip")
		for key, value := range instructions.Headers {
			req.Header.Set(key, value)
		}
		if err := c.applyCrashUploadAuth(req, instructions); err != nil {
			return err
		}
		return c.doCrashUploadRequest(req, redacted)
	})
}

func (c *Client) uploadCrashPresignedForm(ctx context.Context, instructions *crash.PrepareResponse, archivePath string) error {
	redacted := redactedURL(instructions.UploadURL)
	return c.uploadCrashWithRetry(ctx, redacted, func() error {
		reader, contentType := multipartArchiveReader(ctx, archivePath, instructions.FormFields, "file")
		// Close the pipe when the request is never sent, so the writer
		// goroutines exit. A second close after the transport is harmless.
		defer reader.Close() //nolint:errcheck // best-effort close
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, instructions.UploadURL, reader)
		if err != nil {
			return fmt.Errorf("create crash artifact form upload request %s: %w", redacted, redactedURLError(instructions.UploadURL, err))
		}
		req.Header.Set("Content-Type", contentType)
		for key, value := range instructions.Headers {
			req.Header.Set(key, value)
		}
		if err := c.applyCrashUploadAuth(req, instructions); err != nil {
			return err
		}
		return c.doCrashUploadRequest(req, redacted)
	})
}

func (c *Client) uploadCrashProxyMultipart(ctx context.Context, rawURL string, reqPayload *crash.PrepareRequest, archivePath string) error {
	if err := c.requireSecureEndpoint(rawURL, "crash artifact upload"); err != nil {
		return err
	}
	redacted := redactedURL(rawURL)
	manifest, err := json.Marshal(reqPayload)
	if err != nil {
		return fmt.Errorf("marshal crash artifact manifest: %w", err)
	}
	instructions := &crash.PrepareResponse{UploadURL: rawURL, AuthMode: crash.AuthModeBearer, UploadMode: crash.UploadModeCAPRFProxy}
	return c.uploadCrashWithRetry(ctx, redacted, func() error {
		reader, contentType := multipartArchiveReader(ctx, archivePath, map[string]string{"manifest": string(manifest)}, "archive")
		// Close the pipe when the request is never sent, so the writer
		// goroutines exit. A second close after the transport is harmless.
		defer reader.Close() //nolint:errcheck // best-effort close
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, reader)
		if err != nil {
			return fmt.Errorf("create crash artifact proxy upload request %s: %w", redacted, redactedURLError(rawURL, err))
		}
		req.Header.Set("Content-Type", contentType)
		if err := c.applyCrashUploadAuth(req, instructions); err != nil {
			return err
		}
		return c.doCrashUploadRequest(req, redacted)
	})
}

// Test hooks around the multipart writer goroutine.
var (
	multipartWriterStarted = func() {}
	multipartWriterExited  = func() {}
)

func multipartArchiveReader(ctx context.Context, archivePath string, fields map[string]string, fileField string) (body io.ReadCloser, contentType string) {
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	contentType = writer.FormDataContentType()
	done := make(chan struct{})
	multipartWriterStarted()
	go func() {
		defer multipartWriterExited()
		defer close(done)
		defer pw.Close()     //nolint:errcheck // best-effort close
		defer writer.Close() //nolint:errcheck // CloseWithError reports failures
		for key, value := range fields {
			if err := writer.WriteField(key, value); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		file, err := os.Open(archivePath) //nolint:gosec // archive path created by BOOTy
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		defer file.Close() //nolint:errcheck // best-effort close
		part, err := writer.CreateFormFile(fileField, filepath.Base(archivePath))
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = pr.CloseWithError(ctx.Err())
		case <-done:
		}
	}()
	return pr, contentType
}

func (c *Client) applyCrashUploadAuth(req *http.Request, instructions *crash.PrepareResponse) error {
	authMode := instructions.AuthMode
	if authMode == "" {
		if instructions.UploadMode == crash.UploadModeCAPRFProxy {
			authMode = crash.AuthModeBearer
		} else {
			authMode = crash.AuthModeNone
		}
	}
	switch authMode {
	case crash.AuthModeNone:
		return nil
	case crash.AuthModeBearer:
		if instructions.UploadMode != crash.UploadModeCAPRFProxy {
			return fmt.Errorf("crash artifact auth mode %q is only supported for %q uploads",
				authMode, crash.UploadModeCAPRFProxy)
		}
		return c.setAuth(req)
	default:
		return fmt.Errorf("unsupported crash artifact auth mode %q", authMode)
	}
}

func (c *Client) uploadCrashWithRetry(ctx context.Context, redacted string, fn func() error) error {
	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			if !sleepForRetry(ctx, attempt, c.log, redacted) {
				return fmt.Errorf("crash artifact upload retry canceled: %w", ctx.Err())
			}
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, errInsecureTransport) {
			return lastErr
		}
		c.log.Warn("crash artifact upload failed", "url", redacted, "attempt", attempt+1, "error", lastErr)
	}
	return fmt.Errorf("crash artifact upload failed after 3 attempts to %s: %w", redacted, lastErr)
}

func (c *Client) doCrashUploadRequest(req *http.Request, redacted string) error {
	if req.Body != nil {
		defer req.Body.Close() //nolint:errcheck // close multipart pipe on early transport failures
	}
	client := newHTTPClient(c.crashUploadTimeout(), false)
	resp, err := client.Do(req) //nolint:gosec // URL from trusted config or CAPRF response
	if err != nil {
		return fmt.Errorf("upload crash artifact to %s: %w", redacted, redactedURLError(req.URL.String(), err))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upload crash artifact to %s: status %d", redacted, resp.StatusCode)
	}
	return nil
}

func redactedURL(raw string) string {
	safe := redact.URL(raw)
	if safe == "[redacted invalid URL]" {
		return "<invalid-url>"
	}
	return safe
}

// redactedURLError turns an HTTP or URL error into a *url.Error whose text is
// built only from the URL we chose (reduced by redactedURL), the operation and
// a fixed cause. Server-controlled text, such as a redirect Location or an
// upstream message, is never included.
func redactedURLError(raw string, err error) error {
	if err == nil {
		return nil
	}
	op := "request"
	redirected := false
	var urlErr *neturl.Error
	if errors.As(err, &urlErr) {
		// net/http copies the request method into Op, and some methods come
		// from CAPRF responses, so only known values are kept.
		if allowedURLErrorOps[urlErr.Op] {
			op = urlErr.Op
		}
		// url.Parse reports the URL without its fragment, so only compare
		// for requests. A different URL there means a redirect was followed.
		redirected = op != "parse" && urlErr.URL != raw
	}
	cause, timeout, sentinel := classifyTransportError(err, op)
	if redirected {
		cause += " after redirect"
	}
	return &neturl.Error{
		Op:  op,
		URL: redactedURL(raw),
		Err: &transportCause{cause: cause, sentinel: sentinel, timeout: timeout},
	}
}

// allowedURLErrorOps are the url.Error operations that redactedURLError may
// show. Anything else is reported as "request".
var allowedURLErrorOps = map[string]bool{
	"Get": true, "Head": true, "Post": true, "Put": true, "Patch": true, "Delete": true, "parse": true,
}

// transportCause is a fixed transport failure cause. Unwrap returns
// context.Canceled or context.DeadlineExceeded when that was the cause, so
// errors.Is keeps working, and Timeout keeps the net.Error classification.
type transportCause struct {
	cause    string
	sentinel error
	timeout  bool
}

func (e *transportCause) Error() string   { return e.cause }
func (e *transportCause) Unwrap() error   { return e.sentinel }
func (e *transportCause) Timeout() bool   { return e.timeout }
func (e *transportCause) Temporary() bool { return false }

// classifyTransportError maps err to a fixed cause.
func classifyTransportError(err error, op string) (cause string, timeout bool, sentinel error) {
	var netErr net.Error
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var recordErr tls.RecordHeaderError
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled", false, context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true, context.DeadlineExceeded
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout", true, nil
	case errors.As(err, &dnsErr):
		return "dns lookup failed", false, nil
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused", false, nil
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset", false, nil
	case errors.As(err, &certErr), errors.As(err, &authorityErr), errors.As(err, &hostnameErr), errors.As(err, &recordErr):
		return "tls error", false, nil
	case op == "parse":
		return "invalid URL", false, nil
	default:
		return "transport error", false, nil
	}
}

func (c *Client) postWithAuth(ctx context.Context, url, body string) error {
	return c.withRetry(ctx, url, func() error {
		return c.doPost(ctx, url, body)
	})
}

func (c *Client) postJSONWithAuth(ctx context.Context, url string, data []byte) error {
	return c.withRetry(ctx, url, func() error {
		return c.doPostJSON(ctx, url, data)
	})
}

func (c *Client) withRetry(ctx context.Context, url string, fn func() error) error {
	var lastErr error
	redacted := redactedURL(url)
	for attempt := range 3 {
		if attempt > 0 {
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			c.log.Info("retrying request", "url", redacted, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("request retry canceled: %w", ctx.Err())
			}
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, errInsecureTransport) {
			return lastErr
		}
		c.log.Warn("request failed", "url", redacted, "attempt", attempt+1, "error", lastErr)
	}
	return fmt.Errorf("request failed after 3 attempts to %s: %w", redacted, lastErr)
}

// setAuth attaches the Bearer token only when the request URL uses HTTPS
// or targets loopback (localhost or loopback IP literals). Non-HTTPS remote endpoints fail
// closed to avoid credential leakage over plaintext transport.
// When InsecureTransport is set, the HTTPS requirement is bypassed.
func (c *Client) setAuth(req *http.Request) error {
	tok := c.CurrentToken()
	if tok == "" {
		return nil
	}
	if req.URL != nil && req.URL.Scheme != "https" && !isLoopback(req.URL.Hostname()) && !config.BoolOr(c.cfg.Transport.Insecure, false) {
		redacted := redactedURL(req.URL.String())
		c.warnInsecureOnce(redacted)
		return fmt.Errorf("%w: refusing bearer token on non-HTTPS request %s", errInsecureTransport, redacted)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// isLoopback returns true for localhost and loopback IP literals.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func newHTTPClient(timeout time.Duration, skipTLSVerify bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: skipTLSVerify, //nolint:gosec // opt-in legacy CAPRF parity (curl --insecure)
			},
		},
	}
}

func (c *Client) warnInsecureOnce(rawURL string) {
	if _, loaded := c.insecureWarn.LoadOrStore(rawURL, struct{}{}); loaded {
		return
	}
	c.log.Warn("refusing request to non-HTTPS endpoint", "url", rawURL)
}

func (c *Client) requireSecureEndpoint(rawURL, purpose string) error {
	if config.BoolOr(c.cfg.Transport.Insecure, false) {
		return nil
	}
	u, err := neturl.Parse(rawURL)
	if err != nil {
		redacted := redactedURL(rawURL)
		return fmt.Errorf("parse %s URL %s: %w", purpose, redacted, redactedURLError(rawURL, err))
	}
	if u.Scheme == "https" || isLoopback(u.Hostname()) {
		return nil
	}
	redacted := redactedURL(rawURL)
	c.warnInsecureOnce(redacted)
	return fmt.Errorf("%w: %s requires HTTPS, got %s", errInsecureTransport, purpose, redacted)
}

func (c *Client) doPost(ctx context.Context, url, body string) error {
	redacted := redactedURL(url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request %s: %w", redacted, redactedURLError(url, err))
	}
	req.Header.Set("Content-Type", "text/plain")
	if err := c.setAuth(req); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req) //nolint:gosec // URL from trusted config
	if err != nil {
		return fmt.Errorf("POST %s: %w", redacted, redactedURLError(req.URL.String(), err))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: status %d", redacted, resp.StatusCode)
	}
	return nil
}

func (c *Client) doPostJSON(ctx context.Context, url string, data []byte) error {
	redacted := redactedURL(url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request %s: %w", redacted, redactedURLError(url, err))
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.setAuth(req); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req) //nolint:gosec // URL from trusted config
	if err != nil {
		return fmt.Errorf("POST %s: %w", redacted, redactedURLError(req.URL.String(), err))
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("POST %s: status %d", redacted, resp.StatusCode)
	}
	return nil
}

// ParseVars reads a /deploy/vars file and returns a MachineConfig.
// The file format is: export KEY="VALUE" (one per line).
func ParseVars(r io.Reader) (*config.MachineConfig, error) {
	cfg := &config.MachineConfig{}
	scanner := bufio.NewScanner(r)
	// The legacy provision.sh reads EXTRA_KERNEL_PARAMS, which operators set
	// through additionalVars. It goes after MACHINE_EXTRA_KERNEL_PARAMS,
	// whatever the line order.
	legacyExtraKernelParams := ""

	for scanner.Scan() {
		key, value, ok := parseVarLine(scanner.Text())
		if !ok {
			continue
		}

		var err error
		value, err = unquoteVarValue(value)
		if err != nil {
			return nil, fmt.Errorf("parse var %s: %w", key, err)
		}

		if key == "EXTRA_KERNEL_PARAMS" {
			legacyExtraKernelParams = strings.TrimSpace(value)
			continue
		}
		if err := applyVar(cfg, key, value); err != nil {
			return nil, fmt.Errorf("parse var %s: %w", key, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan vars: %w", err)
	}
	cfg.Provision.ExtraKernelParams = strings.TrimSpace(cfg.Provision.ExtraKernelParams + " " + legacyExtraKernelParams)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func unquoteVarValue(value string) (string, error) {
	if len(value) < 2 {
		return value, nil
	}
	switch {
	case value[0] == '"' && value[len(value)-1] == '"':
		return unquoteDoubleQuotedVarValue(value[1 : len(value)-1]), nil
	case value[0] == '\'' && value[len(value)-1] == '\'':
		return value[1 : len(value)-1], nil
	default:
		return value, nil
	}
}

func unquoteDoubleQuotedVarValue(value string) string {
	var b strings.Builder
	b.Grow(len(value))

	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			b.WriteByte(value[i])
			continue
		}
		if i+1 >= len(value) {
			b.WriteByte('\\')
			continue
		}
		switch value[i+1] {
		case '$', '`', '"', '\\':
			b.WriteByte(value[i+1])
			i++
		default:
			b.WriteByte('\\')
		}
	}

	return b.String()
}

func applyVar(cfg *config.MachineConfig, key, value string) error {
	if applyParityVar(cfg, key, value) || applyStringVar(cfg, key, value) {
		return nil
	}
	if handled, err := applyUint32Var(cfg, key, value); handled {
		return err
	}
	return applySpecialVar(cfg, key, value)
}

func applyStringVar(cfg *config.MachineConfig, key, value string) bool {
	if applyNetworkStringVar(cfg, key, value) {
		return true
	}
	if applySecureBootDigestVar(cfg, key, value) {
		return true
	}
	if applyOCIPrePullStringVar(cfg, key, value) {
		return true
	}

	strFields := map[string]*string{
		"HOSTNAME":                    &cfg.Hostname,
		"MACHINE_EXTRA_KERNEL_PARAMS": &cfg.Provision.ExtraKernelParams,
		"PROVISION_TARGET_OS":         &cfg.Provision.TargetOS,
		"TARGET_OS":                   &cfg.Provision.TargetOS,
		"TOKEN":                       &cfg.Transport.Token,
		"MACHINE_SERIAL_CONSOLE":      &cfg.Provision.SerialConsole,
		"SERIAL_CONSOLE":              &cfg.Provision.SerialConsole,
		"FAILURE_DOMAIN":              &cfg.Provision.FailureDomain,
		"REGION":                      &cfg.Provision.Region,
		"PROVIDER_ID":                 &cfg.Provision.ProviderID,
		"MODE":                        &cfg.Mode,
		"OS_FAMILY":                   &cfg.OSFamily,
		"LOG_URL":                     &cfg.Transport.LogURL,
		"INIT_URL":                    &cfg.Transport.InitURL,
		"ERROR_URL":                   &cfg.Transport.ErrorURL,
		"SUCCESS_URL":                 &cfg.Transport.SuccessURL,
		"DEBUG_URL":                   &cfg.Transport.DebugURL,
		"HEARTBEAT_URL":               &cfg.Agent.HeartbeatURL,
		"COMMANDS_URL":                &cfg.Agent.CommandsURL,
		"RESCUE_MODE":                 &cfg.Rescue.Mode,
		"RESCUE_SSH_PUBKEY":           &cfg.Rescue.SSHPubKey,
		"RESCUE_PASSWORD_HASH":        &cfg.Rescue.PasswordHash,
		"CLOUDINIT_DATASOURCE":        &cfg.Provision.CloudInit.Datasource,
		"OVERLAYFS_MODE":              &cfg.Provision.OverlayFS.Mode,
		"OVERLAYFS_DEVICE":            &cfg.Provision.OverlayFS.Device,
		"OVERLAYFS_DIRECTORY":         &cfg.Provision.OverlayFS.Directory,
		"OVERLAYFS_UPPER_DIR":         &cfg.Provision.OverlayFS.UpperDir,
		"OVERLAYFS_WORK_DIR":          &cfg.Provision.OverlayFS.WorkDir,
		"IMAGE_CHECKSUM":              &cfg.Provision.Image.Checksum,
		"IMAGE_CHECKSUM_TYPE":         &cfg.Provision.Image.ChecksumType,
		"IMAGE_MODE":                  &cfg.Provision.Image.Mode,
		"IMAGE_SOURCE_ROOT_LABEL":     &cfg.Provision.Image.SourceRootLabel,
		"DISK_DEVICE":                 &cfg.Provision.Disk.Device,
		"DISK_SERIAL":                 &cfg.Provision.Disk.SerialNumber,
		"DISK_SERIAL_NUMBER":          &cfg.Provision.Disk.SerialNumber,
		"ROOT_PARTITION_LABEL":        &cfg.Provision.Disk.RootPartitionLabel,
		"INVENTORY_URL":               &cfg.Provision.Inventory.URL,
		"FIRMWARE_URL":                &cfg.Provision.Firmware.URL,
		"FIRMWARE_MIN_BIOS":           &cfg.Provision.Firmware.MinBIOS,
		"FIRMWARE_MIN_BMC":            &cfg.Provision.Firmware.MinBMC,
		"HEALTH_SKIP_CHECKS":          &cfg.Health.SkipChecks,
		"HEALTH_CHECK_URL":            &cfg.Health.ReportURL,
		"IMAGE_SIGNATURE_URL":         &cfg.Provision.Image.SignatureURL,
		"IMAGE_GPG_PUBKEY":            &cfg.Provision.Image.GPGPubKey,
		"TELEMETRY_URL":               &cfg.Telemetry.URL,
		"METRICS_URL":                 &cfg.Telemetry.MetricsURL,
		"EVENT_URL":                   &cfg.Telemetry.EventURL,
		"CRASH_ARTIFACTS_PREPARE_URL": &cfg.Provision.CrashArtifacts.PrepareURL,
		"CRASH_ARTIFACTS_UPLOAD_URL":  &cfg.Provision.CrashArtifacts.UploadURL,
		"MOK_CERT_PATH":               &cfg.Provision.SecureBoot.MOKCertPath,
		"MOK_PASSWORD":                &cfg.Provision.SecureBoot.MOKPassword,
		"TOKEN_URL":                   &cfg.Transport.TokenURL,
		"TOKEN_ALGORITHM":             &cfg.Transport.TokenAlgorithm,
		"NVME_NAMESPACES":             &cfg.Provision.Disk.NVMeNamespaces,
		"SYSEXT_DEFAULT_MODE":         &cfg.Provision.Sysext.DefaultMode,
		"SYSEXT_CATALOG_DIR":          &cfg.Provision.Sysext.CatalogDir,
		"SYSEXT_ACTIVE_DIR":           &cfg.Provision.Sysext.ActiveDir,
		"AB_SCHEME":                   &cfg.Provision.AB.Scheme,
		"AB_ACTIVE_SLOT":              &cfg.Provision.AB.ActiveSlot,
		"AB_TARGET_SLOT":              &cfg.Provision.AB.TargetSlot,
		"AB_SOURCE_ROOT_LABEL":        &cfg.Provision.AB.SourceRootLabel,
	}

	if ptr, ok := strFields[key]; ok {
		*ptr = value
		return true
	}
	return false
}

func applyOCIPrePullStringVar(cfg *config.MachineConfig, key, value string) bool {
	switch key {
	case "OCI_PREPULL_CACHE_DIR":
		cfg.Provision.OCIPrePulls.CacheDir = value
	case "OCI_PREPULL_IMPORT_NAMESPACE":
		cfg.Provision.OCIPrePulls.ImportNamespace = value
	default:
		return false
	}
	return true
}

func applySecureBootDigestVar(cfg *config.MachineConfig, key, value string) bool {
	components := map[string]string{
		"SECUREBOOT_SHIM_SHA256":   "shim",
		"SECUREBOOT_GRUB_SHA256":   "grub",
		"SECUREBOOT_KERNEL_SHA256": "kernel",
	}
	component, ok := components[key]
	if !ok {
		return false
	}
	if cfg.Provision.SecureBoot.PinnedDigests == nil {
		cfg.Provision.SecureBoot.PinnedDigests = make(map[string]string)
	}
	cfg.Provision.SecureBoot.PinnedDigests[component] = value
	return true
}

func applyNetworkStringVar(cfg *config.MachineConfig, key, value string) bool {
	strFields := map[string]*string{
		"underlay_subnet":   &cfg.Network.EVPN.UnderlaySubnet,
		"underlay_ip":       &cfg.Network.EVPN.UnderlayIP,
		"overlay_subnet":    &cfg.Network.EVPN.OverlaySubnet,
		"ipmi_subnet":       &cfg.Network.IPMI.Subnet,
		"provision_ip":      &cfg.Network.EVPN.ProvisionIP,
		"provision_gateway": &cfg.Network.EVPN.ProvisionGateway,
		"dns_resolver":      &cfg.Network.DNSResolvers,
		"dcgw_ips":          &cfg.Network.EVPN.DCGWIPs,
		"overlay_aggregate": &cfg.Network.EVPN.OverlayAggregate,
		"vpn_rt":            &cfg.Network.EVPN.VPNRT,
		"STATIC_IP":         &cfg.Network.Static.IP,
		"STATIC_GATEWAY":    &cfg.Network.Static.Gateway,
		"STATIC_IFACE":      &cfg.Network.Static.Iface,
		"BOND_INTERFACES":   &cfg.Network.Bond.Interfaces,
		"BOND_MODE":         &cfg.Network.Bond.Mode,
		"VLANS":             &cfg.Network.VLAN.Config,
		"NETWORK_MODE":      &cfg.Network.Mode,
		"VRF_NAME":          &cfg.Network.VRF.Name,
		"vrf_name":          &cfg.Network.VRF.Name,
		"BGP_PEER_MODE":     &cfg.Network.BGP.PeerMode,
		"BGP_INTERFACES":    &cfg.Network.BGP.Interfaces,
		"BGP_NEIGHBORS":     &cfg.Network.BGP.Neighbors,
		"BGP_AUTH_PASSWORD": &cfg.Network.BGP.AuthPassword,
	}

	if ptr, ok := strFields[key]; ok {
		*ptr = value
		return true
	}
	return false
}

func applyUint32Var(cfg *config.MachineConfig, key, value string) (bool, error) {
	uint32Fields := map[string]*uint32{
		"asn_server":      &cfg.Network.BGP.ASN,
		"provision_vni":   &cfg.Network.EVPN.ProvisionVNI,
		"leaf_asn":        &cfg.Network.EVPN.LeafASN,
		"local_asn":       &cfg.Network.EVPN.LocalASN,
		"VRF_TABLE_ID":    &cfg.Network.VRF.TableID,
		"vrf_table_id":    &cfg.Network.VRF.TableID,
		"BGP_KEEPALIVE":   &cfg.Network.BGP.Keepalive,
		"bgp_keepalive":   &cfg.Network.BGP.Keepalive,
		"BGP_HOLD":        &cfg.Network.BGP.Hold,
		"bgp_hold":        &cfg.Network.BGP.Hold,
		"BFD_TRANSMIT_MS": &cfg.Network.BGP.BFDTransmitMS,
		"bfd_transmit_ms": &cfg.Network.BGP.BFDTransmitMS,
		"BFD_RECEIVE_MS":  &cfg.Network.BGP.BFDReceiveMS,
		"bfd_receive_ms":  &cfg.Network.BGP.BFDReceiveMS,
		"BGP_REMOTE_ASN":  &cfg.Network.BGP.RemoteASN,
		"bgp_remote_asn":  &cfg.Network.BGP.RemoteASN,
	}

	if ptr, ok := uint32Fields[key]; ok {
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return true, fmt.Errorf("invalid uint32 value for %s=%q: %w", key, value, err)
		}
		*ptr = uint32(n)
		return true, nil
	}
	return false, nil
}

func applySpecialVar(cfg *config.MachineConfig, key, value string) error {
	if handled, err := applyBoolIntVar(cfg, key, value); handled {
		return err
	}

	switch key {
	case "IMAGE":
		cfg.Provision.Image.URLs = strings.Fields(strings.ReplaceAll(value, ",", " "))
	case "OCI_PREPULL_IMAGES":
		cfg.Provision.OCIPrePulls.Images = parseOCIPrePullImages(value)
	case "POST_PROVISION_CMDS":
		cfg.Provision.PostProvisionCmds = strings.Split(value, ";")
	case "PARTITION_LAYOUT":
		layout, err := config.ParsePartitionLayout(value)
		if err != nil {
			return fmt.Errorf("invalid PARTITION_LAYOUT: %w", err)
		}
		cfg.Provision.Disk.PartitionLayout = layout
	case "SYSEXT_LAYERS":
		var layers []config.SysextLayerConfig
		if err := unmarshalJSONVar(value, &layers); err != nil {
			return fmt.Errorf("invalid SYSEXT_LAYERS: %w", err)
		}
		cfg.Provision.Sysext.Layers = layers
	case "SECUREBOOT_PINNED_DIGESTS":
		var digests map[string]string
		if err := unmarshalJSONVar(value, &digests); err != nil {
			return fmt.Errorf("invalid SECUREBOOT_PINNED_DIGESTS: %w", err)
		}
		mergeSecureBootPinnedDigests(cfg, digests)
	case "AB_DATA_PARTITIONS":
		var partitions []config.ABDataPartition
		if err := unmarshalJSONVar(value, &partitions); err != nil {
			return fmt.Errorf("invalid AB_DATA_PARTITIONS: %w", err)
		}
		cfg.Provision.AB.DataPartitions = partitions
	default:
		if strings.HasPrefix(key, "LUKS_") {
			return fmt.Errorf("%s is not supported yet", key)
		}
	}

	return nil
}

func mergeSecureBootPinnedDigests(cfg *config.MachineConfig, digests map[string]string) {
	if cfg.Provision.SecureBoot.PinnedDigests == nil {
		cfg.Provision.SecureBoot.PinnedDigests = make(map[string]string, len(digests))
	}
	for component, digest := range digests {
		if _, exists := cfg.Provision.SecureBoot.PinnedDigests[component]; !exists {
			cfg.Provision.SecureBoot.PinnedDigests[component] = digest
		}
	}
}

func unmarshalJSONVar(value string, target any) error {
	err := json.Unmarshal([]byte(value), target)
	if err == nil {
		return nil
	}
	normalized := normalizeGoQuotedJSONWhitespace(value)
	if normalized == value {
		return fmt.Errorf("unmarshal JSON var: %w", err)
	}
	if err := json.Unmarshal([]byte(normalized), target); err != nil {
		return fmt.Errorf("unmarshal normalized JSON var: %w", err)
	}
	return nil
}

func normalizeGoQuotedJSONWhitespace(value string) string {
	var b strings.Builder
	b.Grow(len(value))

	inString := false
	escaped := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if inString {
			b.WriteByte(ch)
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}

		if ch == '"' {
			inString = true
			b.WriteByte(ch)
			continue
		}
		if ch == '\\' && i+1 < len(value) {
			switch value[i+1] {
			case 'n':
				b.WriteByte('\n')
				i++
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				continue
			case 't':
				b.WriteByte('\t')
				i++
				continue
			}
		}
		b.WriteByte(ch)
	}

	return b.String()
}

// applyBoolIntVar handles boolean and integer special vars.
func applyBoolIntVar(cfg *config.MachineConfig, key, value string) (bool, error) {
	if handled, err := applyIntVar(cfg, key, value); handled {
		return true, err
	}
	if applyCrashArtifactsBoolVar(cfg, key, value) {
		return true, nil
	}
	if applyCoreBoolVar(cfg, key, value) {
		return true, nil
	}

	return applyFeatureToggle(cfg, key, value)
}

func applyCoreBoolVar(cfg *config.MachineConfig, key, value string) bool {
	if applyOverlayFSBoolVar(cfg, key, value) {
		return true
	}
	if applyOCIPrePullBoolVar(cfg, key, value) {
		return true
	}
	switch key {
	case "DISABLE_KEXEC":
		cfg.Provision.DisableKexec = parseBoolVar(value)
	case "SECURE_ERASE":
		cfg.Provision.Disk.SecureErase = parseBoolVar(value)
	case "INVENTORY_ENABLED":
		cfg.Provision.Inventory.Enabled = parseBoolVar(value)
	case "FIRMWARE_REPORT":
		cfg.Provision.Firmware.Enabled = parseBoolVar(value)
	case "HEALTH_CHECKS_ENABLED":
		cfg.Health.Enabled = parseBoolVar(value)
	case "CLOUDINIT_ENABLED":
		cfg.Provision.CloudInit.Enabled = parseBoolVar(value)
	case "DRY_RUN":
		cfg.DryRun = parseBoolVar(value)
	case "SYSEXT_ENABLED":
		cfg.Provision.Sysext.Enabled = parseBoolVar(value)
	case "SYSEXT_ALLOW_INSECURE_HTTP":
		cfg.Provision.Sysext.AllowInsecureHTTP = parseBoolVar(value)
	case "AB_PRESERVE_EXISTING":
		cfg.Provision.AB.PreserveExisting = parseBoolVar(value)
	default:
		return false
	}
	return true
}

func applyOverlayFSBoolVar(cfg *config.MachineConfig, key, value string) bool {
	switch key {
	case "OVERLAYFS_ENABLED":
		cfg.Provision.OverlayFS.Enabled = parseBoolVar(value)
	case "OVERLAYFS_RECURSE":
		cfg.Provision.OverlayFS.Recurse = parseBoolVar(value)
	case "OVERLAYFS_SWAP":
		cfg.Provision.OverlayFS.Swap = parseBoolVar(value)
	case "OVERLAYFS_DEBUG":
		cfg.Provision.OverlayFS.Debug = parseBoolVar(value)
	default:
		return false
	}
	return true
}

func applyOCIPrePullBoolVar(cfg *config.MachineConfig, key, value string) bool {
	switch key {
	case "OCI_PREPULL_ENABLED":
		cfg.Provision.OCIPrePulls.Enabled = parseBoolVar(value)
	case "OCI_PREPULL_ALLOW_INSECURE":
		cfg.Provision.OCIPrePulls.AllowInsecure = parseBoolVar(value)
	default:
		return false
	}
	return true
}

func parseOCIPrePullImages(value string) []config.OCIPrePullImageConfig {
	refs := strings.Fields(strings.ReplaceAll(value, ",", " "))
	images := make([]config.OCIPrePullImageConfig, 0, len(refs))
	for _, ref := range refs {
		images = append(images, config.OCIPrePullImageConfig{Reference: ref})
	}
	return images
}

func applyCrashArtifactsBoolVar(cfg *config.MachineConfig, key, value string) bool {
	switch key {
	case "CRASH_ARTIFACTS_ENABLED":
		cfg.Provision.CrashArtifacts.Enabled = parseBoolVar(value)
	case "CRASH_ARTIFACTS_INCLUDE_MEMORY_DUMPS":
		cfg.Provision.CrashArtifacts.IncludeMemoryDumps = parseBoolVar(value)
	default:
		return false
	}
	return true
}

// applyIntVar handles integer special vars.
func applyIntVar(cfg *config.MachineConfig, key, value string) (bool, error) {
	intFields := map[string]*int{
		"MIN_DISK_SIZE_GB":                   &cfg.Provision.Disk.MinSizeGB,
		"NUM_VFS":                            &cfg.Provision.Disk.NumVFs,
		"HEALTH_MIN_MEMORY_GB":               &cfg.Health.MinMemoryGB,
		"HEALTH_MIN_CPUS":                    &cfg.Health.MinCPUs,
		"CRASH_ARTIFACTS_MAX_MB":             &cfg.Provision.CrashArtifacts.MaxMB,
		"CRASH_ARTIFACTS_UPLOAD_TIMEOUT_SEC": &cfg.Provision.CrashArtifacts.UploadTimeoutSec,
		"BGP_MIN_PEERS":                      &cfg.Network.BGP.MinPeers,
		"AB_BOOT_SIZE_MB":                    &cfg.Provision.AB.BootSizeMB,
		"AB_ROOT_SIZE_MB":                    &cfg.Provision.AB.RootSizeMB,
		"AB_STATE_SIZE_MB":                   &cfg.Provision.AB.StateSizeMB,
		"IMAGE_SOURCE_ROOT_PARTITION":        &cfg.Provision.Image.SourceRootPartition,
		"AB_SOURCE_ROOT_PARTITION":           &cfg.Provision.AB.SourceRootPartition,
		"ROOT_PARTITION_NUMBER":              &cfg.Provision.Disk.RootPartitionNumber,
		"OVERLAYFS_TIMEOUT_SEC":              &cfg.Provision.OverlayFS.TimeoutSec,
	}

	if ptr, ok := intFields[key]; ok {
		if err := setIntField(ptr, value); err != nil {
			return true, fmt.Errorf("invalid %s: %w", key, err)
		}
		return true, nil
	}
	return false, nil
}

// applyFeatureToggle handles feature-specific boolean/int vars.
func applyFeatureToggle(cfg *config.MachineConfig, key, value string) (bool, error) {
	switch key {
	case "TELEMETRY_ENABLED":
		cfg.Telemetry.Enabled = parseBoolVar(value)
	case "PERSIST_NETWORK":
		cfg.PersistNetwork = parseBoolVar(value)
	case "CAPRF_LEGACY_COMPAT":
		cfg.LegacyCompat = parseBoolVar(value)
	case "SECUREBOOT_REENABLE":
		cfg.Provision.SecureBoot.ReEnable = parseBoolVar(value)
	case "RESCUE_TIMEOUT":
		if err := setIntField(&cfg.Rescue.Timeout, value); err != nil {
			return true, fmt.Errorf("invalid %s: %w", key, err)
		}
	case "RESCUE_AUTO_MOUNT":
		cfg.Rescue.AutoMountDisks = parseBoolVar(value)
	case "EVPN_L2_ENABLED":
		cfg.Network.EVPN.L2Enabled = parseBoolVar(value)
	case "EVPN_TYPE5_ONLY":
		cfg.Network.EVPN.Type5Only = parseBoolVar(value)
	case "BGP_UNDERLAY_AF":
		cfg.Network.BGP.UnderlayAF = value
	case "BGP_OVERLAY_TYPE":
		cfg.Network.BGP.OverlayType = value
	default:
		return false, nil
	}
	return true, nil
}

// parseBoolVar interprets common truthy string values (case-insensitive).
func parseBoolVar(value string) bool {
	v := strings.ToLower(strings.TrimSpace(value))
	return v == "true" || v == "1" || v == "yes"
}

// setIntField sets an int field from a string value.
func setIntField(field *int, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid integer value %q: %w", value, err)
	}
	*field = n
	return nil
}
