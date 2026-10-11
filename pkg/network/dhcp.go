//go:build linux

package network

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digineo/go-dhclient"
	"github.com/google/gopacket/layers"
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
	ifaces, err := physicalInterfaces(ctx)
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
	var winner atomic.Int32

	for i := range ifaces {
		wg.Add(1)
		go func(iface net.Interface) {
			defer wg.Done()
			d.probeNIC(probeCtx, iface, results, &winner)
		}(ifaces[i])
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		d.client = res.client
		d.log.Info("DHCP succeeded", "interface", res.iface)
		probeCancel()
		break
	}

	if d.client == nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("DHCP probing canceled: %w", err)
		}
		return fmt.Errorf("DHCP failed on all %d interfaces", len(ifaces))
	}
	return nil
}

// probeNIC attempts DHCP on a single interface with a 15 s timeout.
// On success it sends a dhcpResult to the results channel.
func (d *DHCPMode) probeNIC(ctx context.Context, iface net.Interface, results chan<- dhcpResult, winner *atomic.Int32) {
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
	hostname, _ := os.Hostname()
	client := newDHCPClient(&iface, hostname, d.onBound(link, iface.Name, leased, winner))
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

// newDHCPClient builds a client that requests the default parameters
// (subnet mask, router, DNS, MTU and more) and sends the hostname.
// dhclient only adds the parameter request list when no option is set, so
// adding the hostname alone would drop it.
func newDHCPClient(iface *net.Interface, hostname string, onBound dhclient.Callback) *dhclient.Client {
	client := &dhclient.Client{Iface: iface, OnBound: onBound}
	for _, param := range dhclient.DefaultParamsRequestList {
		client.AddParamRequest(param)
	}
	if hostname != "" {
		client.AddOption(layers.DHCPOptHostname, []byte(hostname))
	}
	return client
}

// onBound returns a callback that configures the interface address and default route.
// The winner flag ensures only the first NIC to receive a lease applies netlink changes;
// subsequent leases are ignored by this callback and do not trigger additional configuration.
func (d *DHCPMode) onBound(link netlink.Link, ifName string, leased chan<- struct{}, winner *atomic.Int32) func(*dhclient.Lease) {
	return func(lease *dhclient.Lease) {
		if !winner.CompareAndSwap(0, 1) {
			return
		}
		cidr := net.IPNet{IP: lease.FixedAddress, Mask: lease.Netmask}
		addr, _ := netlink.ParseAddr(cidr.String())
		if err := netlink.AddrAdd(link, addr); err != nil {
			d.log.Warn("Failed to assign DHCP address", "iface", ifName, "error", err)
			winner.Store(0)
			return
		}
		d.log.Info("DHCP lease obtained", "iface", ifName, "addr", cidr.String())
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
func physicalInterfaces(ctx context.Context) ([]net.Interface, error) {
	nicNames, err := DetectPhysicalNICsContext(ctx)
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
