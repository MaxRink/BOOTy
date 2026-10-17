package config

import "context"

// Status represents the provisioning status reported to the server.
type Status string

// Provisioning status constants.
const (
	StatusInit    Status = "init"
	StatusSuccess Status = "success"
	StatusError   Status = "error"
	// StatusRebootRequired tells the server that BOOTy reboots into the
	// ramdisk again before it finishes the task (REBOOT_URL).
	StatusRebootRequired Status = "reboot-required"
)

// Crash artifact collection defaults.
const (
	DefaultCrashArtifactsMaxMB            = 256
	DefaultCrashArtifactsUploadTimeoutSec = 120
)

// Command represents a server-issued command (agent/standby mode).
type Command struct {
	ID      string
	Type    string
	Payload []byte
}

// Provider abstracts provisioning server communication.
type Provider interface {
	// GetConfig fetches machine configuration.
	GetConfig(ctx context.Context) (*Config, error)
	// ReportStatus sends provisioning status to the server.
	ReportStatus(ctx context.Context, status Status, message string) error
	// ShipLog sends a log line to the server.
	ShipLog(ctx context.Context, message string) error
	// Heartbeat sends a keepalive signal (standby/agent mode).
	Heartbeat(ctx context.Context) error
	// FetchCommands retrieves pending commands (standby/agent mode).
	FetchCommands(ctx context.Context) ([]Command, error)
	// AcknowledgeCommand reports command execution result back to the server.
	AcknowledgeCommand(ctx context.Context, cmdID, status, message string) error
	// ReportInventory sends hardware inventory data to the server.
	ReportInventory(ctx context.Context, data []byte) error
	// ReportFirmware sends a firmware report to the server.
	ReportFirmware(ctx context.Context, data []byte) error
}

// TelemetryReporter is an optional provider capability for reporting metrics.
type TelemetryReporter interface {
	ReportMetrics(context.Context, []byte) error
}

// EventReporter is an optional provider capability for reporting lifecycle events.
type EventReporter interface {
	SendEvent(context.Context, []byte) error
}
