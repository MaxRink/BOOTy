package config

const (
	// TargetOSLinux is the only explicit target OS class accepted by the
	// current Linux/GRUB-oriented provisioning flow.
	TargetOSLinux = "linux"
)

// ProvisionConfig holds all settings specific to the provisioning pipeline.
// This section is only read when Mode is "provision" or "dry-run".
// It nests sub-configs for image delivery, disk management, firmware,
// cloud-init, and other provisioning-specific concerns.
type ProvisionConfig struct {
	// TargetOS declares the provisioned image OS class for fail-fast support
	// checks before destructive storage steps.
	// Valid value: "linux". Empty values fail provisioning preflight.
	// Windows, VMware ESXi, and unknown values are rejected.
	// Default: ""
	TargetOS string `yaml:"targetOS" json:"targetOS"`

	// ExtraKernelParams are additional kernel command-line parameters
	// appended to the installed OS's boot entry.
	// Default: ""
	ExtraKernelParams string `yaml:"extraKernelParams" json:"extraKernelParams"`

	// FailureDomain maps to topology.kubernetes.io/zone for the provisioned node.
	// Default: ""
	FailureDomain string `yaml:"failureDomain" json:"failureDomain"`

	// Region is the geographic region identifier for the node.
	// Default: ""
	Region string `yaml:"region" json:"region"`

	// ProviderID is the kubelet --provider-id value for the provisioned node.
	// Default: ""
	ProviderID string `yaml:"providerID" json:"providerID"`

	// DisableKexec skips kexec into the installed kernel and forces a hard reboot.
	// Automatically set to true in dry-run mode.
	// Default: false
	DisableKexec bool `yaml:"disableKexec" json:"disableKexec"`

	// PostProvisionCmds are commands executed in chroot after provisioning completes.
	// In vars format these are semicolon-separated; in YAML they are a list.
	// Default: [] (none)
	PostProvisionCmds []string `yaml:"postProvisionCmds" json:"postProvisionCmds"`

	// Nested provisioning sub-configs.
	Image          ImageConfig          `yaml:"image"          json:"image"`
	Disk           DiskConfig           `yaml:"disk"           json:"disk"`
	Firmware       FirmwareConfig       `yaml:"firmware"       json:"firmware"`
	SecureBoot     SecureBootConfig     `yaml:"secureBoot"     json:"secureBoot"`
	CloudInit      CloudInitConfig      `yaml:"cloudInit"      json:"cloudInit"`
	Sysext         SysextConfig         `yaml:"sysext"         json:"sysext"`
	OCIPrePulls    OCIPrePullConfig     `yaml:"ociPrePulls"    json:"ociPrePulls"`
	AB             ABConfig             `yaml:"ab"             json:"ab"`
	CrashArtifacts CrashArtifactsConfig `yaml:"crashArtifacts" json:"crashArtifacts"`
	Inventory      InventoryConfig      `yaml:"inventory"      json:"inventory"`
}
