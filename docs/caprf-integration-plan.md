# CAPRF Provisioning Integration into BOOTy

> **Historical plan.** This page is the original implementation plan for the
> CAPRF integration. The code sketches, step lists and file names show the
> plan, not the current code. For current behaviour, see
> [CAPRF integration](caprf-integration.md) and
> [Migration from the CAPRF legacy ramdisk](caprf-legacy-migration.md).

## Status: Implemented

The integration is in place. The implementation differs from this plan in
many details. Current CI truth lives in `.github/workflows/ci.yml`. Use the
current default-branch workflow run as status truth rather than historical
job counts or the original integration branch name.

## Overview

Integrate all cluster-api-provider-redfish (CAPRF) provisioning capabilities into BOOTy as a
single-binary ramdisk agent with full FRR/EVPN networking, CAPRF-compatible API, ISO+OCI packaging,
and kexec support. This replaces the current deployer-base-image (~900-line autorun.sh) and
debirf-ironic-ramdisk (22 debirf modules, 5 Python scripts, 10 systemd services) with a single
Go binary.

All existing BOOTy APIs (.bty config format, BOOTYURL, plunderclient) are deliberately broken and
replaced with a clean CAPRF-compatible interface. Architecture supports future persistent "agent mode"
and optional GoBGP replacement for FRR (see Addenda A and B).

### Repository Strategy

- **All new code** lives in the **BOOTy repository** (`github.com/telekom/BOOTy`)
- **CAPRF changes** go in a single branch `feat/booty-provisioner` with a draft MR to main
- CAPRF is on GitLab with separate CI constraints (see CI section)
- Use `git -c commit.gpgsign=false commit` (GPG signing is broken in this environment)

### CI Strategy

| Runner | Platform | Capabilities | Used For |
|--------|----------|-------------|----------|
| `ubuntu-latest` | GitHub Actions | Docker, QEMU (usermode), ContainerLab | Lint, test, build, e2e-clab, e2e-redfish (mock), KVM boot |
| `[shell, gcp, linux]` | GitLab (self-hosted) | Docker, KVM, libvirt, privileged | Full sushy-tools + QEMU + ISO boot cycle |

- **ContainerLab**: Installs via `bash -c "$(curl -sL https://get.containerlab.dev)"` on ubuntu-latest
- **QEMU**: Already proven on ubuntu-latest (KVM Boot Validation job uses `qemu-system-x86_64`)
- **sushy-tools**: On GitHub Actions, use a Go `httptest.Server` mock returning static Redfish JSON.
  On GitLab self-hosted runners, use full sushy-tools with libvirt+QEMU backend.

---

## Phase 0 — E2E Test Infrastructure

**Goal**: CI framework for network and Redfish testing

### Step 0.1 — ContainerLab Topology

Create `test/e2e/clab/topology.clab.yml` with a 2-node FRR setup:

- **spine01** — FRR container, AS 65000, route-reflector
- **leaf01** — FRR container, AS 65001, eBGP peering to spine01

FRR config templates in `test/e2e/clab/`:
- `spine01.frr.conf` — eBGP underlay + EVPN address-family
- `leaf01.frr.conf` — eBGP underlay + EVPN with VXLAN VNI

Topology uses `containerlab` kind `linux` with FRR image `quay.io/frrouting/frr:10.3.1`.

**Validation**: `sudo containerlab deploy --topo topology.clab.yml` succeeds,
`docker exec clab-booty-spine01 vtysh -c "show bgp summary"` shows established peers.

### Step 0.2 — Redfish Mock Server

Create `test/e2e/redfish/mock_server.go` — a Go `httptest.Server` that:
- Serves static Redfish JSON responses (Systems, Managers, VirtualMedia)
- Supports `GET /redfish/v1/Systems/1`
- Supports `POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset`
- Supports virtual media insert/eject
- No libvirt dependency — pure Go, runs anywhere

This replaces sushy-tools for GitHub Actions CI. Full sushy-tools testing deferred to GitLab.

### Step 0.3 — CI Workflow Updates

Add to `.github/workflows/ci.yml`:

```yaml
e2e-clab:
  name: E2E ContainerLab
  runs-on: ubuntu-latest
  needs: [build]
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with: { go-version: "1.26" }
    - name: Install ContainerLab
      run: bash -c "$(curl -sL https://get.containerlab.dev)"
    - name: Deploy topology
      run: sudo containerlab deploy --topo test/e2e/clab/topology.clab.yml
    - name: Run E2E tests
      run: go test -tags e2e_integration -v -race -count=1 ./test/e2e/integration/...
    - name: Cleanup
      if: always()
      run: sudo containerlab destroy --topo test/e2e/clab/topology.clab.yml
```

### Step 0.4 — E2E Test Framework

Create `test/e2e/integration/` with build tag `e2e_integration`:
- `suite_test.go` — Ginkgo bootstrap, connects to ContainerLab containers
- `frr_test.go` — Verify BGP peering establishment, EVPN route exchange
- `redfish_test.go` — Verify BOOTy ↔ CAPRF server communication

---

## Phase 1 — CAPRF API Client & Config Provider

**Goal**: BOOTy communicates with CAPRF server (status reporting, log shipping, config fetching)

### Step 1.1 — Config Provider Abstraction

Create `pkg/config/provider.go`:

