package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadYAML(t *testing.T) {
	content := `
hostname: test-node-01
mode: provision
persistNetwork: true
osFamily: Ubuntu

network:
  mode: gobgp
  dnsResolvers: "8.8.8.8,8.8.4.4"
  bgp:
    asn: 65001
    peerMode: unnumbered
    minPeers: 2
    keepalive: 30
    hold: 90
  evpn:
    underlaySubnet: "192.168.4.0/24"
    provisionVNI: 100
    provisionIP: "10.100.0.20/24"
  static:
    ip: "10.0.0.5/24"
    gateway: "10.0.0.1"
  bond:
    interfaces: "eth0,eth1"
    mode: "802.3ad"
  vrf:
    name: "Vrf_underlay"
    tableID: 1000
  ipmi:
    subnet: "172.30.0.0/24"

transport:
  token: "bootstrap-token-xyz"
  tokenURL: "https://caprf.example.com/token"
  tokenAlgorithm: RS256
  initURL: "https://caprf.example.com/status/init"
  successURL: "https://caprf.example.com/status/success"
  errorURL: "https://caprf.example.com/status/error"
  logURL: "https://caprf.example.com/logs"

health:
  enabled: true
  minMemoryGB: 16
  minCPUs: 4
  skipChecks: "thermal"
  reportURL: "https://caprf.example.com/health"

telemetry:
  enabled: true
  url: "https://caprf.example.com/telemetry"
  metricsURL: "https://caprf.example.com/metrics"
  eventURL: "https://caprf.example.com/events"

rescue:
  mode: retry
  sshPubKey: "ssh-ed25519 AAAAC3... admin@ops"
  timeout: 300

provision:
  extraKernelParams: "console=ttyS0,115200"
  failureDomain: zone-a
  region: eu-west-1
  providerID: "metal://node-01"
  postProvisionCmds:
    - "systemctl enable kubelet"
    - "kubeadm init"
  image:
    urls:
      - "https://registry.example.com/os-image:v1.2.3"
    checksumType: sha256
    checksum: "abc123def456"
    mode: whole-disk
  disk:
    minSizeGB: 100
    numVFs: 32
    raid:
      - name: md0
        level: 1
        devices:
          - /dev/sda
          - /dev/sdb
  firmware:
    enabled: true
    url: "https://caprf.example.com/firmware"
    minBIOS: "2.10"
    minBMC: "4.50"
  cloudInit:
    enabled: true
    datasource: nocloud
  overlayFS:
    enabled: true
    mode: device
    device: "LABEL=BOOTY-OVERLAY"
    directory: "booty"
    upperDir: "/var/lib/booty/overlayfs/upper"
    workDir: "/var/lib/booty/overlayfs/work"
    timeoutSec: 30
  crashArtifacts:
    enabled: true
    prepareURL: "https://caprf.example.com/crash/prepare"
    uploadURL: "https://caprf.example.com/crash/upload"
    maxMB: 256
    uploadTimeoutSec: 120
    includeMemoryDumps: true
  inventory:
    enabled: true
    url: "https://caprf.example.com/inventory"

agent:
  heartbeatURL: "https://caprf.example.com/heartbeat"
  commandsURL: "https://caprf.example.com/commands"
`
	path := writeTestFile(t, "config.yaml", content)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Hostname != "test-node-01" {
		t.Errorf("Hostname = %q", cfg.Hostname)
	}
	if cfg.Mode != "provision" {
		t.Errorf("Mode = %q", cfg.Mode)
	}
	if !cfg.PersistNetwork {
		t.Error("PersistNetwork = false")
	}
	if cfg.OSFamily != "ubuntu" {
		t.Errorf("OSFamily = %q", cfg.OSFamily)
	}
	if cfg.Network.Mode != "gobgp" {
		t.Errorf("Network.Mode = %q", cfg.Network.Mode)
	}
	if cfg.Network.BGP.ASN != 65001 {
		t.Errorf("Network.BGP.ASN = %d", cfg.Network.BGP.ASN)
	}
	if cfg.Network.BGP.MinPeers != 2 {
		t.Errorf("Network.BGP.MinPeers = %d", cfg.Network.BGP.MinPeers)
	}
	if cfg.Network.EVPN.ProvisionVNI != 100 {
		t.Errorf("Network.EVPN.ProvisionVNI = %d", cfg.Network.EVPN.ProvisionVNI)
	}
	if cfg.Network.Bond.Interfaces != "eth0,eth1" {
		t.Errorf("Network.Bond.Interfaces = %q", cfg.Network.Bond.Interfaces)
	}
	if cfg.Network.VRF.TableID != 1000 {
		t.Errorf("Network.VRF.TableID = %d", cfg.Network.VRF.TableID)
	}
	if cfg.Transport.Token != "bootstrap-token-xyz" {
		t.Errorf("Transport.Token = %q", cfg.Transport.Token)
	}
	if cfg.Transport.TokenAlgorithm != "RS256" {
		t.Errorf("Transport.TokenAlgorithm = %q", cfg.Transport.TokenAlgorithm)
	}
	if cfg.Health.Enabled != true {
		t.Error("Health.Enabled = false")
	}
	if cfg.Health.MinMemoryGB != 16 {
		t.Errorf("Health.MinMemoryGB = %d", cfg.Health.MinMemoryGB)
	}
	if cfg.Telemetry.Enabled != true {
		t.Error("Telemetry.Enabled = false")
	}
	if cfg.Rescue.Mode != "retry" {
		t.Errorf("Rescue.Mode = %q", cfg.Rescue.Mode)
	}
	if cfg.Rescue.Timeout != 300 {
		t.Errorf("Rescue.Timeout = %d", cfg.Rescue.Timeout)
	}
	if cfg.Provision.ExtraKernelParams != "console=ttyS0,115200" {
		t.Errorf("Provision.ExtraKernelParams = %q", cfg.Provision.ExtraKernelParams)
	}
	if cfg.Provision.FailureDomain != "zone-a" {
		t.Errorf("Provision.FailureDomain = %q", cfg.Provision.FailureDomain)
	}
	if len(cfg.Provision.Image.URLs) != 1 {
		t.Fatalf("Provision.Image.URLs len = %d", len(cfg.Provision.Image.URLs))
	}
	if cfg.Provision.Image.ChecksumType != "sha256" {
		t.Errorf("Provision.Image.ChecksumType = %q", cfg.Provision.Image.ChecksumType)
	}
	if cfg.Provision.Disk.MinSizeGB != 100 {
		t.Errorf("Provision.Disk.MinSizeGB = %d", cfg.Provision.Disk.MinSizeGB)
	}
	if len(cfg.Provision.Disk.RAID) != 1 {
		t.Fatalf("Provision.Disk.RAID len = %d", len(cfg.Provision.Disk.RAID))
	}
	if cfg.Provision.Disk.RAID[0].Level != 1 {
		t.Errorf("RAID[0].Level = %d", cfg.Provision.Disk.RAID[0].Level)
	}
	if len(cfg.Provision.Disk.RAID[0].Devices) != 2 {
		t.Errorf("RAID[0].Devices len = %d", len(cfg.Provision.Disk.RAID[0].Devices))
	}
	if len(cfg.Provision.PostProvisionCmds) != 2 {
		t.Errorf("PostProvisionCmds len = %d", len(cfg.Provision.PostProvisionCmds))
	}
	if cfg.Provision.Firmware.MinBIOS != "2.10" {
		t.Errorf("Provision.Firmware.MinBIOS = %q", cfg.Provision.Firmware.MinBIOS)
	}
	if !cfg.Provision.OverlayFS.Enabled {
		t.Error("Provision.OverlayFS.Enabled = false")
	}
	if cfg.Provision.OverlayFS.Mode != "device" {
		t.Errorf("Provision.OverlayFS.Mode = %q", cfg.Provision.OverlayFS.Mode)
	}
	if cfg.Provision.OverlayFS.Device != "LABEL=BOOTY-OVERLAY" {
		t.Errorf("Provision.OverlayFS.Device = %q", cfg.Provision.OverlayFS.Device)
	}
	if cfg.Provision.OverlayFS.TimeoutSec != 30 {
		t.Errorf("Provision.OverlayFS.TimeoutSec = %d", cfg.Provision.OverlayFS.TimeoutSec)
	}
	if cfg.Provision.CrashArtifacts.MaxMB != 256 {
		t.Errorf("Provision.CrashArtifacts.MaxMB = %d", cfg.Provision.CrashArtifacts.MaxMB)
	}
	if !cfg.Provision.CrashArtifacts.IncludeMemoryDumps {
		t.Error("Provision.CrashArtifacts.IncludeMemoryDumps = false")
	}
	if cfg.Agent.HeartbeatURL != "https://caprf.example.com/heartbeat" {
		t.Errorf("Agent.HeartbeatURL = %q", cfg.Agent.HeartbeatURL)
	}
}

