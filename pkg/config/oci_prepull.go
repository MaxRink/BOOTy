package config

// OCI pre-pull defaults.
const (
	DefaultOCIPrePullCacheDir        = "/var/lib/booty/oci-prepulls"
	DefaultOCIPrePullImportNamespace = "k8s.io"
)

// OCIPrePullConfig controls optional OCI image pre-pulling into the target OS.
type OCIPrePullConfig struct {
	// Enabled pulls the configured images during provisioning and installs a
	// first-boot importer into the provisioned OS.
	// Default: false
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Images is the set of OCI images to cache as target-root archives.
	// Default: [] (none)
	Images []OCIPrePullImageConfig `yaml:"images" json:"images"`

	// CacheDir is the target-root directory that stores archives, catalog data,
	// and import state.
	// Default: "/var/lib/booty/oci-prepulls"
	CacheDir string `yaml:"cacheDir" json:"cacheDir"`

	// ImportNamespace is used by containerd-compatible importers.
	// Default: "k8s.io"
	ImportNamespace string `yaml:"importNamespace" json:"importNamespace"`

	// AllowInsecure permits HTTP registry access for local or air-gapped labs.
	// Default: false
	AllowInsecure bool `yaml:"allowInsecure" json:"allowInsecure"`
}

// WithDefaults returns a copy with runtime defaults filled in.
func (c *OCIPrePullConfig) WithDefaults() OCIPrePullConfig {
	cfg := *c
	if cfg.CacheDir == "" {
		cfg.CacheDir = DefaultOCIPrePullCacheDir
	}
	if cfg.ImportNamespace == "" {
		cfg.ImportNamespace = DefaultOCIPrePullImportNamespace
	}
	return cfg
}

// OCIPrePullImageConfig identifies one OCI image to cache for first boot.
type OCIPrePullImageConfig struct {
	// Reference is a tag or digest OCI image reference. The oci:// prefix is
	// accepted for consistency with provision.image.urls.
	// Default: "" (required when provision.ociPrePulls.enabled is true)
	Reference string `yaml:"reference" json:"reference"`
}
