//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"

	"github.com/vishvananda/netlink"
)

// physicalLinks returns the NICs that DetectPhysicalNICs finds, with
// their MACs. It uses the same NIC rules and the same wait for settling
// ContainerLab names, so a renamed ContainerLab veth is a NIC here too.
func physicalLinks(ctx context.Context) ([]LinkCandidate, error) {
	names, err := DetectPhysicalNICsContext(ctx)
	if err != nil {
		return nil, err
	}
	links, err := linkList()
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	var out []LinkCandidate
	for _, l := range links {
		if attrs := l.Attrs(); slices.Contains(names, attrs.Name) {
			out = append(out, LinkCandidate{Name: attrs.Name, MAC: attrs.HardwareAddr.String()})
		}
	}
	return out, nil
}

// ApplyLinkRenames renames NICs like netplan set-name. Like netplan, a
// rename needs exactly one matching NIC; other cases are logged and skipped.
// When a rename fails, the renames done before it are undone in reverse
// order, so a failed setup leaves the NIC names as they were.
func ApplyLinkRenames(ctx context.Context, renames []LinkMatch) error {
	var done [][2]string // from, to
	for _, r := range renames {
		// BOOTy finds NICs by name; a NIC renamed to an excluded name
		// (lo*, br*, bond*, vx*, ...) would no longer be a NIC for it.
		if isExcluded(r.SetName) {
			return undoRenames(done, fmt.Errorf("netplan set-name %q: BOOTy does not treat names with this prefix as NICs (%v)", r.SetName, excludedPrefixes))
		}
		links, err := physicalLinks(ctx)
		if err != nil {
			return undoRenames(done, err)
		}
		names := r.Matches(links)
		if len(names) != 1 {
			slog.Warn("netplan set-name needs exactly one matching NIC, skipping", "match_name", r.Name, "match_mac", r.MAC, "set_name", r.SetName, "matches", names)
			continue
		}
		if names[0] == r.SetName {
			continue
		}
		if err := renameLink(names[0], r.SetName); err != nil {
			return undoRenames(done, err)
		}
		done = append(done, [2]string{names[0], r.SetName})
		slog.Info("renamed NIC like netplan set-name", "from", names[0], "to", r.SetName)
	}
	return nil
}

// undoRenames reverts the done renames in reverse order and joins the
// errors of the rollback to err.
func undoRenames(done [][2]string, err error) error {
	errs := []error{err}
	for i := len(done) - 1; i >= 0; i-- {
		from, to := done[i][0], done[i][1]
		if rerr := renameLink(to, from); rerr != nil {
			errs = append(errs, fmt.Errorf("undo rename %s to %s: %w", from, to, rerr))
		} else {
			slog.Info("undid NIC rename after a failed rename", "from", to, "to", from)
		}
	}
	return errors.Join(errs...)
}

// Netlink calls of renameLink; tests replace them.
var (
	renameLinkByName  = netlink.LinkByName
	renameLinkSetDown = netlink.LinkSetDown
	renameLinkSetName = netlink.LinkSetName
	renameLinkSetUp   = netlink.LinkSetUp
)

func renameLink(from, to string) error {
	link, err := renameLinkByName(from)
	if err != nil {
		return fmt.Errorf("find %s: %w", from, err)
	}
	// The kernel renames only links that are down. Keep the admin state, so
	// a failed rename does not leave the provisioning NIC down.
	wasUp := link.Attrs().Flags&net.FlagUp != 0
	if err := renameLinkSetDown(link); err != nil {
		return fmt.Errorf("set %s down: %w", from, err)
	}
	renameErr := renameLinkSetName(link, to)
	if renameErr != nil {
		renameErr = fmt.Errorf("rename %s to %s: %w", from, to, renameErr)
	}
	if !wasUp {
		return renameErr
	}
	if err := renameLinkSetUp(link); err != nil {
		errs := []error{renameErr, fmt.Errorf("set %s up again: %w", to, err)}
		if renameErr == nil {
			// The caller rolls back only renames that returned nil, so
			// restore the old name and admin state here.
			if err := renameLinkSetName(link, from); err != nil {
				errs = append(errs, fmt.Errorf("restore name %s: %w", from, err))
			} else if err := renameLinkSetUp(link); err != nil {
				errs = append(errs, fmt.Errorf("set %s up after restoring the name: %w", from, err))
			}
		}
		return errors.Join(errs...)
	}
	return nil
}

// ResolveLinkMatch returns the single physical NIC that m selects.
func ResolveLinkMatch(ctx context.Context, m LinkMatch) (string, error) {
	links, err := physicalLinks(ctx)
	if err != nil {
		return "", err
	}
	names := m.Matches(links)
	if len(names) != 1 {
		return "", fmt.Errorf("match name %q mac %q selects %d NICs %v, want 1", m.Name, m.MAC, len(names), names)
	}
	return names[0], nil
}
