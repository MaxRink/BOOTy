//go:build linux

package provision

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// legacyMellanoxNumVFs is the NUM_OF_VFS value the CAPRF legacy provisioner
// sets on ConnectX NICs.
const legacyMellanoxNumVFs = 32

var (
	// pciDevicesRoot is the sysfs PCI device directory. Tests replace it.
	pciDevicesRoot = "/sys/bus/pci/devices"
	// lookPath finds host binaries. Tests replace it.
	lookPath = exec.LookPath
)

// legacyConnectXDeviceIDs are the PCI device IDs that match the lspci
// pattern of the legacy setup_mellanox_num_of_vfs:
// [ConnectX-4], [ConnectX-5], [ConnectX-6], [ConnectX-6 Dx],
// [ConnectX-6 Lx] and [ConnectX-7]. ConnectX-4 Lx and ConnectX-5 Ex do not
// match that pattern.
var legacyConnectXDeviceIDs = map[string]bool{
	"0x1013": true, // ConnectX-4
	"0x1017": true, // ConnectX-5
	"0x101b": true, // ConnectX-6
	"0x101d": true, // ConnectX-6 Dx
	"0x101f": true, // ConnectX-6 Lx
	"0x1021": true, // ConnectX-7
}

var mstconfigNumVFs = regexp.MustCompile(`(?m)^\s*NUM_OF_VFS\s+(\d+)`)

// setupMellanoxLegacy raises NUM_OF_VFS to at least numVFs on every
// ConnectX Ethernet function, like setup_mellanox_num_of_vfs in the CAPRF
// legacy provision.sh. It returns whether firmware values changed.
func (c *Configurator) setupMellanoxLegacy(ctx context.Context, numVFs int) (bool, error) {
	if numVFs <= 0 {
		numVFs = legacyMellanoxNumVFs
	}
	if _, err := lookPath("mstconfig"); err != nil {
		slog.Info("mellanox: mstconfig not found, skipping", "error", err)
		return false, nil //nolint:nilerr // the legacy script skips without mstconfig
	}
	nics, err := legacyConnectXFunctions()
	if err != nil {
		// The legacy script ignores lspci failures.
		slog.Warn("mellanox: cannot list PCI devices, skipping", "error", err)
		return false, nil //nolint:nilerr // the legacy script ignores lspci failures
	}
	if len(nics) == 0 {
		slog.Info("mellanox: no ConnectX Ethernet NICs found, skipping")
		return false, nil
	}
	changed := false
	for _, nic := range nics {
		out, err := c.disk.Run(ctx, "mstconfig", "-d", nic, "q")
		if err != nil {
			return changed, fmt.Errorf("mstconfig query %s: %s: %w", nic, truncateOutput(out), err)
		}
		m := mstconfigNumVFs.FindSubmatch(out)
		if m == nil {
			return changed, fmt.Errorf("mstconfig query %s: NUM_OF_VFS not found", nic)
		}
		current, err := strconv.Atoi(string(m[1]))
		if err != nil {
			return changed, fmt.Errorf("mstconfig query %s: %w", nic, err)
		}
		slog.Info("mellanox: current NUM_OF_VFS", "nic", nic, "current", current)
		if current >= numVFs {
			continue
		}
		if out, err := c.disk.Run(ctx, "mstconfig", "-y", "-d", nic, "s", fmt.Sprintf("NUM_OF_VFS=%d", numVFs)); err != nil {
			return changed, fmt.Errorf("mstconfig set %s: %s: %w", nic, truncateOutput(out), err)
		}
		slog.Info("mellanox: set NUM_OF_VFS", "nic", nic, "target", numVFs)
		changed = true
	}
	return changed, nil
}

// legacyConnectXFunctions returns the PCI addresses (domain:bus:dev.fn) of
// ConnectX Ethernet functions.
func legacyConnectXFunctions() ([]string, error) {
	entries, err := os.ReadDir(pciDevicesRoot)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", pciDevicesRoot, err)
	}
	var nics []string
	for _, entry := range entries {
		dir := filepath.Join(pciDevicesRoot, entry.Name())
		if readSysfsValue(dir, "vendor") != "0x15b3" || !legacyConnectXDeviceIDs[readSysfsValue(dir, "device")] {
			continue
		}
		// Ethernet controller class 0x0200xx. InfiniBand mode is 0x0207xx.
		if !strings.HasPrefix(readSysfsValue(dir, "class"), "0x0200") {
			continue
		}
		// Keep the full domain:bus:dev.fn; mstconfig accepts it, and a host
		// with several PCI domains needs it to name the right function.
		nics = append(nics, entry.Name())
	}
	return nics, nil
}

func readSysfsValue(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // sysfs attribute
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(data)))
}

func truncateOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > mstconfigOutputLimit {
		return s[:mstconfigOutputLimit] + "..."
	}
	return s
}
