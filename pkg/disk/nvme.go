//go:build linux

package disk

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// nvmeControllerRE matches NVMe controller device names (nvme0, nvme1, etc.)
// but not namespace devices (nvme0n1, nvme0n1p1).
var nvmeControllerRE = regexp.MustCompile(`^nvme\d+$`)

// NVMeNamespaceConfig defines the desired namespace layout for an NVMe controller.
type NVMeNamespaceConfig struct {
	Controller string          `json:"controller"` // e.g. "/dev/nvme0"
	Namespaces []NVMeNamespace `json:"namespaces"`
}

// NVMeNamespace defines a single namespace to create.
type NVMeNamespace struct {
	Label     string `json:"label"`               // Human-readable label
	SizePct   int    `json:"sizePct"`             // Percentage of total capacity
	BlockSize int    `json:"blockSize,omitempty"` // 512 or 4096 (default: 512)
}

// nvmeControllerPathRE validates that a controller path looks like /dev/nvme0, /dev/nvme1, etc.
var nvmeControllerPathRE = regexp.MustCompile(`^/dev/nvme\d+$`)

// nsidRE validates a namespace ID (positive integer like "1", "2", etc.).
var nsidRE = regexp.MustCompile(`^[1-9]\d*$`)

// cntlidRE validates an NVMe controller ID. 0 is a valid ID.
var cntlidRE = regexp.MustCompile(`^\d+$`)

// nvmeDevicePathRE validates NVMe namespace device paths like /dev/nvme0n1.
var nvmeDevicePathRE = regexp.MustCompile(`^/dev/nvme\d+n\d+$`)

// checkNVMeControllerUniqueness returns an error if any controller path appears more than once.
func checkNVMeControllerUniqueness(configs []NVMeNamespaceConfig) error {
	seen := make(map[string]bool, len(configs))
	for _, cfg := range configs {
		if seen[cfg.Controller] {
			return fmt.Errorf("duplicate controller %q in nvme namespace config", cfg.Controller)
		}
		seen[cfg.Controller] = true
	}
	return nil
}

// ParseNVMeConfig parses a JSON NVMe namespace configuration string.
func ParseNVMeConfig(data string) ([]NVMeNamespaceConfig, error) {
	var configs []NVMeNamespaceConfig
	if err := json.Unmarshal([]byte(data), &configs); err != nil {
		return nil, fmt.Errorf("parsing nvme namespace config: %w", err)
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("nvme namespace config must contain at least one controller entry")
	}
	if err := checkNVMeControllerUniqueness(configs); err != nil {
		return nil, err
	}
	for i := range configs {
		if err := validateNVMeControllerConfig(i, &configs[i]); err != nil {
			return nil, err
		}
	}
	return configs, nil
}

func validateNVMeControllerConfig(index int, cfg *NVMeNamespaceConfig) error {
	if cfg.Controller == "" {
		return fmt.Errorf("config[%d]: controller must not be empty", index)
	}
	if !nvmeControllerPathRE.MatchString(cfg.Controller) {
		return fmt.Errorf("config[%d]: controller %q must be an NVMe controller path (e.g. /dev/nvme0)", index, cfg.Controller)
	}
	if len(cfg.Namespaces) == 0 {
		return fmt.Errorf("config[%d]: namespaces must not be empty", index)
	}

	totalPct := 0
	for j := range cfg.Namespaces {
		ns := &cfg.Namespaces[j]
		if err := validateNVMeNamespace(index, j, ns); err != nil {
			return err
		}
		totalPct += ns.SizePct
	}
	if totalPct > 100 {
		return fmt.Errorf("config[%d]: total sizePct %d exceeds 100%%", index, totalPct)
	}

	return nil
}

