//go:build linux

package network

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

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
	missing := missingLinks(ConfiguredInterfaceNames(c))
	if len(missing) == 0 {
		return nil
	}
	plan, err := waitForConfiguredNICs(ctx, c, missing)
	if err != nil {
		return err
	}
	var done [][2]string // from, to
	for _, r := range plan {
		if err := renameLink(r.From, r.To); err != nil {
			return undoRenames(done, err)
		}
		done = append(done, [2]string{r.From, r.To})
		slog.Info("renamed NIC to its predictable name", "from", r.From, "to", r.To)
	}
	return nil
}

func missingLinks(names []string) []string {
	var missing []string
	for _, name := range names {
		if !linkExists(name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// nicWaitInterval is the time between two looks for the configured NICs.
var nicWaitInterval = 500 * time.Millisecond

// producedLinks are the configured names that BOOTy creates or gives
// itself: the VXLAN link, the bond, and the netplan set-name targets.
func producedLinks(c *Config) []string {
	names := []string{c.VXLANLink}
	if c.IsBondMode() {
		names = append(names, "bond0")
	}
	for _, r := range c.LinkRenames {
		names = append(names, r.SetName)
	}
	return names
}

// waitForConfiguredNICs waits up to NICWait (NIC_WAIT_SECONDS) until every
// configured NIC exists or one NIC has it as its predictable name, and
// returns the renames. NICs can appear after BOOTy starts: a driver brings
// up its ports late, or containerlab moves the veths into the namespace.
// The names that BOOTy creates or gives itself (producedLinks) are not
// waited for. After the wait it goes on with the NICs it has, and the
// setup reports what is missing.
func waitForConfiguredNICs(ctx context.Context, c *Config, missing []string) ([]netname.Rename, error) {
	produced := producedLinks(c)
	nics := slices.DeleteFunc(slices.Clone(missing), func(n string) bool { return slices.Contains(produced, n) })
	deadline := time.Now().Add(c.NICWait)
	for logged := false; ; logged = true {
		links, err := physicalLinks(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(links))
		for _, l := range links {
			names = append(names, l.Name)
		}
		plan := netname.Plan(predictableSysfs, names, missing)
		absent := slices.DeleteFunc(missingLinks(nics), func(n string) bool {
			return slices.ContainsFunc(plan, func(r netname.Rename) bool { return r.To == n })
		})
		if len(absent) == 0 {
			return plan, nil
		}
		if !time.Now().Before(deadline) {
			if len(nics) > 0 {
				slog.Warn("configured NICs are missing after the wait", "nics", absent, "wait", c.NICWait)
			}
			return plan, nil
		}
		if !logged {
			slog.Info("waiting for the configured NICs", "nics", absent, "max", c.NICWait)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for the configured NICs: %w", ctx.Err())
		case <-time.After(nicWaitInterval):
		}
	}
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
