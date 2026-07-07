package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestStatusConstants(t *testing.T) {
	tests := []struct {
		status Status
		want   string
	}{
		{StatusInit, "init"},
		{StatusSuccess, "success"},
		{StatusError, "error"},
	}

	for _, tt := range tests {
		if string(tt.status) != tt.want {
			t.Errorf("Status = %q, want %q", tt.status, tt.want)
		}
	}
}

func TestMachineConfigDefaults(t *testing.T) {
	cfg := &MachineConfig{}

	if cfg.Provision.Disk.MinSizeGB != 0 {
		t.Errorf("expected 0 min disk size, got %d", cfg.Provision.Disk.MinSizeGB)
	}
	if cfg.Hostname != "" {
		t.Errorf("expected empty hostname, got %s", cfg.Hostname)
	}
	if cfg.Provision.Image.URLs != nil {
		t.Errorf("expected nil image URLs, got %v", cfg.Provision.Image.URLs)
	}
	if DefaultCrashArtifactsMaxMB != 256 {
		t.Errorf("DefaultCrashArtifactsMaxMB = %d, want 256", DefaultCrashArtifactsMaxMB)
	}
	if DefaultCrashArtifactsUploadTimeoutSec != 120 {
		t.Errorf("DefaultCrashArtifactsUploadTimeoutSec = %d, want 120", DefaultCrashArtifactsUploadTimeoutSec)
	}
	prePulls := cfg.Provision.OCIPrePulls.WithDefaults()
	if prePulls.CacheDir != DefaultOCIPrePullCacheDir {
		t.Errorf("OCIPrePulls.CacheDir = %q, want %q", prePulls.CacheDir, DefaultOCIPrePullCacheDir)
	}
	if prePulls.ImportNamespace != DefaultOCIPrePullImportNamespace {
		t.Errorf("OCIPrePulls.ImportNamespace = %q, want %q", prePulls.ImportNamespace, DefaultOCIPrePullImportNamespace)
	}
}

func TestParsePartitionLayoutRootInLVM(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/"}]}}`
	layout, err := ParsePartitionLayout(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if layout.LVM == nil {
		t.Fatal("expected lvm config")
	}
}

func TestParsePartitionLayoutMissingRootEverywhere(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"data","filesystem":"xfs","mountpoint":"/data"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error when no root mountpoint exists")
	}
}

func TestParsePartitionLayoutUnsupportedPartitionFilesystem(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","filesystem":"ntfs","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for unsupported partition filesystem")
	}
}

func TestParsePartitionLayoutUnsupportedLVMFilesystem(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","filesystem":"btrfs","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for unsupported lvm filesystem")
	}
}

func TestParsePartitionLayoutLvmPVPartitionExceedsCount(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":2,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for pvPartition exceeding partition count")
	}
}

func TestParsePartitionLayoutLvmPVPartitionMustNotDefineFilesystem(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192,"filesystem":"ext4"}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error when pv partition defines a filesystem")
	}
}

func TestParsePartitionLayoutLvmNegativeLVSize(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","sizeMB":-1,"filesystem":"ext4","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for negative lvm volume size")
	}
}

func TestParsePartitionLayoutLvmSizeAndExtentsExclusive(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","sizeMB":1024,"extents":"100%FREE","filesystem":"ext4","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error when lvm volume sets both sizeMB and extents")
	}
}

func TestParsePartitionLayoutMountpointWhitespace(t *testing.T) {
	input := "{\"table\":\"gpt\",\"partitions\":[{\"label\":\"root\",\"filesystem\":\"ext4\",\"mountpoint\":\"/bad path\"}]}"
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for mountpoint with whitespace")
	}
}