func validateNVMeNamespace(cfgIndex, nsIndex int, ns *NVMeNamespace) error {
	if ns.Label == "" {
		return fmt.Errorf("config[%d].namespaces[%d]: label must not be empty", cfgIndex, nsIndex)
	}
	if ns.SizePct <= 0 || ns.SizePct > 100 {
		return fmt.Errorf("config[%d].namespaces[%d]: sizePct %d out of range 1-100", cfgIndex, nsIndex, ns.SizePct)
	}
	if ns.BlockSize != 0 && ns.BlockSize != 512 && ns.BlockSize != 4096 {
		return fmt.Errorf("config[%d].namespaces[%d]: blockSize %d must be 512 or 4096", cfgIndex, nsIndex, ns.BlockSize)
	}
	if ns.BlockSize == 0 {
		ns.BlockSize = 512
	}
	return nil
}

// DetectNVMeControllers lists NVMe controllers in /dev/.
func DetectNVMeControllers() ([]string, error) {
	entries, err := os.ReadDir("/dev/")
	if err != nil {
		return nil, fmt.Errorf("reading /dev/ for NVMe detection: %w", err)
	}
	var controllers []string
	for _, e := range entries {
		name := e.Name()
		// Match /dev/nvme0, /dev/nvme1, etc. (not nvme0n1 which is a namespace)
		if nvmeControllerRE.MatchString(name) {
			controllers = append(controllers, "/dev/"+name)
		}
	}
	return controllers, nil
}

// NVMeIdentifyController returns basic controller info via nvme id-ctrl.
func (m *Manager) NVMeIdentifyController(ctx context.Context, controller string) (map[string]string, error) {
	if !nvmeControllerPathRE.MatchString(controller) {
		return nil, fmt.Errorf("invalid NVMe controller path %q", controller)
	}
	out, err := m.cmd.Run(ctx, "nvme", "id-ctrl", controller, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("nvme id-ctrl %s: %s: %w", controller, strings.TrimSpace(string(out)), err)
	}

	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parsing nvme id-ctrl JSON for %s: %w", controller, err)
	}

	info := make(map[string]string, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			info[k] = val
		case float64:
			if val == float64(int64(val)) {
				info[k] = fmt.Sprintf("%d", int64(val))
			} else {
				info[k] = fmt.Sprintf("%g", val)
			}
		default:
			info[k] = fmt.Sprintf("%v", v)
		}
	}
	return info, nil
}

// nvmeNSEntry represents a single namespace entry in nvme list-ns JSON output.
type nvmeNSEntry struct {
	NSID int `json:"nsid"`
}

type nvmeNSListOutput struct {
	NSIDList []nvmeNSEntry `json:"nsid_list"`
}

// NVMeListNamespaces lists existing namespace IDs on a controller.
func (m *Manager) NVMeListNamespaces(ctx context.Context, controller string) ([]string, error) {
	return m.listNamespaces(ctx, controller)
}

// listNamespaces runs nvme list-ns with the given extra arguments.
func (m *Manager) listNamespaces(ctx context.Context, controller string, extraArgs ...string) ([]string, error) {
	if !nvmeControllerPathRE.MatchString(controller) {
		return nil, fmt.Errorf("invalid NVMe controller path %q", controller)
	}
	args := append([]string{"list-ns", controller}, extraArgs...)
	out, err := m.cmd.Run(ctx, "nvme", append(args, "-o", "json")...)
	if err != nil {
		return nil, fmt.Errorf("nvme list-ns %s: %w", controller, err)
	}

	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}

	var entries []nvmeNSEntry

	// nvme-cli JSON output is wrapped as {"nsid_list":[{"nsid":1}, ...]}
	// in modern versions. Keep compatibility with legacy array output.
	var wrapped nvmeNSListOutput
	if err := json.Unmarshal(out, &wrapped); err == nil && wrapped.NSIDList != nil {
		entries = wrapped.NSIDList
	} else if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parsing nvme list-ns JSON for %s: %w", controller, err)
	}

	nsids := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.NSID <= 0 {
			continue
		}
		nsids = append(nsids, strconv.Itoa(e.NSID))
	}
	return nsids, nil
}

