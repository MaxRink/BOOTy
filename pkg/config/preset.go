package config

import "strings"

// Values of SuccessAction and FailureAction.
const (
	SuccessActionReboot   = "reboot"
	SuccessActionPowerOff = "poweroff"
	FailureActionReboot   = "reboot"
	FailureActionWait     = "wait"
)

// Values of ProvisionerFilesTarget.
const (
	ProvisionerFilesTarget  = "target"
	ProvisionerFilesRamdisk = "ramdisk"
)

// Values of ProvisionConfig.GRUBCmdlineMode.
const (
	GRUBCmdlineResolved = "resolved"
	GRUBCmdlineAppend   = "append"
)

// Values of FirmwareConfig.NumVFsMode.
const (
	NumVFsModeSet   = "set"
	NumVFsModeRaise = "raise"
)

// Values of DiskConfig.GrowPartition.
const (
	GrowPartitionRoot      = "root"
	GrowPartitionLastLinux = "last-linux"
)

// Values of DiskConfig.Selection.
const (
	DiskSelectionPreferNVMe = "prefer-nvme"
	DiskSelectionLsblk      = "lsblk"
)

// Values of DiskConfig.WipeDepth.
const (
	WipeDepthThorough = "thorough"
	WipeDepthQuick    = "quick"
)

// ApplyPresets sets the options that a preset such as LegacyCompat implies.
// It only sets options that are not set, so an explicit option always wins.
// Validate calls it first. It is safe to call more than once.
func (c *Config) ApplyPresets() {
	if c.LegacyCompat {
		c.applyLegacyCompatPreset()
	}
}

// applyLegacyCompatPreset holds the option values of CAPRF_LEGACY_COMPAT,
// which make BOOTy behave like the CAPRF legacy shell provisioner.
// docs/caprf-legacy-migration.md lists them in the preset column.
func (c *Config) applyLegacyCompatPreset() {
	presetString(&c.Network.WaitMode, NetworkWaitRounds)
	presetString(&c.SuccessAction, SuccessActionPowerOff)
	presetString(&c.FailureAction, FailureActionWait)
	presetString(&c.Provision.Disk.Selection, DiskSelectionLsblk)
	presetString(&c.Provision.Disk.WipeDepth, WipeDepthQuick)
	presetBool(&c.Provision.Disk.WipeAllDisks, true)
	presetBool(&c.Provision.Disk.DiscardNVMe, true)
	// The legacy scripts use `curl --insecure`, accept plain HTTP URLs and
	// `source vars`.
	presetBool(&c.Transport.Insecure, true)
	presetBool(&c.Transport.SkipTLSVerify, true)
	presetBool(&c.Provision.Image.AllowInsecureHTTP, true)
	presetBool(&c.Transport.ExportVars, true)
	presetString(&c.ProvisionerFilesTarget, ProvisionerFilesRamdisk)
	presetBool(&c.Provision.WriteResolvConf, false)
	presetBool(&c.RamdiskHostname, true)
	presetString(&c.Provision.GRUBCmdlineMode, GRUBCmdlineAppend)
	presetBool(&c.Provision.Firmware.LogNICs, true)
	presetString(&c.Provision.Firmware.NumVFsMode, NumVFsModeRaise)
	presetString(&c.Provision.Disk.GrowPartition, GrowPartitionLastLinux)
}

// presetString sets an unset (blank) string option.
func presetString(field *string, value string) {
	if strings.TrimSpace(*field) == "" {
		*field = value
	}
}

// presetBool sets an unset bool option. Options that a preset can set use
// *bool, so an explicit false wins over the preset.
func presetBool(field **bool, value bool) {
	if *field == nil {
		*field = &value
	}
}

// BoolOr returns *b, or def when b is nil.
func BoolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}
