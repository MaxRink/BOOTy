package config

// Values of ImageConfig.ChecksumScope.
const (
	ChecksumScopeDownload     = "download"
	ChecksumScopeDecompressed = "decompressed"
)

// ImageConfig defines the OS image to stream onto the target disk.
// Supports multiple image formats (raw, OCI) and integrity verification
// via checksums and GPG signatures.
type ImageConfig struct {
	// URLs is the list of image source URLs. Multiple URLs enable fallback.
	// Supports HTTP(S) URLs and OCI registry references.
	// Default: [] (required for provisioning)
	URLs []string `yaml:"urls" json:"urls"`

	// AllowInsecureHTTP permits plain HTTP image sources. This is intended for
	// local testing/airgapped environments without TLS infrastructure.
	// Preset by legacyCompat: true.
	// Default: nil (false, requires HTTPS or OCI)
	AllowInsecureHTTP *bool `yaml:"allowInsecureHTTP,omitempty" json:"allowInsecureHTTP,omitempty"`

	// Checksum is the expected hex digest of the image. ChecksumScope
	// selects the bytes it covers. Verified after streaming completes.
	// Default: "" (no verification)
	Checksum string `yaml:"checksum" json:"checksum"`

	// ChecksumType is the hash algorithm for Checksum.
	// Valid values: "sha256", "sha512"
	// Default: "" (inferred from checksum length if possible)
	ChecksumType string `yaml:"checksumType" json:"checksumType"`

	// ChecksumScope selects the bytes that Checksum covers.
	// "download": the file as downloaded, before decompression or qcow2
	// conversion, like the checksum files published next to the images
	// (image.raw.lz4.sha256) and like the CAPRF legacy provisioner.
	// "decompressed": the decompressed image; for qcow2 the converted raw
	// image.
	// Valid values: "download", "decompressed"
	// Default: "" (download)
	ChecksumScope string `yaml:"checksumScope" json:"checksumScope"`

	// Mode controls how the image is written to disk.
	// Valid values: "whole-disk" (default), "partition", "ab"
	//   - whole-disk: dd-style write of the entire image to the block device,
	//     or source-root filesystem streaming into the declared root partition
	//     when provision.disk.partitionLayout is set
	//   - partition: per-partition extraction (streams each partition independently;
	//     cannot be combined with Provision.Disk.PartitionLayout)
	//   - ab: copy the image boot/root partitions into a generated dual-root
	//     A/B layout and boot the selected target slot
	// Default: "whole-disk"
	Mode string `yaml:"mode" json:"mode"`

	// SourceRootLabel selects the source-image GPT partition label to stream
	// into a declarative partition layout root. It is mutually exclusive with
	// SourceRootPartition. A/B mode uses provision.ab.sourceRootLabel instead.
	// Default: "" (auto-select a common root label or unambiguous Linux partition)
	SourceRootLabel string `yaml:"sourceRootLabel" json:"sourceRootLabel"`

	// SourceRootPartition selects the 1-based source-image partition number to
	// stream into a declarative partition layout root. It is mutually exclusive
	// with SourceRootLabel. A/B mode uses provision.ab.sourceRootPartition.
	// Default: 0 (not set)
	SourceRootPartition int `yaml:"sourceRootPartition" json:"sourceRootPartition"`

	// SignatureURL is the URL to a detached GPG signature for image verification.
	// Default: ""
	SignatureURL string `yaml:"signatureURL" json:"signatureURL"`

	// GPGPubKey is the path to the GPG public key for signature verification.
	// Default: ""
	GPGPubKey string `yaml:"gpgPubKey" json:"gpgPubKey"`
}
