package config

// TelemetryConfig controls observability data collection and reporting.
// Metrics and events are POSTed to the configured endpoints during provisioning.
type TelemetryConfig struct {
	// Enabled activates provisioning metrics collection.
	// Default: false
	Enabled bool `yaml:"enabled" json:"enabled"`

	// URL is the POST endpoint for telemetry metric snapshots.
	// Default: ""
	URL string `yaml:"url" json:"url"`

	// MetricsURL is the POST endpoint for structured provisioning metrics.
	// Default: ""
	MetricsURL string `yaml:"metricsURL" json:"metricsURL"`

	// EventURL is the POST endpoint for provisioning lifecycle events.
	// Default: ""
	EventURL string `yaml:"eventURL" json:"eventURL"`

	// OOBStatus selects the out-of-band status channel.
	// "serial": a BOOTY1 status line on the active consoles at each phase
	// change and at the end of a run. "off": none.
	// Valid values: "serial", "off". Default: "" (serial)
	OOBStatus string `yaml:"oobStatus" json:"oobStatus"`

	// OOBEFIVar writes the result of a run to the BootyLastResult UEFI
	// variable when it changes, and logs the previous result at start.
	// Default: nil (false)
	OOBEFIVar *bool `yaml:"oobEFIVar,omitempty" json:"oobEFIVar,omitempty"`
}

// Values of TelemetryConfig.OOBStatus.
const (
	OOBStatusSerial = "serial"
	OOBStatusOff    = "off"
)