// NVMeSupportsMultiNS checks whether the controller supports multiple namespaces.
// Returns false for consumer drives (nn == 1).
func (m *Manager) NVMeSupportsMultiNS(ctx context.Context, controller string) (bool, error) {
	info, err := m.NVMeIdentifyController(ctx, controller)
	if err != nil {
		return false, err
	}
	nn, ok := info["nn"]
	if !ok {
		return false, nil
	}
	var count int
	if _, err := fmt.Sscanf(nn, "%d", &count); err != nil {
		return false, fmt.Errorf("parsing namespace count %q: %w", nn, err)
	}
	return count > 1, nil
}

// CreateNVMeNamespace creates a namespace with the given size in blocks and block size.
func (m *Manager) CreateNVMeNamespace(ctx context.Context, controller string, sizeBlocks uint64, blockSize int) (string, error) {
	if !nvmeControllerPathRE.MatchString(controller) {
		return "", fmt.Errorf("invalid NVMe controller path %q", controller)
	}
	bs := blockSize
	if bs == 0 {
		bs = 512
	}

	slog.Info("creating nvme namespace", "controller", controller, "blocks", sizeBlocks, "blockSize", bs)

	out, err := m.cmd.Run(ctx, "nvme", "create-ns", controller,
		"-s", fmt.Sprintf("%d", sizeBlocks),
		"-c", fmt.Sprintf("%d", sizeBlocks),
		"-b", fmt.Sprintf("%d", bs))
	if err != nil {
		return "", fmt.Errorf("nvme create-ns %s: %s: %w", controller, string(out), err)
	}

	// Parse NSID from output (format: "create-ns: Success, created nsid:2").
	for _, line := range strings.Split(string(out), "\n") {
		if idx := strings.Index(line, "nsid:"); idx >= 0 {
			return strings.TrimSpace(line[idx+5:]), nil
		}
	}
	return "", fmt.Errorf("could not parse NSID from create-ns output: %s", string(out))
}

// AttachNVMeNamespace attaches a namespace to the controller it was created
// on. nvme attach-ns needs the controller ID (cntlid), which is not always 0
// on enterprise and dual-port drives.
func (m *Manager) AttachNVMeNamespace(ctx context.Context, controller, nsid string) error {
	if !nvmeControllerPathRE.MatchString(controller) {
		return fmt.Errorf("invalid NVMe controller path %q", controller)
	}
	if !nsidRE.MatchString(nsid) {
		return fmt.Errorf("invalid NVMe namespace ID %q", nsid)
	}
	cntlid, err := m.nvmeControllerID(ctx, controller)
	if err != nil {
		return err
	}
	return m.attachNVMeNamespace(ctx, controller, nsid, cntlid)
}

// nvmeControllerID reads the controller ID (cntlid) from nvme id-ctrl.
func (m *Manager) nvmeControllerID(ctx context.Context, controller string) (string, error) {
	info, err := m.NVMeIdentifyController(ctx, controller)
	if err != nil {
		return "", fmt.Errorf("identifying controller %s: %w", controller, err)
	}
	return controllerIDFromInfo(controller, info)
}

func controllerIDFromInfo(controller string, info map[string]string) (string, error) {
	cntlid, ok := info["cntlid"]
	if !ok || !cntlidRE.MatchString(cntlid) {
		return "", fmt.Errorf("controller %s has no valid cntlid %q", controller, cntlid)
	}
	if id, err := strconv.ParseUint(cntlid, 10, 16); err != nil || id > 0xFFEF {
		return "", fmt.Errorf("controller %s has out-of-range cntlid %q", controller, cntlid)
	}
	return cntlid, nil
}

func (m *Manager) attachNVMeNamespace(ctx context.Context, controller, nsid, cntlid string) error {
	if !nvmeControllerPathRE.MatchString(controller) {
		return fmt.Errorf("invalid NVMe controller path %q", controller)
	}
	if !nsidRE.MatchString(nsid) {
		return fmt.Errorf("invalid NVMe namespace ID %q", nsid)
	}
	out, err := m.cmd.Run(ctx, "nvme", "attach-ns", controller, "-n", nsid, "-c", cntlid)
	if err != nil {
		return fmt.Errorf("nvme attach-ns %s -n %s: %s: %w", controller, nsid, string(out), err)
	}
	return nil
}

