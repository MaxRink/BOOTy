//go:build linux

package network

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strings"

	"github.com/telekom/BOOTy/pkg/network/netname"
)

// predictableSysfs is the sysfs mount the predictable names are read from.
var predictableSysfs = "/sys"

// linkExists reports whether a link with this name exists; tests replace it.
var linkExists = func(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// ApplyPredictableNames renames NICs to the predictable names (systemd
// net_id) that the network config uses and that do not exist yet, so a
// netplan or frr.conf written for the installed OS (enp1s0) finds the
// ramdisk NIC (eth0). It runs before the netplan set-name renames. When no
// configured name is missing, it does nothing (no NIC detection). When a
// rename fails, the renames done before it are undone.
func ApplyPredictableNames(ctx context.Context, c *Config) error {
	var missing []string
	for _, name := range ConfiguredInterfaceNames(c) {
		if !linkExists(name) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	links, err := physicalLinks(ctx)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(links))
	for _, l := range links {
		names = append(names, l.Name)
	}
	var done [][2]string // from, to
	for _, r := range netname.Plan(predictableSysfs, names, missing) {
		if err := renameLink(r.From, r.To); err != nil {
			return undoRenames(done, err)
		}
		done = append(done, [2]string{r.From, r.To})
		slog.Info("renamed NIC to its predictable name", "from", r.From, "to", r.To)
	}
	return nil
}

// ConfiguredInterfaceNames returns the interface names of the network
// config: netplan ethernet IDs, frr.conf "neighbor X interface" names,
// literal netplan match names, and the vars (static interface, bond
// members, BGP interfaces, VLAN parents, VXLAN link). Globs are left out.
func ConfiguredInterfaceNames(c *Config) []string {
	names := append([]string{c.StaticIface, c.VXLANLink}, c.Interfaces...)
	names = append(names, c.InterfaceNames...)
	names = append(names, strings.Split(c.BondInterfaces, ",")...)
	names = append(names, strings.Split(c.BGPInterfaces, ",")...)
	for _, v := range c.VLANs {
		names = append(names, v.Parent)
	}
	for _, m := range c.LinkRenames {
		names = append(names, m.Name)
	}
	if c.StaticMatch != nil {
		names = append(names, c.StaticMatch.Name)
	}
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || n == "auto-detect" || strings.ContainsAny(n, "*?[") || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}
	return out
}
