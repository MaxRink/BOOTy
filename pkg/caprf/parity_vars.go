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
	}
	if ptr, ok := boolFields[key]; ok {
		b := parseBoolVar(value)
		*ptr = &b
		return true
	}
	return false
}
