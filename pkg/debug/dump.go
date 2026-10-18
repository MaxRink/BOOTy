//go:build linux

// Package debug provides structured debug dump collection.
package debug

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	exec "github.com/telekom/BOOTy/pkg/executil"
)

// Dump holds a structured debug snapshot collected on failure.
type Dump struct {
	System  SystemSnapshot  `json:"system"`
	Disk    DiskSnapshot    `json:"disk"`
	Network NetworkSnapshot `json:"network"`
	Kernel  KernelSnapshot  `json:"kernel"`
}

// SystemSnapshot captures basic system information.
type SystemSnapshot struct {
	Hostname string `json:"hostname"`
	Uptime   string `json:"uptime"`
	MemInfo  string `json:"meminfo"`
	LoadAvg  string `json:"loadavg"`
	Vendor   string `json:"vendor"`
	Product  string `json:"product"`
}

// DiskSnapshot captures disk layout and state.
type DiskSnapshot struct {
	Lsblk  string `json:"lsblk"`
	Mounts string `json:"mounts"`
	Fdisk  string `json:"fdisk"`
	Df     string `json:"df"`
}

// NetworkSnapshot captures network configuration.
type NetworkSnapshot struct {
	IPAddr     string `json:"ipAddr"`
	IPRoute    string `json:"ipRoute"`
	IPNeigh    string `json:"ipNeigh"`
	ResolvConf string `json:"resolvConf"`
}

// KernelSnapshot captures kernel and boot state.
type KernelSnapshot struct {
	Dmesg      string `json:"dmesg"`
	Lsmod      string `json:"lsmod"`
	Cmdline    string `json:"cmdline"`
	EFIBootmgr string `json:"efibootmgr"`
}

// Collect gathers a debug dump from the running system.
func Collect(ctx context.Context) *Dump {
	d := &Dump{}
	d.System = collectSystem()
	d.Disk = collectDisk(ctx)
	d.Network = collectNetwork(ctx)
	d.Kernel = collectKernel(ctx)
	return d
}

// Marshal returns the dump as JSON.
func (d *Dump) Marshal() ([]byte, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("marshaling debug dump: %w", err)
	}
	return data, nil
}

func collectSystem() SystemSnapshot {
	hostname, _ := os.Hostname()
	return SystemSnapshot{
		Hostname: hostname,
		Uptime:   readFile("/proc/uptime"),
		MemInfo:  readFile("/proc/meminfo"),
		LoadAvg:  readFile("/proc/loadavg"),
		Vendor:   readFile("/sys/class/dmi/id/sys_vendor"),
		Product:  readFile("/sys/class/dmi/id/product_name"),
	}
}

func collectDisk(ctx context.Context) DiskSnapshot {
	return DiskSnapshot{
		Lsblk:  runCmd(ctx, "lsblk", "--json"),
		Mounts: readFile("/proc/mounts"),
		Fdisk:  runCmd(ctx, "fdisk", "-l"),
		Df:     runCmd(ctx, "df", "-h"),
	}
}

func collectNetwork(ctx context.Context) NetworkSnapshot {
	return NetworkSnapshot{
		IPAddr:     runCmd(ctx, "ip", "addr"),
		IPRoute:    runCmd(ctx, "ip", "route"),
		IPNeigh:    runCmd(ctx, "ip", "neigh"),
		ResolvConf: readFile("/etc/resolv.conf"),
	}
}

func collectKernel(ctx context.Context) KernelSnapshot {
	return KernelSnapshot{
		Dmesg:      runCmd(ctx, "dmesg"),
		Lsmod:      runCmd(ctx, "lsmod"),
		Cmdline:    readFile("/proc/cmdline"),
		EFIBootmgr: runCmd(ctx, "efibootmgr", "-v"),
	}
}

func readFile(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // trusted system paths
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func runCmd(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // trusted internal commands
	if err != nil {
		slog.Debug("debug dump command failed", "cmd", name, "error", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}