```go
// Provider abstracts provisioning server communication.
type Provider interface {
    // GetConfig fetches machine configuration (image URLs, hostname, etc.)
    GetConfig(ctx context.Context) (*MachineConfig, error)
    // ReportStatus sends provisioning status to the server.
    ReportStatus(ctx context.Context, status Status, message string) error
    // ShipLog sends a log line to the server.
    ShipLog(ctx context.Context, message string) error
    // Heartbeat sends a keepalive signal (no-op in current mode, used by future agent mode).
    Heartbeat(ctx context.Context) error
    // FetchCommands retrieves pending commands (nil in current mode, used by future agent mode).
    FetchCommands(ctx context.Context) ([]Command, error)
}

type Status string
const (
    StatusInit    Status = "init"
    StatusSuccess Status = "success"
    StatusError   Status = "error"
)

// MachineConfig holds all configuration needed for provisioning.
type MachineConfig struct {
    ImageURLs           []string          // Space-separated IMAGE field
    Hostname            string            // HOSTNAME
    Token               string            // TOKEN (Bearer auth)
    ExtraKernelParams   string            // MACHINE_EXTRA_KERNEL_PARAMS
    FailureDomain       string            // FAILURE_DOMAIN
    Region              string            // REGION
    ProviderID          string            // PROVIDER_ID
    Mode                string            // MODE (provision/deprovision/soft-deprovision)
    MinDiskSizeGB       int               // MIN_DISK_SIZE_GB (optional)
}

// Command represents a server-issued command (future agent mode).
type Command struct {
    ID      string
    Type    string
    Payload []byte
}
```

### Step 1.2 — CAPRF Client Implementation

Create `pkg/caprf/client.go`:

```go
type Client struct {
    baseURL    string   // CAPRF server endpoint (derived from status URLs)
    token      string   // Bearer token from /deploy/vars
    httpClient *http.Client
}
```

Methods:
- `GetConfig(ctx) (*config.MachineConfig, error)` — Parse kernel cmdline for `IMAGE`, `HOSTNAME`,
  `TOKEN`, etc. (injected by CAPRF into `/deploy/vars` which sets kernel params)
- `ReportStatus(ctx, status, message)` — POST to `/status/{init,success,error}` with Bearer auth
- `ShipLog(ctx, message)` — POST to `/log` with Bearer auth
- `ShipDebug(ctx, message)` — POST to `/debug` with Bearer auth
- `Heartbeat(ctx)` — No-op (returns nil) in current mode
- `FetchCommands(ctx)` — No-op (returns nil, nil) in current mode

Create `pkg/caprf/types.go` — parsing logic for the 13-variable `/deploy/vars` format:

```
export IMAGE="..."
export HOSTNAME="..."
export TOKEN="..."
export MACHINE_EXTRA_KERNEL_PARAMS="..."
export FAILURE_DOMAIN="..."
export REGION="..."
export PROVIDER_ID="..."
export LOG_URL="..."
export INIT_URL="..."
export ERROR_URL="..."
export SUCCESS_URL="..."
export DEBUG_URL="..."
export MODE="..."
```

### Step 1.3 — Remote slog Handler

Create `pkg/caprf/loghandler.go`:

```go
// RemoteHandler is a slog.Handler that ships log lines to CAPRF /log endpoint.
type RemoteHandler struct {
    client  *Client
    level   slog.Leveler
    buf     chan string     // buffered, non-blocking
    attrs   []slog.Attr
    groups  []string
}
```

- Implements `slog.Handler` interface
- Buffers log lines in a channel (capacity 1000)
- Background goroutine drains buffer, POSTs to `/log`
- Non-blocking: if buffer full, drops oldest entry (never blocks caller)
- Wraps existing terminal handler (logs go to both console and remote)

### Step 1.4 — Main Integration

Rewrite `main.go` boot flow:

1. DefaultMounts, device setup (unchanged)
2. **Detect mode**: Check for CAPRF kernel cmdline params (`TOKEN`, `IMAGE`, `HOSTNAME`).
   If present → CAPRF mode. Otherwise → legacy mode (or error).
3. CAPRF mode:
   - Parse `/deploy/vars` (mounted from ISO or injected)
   - Create `caprf.Client` with parsed config
   - Create `RemoteHandler`, add to slog
   - Report `StatusInit`
   - Proceed to provisioning orchestrator
4. Legacy BOOTy mode: removed (APIs broken as agreed)

**Coverage target**: 80%+ for `pkg/config/`, `pkg/caprf/`

---

## Phase 2 — Network Stack (FRR EVPN)

**Goal**: Full FRR/EVPN networking — replaces configure-frr.py + bgp-interface.py

### Step 2.1 — Network Mode Abstraction

Create `pkg/network/mode.go`:

```go
// NetworkMode configures network connectivity in the ramdisk.
type NetworkMode interface {
    // Setup configures all networking (interfaces, routing, etc.)
    Setup(ctx context.Context, cfg NetworkConfig) error
    // WaitForConnectivity blocks until network is reachable.
    WaitForConnectivity(ctx context.Context, target string, timeout time.Duration) error
    // Teardown cleans up network configuration.
    Teardown(ctx context.Context) error
}

type NetworkConfig struct {
    // DHCP mode
    Interfaces []string      // NICs to configure

    // FRR/EVPN mode (from kernel cmdline)
    IPMIMAC       string     // Used for IP derivation
    IPMIIP        string     // Used for IP derivation
    UnderlayASN   uint32     // BGP ASN for underlay
    OverlayVNI    uint32     // VXLAN VNI
    BridgeName    string     // Default: "br.provision"
    VRFName       string     // Default: "Vrf_underlay"
    MTU           int        // Default: 9000
}
```

Implementations:
- `DHCPMode` — Current DHCP-on-eth0 behavior
- `FRRMode` — Full FRR/EVPN setup (Step 2.2)
- `StaticMode` — Static IP configuration
- Future: `GoBGPMode` (Addendum B)

### Step 2.2 — FRR Manager

Create `pkg/network/frr/manager.go`:

Replicates the complete logic of:
- `configure-frr.py` (~200 lines Python) — VRF/VXLAN/bridge creation, IP derivation from IPMI
- `bgp-interface.py` — Per-NIC BGP neighbor configuration via vtysh