func TestLoadJSON(t *testing.T) {
	content := `{
  "hostname": "json-node",
  "mode": "provision",
  "persistNetwork": true,
  "osFamily": "ubuntu",
  "network": {
    "mode": "static",
    "static": {"ip": "10.0.0.5/24", "gateway": "10.0.0.1", "iface": "eth0"}
  },
  "transport": {"token": "json-token", "initURL": "https://example.com/init"},
  "provision": {
    "image": {"urls": ["https://example.com/image.gz"], "mode": "whole-disk"},
    "disk": {"minSizeGB": 50}
  }
}`
	path := writeTestFile(t, "config.json", content)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Hostname != "json-node" {
		t.Errorf("Hostname = %q", cfg.Hostname)
	}
	if cfg.Network.Static.IP != "10.0.0.5/24" {
		t.Errorf("Network.Static.IP = %q", cfg.Network.Static.IP)
	}
	if !cfg.PersistNetwork {
		t.Error("PersistNetwork = false")
	}
	if cfg.OSFamily != "ubuntu" {
		t.Errorf("OSFamily = %q", cfg.OSFamily)
	}
	if cfg.Provision.Disk.MinSizeGB != 50 {
		t.Errorf("Provision.Disk.MinSizeGB = %d", cfg.Provision.Disk.MinSizeGB)
	}
}