func TestParsePartitionLayoutMountOptions(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","sizeMB":1024,"filesystem":"ext4","mountpoint":"/","mountOptions":"ro"},{"label":"var","filesystem":"xfs","mountpoint":"/var","mountOptions":"defaults,noatime"}]}`
	layout, err := ParsePartitionLayout(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if layout.Partitions[0].MountOptions != "ro" {
		t.Fatalf("root mountOptions = %q, want ro", layout.Partitions[0].MountOptions)
	}
	if layout.Partitions[1].MountOptions != "defaults,noatime" {
		t.Fatalf("var mountOptions = %q, want defaults,noatime", layout.Partitions[1].MountOptions)
	}
}

func TestParsePartitionLayoutRejectsMountOptionsWhitespace(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","filesystem":"ext4","mountpoint":"/","mountOptions":"ro noexec"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for mountOptions with whitespace")
	}
}

func TestParsePartitionLayoutRejectsLvmMountOptionsWhitespace(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/","mountOptions":"ro noexec"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for lvm mountOptions with whitespace")
	}
}

func TestParsePartitionLayoutDuplicatePartitionMountpoints(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root1","filesystem":"ext4","mountpoint":"/"},{"label":"root2","filesystem":"xfs","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for duplicate partition mountpoints")
	}
}

func TestParsePartitionLayoutDuplicateMountpointAcrossPartitionAndLVM(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","filesystem":"ext4","mountpoint":"/"},{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":2,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for duplicate mountpoint across partition and lvm volume")
	}
}

func TestParsePartitionLayoutSpecialCharLabel(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root#1","filesystem":"ext4","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Error("expected error for label with special characters")
	}
}

func TestParsePartitionLayoutDuplicateLabels(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","sizeMB":4096,"filesystem":"ext4","mountpoint":"/"},{"label":"root","sizeMB":4096,"filesystem":"xfs","mountpoint":"/data"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Error("expected error for duplicate partition labels")
	}
}

func TestParsePartitionLayoutLabelTooLong(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"this-label-is-way-too-long-for-a-gpt-partition-label-maximum","filesystem":"ext4","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Error("expected error for label exceeding 36 characters")
	}
}

func TestParsePartitionLayoutLvmVGNameMustNotStartWithDashOrDot(t *testing.T) {
	tests := []string{"-sysvg", ".sysvg", ".."}

	for _, vgName := range tests {
		input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192},{"label":"root","filesystem":"ext4","mountpoint":"/"}],"lvm":{"volumeGroup":"` + vgName + `","pvPartition":1,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/"}]}}`
		_, err := ParsePartitionLayout(input)
		if err == nil {
			t.Fatalf("expected error for invalid volumeGroup %q", vgName)
		}
	}
}

func TestParsePartitionLayoutLvmLVNameMustNotStartWithDashOrDot(t *testing.T) {
	tests := []string{"-root", ".root", ".."}

	for _, lvName := range tests {
		input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"` + lvName + `","filesystem":"ext4","mountpoint":"/"}]}}`
		_, err := ParsePartitionLayout(input)
		if err == nil {
			t.Fatalf("expected error for invalid lvm volume name %q", lvName)
		}
	}
}

func TestParsePartitionLayoutRejectsUnknownFields(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","filesytem":"ext4","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for unknown field in partition layout")
	}
}

func TestParsePartitionLayoutDuplicateLVMVolumeNames(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/"},{"name":"root","filesystem":"xfs","mountpoint":"/var"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for duplicate lvm volume names")
	}
}

func TestParsePartitionLayoutFillRemainingLVMVolumeMustBeLast(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","filesystem":"ext4","mountpoint":"/"},{"name":"var","sizeMB":1024,"filesystem":"xfs","mountpoint":"/var"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error when fill-remaining lvm volume is not last")
	}
}

func TestParsePartitionLayoutTrailingContent(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","filesystem":"ext4","mountpoint":"/"}]}{"extra":true}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for trailing JSON content")
	}
}

func TestParsePartitionLayoutInvalidExtentsFormat(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","extents":"foo bar","filesystem":"ext4","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for invalid extents format")
	}
}

func TestParsePartitionLayoutTrimmedDevice(t *testing.T) {
	input := `{"table":"gpt","device":"  /dev/sda  ","partitions":[{"label":"root","filesystem":"ext4","mountpoint":"/"}]}`
	layout, err := ParsePartitionLayout(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if layout.Device != "/dev/sda" {
		t.Fatalf("device = %q, want /dev/sda", layout.Device)
	}
}

func TestParsePartitionLayoutRejectsRelativeDevice(t *testing.T) {
	input := `{"table":"gpt","device":"dev/sda","partitions":[{"label":"root","filesystem":"ext4","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for relative device path")
	}
}