**IP derivation logic** (from IPMI MAC/IP):
```
underlay_ip = derive from IPMI IP with offset
overlay_ip  = derive from IPMI IP with different offset
bridge_mac  = "02:54:" + last 4 bytes of IPMI MAC
```

**Interface creation sequence** (via `ip` commands or netlink):
1. Create VRF `Vrf_underlay` with table ID 1
2. Create dummy interface `dummy.underlay`, assign to VRF
3. Assign underlay IP to `dummy.underlay`
4. Create VXLAN interface `vx{VNI}` (nolearning, dstport 4789)
5. Create bridge `br.provision` with MAC `bridge_mac`
6. Attach VXLAN to bridge
7. Assign overlay IP to loopback
8. Set MTU 9000 on all physical NICs

**FRR configuration generation**:
```
frr version 8.2.2
router bgp {ASN} vrf Vrf_underlay
 bgp router-id {underlay_ip}
 neighbor fabric peer-group
 neighbor fabric remote-as external
 {per-NIC neighbors}
 address-family ipv4 unicast
  redistribute connected
 exit-address-family
 address-family l2vpn evpn
  neighbor fabric activate
  advertise-all-vni
 exit-address-family
exit
```

**FRR daemon management**:
- Write config to `/etc/frr/frr.conf`
- Start daemons: `bgpd`, `zebra`, `bfdd` (via direct exec or systemctl)
- Enable daemons in `/etc/frr/daemons`: bgpd=yes, zebra=yes, bfdd=yes

### Step 2.3 — Per-NIC BGP Peering

Auto-detect physical NICs (exclude lo, docker*, veth*, vx*, br*):
```go
func DetectPhysicalNICs() ([]string, error)
```

For each NIC:
- Set MTU 9000
- Assign to VRF `Vrf_underlay`
- Configure eBGP unnumbered peer via vtysh:
  ```
  vtysh -c "conf t" \
        -c "router bgp {ASN} vrf Vrf_underlay" \
        -c "neighbor {NIC} interface peer-group fabric"
  ```

### Step 2.4 — Network Connectivity Wait

Implement `WaitForConnectivity`:
- Poll target URL (INIT_URL) with HTTP HEAD, 10s per attempt
- Every 20s: restart FRR (`systemctl restart frr` or re-exec daemons)
- Log each attempt
- Timeout configurable (default 300s)

Replicates `wait_for_network()` from common.sh.

**Coverage target**: 80%+ for `pkg/network/`

---

## Phase 3 — Disk & Provisioning Pipeline

**Goal**: Full CAPRF provisioning pipeline in Go

### Step 3.1 — Image Streamer

Rewrite `pkg/image/image.go`:

```go
// Stream downloads an image from URL and writes it to a block device.
func Stream(ctx context.Context, urls []string, device string, opts StreamOpts) error

type StreamOpts struct {
    Compression   string // "auto", "gzip", "lz4", "raw"
    RetryAttempts int    // default 3
    RetryDelay    time.Duration
    ProgressFn    func(bytesWritten int64) // optional progress callback
}
```

**Multi-source support**: Test each URL's response time (like `pick_best_source` in common.sh),
select fastest. Fall back to next URL on failure.

**Compression auto-detection**: Check URL suffix (.gz → gzip, .lz4 → lz4, else raw) or
Content-Type header. Stream through decompressor pipe.

**Pipeline**: `HTTP GET → decompress → write to device (O_DIRECT, bs=1M)`

### Step 3.2 — Disk Manager

Create `pkg/disk/manager.go`:

```go
type Manager struct{}

// StopRAIDArrays stops all RAID arrays.
func (m *Manager) StopRAIDArrays(ctx context.Context) error

// WipeAllDisks runs wipefs -af on all disks (excluding loop, CD-ROM).
func (m *Manager) WipeAllDisks(ctx context.Context) error

// DetectDisk finds the target disk. Prefers NVMe, falls back to SATA.
func (m *Manager) DetectDisk(ctx context.Context, minSizeGB int) (string, error)

// ParsePartitions reads partition table via sfdisk --json.
func (m *Manager) ParsePartitions(ctx context.Context, disk string) ([]Partition, error)

// FindBootPartition finds the EFI System Partition.
func (m *Manager) FindBootPartition(parts []Partition) (*Partition, error)

// FindRootPartition finds the primary Linux partition.
func (m *Manager) FindRootPartition(parts []Partition) (*Partition, error)

// GrowPartition grows partition to fill available space.
func (m *Manager) GrowPartition(ctx context.Context, disk string, partNum int) error

// ResizeFilesystem resizes ext4/xfs filesystem to fill partition.
func (m *Manager) ResizeFilesystem(ctx context.Context, device string) error

// MountPartition mounts a partition at the given path.
func (m *Manager) MountPartition(ctx context.Context, device, mountpoint string) error
```

External tool execution: `exec.CommandContext` with proper error handling and logging.

**Partition type GUIDs**:
- EFI System: `C12A7328-F81F-11D2-BA4B-00A0C93EC93B`
- Linux filesystem: `0FC63DAF-8483-4772-8E79-3D69D8477DE4`

### Step 3.3 — OS Configurator

Create `pkg/provision/configurator.go`:

Operations on mounted root filesystem at `/newroot`:

1. **Set hostname**: Write to `/newroot/etc/hostname`
2. **Configure kubelet**:
   - Write `KUBELET_EXTRA_ARGS` to kubeadm-supported env files:
     `/etc/default/kubelet` on Debian-like targets or
     `/etc/sysconfig/kubelet` on RPM-like targets
   - Merge `--provider-id={PROVIDER_ID}` and topology labels for
     `topology.kubernetes.io/zone` and `region`
3. **Write custom files**: Iterate `MachineConfig.Files`, write each to filesystem
   with correct path, owner, permissions
