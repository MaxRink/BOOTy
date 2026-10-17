package caprf

import "github.com/telekom/BOOTy/pkg/config"

// applyParityVar sets the options that select CAPRF legacy behavior
// without CAPRF_LEGACY_COMPAT. The preset fills the ones that the vars file
// does not set (config.Config.ApplyPresets).
func applyParityVar(cfg *config.MachineConfig, key, value string) bool {
	strFields := map[string]*string{
		"SUCCESS_ACTION":           &cfg.SuccessAction,
		"FAILURE_ACTION":           &cfg.FailureAction,
		"DISK_SELECTION":           &cfg.Provision.Disk.Selection,
		"PROVISIONER_FILES_TARGET": &cfg.ProvisionerFilesTarget,
		"GRUB_CMDLINE_MODE":        &cfg.Provision.GRUBCmdlineMode,
		"NUM_VFS_MODE":             &cfg.Provision.Firmware.NumVFsMode,
		"WIPE_DEPTH":               &cfg.Provision.Disk.WipeDepth,
		"OOB_STATUS":               &cfg.Telemetry.OOBStatus,
		"RAID_SUPERBLOCK_CLEAR":    &cfg.Provision.Disk.RAIDSuperblockClear,
		"GROW_PARTITION":           &cfg.Provision.Disk.GrowPartition,
		"NETWORK_WAIT_MODE":        &cfg.Network.WaitMode,
		"MACHINE_COMMANDS_MODE":    &cfg.Provision.MachineCommandsMode,
	}
	if ptr, ok := strFields[key]; ok {
		*ptr = value
		return true
	}
	// Bool options are pointers, so an explicit false wins over the preset.
	boolFields := map[string]**bool{
		"WIPE_ALL_DISKS":            &cfg.Provision.Disk.WipeAllDisks,
		"DISCARD_NVME":              &cfg.Provision.Disk.DiscardNVMe,
		"INSECURE_TRANSPORT":        &cfg.Transport.Insecure,
		"SKIP_TLS_VERIFY":           &cfg.Transport.SkipTLSVerify,
		"EXPORT_VARS":               &cfg.Transport.ExportVars,
		"IMAGE_ALLOW_INSECURE_HTTP": &cfg.Provision.Image.AllowInsecureHTTP,
		"WRITE_RESOLV_CONF":         &cfg.Provision.WriteResolvConf,
		"RAMDISK_HOSTNAME":          &cfg.RamdiskHostname,
		"NIC_FIRMWARE_LOG":          &cfg.Provision.Firmware.LogNICs,
		"DISK_LINKS":                &cfg.Provision.DiskLinks,
		"KEXEC_UNMOUNT_TARGET":      &cfg.Provision.KexecUnmountTarget,
		"PREDICTABLE_NIC_NAMES":     &cfg.Network.PredictableNames,
		"EFI_BOOT_ENTRY_REQUIRED":   &cfg.Provision.EFIBootEntryRequired,
	}
	if ptr, ok := boolFields[key]; ok {
		b := parseBoolVar(value)
		*ptr = &b
		return true
	}
	return false
}