// FormatNVMeNamespace performs a secure erase and reformat of a namespace.
// lbafIndex must be provided explicitly from device capabilities (e.g. nvme id-ns),
// because LBAF index ordering is vendor/device-specific.
func (m *Manager) FormatNVMeNamespace(ctx context.Context, device string, blockSize, lbafIndex int) error {
	if !nvmeDevicePathRE.MatchString(device) {
		return fmt.Errorf("invalid NVMe device path %q", device)
	}
	if lbafIndex < 0 {
		return fmt.Errorf("formatting %s: explicit lbafIndex is required", device)
	}
	lbaf := fmt.Sprintf("%d", lbafIndex)

	slog.Info("formatting nvme namespace", "device", device, "blockSize", blockSize)

	out, err := m.cmd.Run(ctx, "nvme", "format", device, "-l", lbaf, "-s", "1")
	if err != nil {
		return fmt.Errorf("nvme format %s: %s: %w", device, string(out), err)
	}
	return nil
}

// ApplyNVMeNamespaceLayout creates namespaces from config, using percentage-based sizing.
// Returns created NSIDs per controller in creation order.
func (m *Manager) ApplyNVMeNamespaceLayout(ctx context.Context, cfgs []NVMeNamespaceConfig) (map[string][]string, error) {
	created := make(map[string][]string, len(cfgs))
	for _, cfg := range cfgs {
		nsids, err := m.applyControllerLayout(ctx, cfg)
		if err != nil {
			return nil, err
		}
		created[cfg.Controller] = nsids
	}
	return created, nil
}

func (m *Manager) applyControllerLayout(ctx context.Context, cfg NVMeNamespaceConfig) ([]string, error) {
	controller := cfg.Controller

	supported, err := m.NVMeSupportsMultiNS(ctx, controller)
	if err != nil {
		return nil, fmt.Errorf("checking NVMe multi-namespace support: %w", err)
	}
	if !supported {
		return nil, fmt.Errorf("controller %s does not support multiple namespaces; cannot apply requested layout", controller)
	}

	totalBytes, err := m.nvmeControllerCapacity(ctx, controller)
	if err != nil {
		return nil, err
	}
	// Resolve the controller ID before the destructive reset below.
	cntlid, err := m.nvmeControllerID(ctx, controller)
	if err != nil {
		return nil, err
	}

	// Delete existing namespaces first so layout creation is deterministic.
	if err := m.deleteAllNamespaces(ctx, controller); err != nil {
		return nil, fmt.Errorf("reset namespaces on %s: %w", controller, err)
	}

	created := make([]string, 0, len(cfg.Namespaces))
	for _, ns := range cfg.Namespaces {
		bs := ns.BlockSize
		if bs == 0 {
			bs = 512
		}
		sizeBlocks := (totalBytes * uint64(ns.SizePct)) / (100 * uint64(bs))
		if sizeBlocks == 0 {
			return nil, fmt.Errorf("controller %s namespace %q: computed size is 0 blocks (device too small for sizePct=%d%% with blockSize=%d)", controller, ns.Label, ns.SizePct, bs)
		}

		nsid, err := m.CreateNVMeNamespace(ctx, controller, sizeBlocks, bs)
		if err != nil {
			return nil, err
		}
		if err := m.attachNVMeNamespace(ctx, controller, nsid, cntlid); err != nil {
			return nil, err
		}
		created = append(created, nsid)
		slog.Info("created nvme namespace", "controller", controller, "nsid", nsid, "label", ns.Label, "sizePct", ns.SizePct)
	}
	return created, nil
}