func TestLoadStrictYAMLRejectsUnknownFields(t *testing.T) {
	content := `
hostname: test
unknownField: bad
`
	path := writeTestFile(t, "strict.yaml", content)
	_, err := LoadWithOptions(LoadOptions{Path: path, Strict: true})
	if err == nil {
		t.Fatal("expected error for unknown field in strict mode")
	}
	if !strings.Contains(err.Error(), "unknownField") {
		t.Fatalf("error should mention unknown field: %v", err)
	}
}

func TestLoadStrictJSONRejectsUnknownFields(t *testing.T) {
	content := `{"hostname":"test","badField":true}`
	path := writeTestFile(t, "strict.json", content)
	_, err := LoadWithOptions(LoadOptions{Path: path, Strict: true})
	if err == nil {
		t.Fatal("expected error for unknown field in strict mode")
	}
}

func TestLoadValidationRuns(t *testing.T) {
	content := `
hostname: test
mode: totally-invalid-mode
`
	path := writeTestFile(t, "invalid.yaml", content)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "invalid mode") {
		t.Fatalf("expected 'invalid mode' in error: %v", err)
	}
}

func TestValidateOverlayFSConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "enabled requires ubuntu os family",
			cfg: Config{
				OSFamily:  "flatcar",
				Provision: ProvisionConfig{OverlayFS: OverlayFSConfig{Enabled: true}},
			},
			wantErr: "requires osFamily=ubuntu",
		},
		{
			name: "device mode requires device",
			cfg: Config{
				OSFamily: "ubuntu",
				Provision: ProvisionConfig{OverlayFS: OverlayFSConfig{
					Enabled: true,
					Mode:    "device",
				}},
			},
			wantErr: "device required",
		},
		{
			name: "unsafe directory",
			cfg: Config{
				OSFamily: "ubuntu",
				Provision: ProvisionConfig{OverlayFS: OverlayFSConfig{
					Enabled:   true,
					Directory: "bad,path",
				}},
			},
			wantErr: "provision.overlayFS.directory",
		},
		{
			name: "upper and work set together",
			cfg: Config{
				OSFamily: "ubuntu",
				Provision: ProvisionConfig{OverlayFS: OverlayFSConfig{
					Enabled:  true,
					UpperDir: "/var/lib/booty/overlayfs/upper",
				}},
			},
			wantErr: "upperDir and provision.overlayFS.workDir",
		},
		{
			name: "valid device mode normalizes",
			cfg: Config{
				OSFamily: "Ubuntu",
				Provision: ProvisionConfig{OverlayFS: OverlayFSConfig{
					Enabled:    true,
					Mode:       "Device",
					Device:     "LABEL=BOOTY-OVERLAY",
					Directory:  "booty",
					UpperDir:   "/var/lib/booty/overlayfs/upper",
					WorkDir:    "/var/lib/booty/overlayfs/work",
					TimeoutSec: 30,
				}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error: %v", err)
				}
				if tc.cfg.Provision.OverlayFS.Mode != "device" {
					t.Fatalf("OverlayFS.Mode = %q, want device", tc.cfg.Provision.OverlayFS.Mode)
				}
				return
			}
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadUnsupportedExtension(t *testing.T) {
	path := writeTestFile(t, "config.toml", "hostname = test")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unsupported extension")
	}
	if !strings.Contains(err.Error(), "unsupported config format") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestLoadEmptyPath(t *testing.T) {
	_, err := Load("")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestLoadPartialYAML(t *testing.T) {
	content := `
hostname: partial-node
provision:
  image:
    urls:
      - "https://example.com/image.gz"
`
	path := writeTestFile(t, "partial.yaml", content)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Hostname != "partial-node" {
		t.Errorf("Hostname = %q", cfg.Hostname)
	}
	if cfg.Mode != "" {
		t.Errorf("Mode should be empty, got %q", cfg.Mode)
	}
	if len(cfg.Provision.Image.URLs) != 1 {
		t.Errorf("Image.URLs len = %d", len(cfg.Provision.Image.URLs))
	}
}

func TestLoadJSONTrailingContent(t *testing.T) {
	content := `{"hostname":"test"}{"extra":true}`
	path := writeTestFile(t, "trailing.json", content)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for trailing JSON content")
	}
}

func TestLoadYMLExtension(t *testing.T) {
	content := `hostname: yml-node`
	path := writeTestFile(t, "config.yml", content)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Hostname != "yml-node" {
		t.Errorf("Hostname = %q", cfg.Hostname)
	}
}

func TestLoadYAMLMultiDocumentRejected(t *testing.T) {
	content := "hostname: first\n---\nhostname: second\n"
	path := writeTestFile(t, "multi.yaml", content)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for multi-document YAML")
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("expected 'trailing' in error: %v", err)
	}
}