4. **Execute custom commands**: For each command in `MachineConfig.Commands`,
   run via `chroot /newroot /bin/bash -c "{command}"`
5. **Configure GRUB**:
   - Write `/etc/default/grub.d/10-caprf-kernel-params.cfg`
   - Content includes the selected cloud-init datasource parameter:
     `ds=nocloud` for NoCloud, `ci.datasource=ConfigDrive` for ConfigDrive.
   - Console selection: resolved by `pkg/serialconsole` from explicit override,
     ACPI SPCR, device-tree `stdout-path`, the running boot console, and sysfs
     UART enumeration. DMI is recorded as context only and never selects a port.
     Exactly one `console=` parameter is emitted and it fails closed on
     ambiguous evidence. See
     [serial-console-resolution.md](serial-console-resolution.md).
   - Run `chroot /newroot update-grub`
6. **Copy machine files**: Copy files from machine config to root filesystem
7. **Cloud-init setup**:
   - NoCloud writes seed files below
     `/var/lib/cloud/seed/nocloud/`
   - ConfigDrive writes OpenStack seed files below
     `/var/lib/cloud/seed/config_drive/openstack/latest/`
8. **EFI boot management**:
   - Mount efivarfs
   - Remove old ubuntu boot entries (efibootmgr)
9. **Mellanox NIC configuration**:
   - Detect ConnectX-4/5/6/6Lx via lspci
   - Set NUM_OF_VFS=32 via mstconfig

**Bind mounts for chroot**:
```
mount --bind /dev  /newroot/dev
mount --bind /proc /newroot/proc
mount --bind /sys  /newroot/sys
mount --bind /run  /newroot/run
mount --bind /sys/firmware/efi/efivars /newroot/sys/firmware/efi/efivars
```

### Step 3.4 — Provisioning Orchestrator

Create `pkg/provision/orchestrator.go`:

Replaces CAPRF's 17-step provision.sh pipeline:

```go
func (o *Orchestrator) Provision(ctx context.Context, cfg *config.MachineConfig, provider config.Provider) error {
    steps := []Step{
        {"report-init",          o.reportInit},
        {"set-hostname",         o.setHostname},
        {"mount-efivarfs",       o.mountEFIVars},
        {"copy-provisioner-files", o.copyProvisionerFiles},
        {"configure-dns",        o.configureDNS},
        {"stop-raid",            o.stopRAID},
        {"remove-efi-entries",   o.removeEFIBootEntries},
        {"setup-mellanox",       o.setupMellanox},
        {"wipe-disks",           o.wipeDisks},
        {"detect-disk",          o.detectDisk},
        {"stream-image",         o.streamImage},
        {"parse-partitions",     o.parsePartitions},
        {"check-filesystem",     o.checkFilesystem},
        {"mount-root",           o.mountRoot},
        {"grow-partition",       o.growPartition},
        {"resize-filesystem",    o.resizeFilesystem},
        {"configure-kubelet",    o.configureKubelet},
        {"read-dmi",             o.readDMI},
        {"configure-grub",       o.configureGRUB},
        {"write-custom-files",   o.writeCustomFiles},
        {"run-custom-commands",  o.runCustomCommands},
        {"write-machine-files",  o.writeMachineFiles},
        {"unmount-chroot",       o.unmountChroot},
        {"report-success",       o.reportSuccess},
    }
    for _, step := range steps {
        if err := step.Fn(ctx, cfg); err != nil {
            provider.ReportStatus(ctx, config.StatusError, fmt.Sprintf("step %s failed: %v", step.Name, err))
            return fmt.Errorf("provision step %s: %w", step.Name, err)
        }
    }
    return nil
}
```

**Coverage target**: 80%+ for `pkg/disk/`, `pkg/provision/`, `pkg/image/`

---

## Phase 4 — Kexec Support

**Goal**: After writing OS to disk, kexec into installed kernel (skips full reboot)

### Step 4.1 — Grub Parser

Create `pkg/kexec/grub.go`:

Port tinkerbell/actions grub.cfg parser:

```go
// Config represents a grub boot entry.
type Config struct {
    Name       string
    Kernel     string   // Path to kernel (e.g., /boot/vmlinuz-5.15.0-generic)
    Initramfs  string   // Path to initrd (e.g., /boot/initrd.img-5.15.0-generic)
    KernelArgs string   // Kernel command line
}

// ParseGrubCfg parses a grub.cfg file and returns all boot entries.
func ParseGrubCfg(r io.Reader) ([]Config, error)

// GetDefaultConfig returns the first (default) boot entry.
func GetDefaultConfig(configs []Config) (*Config, error)
```

Handles `menuentry`, `linux`/`linux16`/`linuxefi`, `initrd`/`initrd16`/`initrdefi` directives.

### Step 4.2 — Kexec Loader

Create `pkg/kexec/kexec.go` (build tag `//go:build linux`):

```go
// Load loads a kernel for kexec. Call Execute() to boot into it.
func Load(kernelPath, initrdPath, cmdline string) error {
    kernelFd, _ := os.Open(kernelPath)
    defer kernelFd.Close()
    initrdFd, _ := os.Open(initrdPath)
    defer initrdFd.Close()
    return unix.KexecFileLoad(int(kernelFd.Fd()), int(initrdFd.Fd()), cmdline, 0)
}

// Execute performs the kexec reboot.
func Execute() error {
    return unix.Reboot(unix.LINUX_REBOOT_CMD_KEXEC)
}
```

Fallback: If kexec fails (unsupported kernel, secure boot), fall back to normal reboot.

### Step 4.3 — Integration

After provisioning completes:
1. Look for installed kernel in `/newroot/boot/vmlinuz-*` (glob, pick latest)
2. Parse `/newroot/boot/grub/grub.cfg` with `ParseGrubCfg`
3. Get default boot entry
4. Adjust paths (prepend `/newroot` to kernel and initrd paths)
5. Load via `KexecFileLoad`
6. Unmount all filesystems
7. Execute kexec (or fallback to normal reboot)

