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
	//              "decommission", "standby", "dry-run", "check"
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
	// It is a preset: Validate sets every option that it implies and that
	// is not set explicitly. See applyLegacyCompatPreset for the list.
	// Default: false
	LegacyCompat bool `yaml:"legacyCompat" json:"legacyCompat"`

	// SuccessAction is what BOOTy does after a successful provision,
	// deprovision or decommission.
	// "reboot": provision kexecs into the installed kernel, or reboots when
	// kexec is disabled or fails. The other modes reboot.
	// "poweroff": power off in all modes, never kexec.
	// Valid values: "reboot", "poweroff". Preset by legacyCompat: "poweroff".
	// Default: "" (reboot)
	SuccessAction string `yaml:"successAction" json:"successAction"`

	// FailureAction is what BOOTy does after a failed run.
	// "reboot": reboot the ramdisk.
	// "wait": report the error to ERROR_URL and keep the ramdisk running.
	// It applies to a failed deprovision, to a failure before the mode runs
	// (vars file, network, JWT) and to exhausted JWT renewal. For a failed
	// provision it is the default of Rescue.Mode; an explicit Rescue.Mode
	// wins.
	// Valid values: "reboot", "wait". Preset by legacyCompat: "wait".
	// Default: "" (reboot)
	FailureAction string `yaml:"failureAction" json:"failureAction"`

	// ProvisionerFilesTarget is where BOOTy copies the CAPRF provisioner
	// files (/deploy/file-system).
	// "target": into the installed OS (copy-provisioner-files step).
	// "ramdisk": into the ramdisk root before the network starts, like
	// copy_files of the CAPRF legacy scripts. A provisioner /etc/resolv.conf
	// then stays in the ramdisk, and the files do not reach the installed OS.
	// Valid values: "target", "ramdisk". Preset by legacyCompat: "ramdisk".
	// Default: "" (target)
	ProvisionerFilesTarget string `yaml:"provisionerFilesTarget" json:"provisionerFilesTarget"`

	// RamdiskHostname sets Hostname as the ramdisk host name for
	// provisioning, like `hostname "$HOSTNAME"` in the CAPRF legacy
	// provision.sh. A failure is only logged.
	// Preset by legacyCompat: true.
	// Default: nil (false)
	RamdiskHostname *bool `yaml:"ramdiskHostname,omitempty" json:"ramdiskHostname,omitempty"`

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
	return c.Provision.DisableKexec || c.PowerOffOnSuccess()
}

// PowerOffOnSuccess reports whether BOOTy powers off after a successful run.
func (c *Config) PowerOffOnSuccess() bool {
	return c.SuccessAction == SuccessActionPowerOff
}

// ProvisionerFilesInRamdisk reports whether the provisioner files go into
// the ramdisk instead of the installed OS.
func (c *Config) ProvisionerFilesInRamdisk() bool {
	return c.ProvisionerFilesTarget == ProvisionerFilesRamdisk
}

// WaitOnFailure reports whether BOOTy reports a failed run and keeps the
// ramdisk running instead of rebooting.
func (c *Config) WaitOnFailure() bool {
	return c.FailureAction == FailureActionWait
}

// MachineConfig is a type alias for backward compatibility during migration.
// New code should use Config directly.
type MachineConfig = Config