func (m *Manager) nvmeControllerCapacity(ctx context.Context, controller string) (uint64, error) {
	info, err := m.NVMeIdentifyController(ctx, controller)
	if err != nil {
		return 0, fmt.Errorf("identifying controller %s: %w", controller, err)
	}
	tnvmcap, ok := info["tnvmcap"]
	if !ok {
		return 0, fmt.Errorf("controller %s missing tnvmcap field", controller)
	}
	var totalBytes uint64
	if _, err := fmt.Sscanf(tnvmcap, "%d", &totalBytes); err != nil {
		return 0, fmt.Errorf("parsing tnvmcap %q: %w", tnvmcap, err)
	}
	return totalBytes, nil
}

// NVMeResetNamespaces deletes all namespaces and recreates a single default namespace
// using the controller's full capacity at 512-byte block size. Used during deprovisioning
// to return the drive to a single-namespace factory state.
func (m *Manager) NVMeResetNamespaces(ctx context.Context, controller string) error {
	if !nvmeControllerPathRE.MatchString(controller) {
		return fmt.Errorf("invalid NVMe controller path %q", controller)
	}
	slog.Info("resetting nvme namespaces to factory default", "controller", controller)

	// Read capacity and controller ID before the destructive delete.
	totalBytes, err := m.nvmeControllerCapacity(ctx, controller)
	if err != nil {
		return fmt.Errorf("reading controller capacity for reset: %w", err)
	}
	cntlid, err := m.nvmeControllerID(ctx, controller)
	if err != nil {
		return err
	}
	sizeBlocks := totalBytes / 512

	if err := m.deleteAllNamespaces(ctx, controller); err != nil {
		return err
	}

	// Create and attach single namespace using full capacity.
	nsid, err := m.CreateNVMeNamespace(ctx, controller, sizeBlocks, 512)
	if err != nil {
		m.tryAttachFallbackNamespace(ctx, controller, sizeBlocks, cntlid)
		return fmt.Errorf("creating default namespace: %w", err)
	}
	if err := m.attachNVMeNamespace(ctx, controller, nsid, cntlid); err != nil {
		return fmt.Errorf("attaching default namespace: %w", err)
	}

	slog.Info("nvme namespaces reset to factory default", "controller", controller)
	return nil
}

func (m *Manager) tryAttachFallbackNamespace(ctx context.Context, controller string, sizeBlocks uint64, cntlid string) {
	// Avoid leaving the drive without namespaces: try to recreate a minimal
	// fallback namespace so the controller remains usable.
	fallbackBlocks := uint64(1 << 21) // 1 GiB @ 512-byte sectors
	if sizeBlocks > 0 && fallbackBlocks > sizeBlocks {
		fallbackBlocks = sizeBlocks
	}
	if fallbackBlocks == 0 {
		fallbackBlocks = 1
	}

	fbNSID, fbErr := m.CreateNVMeNamespace(ctx, controller, fallbackBlocks, 512)
	if fbErr != nil {
		slog.Warn("failed to create fallback namespace after reset failure",
			"controller", controller,
			"fallback_blocks", fallbackBlocks,
			"error", fbErr,
		)
		return
	}

	if attachErr := m.attachNVMeNamespace(ctx, controller, fbNSID, cntlid); attachErr != nil {
		slog.Warn("failed to attach fallback namespace after reset failure",
			"controller", controller,
			"fallback_nsid", fbNSID,
			"fallback_blocks", fallbackBlocks,
			"error", attachErr,
		)
		return
	}

	slog.Warn("full-capacity namespace creation failed; attached fallback namespace to keep controller accessible",
		"controller", controller,
		"fallback_nsid", fbNSID,
		"fallback_blocks", fallbackBlocks,
	)
}

func (m *Manager) deleteAllNamespaces(ctx context.Context, controller string) error {
	// --all also lists allocated namespaces that are not attached.
	nsids, err := m.listNamespaces(ctx, controller, "--all")
	if err != nil {
		return err
	}
	for _, nsid := range nsids {
		if out, err := m.cmd.Run(ctx, "nvme", "delete-ns", controller, "-n", nsid); err != nil {
			return fmt.Errorf("deleting namespace %s on %s: %s: %w", nsid, controller, strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}