**Coverage target**: 80%+ for `pkg/kexec/`

---

## Phase 5 — Deprovision & ISO/OCI Packaging

**Goal**: Deprovision support + build artifacts matching CAPRF expectations

### Step 5.1 — Deprovisioner

Create `pkg/provision/deprovision.go`:

```go
func (o *Orchestrator) Deprovision(ctx context.Context, cfg *config.MachineConfig, provider config.Provider) error
```

**Modes** (from `MODE` env var):
- `soft`: Rename `/boot/grub/grub.cfg` → `grub.cfg.bak` (makes system unbootable)
- `hard` (default): Run `wipefs -af` on all disks, remove EFI boot entries

**Steps**:
1. Report init
2. Copy provisioner files
3. Configure DNS fallback
4. Wait for network
5. If soft → rename grub.cfg
6. If hard → wipe all disks
7. Mount efivarfs, remove EFI boot entries
8. Report success
9. Shutdown

### Step 5.2 — ISO Builder

Create `cmd/geniso/` or Makefile target for ISO generation:

Uses `xorrisofs` to create a hybrid BIOS+UEFI bootable ISO:

```bash
xorrisofs \
  -input-charset utf-8 \
  -rational-rock \
  -volid "caas-deploy-image" \
  -cache-inodes -joliet -full-iso9660-filenames \
  -eltorito-catalog boot.cat \
  -eltorito-boot isolinux/isolinux.bin \
    -no-emul-boot -boot-load-size 4 -boot-info-table \
  -eltorito-alt-boot \
    -e EFI/ubuntu/efiboot.img -no-emul-boot \
  -o output.iso \
  iso-root/
```

**ISO directory structure** (must match CAPRF builder expectations):
```
/isolinux/         — isolinux.bin, isolinux.cfg (BIOS boot)
/EFI/ubuntu/       — grub.cfg, efiboot.img (UEFI boot)
/deploy/vars       — environment variables (IMAGE, TOKEN, etc.)
/deploy/autorun.sh — BOOTy binary launch script
/deploy/file-system/ — provisioner files
/deploy/machine-files/ — machine configuration files
/deploy/machine-commands/ — custom commands
/boot/             — kernel + initrd (optional, for kexec)
```

**Compatibility**: The ISO structure must match what CAPRF's `ramdisk.Builder` produces so that
the CAPRF server can serve it via virtual media and the BMC boots from it.

### Step 5.3 — OCI Image

Create `Dockerfile.oci`:

```dockerfile
FROM scratch
COPY booty /booty
COPY busybox /bin/busybox
ENTRYPOINT ["/booty"]
```

Publishable to container registry for OCI-based deployment scenarios.

### Step 5.4 — Initrd Enhancement

Update `initrd.Dockerfile` to include additional tools needed for provisioning:

**FRR binaries**: bgpd, zebra, bfdd, vtysh
**Disk tools**: mdadm, wipefs, growpart, sgdisk, sfdisk, lsblk, resize2fs, xfs_growfs,
e2fsck, partprobe, partx, parted
**Networking tools**: ip, bridge, ethtool, curl
**System tools**: dmidecode, efibootmgr, chroot, mount, umount, hostname
**Mellanox tools**: mstconfig (optional, large)

Multi-stage build to keep image minimal.

**Coverage target**: 80%+ for `pkg/provision/` (deprovision paths)

---

## Phase 6 — CAPRF Integration Branch

**Goal**: Minimal CAPRF changes to use BOOTy as provisioning agent

### Step 6.1 — Ramdisk Builder Update

In CAPRF branch `feat/booty-provisioner`:

Modify `internal/ramdisk/builder.go`:
- Replace deployer-base-image with BOOTy ISO as base
- Keep `/deploy/vars` format unchanged (13 env vars)
- BOOTy reads vars file at boot and enters provisioning mode
- ISO structure remains compatible

### Step 6.2 — Server Compatibility Verification

Verify BOOTy's CAPRF client works against actual CAPRF server endpoints:
- Same auth flow (Bearer token from `?t=` param or Authorization header)
- Same status codes and event emission
- Same log/debug endpoint format

### Step 6.3 — E2E Validation (GitLab)

On GitLab `[shell, gcp, linux]` runners:
1. Build BOOTy ISO
2. Start sushy-tools with QEMU backend
3. Create virtual machine
4. Boot from BOOTy ISO via Redfish virtual media
5. Verify provisioning pipeline completes
6. Verify status reports received by CAPRF server

---

## Phase 7 — Documentation & Hardening

### Step 7.1 — Architecture Documentation

Update README.md with:
- Architecture diagram (BOOTy as CAPRF provisioning agent)
- Build instructions (binary, initramfs, ISO, OCI)
- CAPRF integration guide
- Network mode documentation (DHCP, FRR, Static)

### Step 7.2 — Coverage Gate

Add CI step:
```bash
go tool cover -func=coverage.out | grep total | awk '{print $NF}' | \
  sed 's/%//' | awk '{if ($1 < 80) exit 1}'
```

### Step 7.3 — Hardening

- Retry logic on HTTP calls (exponential backoff)
- Timeout handling on all external commands
- Graceful degradation (network down, disk not found)
- Signal handling (SIGTERM → clean unmount → status report)

---

## Phase Dependency Graph

```
Phase 0 (Test Infra)
  ├──→ Phase 1 (CAPRF Client) ──→ Phase 3 (Provisioning) ──→ Phase 4 (Kexec)
  │                                                        ──→ Phase 5 (ISO/OCI)
  └──→ Phase 2 (Network/FRR)  ──→ Phase 3                 ──→ Phase 6 (CAPRF Branch)
                                                           ──→ Phase 7 (Docs/Hardening)
```

