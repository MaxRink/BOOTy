//go:build linux

package network

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/digineo/go-dhclient"
	"github.com/vishvananda/netlink"
)

// DHCPMode implements the Mode interface using DHCP on all physical interfaces.
type DHCPMode struct {
	client *dhclient.Client
	log    *slog.Logger
}

// NewDHCPMode creates a DHCPMode with a component logger.
func NewDHCPMode() *DHCPMode {
	return &DHCPMode{
		log: slog.Default().With("component", "dhcp"),
	}
}

// dhcpResult carries the outcome of a single NIC probe.
type dhcpResult struct {
	client *dhclient.Client
	iface  string
}

// Setup tries DHCP on all physical interfaces in parallel.
// Each NIC gets its own 15 s timeout; the first lease wins.
func (d *DHCPMode) Setup(ctx context.Context, _ *Config) error {
	if d.log == nil {
		d.log = slog.Default().With("component", "dhcp")
	}
	ifaces, err := physicalInterfaces()
	if err != nil {
		return fmt.Errorf("listing interfaces: %w", err)
	}
	if len(ifaces) == 0 {
		return fmt.Errorf("no physical interfaces found for DHCP")
	}

	d.log.Info("Probing DHCP on all interfaces in parallel", "count", len(ifaces))

	probeCtx, probeCancel := context.WithCancel(ctx)
	defer probeCancel()

	results := make(chan dhcpResult, len(ifaces))
	var wg sync.WaitGroup

	for i := range ifaces {
		wg.Add(1)
		go func(iface net.Interface) {
			defer wg.Done()
			d.probeNIC(probeCtx, iface, results)
		}(ifaces[i])
	}

	// Close results channel once all goroutines finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Take the first successful result; cancel remaining probes immediately.
	for res := range results {
		if d.client == nil {
			d.client = res.client
			d.log.Info("DHCP succeeded", "interface", res.iface)
			probeCancel()
		} else {
			res.client.Stop()
		}
	}

	if d.client == nil {
		return fmt.Errorf("DHCP failed on all %d interfaces", len(ifaces))
	}
	return nil
}

// probeNIC attempts DHCP on a single interface with a 15 s timeout.
// On success it sends a dhcpResult to the results channel.
func (d *DHCPMode) probeNIC(ctx context.Context, iface net.Interface, results chan<- dhcpResult) {
	d.log.Info("Attempting DHCP", "interface", iface.Name)

	link, err := netlink.LinkByName(iface.Name)
	if err != nil {
		d.log.Warn("Cannot find link for DHCP", "interface", iface.Name, "error", err)
		return
	}
	if err := netlink.LinkSetUp(link); err != nil {
		d.log.Warn("Cannot bring up link for DHCP", "interface", iface.Name, "error", err)
		return
	}

	leased := make(chan struct{}, 1)
	client := &dhclient.Client{
		Iface:   &iface,
		OnBound: d.onBound(link, iface.Name, leased),
	}

	hostname, _ := os.Hostname()
	if hostname != "" {
		client.AddOption(0x0c, []byte(hostname)) // DHCPOptHostname
	}
	client.Start()

	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()

	select {
	case <-leased:
		results <- dhcpResult{client: client, iface: iface.Name}
	case <-timer.C:
		client.Stop()
		d.log.Info("DHCP timeout on interface", "interface", iface.Name)
	case <-ctx.Done():
		client.Stop()
		d.log.Info("DHCP probe canceled", "interface", iface.Name, "error", ctx.Err())
	}
}

// onBound returns a callback that configures the interface address and default route.
func (d *DHCPMode) onBound(link netlink.Link, ifName string, leased chan<- struct{}) func(*dhclient.Lease) {
	return func(lease *dhclient.Lease) {
		cidr := net.IPNet{IP: lease.FixedAddress, Mask: lease.Netmask}
		addr, _ := netlink.ParseAddr(cidr.String())
		if err := netlink.AddrAdd(link, addr); err != nil {
			d.log.Warn("Failed to assign DHCP address", "iface", ifName, "error", err)
			return
		}
		d.log.Info("DHCP lease obtained", "iface", ifName, "addr", cidr.String())
		// Default gateway from DHCP option 3 (routers).
		if len(lease.Router) > 0 {
			if err := netlink.RouteAdd(&netlink.Route{Gw: lease.Router[0]}); err != nil {
				d.log.Warn("failed to add default route", "gw", lease.Router[0], "error", err)
			}
		}
		select {
		case leased <- struct{}{}:
		default:
		}
	}
}

// WaitForConnectivity polls the target URL until reachable or timeout.
func (d *DHCPMode) WaitForConnectivity(ctx context.Context, target string, timeout time.Duration) error {
	return WaitForHTTP(ctx, target, timeout)
}

// Teardown stops the DHCP client.
func (d *DHCPMode) Teardown(_ context.Context) error {
	if d.client != nil {
		d.client.Stop()
	}
	return nil
}

// physicalInterfaces returns non-loopback, non-virtual interfaces.
func physicalInterfaces() ([]net.Interface, error) {
	nicNames, err := DetectPhysicalNICs()
	if err != nil {
		return nil, fmt.Errorf("detect physical nics: %w", err)
	}

	result := make([]net.Interface, 0, len(nicNames))
	for _, name := range nicNames {
		iface, ifErr := net.InterfaceByName(name)
		if ifErr != nil {
			slog.Debug("skipping NIC missing during DHCP scan", "interface", name, "error", ifErr)
			continue
		}
		result = append(result, *iface)
	}
	return result, nil
}

// WaitForHTTP polls target with HTTP HEAD until reachable.
func WaitForHTTP(ctx context.Context, target string, timeout time.Duration) error {
	if target == "" {
		return fmt.Errorf("empty connectivity target URL")
	}

	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 10 * time.Second}
	attempt := 0
	retryTicker := time.NewTicker(1 * time.Second)
	defer retryTicker.Stop()

	for time.Now().Before(deadline) {
		attempt++
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, http.NoBody)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		resp, err := client.Do(req) //nolint:gosec // target is from trusted config
		if err == nil {
			_ = resp.Body.Close()
			// Any HTTP response proves the network path works.  The server
			// may return 401 (auth required) or other non-2xx codes, but
			// that still means connectivity is established.
			slog.Info("network connectivity established", "target", target, "status", resp.StatusCode, "attempt", attempt)
			return nil
		}

		slog.Debug("connectivity check failed", "target", target, "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return fmt.Errorf("context canceled: %w", ctx.Err())
		case <-retryTicker.C:
		}
	}

	return fmt.Errorf("network connectivity timeout after %s (%d attempts)", timeout, attempt)
}
