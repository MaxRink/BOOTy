package config

// DiskConfig defines target disk selection, preparation, and layout settings.
// Used during provisioning for RAID assembly, NVMe namespace management,
// disk detection, wiping, and partitioning.
//
// Processing order: NVMe namespaces → RAID creation → disk detection → wipe → partition.
type DiskConfig struct {
	// Device overrides automatic disk detection with an explicit block device path.
	// When RAID is configured, this typically points to the resulting /dev/mdX device.
	// Example: "/dev/sda", "/dev/nvme0n1", "/dev/md0", "/dev/loop0"
	// Default: "" (auto-detect largest non-removable disk, or RAID device if configured)
	Device string `yaml:"device" json:"device"`

	// SerialNumber selects a target disk by its sysfs device serial number when
	// Device is unset. This is intended for provider-level disk selectors where
	// the kernel device name is not stable across boots.
	// Default: "" (do not select by serial)
	SerialNumber string `yaml:"serialNumber" json:"serialNumber"`

	// RootPartitionLabel selects the streamed whole-disk image root partition by
	// GPT partition name/PARTLABEL. Use this when the image contains multiple
	// Linux filesystem partitions and the root partition is not named "root" or "/".
	// Mutually exclusive with RootPartitionNumber.
	// Default: "" (auto-detect)
	RootPartitionLabel string `yaml:"rootPartitionLabel" json:"rootPartitionLabel"`

	// RootPartitionNumber selects the streamed whole-disk image root partition by
	// 1-based partition number. Use this only when the image does not provide a
	// stable GPT partition name/PARTLABEL. Mutually exclusive with RootPartitionLabel.
	// Default: 0 (auto-detect)
	RootPartitionNumber int `yaml:"rootPartitionNumber" json:"rootPartitionNumber"`

	// MinSizeGB sets the minimum acceptable disk size in GiB.
	// Disks smaller than this are rejected during auto-detection.
	// Default: 0 (no minimum)
	MinSizeGB int `yaml:"minSizeGB" json:"minSizeGB"`

	// Selection selects the target disk when Device and SerialNumber are unset.
	// "prefer-nvme": NVMe disks first, then the largest disk; removable media
	// are skipped unless BOOTY_ALLOW_REMOVABLE=true.
	// "lsblk": the first disk of at least MinSizeGB in lsblk order, NVMe and
	// virtio disks first, then SCSI/SATA disks, like _select_single_disk of
	// the CAPRF legacy scripts. Removable media are not skipped.
	// Valid values: "prefer-nvme", "lsblk". Preset by legacyCompat: "lsblk".
	// Default: "" (prefer-nvme)
	Selection string `yaml:"selection" json:"selection"`

	// WipeAllDisks makes wipe-disks wipe every disk instead of the target
	// disk, for provisioning and for hard deprovisioning without Device,
	// like wipe_all_disks of the CAPRF legacy scripts. Failures are logged
	// and do not stop the run. SecureErase has no effect then. It does not
	// apply to dry-run, RAID configs and A/B preserve-existing upgrades.
	// Preset by legacyCompat: true.
	// Default: nil (false)
	WipeAllDisks *bool `yaml:"wipeAllDisks,omitempty" json:"wipeAllDisks,omitempty"`

	// DiscardNVMe runs blkdiscard on every NVMe disk before a wipe of all
	// disks (WipeAllDisks or MODE=decommission). Failures are logged.
	// Preset by legacyCompat: true.
	// Default: nil (false)
	DiscardNVMe *bool `yaml:"discardNVMe,omitempty" json:"discardNVMe,omitempty"`

	// WipeDepth selects how wipe-disks clears a disk when SecureErase is off.
	// "thorough": stop the LVM, dm-crypt and MD devices on the disk, remove
	// LVM PV and MD superblocks, the signatures and the first and last MiB of
	// every partition, then the partition table with the GPT backup, the
	// disk signatures and the first and last MiB of the disk, and discard
	// the disk where supported. Restoring the old partition table then finds
	// no file system, PV or array.
	// "quick": sgdisk --zap-all and wipefs on the disk only. Partition
	// contents come back when the old partition table is restored.
	// Valid values: "thorough", "quick". Preset by legacyCompat: "quick",
	// because the legacy wipe_all_disks clears only disk signatures.
	// Default: "" (thorough)
	WipeDepth string `yaml:"wipeDepth" json:"wipeDepth"`

	// RAIDSuperblockClear selects the disks on which provisioning zeroes the
	// MD superblock of every linux_raid_member after stop-raid stopped the
	// arrays. The quick wipe keeps metadata 1.0 superblocks at the end of old
	// member partitions, which a replicated or restored partition table
	// brings back.
	// "wiped": only the disks that wipe-disks wipes, right before the wipe:
	// the target disk and its mirror disk, or every disk with WipeAllDisks.
	// "all": every disk, in stop-raid, like clear_mdadm_superblocks of the
	// CAPRF legacy provisioner.
	// Hard deprovision wipes the deprovision device and its mirror companion,
	// or every disk, and zeroes the superblocks on these disks with both values.
	// Valid values: "wiped", "all". Preset by legacyCompat: "all".
	// Default: "" (wiped)
	RAIDSuperblockClear string `yaml:"raidSuperblockClear" json:"raidSuperblockClear"`

	// GrowPartition selects the partition that grow-partition and
	// resize-filesystem enlarge to fill the disk.
	// "root": the root partition.
	// "last-linux": the last GPT Linux filesystem partition, like
	// provision.sh of the CAPRF legacy provisioner, also when it is a data
	// partition after the root. That partition gets e2fsck -p -f before
	// growpart and an offline resize2fs; the root keeps its size. Without a
	// GPT Linux partition BOOTy grows the root.
	// Valid values: "root", "last-linux". Preset by legacyCompat: "last-linux".
	// Default: "" (root)
	GrowPartition string `yaml:"growPartition" json:"growPartition"`

	// SettleTimeoutSec bounds the wait for the block devices before the
	// target disk is selected and before a wipe of all disks: BOOTy waits
	// until the set of disks in /sys/block did not change for 2 seconds.
	// SAS/RAID HBA drivers scan asynchronously after the module load.
	// A configured device path (Device, PartitionLayout.Device, the
	// deprovision device) or SerialNumber that the selection uses is also
	// waited for, within the same timeout.
	// 0 disables the wait. Valid values: 0 to 600.
	// Default: nil (30)
	SettleTimeoutSec *int `yaml:"settleTimeoutSec,omitempty" json:"settleTimeoutSec,omitempty"`

	// SecureErase uses ATA Secure Erase or NVMe Format instead of wipefs
	// for disk sanitization before provisioning.
	// Default: false (use wipefs)
	SecureErase bool `yaml:"secureErase" json:"secureErase"`

	// NumVFs is the number of SR-IOV Virtual Functions to create on Mellanox NICs.
	// Applied during the Mellanox firmware configuration step.
	// Default: 0 (skip SR-IOV firmware changes)
	NumVFs int `yaml:"numVFs" json:"numVFs"`

	// NVMeNamespaces is a JSON configuration string for NVMe namespace creation.
	// Parsed and applied before disk detection. Allows creating namespaces on
	// NVMe drives that ship with no default namespace.
	// Default: "" (no namespace management)
	NVMeNamespaces string `yaml:"nvmeNamespaces" json:"nvmeNamespaces"`

	// MirroringMode mirrors the installed root filesystem onto a pair of
	// equally sized disks with software RAID1, matching the CAPRF
	// RedfishHost diskMirroringMode field.
	// Valid values: "raid-1" (require a matching pair), "auto" (mirror when a
	// matching pair exists, single disk otherwise), "none" or "" (single disk).
	// Ignored when Device or SerialNumber selects the target disk. Rejected
	// before any disk is changed when combined with RAID, PartitionLayout or
	// an image mode other than whole-disk (such as A/B).
	// Default: "" (single disk)
	MirroringMode string `yaml:"mirroringMode" json:"mirroringMode"`

	// RAID defines software RAID arrays to create before partitioning.
	// Existing arrays are stopped first, then new arrays are assembled.
	// The resulting /dev/mdX device becomes available for Device or auto-detection.
	// Default: [] (no RAID)
	RAID []RAIDConfig `yaml:"raid" json:"raid"`

	// PartitionLayout defines declarative GPT partitioning.
	// When set with whole-disk image mode, BOOTy creates this layout and streams
	// the selected source root filesystem into the declared root partition.
	// Default: nil
	PartitionLayout *PartitionLayout `yaml:"partitionLayout" json:"partitionLayout"`
}

// RAIDConfig defines a single software RAID array to create via mdadm.
// Arrays are created in order after existing arrays are stopped.
type RAIDConfig struct {
	// Name is the mdadm array name WITHOUT the /dev/ prefix.
	// The disk manager prepends /dev/ automatically.
	// Example: "md0", "md/boot"
	// Required.
	Name string `yaml:"name" json:"name"`

	// Level is the RAID level.
	// Valid values: 0, 1, 5, 6, 10
	// Required.
	Level int `yaml:"level" json:"level"`

	// Devices is the list of member block devices for the array.
	// Minimum 2 devices required.
	// Example: ["/dev/sda", "/dev/sdb"]
	// Required.
	Devices []string `yaml:"devices" json:"devices"`
}