Phases 1 and 2 can run in parallel after Phase 0.
Phases 4 and 5 can run in parallel after Phase 3.

---

## Feature Parity Checklist

The parity matrix in
[Migration from the CAPRF legacy ramdisk](caprf-legacy-migration.md#parity-matrix)
replaces this checklist. It covers every row of the former checklist and
shows the current status.

---

## Addendum A — Future Agent Mode

### Concept

Instead of boot → provision → shutdown, the ramdisk agent stays running and maintains a persistent
connection to CAPRF. This enables in-service updates, rolling config changes, health monitoring,
and graceful drain.

### BOOTy Side (Already Scaffolded)

The `Provider` interface includes `Heartbeat(ctx)` and `FetchCommands(ctx)`. This plan
described them as no-op stubs. Today `MODE=standby` uses them: it sends heartbeats to
`HEARTBEAT_URL` and polls `COMMANDS_URL` (see [CAPRF integration](caprf-integration.md#modes)).
The planned agent loop:

```go
// Agent mode main loop (future implementation)
func agentLoop(ctx context.Context, provider config.Provider) error {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            if err := provider.Heartbeat(ctx); err != nil {
                slog.Error("heartbeat failed", "error", err)
            }
            cmds, err := provider.FetchCommands(ctx)
            if err != nil {
                slog.Error("fetch commands failed", "error", err)
                continue
            }
            for _, cmd := range cmds {
                executeCommand(ctx, cmd, provider)
            }
        }
    }
}
```

### CAPRF Side (Required Changes)

**Estimated LOC**: ~4,100–5,250

#### Phase Model Expansion

File: `api/v1alpha1/redfishmachine_phases.go`

Add phases:
- `PhaseRunning` — Agent booted successfully, alive but idle
- `PhaseConnected` — Agent handshake complete, awaiting commands
- `PhaseUpdating` — Agent executing in-service config/update commands
- `PhaseHealthCheckFailed` — Heartbeat timeout detected (requires recovery)
- `PhaseDraining` — Graceful shutdown initiated, waiting for agent readiness

#### Status Model Enhancement

File: `api/v1alpha1/redfishmachine_types.go`

```go
type AgentHealthStatus struct {
    LastHeartbeat     metav1.Time   `json:"lastHeartbeat,omitempty"`
    HeartbeatInterval string        `json:"heartbeatInterval,omitempty"` // e.g., "30s"
    Healthy           bool          `json:"healthy,omitempty"`
    Message           string        `json:"message,omitempty"`
}

// Add to RedfishMachineStatus:
//   AgentHealth      AgentHealthStatus `json:"agentHealth,omitempty"`
//   AgentConnected   bool              `json:"agentConnected,omitempty"`
//   PendingCommands  int               `json:"pendingCommands,omitempty"`
//   RunningCommand   *CommandStatus    `json:"runningCommand,omitempty"`
```

#### Spec Changes

```go
// Add to RedfishMachineSpec:
//   AgentMode              bool   `json:"agentMode,omitempty"`
//   AgentHeartbeatInterval string `json:"agentHeartbeatInterval,omitempty"` // default "30s"
//   AgentHeartbeatTimeout  string `json:"agentHeartbeatTimeout,omitempty"`  // default "120s"
```

#### Provisioning Manager Changes

File: `internal/provision/manager.go`

Key changes:
1. Skip `shutdownSystem()` + `ejectAllMedia()` + `turnOnSystem()` in agent mode
2. Keep auth token alive (heartbeat extends TTL instead of fixed 30min JWT)
3. `handleEvents()` runs indefinitely in agent mode instead of terminating on "success"
4. Transition to "listening" state after successful provisioning
5. New step: `waitForAgentReady()` — Wait for first heartbeat after "success" status

Current code (terminates on success):
```go
case "success":
    cancelEventHandling()  // EXITS HERE
```

Agent mode code:
```go
case "success":
    if agentMode {
        // Transition to monitoring state, don't exit
        phase = PhaseRunning
        continue  // Keep listening for heartbeats
    }
    cancelEventHandling()
```

#### HTTP Server Changes

File: `internal/server/server.go`

New endpoints:
| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/heartbeat` | POST | Agent health check, returns pending commands + heartbeat schedule |
| `/agent/{nn}/commands` | GET | Agent polls for queued commands |
| `/agent/{nn}/command/{id}/result` | POST | Command completion reporting |

New file: `internal/server/commands.go`

```go
type AgentCommand struct {
    ID        string    `json:"id"`
    Type      string    `json:"type"`       // "update-config", "reboot", "shutdown", "drain"
    Payload   []byte    `json:"payload"`
    CreatedAt time.Time `json:"createdAt"`
    Deadline  time.Time `json:"deadline"`
}

type CommandManager struct {
    mu     sync.RWMutex
    queues map[types.NamespacedName][]AgentCommand
    beats  map[types.NamespacedName]time.Time
}

func (cm *CommandManager) Queue(nn types.NamespacedName, cmd AgentCommand) error
func (cm *CommandManager) Claim(nn types.NamespacedName) []AgentCommand
func (cm *CommandManager) RecordHeartbeat(nn types.NamespacedName)
func (cm *CommandManager) IsHealthy(nn types.NamespacedName, timeout time.Duration) bool
```

#### Controller Reconciliation Changes

File: `internal/controllers/redfishmachine_controller.go`

New reconciliation path:
```go
func (r *RedfishMachineReconciler) reconcileAgentMode(ctx context.Context, machine *RedfishMachine) (ctrl.Result, error) {
    // Check heartbeat freshness
    if !r.server.Commands().IsHealthy(machineNN, heartbeatTimeout) {
        machine.Status.Phase = PhaseHealthCheckFailed
        machine.Status.Ready = false
        return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
    }
    // Check for spec changes → queue commands
    if specChanged(machine) {
        r.server.Commands().Queue(machineNN, buildUpdateCommand(machine))
        machine.Status.Phase = PhaseUpdating
        return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
    }
    // Healthy state
    machine.Status.Phase = PhaseConnected
    machine.Status.Ready = true
    return ctrl.Result{RequeueAfter: heartbeatInterval}, nil
}
```

#### Deprovisioning with Drain

```go
func (r *Reconciler) reconcileDeleteAgentMode(ctx context.Context, machine *RedfishMachine) (ctrl.Result, error) {
    if machine.Status.Phase != PhaseDraining {
        // Initiate drain
        r.server.Commands().Queue(machineNN, AgentCommand{Type: "prepare-shutdown"})
        machine.Status.Phase = PhaseDraining
        return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
    }
    // Check if agent acknowledged drain
    if agentAckedDrain(machine) || drainTimeoutExceeded(machine, 60*time.Second) {
        // Proceed with normal deprovision
        return r.reconcileDelete(ctx, machine)
    }
    return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
```

#### Token Security

Heartbeat-extended token TTL:
- Initial token: 30min JWT (unchanged)
- On each heartbeat: issue new token with fresh 30min TTL
- Agent uses new token for subsequent requests
- If agent doesn't heartbeat within 30min, token expires → agent must re-auth or be recovered

#### Backward Compatibility

ALL changes opt-in via `spec.agentMode: true`. Machines without this flag use current
boot-provision-shutdown behavior. Default is `false`.

#### Open Design Questions

1. **Push vs Pull for commands?**
   Current design: agent polls via heartbeat response (simpler, no websockets).
   Alternative: WebSocket `/agent-stream` for low-latency bidirectional (more complex).
   **Recommendation**: Start with polling, add WebSocket later if latency matters.

2. **Full config vs diffs on heartbeat?**
   **Recommendation**: Return config version counter in heartbeat response.
   Agent compares to its version, fetches full config only if version differs.

3. **Heartbeat timeout recovery?**
   **Recommendation**: Graceful drain (queue "prepare-shutdown", wait 60s) before hard reset.
   Add `spec.agentRecoveryAction` field: `drain-then-reset` (default) or `manual`.

4. **Per-command auth?**
   **Recommendation**: No. Existing Bearer token suffices. Add rate limiting on command API.

---

## Addendum B — GoBGP as FRR Replacement

### Vision

Replace FRR daemons (bgpd, zebra, bfdd) with embedded GoBGP library + vishvananda/netlink
for a single-binary EVPN networking stack. No external processes, no configuration files,
no inter-process communication — everything in one Go binary.

### GoBGP Capabilities (v4.3.0)

Module: `github.com/osrg/gobgp/v4`
License: Apache 2.0 (compatible with BOOTy)

| Feature | Support |
|---------|---------|
| EVPN Type 1 (Auto-Discovery) | Yes |
| EVPN Type 2 (MAC/IP Advertisement) | Yes |
| EVPN Type 3 (Inclusive Multicast) | Yes |
| EVPN Type 4 (Ethernet Segment) | Yes |
| EVPN Type 5 (IP Prefix) | Yes |
| VXLAN encapsulation | Yes |
| Route Targets (import/export) | Yes |
| Unnumbered BGP (DC peering) | Yes |
| eBGP / iBGP | Yes |
| Route Reflector | Yes |
| Graceful Restart | Yes |
| Peer Groups | Yes |
| Route Policies | Yes |
| BFD | No (not native) |
| Kernel route programming | No |
| Interface management | No |

GoBGP's go.mod already depends on `vishvananda/netlink` v1.3.1 (confirmed).

### Go Native Library API

```go
import (
    "github.com/osrg/gobgp/v4/pkg/server"
    "github.com/osrg/gobgp/v4/api"
    "github.com/osrg/gobgp/v4/pkg/apiutil"
    "github.com/osrg/gobgp/v4/pkg/packet/bgp"
)

s := server.NewBgpServer(server.LoggerOption(log, lvl))
go s.Serve()

s.StartBgp(ctx, &api.StartBgpRequest{
    Global: &api.Global{
        Asn:        65001,
        RouterId:   "192.168.1.1",
        ListenPort: 179,
    },
})

// Add peer with EVPN AFI/SAFI
s.AddPeer(ctx, &api.AddPeerRequest{
    Peer: &api.Peer{
        Conf: &api.PeerConf{
            NeighborAddress: "192.168.1.2",
            PeerAsn:         65000,
        },
        AfiSafis: []*api.AfiSafi{{
            Config: &api.AfiSafiConfig{
                Family: &api.Family{Afi: api.Family_AFI_L2VPN, Safi: api.Family_SAFI_EVPN},
            },
        }},
    },
})

// Watch for route changes
s.WatchEvent(ctx, server.WatchEventMessageCallbacks{
    OnPathUpdate: func(paths []*apiutil.Path, timestamp time.Time) {
        for _, path := range paths {
            // Translate EVPN route → netlink operations
            programKernel(path)
        }
    },
})
```

### What Must Be Implemented (GoBGP lacks these)

| Component | Purpose | Go Library |
|-----------|---------|-----------|
| VRF creation | Create Linux VRF interface | `vishvananda/netlink` |
| VXLAN interface | Create VXLAN tunnel interface | `vishvananda/netlink` |
| Bridge interface | Create Linux bridge | `vishvananda/netlink` |
| Dummy interface | Create dummy for loopback IPs | `vishvananda/netlink` |
| FDB programming | MAC→VTEP mappings from EVPN Type 2 | `vishvananda/netlink` |
| Neighbor/ARP entries | IP→MAC from EVPN Type 2 | `vishvananda/netlink` |
| Route programming | IP prefix routes from EVPN Type 5 | `vishvananda/netlink` |
| BUM flooding | Multicast entries from EVPN Type 3 | `vishvananda/netlink` |
| Local MAC learning | Watch local ARP → advertise via GoBGP | `vishvananda/netlink` |
| BFD | Fast failure detection | **Not available** — use aggressive BGP hold timers |

### Implementation Architecture

```
pkg/network/gobgp/
├── bgp.go        — Embedded GoBGP server, peer config, route advertisement
├── dataplane.go  — Netlink programming (VXLAN, VRF, bridge, FDB, routes)
├── watcher.go    — Watch GoBGP RIB changes → translate to netlink ops
└── learning.go   — Watch local ARP/ND → advertise as EVPN Type 2
```

> **Note**: GoBGP does **not** support BFD. A previously proposed `bfd.go`
> has been removed. Link failure detection relies on aggressive BGP
> hold timers (3s keepalive / 9s hold → ~9s failover), which is
> acceptable for provisioning workloads. See proposal-gobgp.md for details.

### Data Flow

```
GoBGP (control plane)
  │ WatchEvent → EVPN Type 2/3/5 routes learned from peers
  ▼
watcher.go (route translator)
  │ Parse EVPN NLRI → determine netlink operations needed
  ▼
dataplane.go (kernel programming)
  │ netlink.LinkAdd(vxlan), netlink.NeighAdd(FDB), netlink.RouteAdd
  ▼
Linux kernel (forwarding plane)
  │ Actual packet forwarding via VXLAN/bridge
  ▲
learning.go (local learning)
  │ Watch local ARP table changes → advertise via GoBGP AddPath
  ▲
Local network interfaces
```

### EVPN Route → Netlink Operation Mapping

| EVPN Route Type | Netlink Operation |
|----------------|-------------------|
| Type 2 (MAC/IP) remote | `netlink.NeighAdd()` — FDB: MAC→VTEP_IP on VXLAN iface |
| Type 2 (MAC/IP) remote with IP | `netlink.NeighAdd()` — ARP: IP→MAC on bridge iface |
| Type 3 (Multicast) | `netlink.NeighAdd()` — BUM: `00:00:00:00:00:00`→VTEP_IP on VXLAN |
| Type 5 (IP Prefix) | `netlink.RouteAdd()` — Route in VRF via VXLAN next-hop |
| Type 2 (MAC/IP) local | `bgpServer.AddPath()` — Advertise local MAC/IP |

### Advantages

- **Single binary**: No FRR daemons (bgpd ~8MB, zebra ~5MB, bfdd ~3MB — saves ~16MB)
- **Pure Go**: Testable, debuggable, no inter-process communication
- **Direct control**: Choose exactly when/how routes are programmed
- **Startup speed**: No daemon startup/config parsing delay
- **Dependency reduction**: Remove FRR packages from initramfs

### Disadvantages / Risks

- **No BFD**: GoBGP does not support BFD. Failover relies on aggressive
  BGP hold timers (~9s with 3s keepalive vs FRR's ~300ms BFD). Acceptable
  for provisioning workloads where sub-second failover is not critical.
- **EVPN interop**: GoBGP less battle-tested with some TOR switch vendors than FRR
- **ARP/ND learning**: Must implement local MAC learning (FRR's zebra does this automatically)
- **More code**: ~2,500–3,500 LOC vs "configure FRR and let it handle everything"
- **Debugging**: Harder to debug BGP issues without vtysh CLI

### Recommended Approach

Implement behind `NetworkMode` interface as `GoBGPMode`. Keep `FRRMode` as default.
Switch via kernel cmdline param:

```
booty.network=gobgp    → Use embedded GoBGP + netlink
booty.network=frr      → Use FRR daemons (default)
booty.network=dhcp     → Simple DHCP on eth0
booty.network=static   → Static IP configuration
```

The implementation does not use a kernel parameter. `NETWORK_MODE=gobgp` in
`/deploy/vars` selects GoBGP. Otherwise the network variables select FRR,
static or DHCP. See [Stack selection](networking.md#stack-selection).

**Estimated effort**: ~2,500–3,500 LOC (excluding tests). Majority in `dataplane.go`
(netlink programming) and `watcher.go` (EVPN route translation).

### GoBGP Unnumbered BGP for Data Center

GoBGP supports unnumbered BGP — critical for leaf-spine peering without IP address management:

```toml
# Configuration
[global.config]
  as = 65001
  router-id = "192.168.255.1"

[[neighbors]]
  [neighbors.config]
    neighbor-interface = "eth0"  # Auto-detect peer via IPv6 link-local
```

In Go library:
```go
s.AddPeer(ctx, &api.AddPeerRequest{
    Peer: &api.Peer{
        Conf: &api.PeerConf{
            NeighborInterface: "eth0",  // Unnumbered BGP
        },
    },
})
```

This eliminates IP address assignment for point-to-point DC links, matching the
FRR unnumbered BGP configuration used in the current ramdisk.

---

## Timeline & Dependencies

```
Week 1-2:  Phase 0 (Test Infrastructure) + Phase 1 (CAPRF Client)
Week 2-3:  Phase 2 (Network/FRR)
Week 3-5:  Phase 3 (Provisioning Pipeline)
Week 5-6:  Phase 4 (Kexec) + Phase 5 (ISO/OCI) [parallel]
Week 6-7:  Phase 6 (CAPRF Integration) + Phase 7 (Docs/Hardening)
```

When this plan was written, Agent Mode (Addendum A) and GoBGP (Addendum B) were
research only. Since then, BOOTy implements `MODE=standby` and the GoBGP stack
(`NETWORK_MODE=gobgp`, see [Networking](networking.md)). The CAPRF side of Addendum A is
not part of this repository.