func TestParsePartitionLayoutRejectsDeviceWithWhitespace(t *testing.T) {
	input := `{"table":"gpt","device":"/dev/my disk","partitions":[{"label":"root","filesystem":"ext4","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for whitespace in device path")
	}
}

func TestParsePartitionLayoutMountpointRequiresFilesystem(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","mountpoint":"/"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error when mountpoint is set without filesystem")
	}
}

func TestParsePartitionLayoutLVMMountpointRequiresFilesystem(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"pv","sizeMB":8192}],"lvm":{"volumeGroup":"sysvg","pvPartition":1,"volumes":[{"name":"root","mountpoint":"/"}]}}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error when lvm mountpoint is set without filesystem")
	}
}

func TestParsePartitionLayoutInvalidTypeGUID(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","sizeMB":0,"mountpoint":"/","filesystem":"ext4","typeGUID":"not-a-guid"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for invalid typeGUID")
	}
}

func TestParsePartitionLayoutValidTypeGUID(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","sizeMB":0,"mountpoint":"/","filesystem":"ext4","typeGUID":"0FC63DAF-8483-4772-8E79-3D69D8477DE4"}]}`
	layout, err := ParsePartitionLayout(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if layout.Partitions[0].TypeGUID != "0FC63DAF-8483-4772-8E79-3D69D8477DE4" {
		t.Errorf("TypeGUID = %q, want valid UUID", layout.Partitions[0].TypeGUID)
	}
}

func TestParsePartitionLayoutRejectsDeviceTraversal(t *testing.T) {
	input := `{"table":"gpt","device":"/dev/../etc/passwd","partitions":[{"label":"root","sizeMB":0,"mountpoint":"/","filesystem":"ext4"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for device path with ..")
	}
}

func TestParsePartitionLayoutRejectsMountpointTraversal(t *testing.T) {
	input := `{"table":"gpt","partitions":[{"label":"root","sizeMB":0,"mountpoint":"/boot/../../etc","filesystem":"ext4"}]}`
	_, err := ParsePartitionLayout(input)
	if err == nil {
		t.Fatal("expected error for mountpoint with ..")
	}
}

func TestParsePartitionLayoutTooManyPartitions(t *testing.T) {
	parts := `{"table":"gpt","partitions":[`
	for i := range 129 {
		if i > 0 {
			parts += ","
		}
		parts += fmt.Sprintf(`{"label":"p%d","sizeMB":100,"filesystem":"ext4","mountpoint":"/mnt/p%d"}`, i, i)
	}
	parts += `]}`

	_, err := ParsePartitionLayout(parts)
	if err == nil {
		t.Fatal("expected error for too many partitions")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name           string
		cfg            Config
		wantErr        string
		wantNormalized func(t *testing.T, cfg *Config)
	}{
		{name: "empty config is valid", cfg: Config{}},
		{name: "valid mode provision", cfg: Config{Mode: "provision"}},
		{name: "valid mode dry-run", cfg: Config{Mode: "dry-run"}},
		{name: "invalid mode", cfg: Config{Mode: "invalid"}, wantErr: "invalid mode"},
		{name: "valid provision target os", cfg: Config{Provision: ProvisionConfig{TargetOS: " Linux "}}, wantNormalized: func(t *testing.T, cfg *Config) {
			t.Helper()
			if cfg.Provision.TargetOS != TargetOSLinux {
				t.Fatalf("Provision.TargetOS = %q, want %q", cfg.Provision.TargetOS, TargetOSLinux)
			}
		}},
		{name: "valid kubelet provider and topology fields", cfg: Config{Provision: ProvisionConfig{ProviderID: "redfish://host/sys/1", FailureDomain: "dc1_az.1", Region: "eu-central-1"}}},
		{name: "provider id rejects newline", cfg: Config{Provision: ProvisionConfig{ProviderID: "redfish://host/sys/1\n--node-labels=bad=true"}}, wantErr: "provision.providerID must not contain whitespace or control characters"},
		{name: "provider id rejects shell-breaking whitespace", cfg: Config{Provision: ProvisionConfig{ProviderID: "redfish://host/sys/1 bad"}}, wantErr: "provision.providerID must not contain whitespace or control characters"},
		{name: "failure domain rejects invalid label value", cfg: Config{Provision: ProvisionConfig{FailureDomain: "-dc1"}}, wantErr: "provision.failureDomain must be a valid Kubernetes label value"},
		{name: "region rejects long label value", cfg: Config{Provision: ProvisionConfig{Region: strings.Repeat("a", 64)}}, wantErr: "provision.region must be no more than 63 characters"},
		{name: "windows provision target os rejected", cfg: Config{Provision: ProvisionConfig{TargetOS: "windows"}}, wantErr: "Windows targets are not supported"},
		{name: "esxi provision target os rejected", cfg: Config{Provision: ProvisionConfig{TargetOS: "vmware-esxi"}}, wantErr: "VMware ESXi targets are not supported"},
		{name: "unknown provision target os rejected", cfg: Config{Provision: ProvisionConfig{TargetOS: "sunos"}}, wantErr: "only \"linux\" is currently accepted"},
		{name: "valid image mode", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{Mode: "whole-disk"}}}},
		{name: "invalid image mode", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{Mode: "raw"}}}, wantErr: "invalid provision.image.mode"},
		{name: "valid image source root selector", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{SourceRootLabel: " ROOT-A "}}}, wantNormalized: func(t *testing.T, cfg *Config) {
			t.Helper()
			if cfg.Provision.Image.SourceRootLabel != "ROOT-A" {
				t.Fatalf("sourceRootLabel = %q, want ROOT-A", cfg.Provision.Image.SourceRootLabel)
			}
		}},
		{name: "invalid blank image source root selector", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{SourceRootLabel: "   "}}}, wantErr: "provision.image.sourceRootLabel must not be blank"},
		{name: "invalid image source root selector conflict", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{SourceRootLabel: "ROOT-A", SourceRootPartition: 2}}}, wantErr: "provision.image.sourceRootLabel and provision.image.sourceRootPartition are mutually exclusive"},
		{name: "invalid image source root partition", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{SourceRootPartition: -1}}}, wantErr: "provision.image.sourceRootPartition must be non-negative"},
		{name: "valid network mode", cfg: Config{Network: NetworkConfig{Mode: "gobgp"}}},
		{name: "invalid network mode", cfg: Config{Network: NetworkConfig{Mode: "ospf"}}, wantErr: "invalid network.mode"},
		{name: "valid checksum type", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{ChecksumType: "sha256"}}}},
		{name: "invalid checksum type", cfg: Config{Provision: ProvisionConfig{Image: ImageConfig{ChecksumType: "md5"}}}, wantErr: "invalid provision.image.checksumType"},
		{name: "valid direct partition layout", cfg: Config{Provision: ProvisionConfig{Disk: DiskConfig{PartitionLayout: &PartitionLayout{
			Device:     "/dev/sda",
			Partitions: []Partition{{Label: "root", Filesystem: "ext4", Mountpoint: "/"}},
		}}}}, wantNormalized: func(t *testing.T, cfg *Config) {
			t.Helper()
			if cfg.Provision.Disk.PartitionLayout.Table != "gpt" {
				t.Fatalf("partition layout table = %q, want gpt", cfg.Provision.Disk.PartitionLayout.Table)
			}
		}},
		{name: "invalid direct partition layout missing root", cfg: Config{Provision: ProvisionConfig{Disk: DiskConfig{PartitionLayout: &PartitionLayout{
			Table:      "gpt",
			Partitions: []Partition{{Label: "data", Filesystem: "ext4", Mountpoint: "/data"}},
		}}}}, wantErr: `partition layout must include mountpoint "/"`},
		{name: "invalid direct partition layout filesystem", cfg: Config{Provision: ProvisionConfig{Disk: DiskConfig{PartitionLayout: &PartitionLayout{
			Table:      "gpt",
			Partitions: []Partition{{Label: "root", Filesystem: "ntfs", Mountpoint: "/"}},
		}}}}, wantErr: `unsupported filesystem "ntfs"`},
		{name: "valid rescue mode", cfg: Config{Rescue: RescueConfig{Mode: "shell"}}},
		{name: "invalid rescue mode", cfg: Config{Rescue: RescueConfig{Mode: "panic"}}, wantErr: "invalid rescue.mode"},
		{name: "valid peer mode", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{PeerMode: "unnumbered"}}}},
		{name: "valid dual peer mode with neighbors", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{PeerMode: "dual", Neighbors: "10.0.0.1"}}}},
		{name: "valid numbered peer mode with neighbors", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{PeerMode: "numbered", Neighbors: "10.0.0.1"}}}},
		{name: "dual peer mode requires neighbors", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{PeerMode: "dual"}}}, wantErr: "network.bgp.neighbors required"},
		{name: "numbered peer mode requires neighbors", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{PeerMode: "numbered"}}}, wantErr: "network.bgp.neighbors required"},
		{name: "invalid peer mode", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{PeerMode: "mesh"}}}, wantErr: "invalid network.bgp.peerMode"},
		{name: "valid frr bfd timers", cfg: Config{Network: NetworkConfig{Mode: "frr", BGP: BGPConfig{BFDTransmitMS: 150, BFDReceiveMS: 150}}}},
		{name: "bfd transmit requires receive", cfg: Config{Network: NetworkConfig{Mode: "frr", BGP: BGPConfig{BFDTransmitMS: 150}}}, wantErr: "must be set together"},
		{name: "bfd receive requires transmit", cfg: Config{Network: NetworkConfig{Mode: "frr", BGP: BGPConfig{BFDReceiveMS: 150}}}, wantErr: "must be set together"},
		{name: "gobgp rejects bfd timers", cfg: Config{Network: NetworkConfig{Mode: "gobgp", BGP: BGPConfig{BFDTransmitMS: 150, BFDReceiveMS: 150}}}, wantErr: "gobgp does not support BFD"},
		{name: "valid underlay AF", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{UnderlayAF: "ipv4"}}}},
		{name: "ipv6 underlay AF not implemented", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{UnderlayAF: "ipv6"}}}, wantErr: "invalid network.bgp.underlayAF"},
		{name: "dual stack underlay AF not implemented", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{UnderlayAF: "dual-stack"}}}, wantErr: "invalid network.bgp.underlayAF"},
		{name: "invalid underlay AF", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{UnderlayAF: "ipv3"}}}, wantErr: "invalid network.bgp.underlayAF"},
		{name: "valid overlay type", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{OverlayType: "evpn-vxlan"}}}},
		{name: "invalid overlay type", cfg: Config{Network: NetworkConfig{BGP: BGPConfig{OverlayType: "gre"}}}, wantErr: "invalid network.bgp.overlayType"},
		{name: "valid cloud-init ds", cfg: Config{Provision: ProvisionConfig{CloudInit: CloudInitConfig{Datasource: "nocloud"}}}},
		{name: "trims cloud-init ds", cfg: Config{Provision: ProvisionConfig{CloudInit: CloudInitConfig{Datasource: " NoCloud "}}}},
		{name: "valid configdrive cloud-init ds", cfg: Config{Provision: ProvisionConfig{CloudInit: CloudInitConfig{Datasource: " configDrive "}}}},
		{name: "invalid cloud-init ds", cfg: Config{Provision: ProvisionConfig{CloudInit: CloudInitConfig{Datasource: "ec2"}}}, wantErr: "invalid provision.cloudInit.datasource"},
		{name: "flatcar cloud-init rejected", cfg: Config{OSFamily: " Flatcar ", Provision: ProvisionConfig{CloudInit: CloudInitConfig{Enabled: true, Datasource: "nocloud"}}}, wantErr: "Flatcar first-boot provisioning requires Ignition"},
		{name: "valid network persistence os family", cfg: Config{PersistNetwork: true, OSFamily: " Ubuntu ", Network: NetworkConfig{Static: StaticConfig{Iface: "eth0"}}}, wantNormalized: func(t *testing.T, cfg *Config) {
			t.Helper()
			if cfg.OSFamily != "ubuntu" {
				t.Fatalf("OSFamily = %q, want ubuntu", cfg.OSFamily)
			}
		}},
		{name: "network persistence requires os family", cfg: Config{PersistNetwork: true, Network: NetworkConfig{Static: StaticConfig{Iface: "eth0"}}}, wantErr: "osFamily required when persistNetwork is true"},
		{name: "network persistence requires target network", cfg: Config{PersistNetwork: true, OSFamily: "ubuntu"}, wantErr: "network.static.iface, network.bond.interfaces, or network.vlan.config required"},
		{name: "network persistence static ip requires interface", cfg: Config{PersistNetwork: true, OSFamily: "ubuntu", Network: NetworkConfig{Static: StaticConfig{IP: "10.1.0.5/24"}}}, wantErr: "network.static.iface required"},
		{name: "network persistence accepts vlan without gateway", cfg: Config{PersistNetwork: true, OSFamily: "ubuntu", Network: NetworkConfig{VLAN: VLANConfig{Config: "100:eth0:10.100.0.5/24"}}}},
		{name: "network persistence accepts ubuntu bond and vlan", cfg: Config{PersistNetwork: true, OSFamily: "ubuntu", Network: NetworkConfig{Static: StaticConfig{IP: "10.1.0.5/24"}, Bond: BondConfig{Interfaces: "eth0,eth1"}, VLAN: VLANConfig{Config: "100:bond0:10.100.0.5/24"}}}},
		{name: "network persistence rejects rhel before provisioning", cfg: Config{PersistNetwork: true, OSFamily: "rhel", Network: NetworkConfig{Static: StaticConfig{Iface: "ens3", IP: "10.1.0.5/24"}}}, wantErr: `osFamily "rhel" with persistNetwork=true is blocked: target network persistence is blocked`},
		{name: "network persistence rejects rhel bond before provisioning", cfg: Config{PersistNetwork: true, OSFamily: "rhel", Network: NetworkConfig{Static: StaticConfig{IP: "10.1.0.5/24"}, Bond: BondConfig{Interfaces: "eth0,eth1"}}}, wantErr: `osFamily "rhel" with persistNetwork=true is blocked: target network persistence is blocked`},
		{name: "network persistence accepts flatcar vlan", cfg: Config{PersistNetwork: true, OSFamily: "flatcar", Network: NetworkConfig{VLAN: VLANConfig{Config: "100:eth0:10.100.0.5/24"}}}},
		{name: "network persistence rejects vlan gateway", cfg: Config{PersistNetwork: true, OSFamily: "ubuntu", Network: NetworkConfig{VLAN: VLANConfig{Config: "100:eth0:10.100.0.5/24:10.100.0.1"}}}, wantErr: "network.vlan.config vlan 100 on eth0 includes gateway"},
		{name: "network persistence bare bond requires address or vlan", cfg: Config{PersistNetwork: true, OSFamily: "ubuntu", Network: NetworkConfig{Bond: BondConfig{Interfaces: "eth0,eth1"}}}, wantErr: "network.bond.interfaces requires network.static.ip or network.vlan.config"},
		{name: "invalid network persistence os family", cfg: Config{OSFamily: "windows"}, wantErr: "invalid osFamily"},
		{name: "valid sysext preload mode", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{DefaultMode: "preload"}}}},
		{name: "normalizes sysext default mode", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{DefaultMode: "PreLoad"}}}, wantNormalized: func(t *testing.T, cfg *Config) {
			t.Helper()
			if cfg.Provision.Sysext.DefaultMode != "preload" {
				t.Fatalf("DefaultMode = %q, want preload", cfg.Provision.Sysext.DefaultMode)
			}
		}},
		{name: "invalid sysext default mode", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{DefaultMode: "enabled"}}}, wantErr: "invalid provision.sysext.defaultMode"},
		{name: "invalid sysext catalog dir", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{CatalogDir: "var/lib/sysext"}}}, wantErr: "provision.sysext.catalogDir"},
		{name: "invalid sysext active dir", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{ActiveDir: "/"}}}, wantErr: "provision.sysext.activeDir"},
		{name: "invalid sysext layer mode", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Layers: []SysextLayerConfig{{Name: "debug", Mode: "now"}}}}}, wantErr: "invalid provision.sysext.layers[0].mode"},
		{name: "invalid sysext filename", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Layers: []SysextLayerConfig{{Name: "debug", FileName: "../debug.raw"}}}}}, wantErr: "provision.sysext.layers[0].fileName"},
		{name: "enabled sysext layer requires source", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug"}}}}}, wantErr: "source is required"},
		{name: "enabled sysext https source requires sha256", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "https://images.example.invalid/debug.raw"}}}}}, wantErr: "sha256: required"},
		{name: "enabled sysext local source requires sha256", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "/deploy/sysext/debug.raw"}}}}}, wantErr: "sha256: required"},
		{name: "enabled sysext rejects empty sha256 prefix", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "https://images.example.invalid/debug.raw", SHA256: "sha256:"}}}}}, wantErr: "sha256: must be 64 hex characters"},
		{name: "enabled sysext rejects plain http source by default", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "http://images.example.invalid/debug.raw", SHA256: strings.Repeat("a", 64)}}}}}, wantErr: "allowInsecureHTTP"},
		{name: "enabled sysext rejects malformed http source", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, AllowInsecureHTTP: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "http://[::1/debug.raw", SHA256: strings.Repeat("a", 64)}}}}}, wantErr: "invalid HTTP(S) sysext source"},
		{name: "enabled sysext rejects malformed oci source", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "oci://registry.example.invalid/%zz", SHA256: strings.Repeat("a", 64)}}}}}, wantErr: "invalid sysext source"},
		{name: "enabled sysext rejects malformed url-like source", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "custom://registry.example.invalid/%zz", SHA256: strings.Repeat("a", 64)}}}}}, wantErr: "invalid sysext source"},
		{name: "enabled sysext accepts local path with percent escape as local source", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "/deploy/sysext/%zz.raw", SHA256: strings.Repeat("a", 64)}}}}}},
		{name: "enabled sysext rejects unsupported url scheme", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "htps://images.example.invalid/debug.raw", SHA256: strings.Repeat("a", 64)}}}}}, wantErr: `unsupported sysext source scheme "htps"`},
		{name: "enabled sysext rejects file url scheme", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "file:///deploy/sysext/debug.raw", SHA256: strings.Repeat("a", 64)}}}}}, wantErr: `unsupported sysext source scheme "file"`},
		{name: "enabled sysext accepts plain http source with explicit opt in", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, AllowInsecureHTTP: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "http://images.example.invalid/debug.raw", SHA256: strings.Repeat("a", 64)}}}}}},
		{name: "enabled sysext accepts https source with sha256", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "https://images.example.invalid/debug.raw", SHA256: strings.Repeat("a", 64)}}}}}},
		{name: "enabled sysext rejects empty oci digest source", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "oci://registry.example.invalid/tcaas/debug@sha256:"}}}}}, wantErr: "sha256: required"},
		{name: "enabled sysext accepts oci digest source without sha256", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "oci://registry.example.invalid/tcaas/debug@sha256:" + strings.Repeat("a", 64)}}}}}},
		{name: "enabled sysext accepts uppercase oci digest source without sha256", cfg: Config{Provision: ProvisionConfig{Sysext: SysextConfig{Enabled: true, Layers: []SysextLayerConfig{{Name: "debug", Source: "OCI://registry.example.invalid/tcaas/debug@sha256:" + strings.Repeat("a", 64)}}}}}},
		{name: "valid oci prepull config", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{Enabled: true, Images: []OCIPrePullImageConfig{{Reference: "oci://registry.example.invalid/team/app:v1"}}}}}},
		{name: "normalizes oci prepull strings", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{Enabled: true, CacheDir: " /var/lib/booty/cache ", ImportNamespace: " k8s.io ", Images: []OCIPrePullImageConfig{{Reference: " oci://registry.example.invalid/team/app:v1 "}}}}}, wantNormalized: func(t *testing.T, cfg *Config) {
			t.Helper()
			if cfg.Provision.OCIPrePulls.CacheDir != "/var/lib/booty/cache" {
				t.Fatalf("CacheDir = %q", cfg.Provision.OCIPrePulls.CacheDir)
			}
			if cfg.Provision.OCIPrePulls.ImportNamespace != "k8s.io" {
				t.Fatalf("ImportNamespace = %q", cfg.Provision.OCIPrePulls.ImportNamespace)
			}
			if cfg.Provision.OCIPrePulls.Images[0].Reference != "oci://registry.example.invalid/team/app:v1" {
				t.Fatalf("Reference = %q", cfg.Provision.OCIPrePulls.Images[0].Reference)
			}
		}},
		{name: "enabled oci prepull requires images", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{Enabled: true}}}, wantErr: "provision.ociPrePulls.images is required"},
		{name: "enabled oci prepull image requires reference", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{Enabled: true, Images: []OCIPrePullImageConfig{{}}}}}, wantErr: "reference is required"},
		{name: "oci prepull rejects invalid cache dir", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{CacheDir: "var/lib/booty"}}}, wantErr: "provision.ociPrePulls.cacheDir"},
		{name: "oci prepull rejects whitespace cache dir", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{CacheDir: "/var/lib/booty/oci pre pulls"}}}, wantErr: "must not contain whitespace"},
		{name: "oci prepull rejects invalid namespace", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{ImportNamespace: "k8s/io"}}}, wantErr: "provision.ociPrePulls.importNamespace"},
		{name: "oci prepull rejects malformed reference", cfg: Config{Provision: ProvisionConfig{OCIPrePulls: OCIPrePullConfig{Images: []OCIPrePullImageConfig{{Reference: "oci://registry.example.invalid/%zz"}}}}}, wantErr: "invalid OCI reference"},
		{name: "valid token algorithm", cfg: Config{Transport: TransportConfig{TokenAlgorithm: "ES256"}}},
		{name: "invalid token algorithm", cfg: Config{Transport: TransportConfig{TokenAlgorithm: "HS256"}}, wantErr: "invalid transport.tokenAlgorithm"},
		{
			name:    "multiple errors",
			cfg:     Config{Mode: "bad", Provision: ProvisionConfig{Image: ImageConfig{Mode: "bad"}}},
			wantErr: "invalid mode",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if tc.wantNormalized != nil {
					tc.wantNormalized(t, &tc.cfg)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateSysextMalformedHTTPSourceRedactsSensitiveURLParts(t *testing.T) {
	cfg := Config{
		Provision: ProvisionConfig{
			Sysext: SysextConfig{
				Enabled:           true,
				AllowInsecureHTTP: true,
				Layers: []SysextLayerConfig{{
					Name:   "debug",
					Source: "https://robot:secret@example.invalid/%zz?token=abc#frag",
					SHA256: strings.Repeat("a", 64),
				}},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, sensitive := range []string{"robot", "secret", "token=abc", "#frag"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("validation error leaked %q: %q", sensitive, err.Error())
		}
	}
	if !strings.Contains(err.Error(), "[redacted invalid URL]") {
		t.Fatalf("validation error = %q, want redacted invalid URL context", err.Error())
	}
}
