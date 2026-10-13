package config

// FirmwareConfig controls hardware firmware version reporting and enforcement.
// When enabled, BOOTy collects firmware versions (BIOS, BMC) and reports them
// to the configured endpoint. Minimum version checks can block provisioning.
type FirmwareConfig struct {
	// Enabled activates firmware collection and reporting.
	// Default: false
	Enabled bool `yaml:"enabled" json:"enabled"`

	// URL is the POST endpoint for the firmware version report.
	// Default: ""
	URL string `yaml:"url" json:"url"`

	// MinBIOS is the minimum acceptable BIOS/UEFI firmware version.
	// Provisioning fails if the detected version is older.
	// Default: "" (no minimum)
	MinBIOS string `yaml:"minBIOS" json:"minBIOS"`

	// MinBMC is the minimum acceptable BMC/iLO/iDRAC firmware version.
	// Default: "" (no minimum)
	MinBMC string `yaml:"minBMC" json:"minBMC"`

	// LogNICs logs a nic_firmware line with the firmware version of every
	// NIC on each provision, like provision.sh of the CAPRF legacy
	// provisioner. It does not need Enabled.
	// Preset by legacyCompat: true.
	// Default: nil (false)
	LogNICs *bool `yaml:"logNICs,omitempty" json:"logNICs,omitempty"`

	// NumVFsMode selects how setup-mellanox applies Disk.NumVFs.
	// "set": only with NumVFs > 0, set SRIOV_EN and NUM_OF_VFS on each
	// Mellanox NIC; a missing NIC or mstconfig fails the run.
	// "raise": on every provision, raise NUM_OF_VFS to NumVFs (32 when
	// NumVFs is 0) on each ConnectX-4/5/6/6 Dx/6 Lx/7 Ethernet function, like
	// setup_mellanox_num_of_vfs; skip quietly without mstconfig or such a NIC.
	// Valid values: "set", "raise". Preset by legacyCompat: "raise".
	// Default: "" (set)
	NumVFsMode string `yaml:"numVFsMode" json:"numVFsMode"`
}
