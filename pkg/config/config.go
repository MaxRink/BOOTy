// Package config defines the provisioning configuration types and interfaces.
package config

// Config is the top-level configuration for a BOOTy machine.
// It contains the machine identity, operating mode selector, and
// references to cross-cutting and per-mode configuration sections.
//
// Cross-cutting sections (Network, Transport, Health, Telemetry, Rescue)
// are shared across all modes. Per-mode sections (Provision, Deprovision, Agent)
// contain settings specific to that mode, but some paths (e.g. disk detection)
// currently reuse Provision.Disk for consistency across modes.
type Config struct {
	// Hostname is the machine's network hostname, used for logging,
	// JWT token acquisition, and status reporting.
	// Required for all modes.
	Hostname string `yaml:"hostname" json:"hostname"`

	// Mode selects the operating mode. Determines which per-mode config
	// section is active.
	// Valid values: "provision", "deprovision", "soft-deprovision",
	//              "standby", "dry-run", "check"
	// Legacy aliases: "soft" (= soft-deprovision), "hard" (= deprovision)
	// Default: "provision"
	Mode string `yaml:"mode" json:"mode"`

	// DryRun enables dry-run behavior globally, overriding Mode to "dry-run".
	// When true, no destructive operations are performed.
	// Default: false
	DryRun bool `yaml:"dryRun" json:"dryRun"`

	// PersistNetwork writes explicit network configuration into the provisioned
	// target OS. It requires OSFamily because BOOTy does not auto-detect the
	// target network manager from arbitrary images.
	// Default: false
	PersistNetwork bool `yaml:"persistNetwork" json:"persistNetwork"`

	// OSFamily primarily selects the target OS network writer when
	// PersistNetwork is set. Provisioning preflight also uses it to reject
	// explicitly unsupported target families before destructive storage steps.
	// Valid values: "ubuntu", "rhel", "flatcar".
	// Default: ""
	OSFamily string `yaml:"osFamily" json:"osFamily"`

	// LegacyCompat makes BOOTy behave like the CAPRF legacy shell
	// provisioner where the two differ. CAPRF sets it through
	// CAPRF_LEGACY_COMPAT=true when BOOTy replaces the legacy ramdisk.
	// With it set, BOOTy powers off after a successful provision or
	// deprovision instead of using kexec or a reboot. After a failure it keeps
	// the ramdisk running and waits, like the legacy scripts. For a failed
	// provision this is the default rescue mode; an explicit RESCUE_MODE
	// still wins.
	// Default: false
	LegacyCompat bool `yaml:"legacyCompat" json:"legacyCompat"`

	// Cross-cutting configuration sections.
	Network   NetworkConfig   `yaml:"network"   json:"network"`
	Transport TransportConfig `yaml:"transport" json:"transport"`
	Health    HealthConfig    `yaml:"health"    json:"health"`
	Telemetry TelemetryConfig `yaml:"telemetry" json:"telemetry"`
	Rescue    RescueConfig    `yaml:"rescue"    json:"rescue"`

	// Per-mode configuration sections.
	Provision   ProvisionConfig   `yaml:"provision"   json:"provision"`
	Deprovision DeprovisionConfig `yaml:"deprovision" json:"deprovision"`
	Agent       AgentConfig       `yaml:"agent"       json:"agent"`

	// ISO-injected file lists (populated by the CAPRF client, not from config files).
	ProvisionerFiles []string `yaml:"-" json:"-"`
	MachineFiles     []string `yaml:"-" json:"-"`
	MachineCommands  []string `yaml:"-" json:"-"`
}

// KexecDisabled reports whether BOOTy must not kexec into the installed
// kernel after provisioning.
func (c *Config) KexecDisabled() bool {
	return c.Provision.DisableKexec || c.LegacyCompat
}

// MachineConfig is a type alias for backward compatibility during migration.
// New code should use Config directly.
type MachineConfig = Config
