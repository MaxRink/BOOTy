package config

const (
	// TargetOSLinux is the only explicit target OS class accepted by the
	// current Linux/GRUB-oriented provisioning flow.
	TargetOSLinux = "linux"
)

// Values of ProvisionConfig.MachineCommandsMode.
const (
	MachineCommandsCommand = "command"
	MachineCommandsScript  = "script"
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
	// A console= parameter here is treated as explicit serial console intent
	// and is folded into the resolved console instead of being appended
	// verbatim, so the installed system always gets exactly one console.
	// Default: ""
	ExtraKernelParams string `yaml:"extraKernelParams" json:"extraKernelParams"`

	// SerialConsole explicitly pins the serial console of the provisioned
	// system, for example "ttyS1,115200n8" or "ttyS0". It overrides all
	// firmware evidence (ACPI SPCR, device tree, sysfs, DMI).
	// Set it to "none" to configure no serial console at all.
	// Default: "" (resolve from firmware evidence)
	SerialConsole string `yaml:"serialConsole" json:"serialConsole"`

	// GRUBCmdlineMode selects how BOOTy writes the kernel parameters to
	// /etc/default/grub.d/10-caprf-kernel-params.cfg.
	// "resolved": the serial console resolver picks at most one console;
	// for that console BOOTy writes console=, a GRUB serial terminal and
	// getty units (none with SerialConsole "none"); update-grub must exist.
	// "append": like provision.sh of the CAPRF legacy provisioner, append
	// "ds=nocloud console=ttyS0,115200n8 <ExtraKernelParams>" (ttyS1 on
	// Lenovo) to the image GRUB_CMDLINE_LINUX, run update-grub only when
	// the image has it, and write no getty units. SerialConsole is ignored.
	// Valid values: "resolved", "append". Preset by legacyCompat: "append".
	// Default: "" (resolved)
	GRUBCmdlineMode string `yaml:"grubCmdlineMode" json:"grubCmdlineMode"`

	// MachineCommandsMode selects how run-machine-commands runs the files in
	// /deploy/machine-commands, in CAPRF index order.
	// "command": each file holds one command without shell metacharacters.
	// "script": like run_script_in_chroot of the CAPRF legacy provisioner,
	// each file runs as a script in the target root, with its #! interpreter
	// or /bin/sh.
	// Valid values: "command", "script". Preset by legacyCompat: "script".
	// Default: "" (command)
	MachineCommandsMode string `yaml:"machineCommandsMode" json:"machineCommandsMode"`

	// FailureDomain maps to topology.kubernetes.io/zone for the provisioned node.
	// Default: ""
	FailureDomain string `yaml:"failureDomain" json:"failureDomain"`

	// Region is the geographic region identifier for the node.
	// Default: ""
	Region string `yaml:"region" json:"region"`

	// ProviderID is the kubelet --provider-id value for the provisioned node.
	// Default: ""
	ProviderID string `yaml:"providerID" json:"providerID"`

	// WriteResolvConf writes the installed OS /etc/resolv.conf from
	// dns_resolver. false keeps the resolv.conf of the image, like the CAPRF
	// legacy scripts.
	// Preset by legacyCompat: false.
	// Default: nil (true)
	WriteResolvConf *bool `yaml:"writeResolvConf,omitempty" json:"writeResolvConf,omitempty"`

	// DiskLinks creates the /dev/disk/by-uuid, by-partuuid, by-label and
	// by-partlabel links in the ramdisk before the target chroot, like udev
	// on the installed system. update-grub then finds the root UUID and
	// writes root=UUID= instead of root=/dev/sdXN, which breaks the boot
	// when the disk order changes.
	// Default: nil (true)
	DiskLinks *bool `yaml:"diskLinks,omitempty" json:"diskLinks,omitempty"`

	// KexecUnmountTarget unmounts every target mount after the kexec kernel
	// is loaded and before kexec runs. A busy mount of a block device is
	// remounted read-only; a busy virtual mount is left as it is.
	// The installed OS then starts with clean filesystems instead of a
	// journal that needs recovery.
	// Default: nil (true)
	KexecUnmountTarget *bool `yaml:"kexecUnmountTarget,omitempty" json:"kexecUnmountTarget,omitempty"`

	// EFIBootEntryRequired makes EFI boot entry management mandatory on
	// systems booted via UEFI, like the CAPRF legacy scripts. false turns
	// only "EFI variables unavailable" failures (efivarfs, a missing
	// efibootmgr, unreadable variables, a refused delete) into warnings; a
	// failed efibootmgr -c or a missing EFI loader always fails the run.
	// BIOS systems skip these steps, and MODE=decommission is always best
	// effort.
	// Default: nil (true)
	EFIBootEntryRequired *bool `yaml:"efiBootEntryRequired,omitempty" json:"efiBootEntryRequired,omitempty"`

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
	OverlayFS      OverlayFSConfig      `yaml:"overlayFS"      json:"overlayFS"`
	Sysext         SysextConfig         `yaml:"sysext"         json:"sysext"`
	OCIPrePulls    OCIPrePullConfig     `yaml:"ociPrePulls"    json:"ociPrePulls"`
	AB             ABConfig             `yaml:"ab"             json:"ab"`
	CrashArtifacts CrashArtifactsConfig `yaml:"crashArtifacts" json:"crashArtifacts"`
	Inventory      InventoryConfig      `yaml:"inventory"      json:"inventory"`
}