func TestLoadJSONTrailingNull(t *testing.T) {
	content := `{"hostname":"test"} null`
	path := writeTestFile(t, "trailing-null.json", content)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for trailing null in JSON")
	}
}

func TestLoadValidationErrorIncludesPath(t *testing.T) {
	content := "hostname: test\nmode: totally-invalid\n"
	path := writeTestFile(t, "invalid.yaml", content)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("expected file path in error, got: %v", err)
	}
}

// TestLoadFullYAMLReference loads testdata/full.yaml and verifies that all key
// fields round-trip correctly. This acts as a canary: if a field is renamed or
// removed in the structs, the reference YAML will stop loading correctly.
func TestLoadFullYAMLReference(t *testing.T) {
	cfg, err := Load("testdata/full.yaml")
	if err != nil {
		t.Fatalf("Load(testdata/full.yaml): %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"Hostname", cfg.Hostname, "node-01"},
		{"Mode", cfg.Mode, "provision"},
		{"OSFamily", cfg.OSFamily, "ubuntu"},
		{"Network.Mode", cfg.Network.Mode, "gobgp"},
		{"Network.DNSResolvers", cfg.Network.DNSResolvers, "8.8.8.8,1.1.1.1"},
		{"Network.BGP.ASN", cfg.Network.BGP.ASN, uint32(65001)},
		{"Network.BGP.PeerMode", cfg.Network.BGP.PeerMode, "unnumbered"},
		{"Network.BGP.MinPeers", cfg.Network.BGP.MinPeers, 2},
		{"Network.BGP.BFDTransmitMS", cfg.Network.BGP.BFDTransmitMS, uint32(0)},
		{"Network.BGP.BFDReceiveMS", cfg.Network.BGP.BFDReceiveMS, uint32(0)},
		{"Network.EVPN.UnderlaySubnet", cfg.Network.EVPN.UnderlaySubnet, "192.168.4.0/24"},
		{"Network.EVPN.ProvisionVNI", cfg.Network.EVPN.ProvisionVNI, uint32(100)},
		{"Network.EVPN.ProvisionIP", cfg.Network.EVPN.ProvisionIP, "10.100.0.20/24"},
		{"Network.Bond.Mode", cfg.Network.Bond.Mode, "802.3ad"},
		{"Network.VRF.TableID", cfg.Network.VRF.TableID, uint32(1000)},
		{"Network.IPMI.Subnet", cfg.Network.IPMI.Subnet, "172.30.0.0/24"},
		{"Transport.Token", cfg.Transport.Token, "bootstrap-token-xyz"},
		{"Transport.TokenAlgorithm", cfg.Transport.TokenAlgorithm, "RS256"},
		{"Transport.InitURL", cfg.Transport.InitURL, "https://caprf.example.com/status/init"},
		{"Health.Enabled", cfg.Health.Enabled, true},
		{"Health.MinMemoryGB", cfg.Health.MinMemoryGB, 16},
		{"Health.MinCPUs", cfg.Health.MinCPUs, 4},
		{"Health.SkipChecks", cfg.Health.SkipChecks, "thermal"},
		{"Telemetry.Enabled", cfg.Telemetry.Enabled, true},
		{"Telemetry.MetricsURL", cfg.Telemetry.MetricsURL, "https://caprf.example.com/metrics"},
		{"Rescue.Mode", cfg.Rescue.Mode, "retry"},
		{"Rescue.Timeout", cfg.Rescue.Timeout, 300},
		{"Provision.TargetOS", cfg.Provision.TargetOS, TargetOSLinux},
		{"Provision.ExtraKernelParams", cfg.Provision.ExtraKernelParams, "console=ttyS0,115200"},
		{"Provision.FailureDomain", cfg.Provision.FailureDomain, "zone-a"},
		{"Provision.Region", cfg.Provision.Region, "eu-west-1"},
		{"Provision.ProviderID", cfg.Provision.ProviderID, "metal://node-01"},
		{"Provision.Image.ChecksumType", cfg.Provision.Image.ChecksumType, "sha256"},
		{"Provision.Image.ChecksumScope", cfg.Provision.Image.ChecksumScope, "download"},
		{"Provision.Image.Mode", cfg.Provision.Image.Mode, "whole-disk"},
		{"Provision.Disk.MinSizeGB", cfg.Provision.Disk.MinSizeGB, 100},
		{"Provision.Disk.NumVFs", cfg.Provision.Disk.NumVFs, 32},
		{"Provision.SecureBoot.PinnedDigests.Shim", cfg.Provision.SecureBoot.PinnedDigests["shim"], strings.Repeat("a", 64)},
		{"Provision.SecureBoot.PinnedDigests.Grub", cfg.Provision.SecureBoot.PinnedDigests["grub"], "sha256:" + strings.Repeat("b", 64)},
		{"Provision.SecureBoot.PinnedDigests.Kernel", cfg.Provision.SecureBoot.PinnedDigests["kernel"], strings.Repeat("c", 64)},
		{"Provision.Sysext.DefaultMode", cfg.Provision.Sysext.DefaultMode, "preload"},
		{"Provision.Sysext.CatalogDir", cfg.Provision.Sysext.CatalogDir, "/usr/lib/tcaas-sysext/preloaded"},
		{"Provision.OverlayFS.Enabled", cfg.Provision.OverlayFS.Enabled, true},
		{"Provision.OverlayFS.Mode", cfg.Provision.OverlayFS.Mode, "device"},
		{"Provision.OverlayFS.Device", cfg.Provision.OverlayFS.Device, "LABEL=BOOTY-OVERLAY"},
		{"Provision.OverlayFS.Directory", cfg.Provision.OverlayFS.Directory, "booty"},
		{"Provision.OverlayFS.UpperDir", cfg.Provision.OverlayFS.UpperDir, "/var/lib/booty/overlayfs/upper"},
		{"Provision.OverlayFS.WorkDir", cfg.Provision.OverlayFS.WorkDir, "/var/lib/booty/overlayfs/work"},
		{"Provision.OverlayFS.TimeoutSec", cfg.Provision.OverlayFS.TimeoutSec, 30},
		{"Provision.Firmware.MinBIOS", cfg.Provision.Firmware.MinBIOS, "2.10"},
		{"Provision.Firmware.MinBMC", cfg.Provision.Firmware.MinBMC, "4.50"},
		{"Provision.CrashArtifacts.MaxMB", cfg.Provision.CrashArtifacts.MaxMB, 256},
		{"Provision.CrashArtifacts.UploadTimeoutSec", cfg.Provision.CrashArtifacts.UploadTimeoutSec, 120},
		{"Provision.CrashArtifacts.IncludeMemoryDumps", cfg.Provision.CrashArtifacts.IncludeMemoryDumps, true},
		{"Agent.HeartbeatURL", cfg.Agent.HeartbeatURL, "https://caprf.example.com/agent/heartbeat"},
		{"Agent.CommandsURL", cfg.Agent.CommandsURL, "https://caprf.example.com/agent/commands"},
		{"Agent.DeprovisionSuccessAction", cfg.Agent.DeprovisionSuccessAction, "standby"},
	}

	for _, c := range checks {
		if fmt.Sprintf("%v", c.got) != fmt.Sprintf("%v", c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}

	if len(cfg.Provision.Image.URLs) != 1 {
		t.Errorf("Provision.Image.URLs len = %d, want 1", len(cfg.Provision.Image.URLs))
	}
	if len(cfg.Provision.Disk.RAID) != 1 {
		t.Errorf("Provision.Disk.RAID len = %d, want 1", len(cfg.Provision.Disk.RAID))
	}
	if cfg.Provision.Disk.RAID[0].Name != "md0" {
		t.Errorf("RAID[0].Name = %q, want md0", cfg.Provision.Disk.RAID[0].Name)
	}
	if cfg.Provision.Disk.RAID[0].Level != 1 {
		t.Errorf("RAID[0].Level = %d, want 1", cfg.Provision.Disk.RAID[0].Level)
	}
	if len(cfg.Provision.PostProvisionCmds) != 1 {
		t.Errorf("PostProvisionCmds len = %d, want 1", len(cfg.Provision.PostProvisionCmds))
	}
	if len(cfg.Provision.Sysext.Layers) != 1 {
		t.Fatalf("Provision.Sysext.Layers len = %d, want 1", len(cfg.Provision.Sysext.Layers))
	}
	if cfg.Provision.Sysext.Layers[0].Name != "node-tuning" {
		t.Errorf("Provision.Sysext.Layers[0].Name = %q, want node-tuning", cfg.Provision.Sysext.Layers[0].Name)
	}
}

func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
