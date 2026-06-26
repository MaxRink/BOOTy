//go:build linux

package frr

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"

	"github.com/telekom/BOOTy/pkg/executil"
	"github.com/telekom/BOOTy/pkg/network"
)

// Manager handles FRR/EVPN network setup and teardown.
type Manager struct {
	cfg              network.Config
	commander        Commander
	log              *slog.Logger
	frrStartMethod   string
	directDaemonList []string
}

// Commander abstracts command execution for testing.
type Commander = executil.Commander

// ExecCommander executes real system commands.
type ExecCommander = executil.ExecCommander

// NewManager creates an FRR manager.
func NewManager(commander Commander) *Manager {
	if commander == nil {
		commander = &ExecCommander{}
	}
	return &Manager{
		commander: commander,
		log:       slog.Default().With("component", "frr"),
	}
}

// Setup configures the full FRR/EVPN network stack.
func (m *Manager) Setup(ctx context.Context, cfg *network.Config) error {
	cfg.ApplyDefaults()
	m.cfg = *cfg

	underlayIP, overlayIP, bridgeMAC, err := DeriveAddresses(cfg)
	if err != nil {
		return fmt.Errorf("derive addresses: %w", err)
	}

	m.log.Info("FRR setup",
		"underlay_ip", underlayIP,
		"overlay_ip", overlayIP,
		"bridge_mac", bridgeMAC,
		"asn", cfg.ASN,
		"vni", cfg.ProvisionVNI,
	)

	nics, err := m.setupInterfaces(cfg, underlayIP, overlayIP, bridgeMAC)
	if err != nil {
		return m.rollbackSetup(ctx, err)
	}

	if err := m.startFRRStack(ctx, cfg, underlayIP, overlayIP, nics); err != nil {
		return m.rollbackSetup(ctx, err)
	}

	m.log.Info("FRR/EVPN network setup complete", "nics", nics)
	return nil
}

func (m *Manager) rollbackSetup(ctx context.Context, setupErr error) error {
	if m.frrStartMethod != "" || len(m.directDaemonList) > 0 {
		m.DumpFRRState()
		if err := m.Teardown(ctx); err != nil {
			return errors.Join(setupErr, fmt.Errorf("rollback FRR setup: %w", err))
		}
		return setupErr
	}
	if err := m.cleanupNetworkState(); err != nil {
		return errors.Join(setupErr, fmt.Errorf("rollback FRR setup: %w", err))
	}
	return setupErr
}

// setupInterfaces creates VRF, dummy, VXLAN, bridge, loopback, and configures NICs.
func (m *Manager) setupInterfaces(cfg *network.Config, underlayIP, overlayIP, bridgeMAC string) ([]string, error) {
	if cfg.VRFName != "" {
		if err := m.createVRF(cfg.VRFName, cfg.VRFTableID); err != nil {
			return nil, fmt.Errorf("create VRF: %w", err)
		}
	}

	if err := m.createDummy("dummy.underlay", cfg.VRFName, underlayIP+"/32"); err != nil {
		m.log.Warn("Cannot create dummy interface, using loopback for underlay IP", "error", err)
		if loErr := m.addLoopbackAddress(underlayIP); loErr != nil {
			return nil, fmt.Errorf("add underlay IP to loopback: %w", loErr)
		}
	}

	if err := m.createVXLAN(cfg.ProvisionVNI, underlayIP, cfg.BridgeName, bridgeMAC, cfg.MTU); err != nil {
		return nil, fmt.Errorf("create VXLAN: %w", err)
	}

	if cfg.ProvisionIP != "" {
		if err := m.addBridgeAddress(cfg.BridgeName, cfg.ProvisionIP); err != nil {
			return nil, fmt.Errorf("add bridge address: %w", err)
		}
	}

	if err := m.addLoopbackAddress(overlayIP); err != nil {
		return nil, fmt.Errorf("add loopback address: %w", err)
	}

	nics, err := network.DetectPhysicalNICs()
	if err != nil {
		return nil, fmt.Errorf("detect NICs: %w", err)
	}

	if err := m.configureNICs(nics, cfg.VRFName, cfg.MTU); err != nil {
		return nil, fmt.Errorf("configure NICs: %w", err)
	}

	if err := m.enableForwarding(); err != nil {
		m.log.Warn("Failed to enable IP forwarding", "error", err)
	}

	return nics, nil
}

// startFRRStack renders config, writes it, starts FRR daemons, and adds BGP peers.
func (m *Manager) startFRRStack(ctx context.Context, cfg *network.Config, underlayIP, overlayIP string, nics []string) error {
	frrConf, err := RenderConfig(cfg, underlayIP, overlayIP, nics)
	if err != nil {
		return fmt.Errorf("render FRR config: %w", err)
	}

	if err := m.writeFRRConfig(frrConf); err != nil {
		return fmt.Errorf("write FRR config: %w", err)
	}

	if err := m.writeDaemonsConfig(); err != nil {
		return fmt.Errorf("write daemons config: %w", err)
	}

	ensureFRRDirs()

	if err := m.startFRR(ctx); err != nil {
		return fmt.Errorf("start FRR: %w", err)
	}

	// Peers are declared in the dynamically generated frr.conf (via RenderConfig
	// which receives the detected NIC names). No vtysh addBGPPeer calls needed —
	// FRR loads them from the config file at startup.

	return nil
}

// WaitForConnectivity polls the target URL until reachable, restarting FRR periodically.
func (m *Manager) WaitForConnectivity(ctx context.Context, target string, timeout time.Duration) error {
	return waitForHTTPWithFRR(ctx, target, timeout, m)
}

// Teardown removes the FRR network configuration.
func (m *Manager) Teardown(ctx context.Context) error {
	var firstErr error

	if err := m.stopFRR(ctx); err != nil {
		m.log.Warn("failed to stop FRR", "error", err)
		firstErr = err
	}

	if err := m.cleanupNetworkState(); err != nil {
		m.log.Warn("failed to fully clean FRR network resources", "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}

	m.log.Info("FRR teardown complete")
	return firstErr
}

func (m *Manager) cleanupNetworkState() error {
	var errs []error
	for _, err := range []error{
		m.cleanupLoopbackAddresses(),
		m.cleanupBridgeProvisionAddress(),
		m.cleanupVXLANLink(),
		m.cleanupNamedLinks(),
	} {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) cleanupLoopbackAddresses() error {
	underlayIP, overlayIP, _, err := DeriveAddresses(&m.cfg)
	if err != nil {
		m.log.Debug("skipping loopback cleanup; failed to derive addresses", "error", err)
		return nil
	}

	var errs []error
	for _, ip := range []string{underlayIP, overlayIP} {
		cidr := loopbackCIDR(ip)
		if remErr := removeAddrFromLink("lo", cidr); remErr != nil {
			errs = append(errs, remErr)
			m.log.Debug("failed to remove loopback address", "ip", ip, "cidr", cidr, "error", remErr)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) cleanupBridgeProvisionAddress() error {
	if m.cfg.BridgeName == "" || m.cfg.ProvisionIP == "" {
		return nil
	}

	if err := removeAddrFromLink(m.cfg.BridgeName, m.cfg.ProvisionIP); err != nil {
		m.log.Debug("failed to remove bridge provision address", "bridge", m.cfg.BridgeName, "addr", m.cfg.ProvisionIP, "error", err)
		return err
	}
	return nil
}

func (m *Manager) cleanupVXLANLink() error {
	if m.cfg.ProvisionVNI == 0 {
		return nil
	}

	vxName := fmt.Sprintf("vx%d", m.cfg.ProvisionVNI)
	if err := removeLinkByName(vxName); err != nil {
		m.log.Debug("failed to remove VXLAN", "name", vxName, "error", err)
		return err
	}
	return nil
}

func (m *Manager) cleanupNamedLinks() error {
	var errs []error
	for _, name := range []string{m.cfg.BridgeName, "dummy.underlay", m.cfg.VRFName} {
		if strings.TrimSpace(name) == "" {
			continue
		}
		if err := removeLinkByName(name); err != nil {
			errs = append(errs, err)
			m.log.Debug("failed to remove link", "name", name, "error", err)
		}
	}
	return errors.Join(errs...)
}

func loopbackCIDR(ip string) string {
	if strings.Contains(ip, ":") {
		return ip + "/128"
	}
	return ip + "/32"
}

func removeAddrFromLink(linkName, cidr string) error {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		if isMissingNetlinkObject(err) {
			return nil
		}
		return fmt.Errorf("find link %s: %w", linkName, err)
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("parse addr %s: %w", cidr, err)
	}
	if err := netlink.AddrDel(link, addr); err != nil {
		if isMissingNetlinkObject(err) {
			return nil
		}
		return fmt.Errorf("delete addr %s from %s: %w", cidr, linkName, err)
	}
	return nil
}

func removeLinkByName(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		if isMissingNetlinkObject(err) {
			return nil
		}
		return fmt.Errorf("find link %s: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		if isMissingNetlinkObject(err) {
			return nil
		}
		return fmt.Errorf("delete link %s: %w", name, err)
	}
	return nil
}

func isMissingNetlinkObject(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "cannot find")
}

// DumpFRRState logs FRR diagnostic state via the commander abstraction.
// Called on FRR setup failure or connectivity timeout to capture BGP/EVPN state.
func (m *Manager) DumpFRRState() {
	ctx := context.Background()
	type frrCmd struct {
		label string
		args  []string
	}
	cmds := []frrCmd{
		{"bgp summary", []string{"-c", "show bgp summary"}},
		{"bgp ipv4", []string{"-c", "show bgp ipv4 unicast"}},
		{"bgp ipv6", []string{"-c", "show bgp ipv6 unicast"}},
		{"bgp l2vpn evpn", []string{"-c", "show bgp l2vpn evpn"}},
		{"bfd peers", []string{"-c", "show bfd peers"}},
		{"interface brief", []string{"-c", "show interface brief"}},
	}
	m.log.Warn("=== FRR STATE DUMP START ===")
	for _, c := range cmds {
		out, err := m.commander.Run(ctx, "vtysh", c.args...)
		if err != nil {
			m.log.Error("FRR dump command failed", "label", c.label, "error", err)
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line != "" {
				m.log.Warn("FRR", "label", c.label, "data", line)
			}
		}
	}
	m.log.Warn("=== FRR STATE DUMP END ===")
}

type frrDirOwner struct {
	uid int
	gid int
	ok  bool
}

var (
	frrRuntimeDirs = []string{"/run/frr", "/var/run/frr", "/var/tmp/frr", "/var/lib/frr"}
	lookupFRRUser  = defaultFRRDirOwner
	chownFRRDir    = os.Chown
)

func defaultFRRDirOwner() frrDirOwner {
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return frrDirOwner{}
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[0] != "frr" {
			continue
		}
		uid, uidErr := strconv.Atoi(fields[2])
		gid, gidErr := strconv.Atoi(fields[3])
		if uidErr != nil || gidErr != nil {
			return frrDirOwner{}
		}
		return frrDirOwner{uid: uid, gid: gid, ok: true}
	}
	return frrDirOwner{}
}

// ensureFRRDirs creates runtime directories that FRR daemons expect.
// Without these, zebra cannot create its zserv.api socket and bgpd
// cannot communicate with zebra, breaking BGP unnumbered.
func ensureFRRDirs() {
	owner := lookupFRRUser()
	for _, d := range frrRuntimeDirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			slog.Warn("failed to create FRR directory", "path", d, "error", err)
			continue
		}
		if owner.ok {
			if err := chownFRRDir(d, owner.uid, owner.gid); err != nil {
				slog.Warn("failed to set FRR directory ownership", "path", d, "error", err)
			}
		}
	}
}

func (m *Manager) createVRF(name string, tableID uint32) error {
	vrf := &netlink.Vrf{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		Table:     tableID,
	}
	if err := netlink.LinkAdd(vrf); err != nil {
		if errors.Is(err, syscall.EEXIST) {
			m.log.Debug("VRF already exists", "name", name)
			return nil
		}
		return fmt.Errorf("add VRF %s: %w", name, err)
	}
	if err := netlink.LinkSetUp(vrf); err != nil {
		return fmt.Errorf("bring up VRF %s: %w", name, err)
	}
	return nil
}

func (m *Manager) createDummy(name, vrfName, addr string) error {
	dummy := &netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{Name: name},
	}
	if err := netlink.LinkAdd(dummy); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add dummy %s: %w", name, err)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find dummy %s: %w", name, err)
	}

	if vrfName != "" {
		vrfLink, err := netlink.LinkByName(vrfName)
		if err != nil {
			return fmt.Errorf("find VRF %s: %w", vrfName, err)
		}
		if err := netlink.LinkSetMasterByIndex(link, vrfLink.Attrs().Index); err != nil {
			return fmt.Errorf("assign dummy to VRF: %w", err)
		}
	}

	nlAddr, err := netlink.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("parse addr %s: %w", addr, err)
	}
	if err := netlink.AddrAdd(link, nlAddr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add addr to dummy: %w", err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up dummy %s: %w", name, err)
	}
	return nil
}

func (m *Manager) createVXLAN(vni uint32, srcIP, bridgeName, bridgeMAC string, physicalMTU int) error {
	vxlanName := fmt.Sprintf("vx%d", vni)

	// VXLAN MTU = physical MTU minus 50 bytes overhead (outer IP + UDP + VXLAN headers).
	vxlanMTU := physicalMTU - 50
	if vxlanMTU <= 0 {
		vxlanMTU = 1500
	}

	srcAddr := net.ParseIP(srcIP)
	vxlan := &netlink.Vxlan{
		LinkAttrs:    netlink.LinkAttrs{Name: vxlanName},
		VxlanId:      int(vni),
		SrcAddr:      srcAddr,
		Port:         4789,
		Learning:     false,
		VtepDevIndex: 0,
	}

	if err := netlink.LinkAdd(vxlan); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add VXLAN %s: %w", vxlanName, err)
	}

	vxLink, err := netlink.LinkByName(vxlanName)
	if err != nil {
		return fmt.Errorf("find VXLAN: %w", err)
	}
	if err := netlink.LinkSetMTU(vxLink, vxlanMTU); err != nil {
		return fmt.Errorf("set VXLAN MTU: %w", err)
	}

	hwAddr, err := net.ParseMAC(bridgeMAC)
	if err != nil {
		return fmt.Errorf("parse bridge MAC %s: %w", bridgeMAC, err)
	}

	bridge := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name:         bridgeName,
			HardwareAddr: hwAddr,
		},
	}
	if err := netlink.LinkAdd(bridge); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add bridge %s: %w", bridgeName, err)
	}

	brLink, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("find bridge: %w", err)
	}

	if err := netlink.LinkSetMasterByIndex(vxLink, brLink.Attrs().Index); err != nil {
		return fmt.Errorf("attach VXLAN to bridge: %w", err)
	}

	if err := netlink.LinkSetUp(brLink); err != nil {
		return fmt.Errorf("bring up bridge: %w", err)
	}
	if err := netlink.LinkSetUp(vxLink); err != nil {
		return fmt.Errorf("bring up VXLAN: %w", err)
	}
	return nil
}

func (m *Manager) addLoopbackAddress(ip string) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return fmt.Errorf("find loopback: %w", err)
	}

	addr, err := netlink.ParseAddr(ip + "/128")
	if err != nil {
		addr, err = netlink.ParseAddr(ip + "/32")
		if err != nil {
			return fmt.Errorf("parse overlay IP %s: %w", ip, err)
		}
	}

	if err := netlink.AddrAdd(lo, addr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add overlay IP to loopback: %w", err)
	}

	return nil
}

func (m *Manager) addBridgeAddress(bridgeName, addr string) error {
	link, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return fmt.Errorf("find bridge %s: %w", bridgeName, err)
	}

	nlAddr, err := netlink.ParseAddr(addr)
	if err != nil {
		return fmt.Errorf("parse provision IP %s: %w", addr, err)
	}

	if err := netlink.AddrAdd(link, nlAddr); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("add provision IP to bridge: %w", err)
	}

	m.log.Info("Assigned provision IP to bridge", "bridge", bridgeName, "ip", addr)
	return nil
}

func (m *Manager) configureNICs(nics []string, vrfName string, mtu int) error {
	for _, nic := range nics {
		if err := m.configureNIC(nic, vrfName, mtu); err != nil {
			m.log.Warn("Failed to configure NIC", "nic", nic, "error", err)
		}
	}
	return nil
}

func (m *Manager) configureNIC(name, vrfName string, mtu int) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find NIC %s: %w", name, err)
	}

	if err := netlink.LinkSetMTU(link, mtu); err != nil {
		return fmt.Errorf("set MTU on %s: %w", name, err)
	}

	if vrfName != "" {
		vrfLink, err := netlink.LinkByName(vrfName)
		if err != nil {
			return fmt.Errorf("find VRF %s: %w", vrfName, err)
		}
		if err := netlink.LinkSetMasterByIndex(link, vrfLink.Attrs().Index); err != nil {
			return fmt.Errorf("assign %s to VRF: %w", name, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bring up NIC %s: %w", name, err)
	}
	return nil
}

func (m *Manager) enableForwarding() error {
	sysctls := map[string]string{
		"/proc/sys/net/ipv4/ip_forward":                    "1",
		"/proc/sys/net/ipv6/conf/all/forwarding":           "2",
		"/proc/sys/net/ipv4/conf/all/rp_filter":            "0",
		"/proc/sys/net/ipv4/conf/default/rp_filter":        "0",
		"/proc/sys/net/ipv6/conf/all/accept_ra":            "2",
		"/proc/sys/net/ipv6/conf/all/accept_ra_defrtr":     "1",
		"/proc/sys/net/ipv6/conf/default/accept_ra":        "2",
		"/proc/sys/net/ipv6/conf/default/accept_ra_defrtr": "1",
	}

	for path, val := range sysctls {
		if err := os.WriteFile(path, []byte(val), 0o644); err != nil { //nolint:gosec // sysctl paths are trusted
			m.log.Debug("Failed to set sysctl", "path", path, "error", err)
		}
	}
	return nil
}

func (m *Manager) writeFRRConfig(conf string) error {
	if err := os.MkdirAll("/etc/frr", 0o755); err != nil {
		return fmt.Errorf("create /etc/frr: %w", err)
	}
	if err := os.WriteFile("/etc/frr/frr.conf", []byte(conf), 0o644); err != nil {
		return fmt.Errorf("write frr.conf: %w", err)
	}
	// vtysh.conf must exist for integrated config mode.
	vtyshConf := "service integrated-vtysh-config\n"
	if err := os.WriteFile("/etc/frr/vtysh.conf", []byte(vtyshConf), 0o644); err != nil {
		return fmt.Errorf("write vtysh.conf: %w", err)
	}
	return nil
}

func (m *Manager) writeDaemonsConfig() error {
	const daemons = `# FRR daemons configuration - managed by BOOTy
zebra=yes
bgpd=yes
ospfd=no
ospf6d=no
ripd=no
ripngd=no
isisd=no
pimd=no
ldpd=no
nhrpd=no
eigrpd=no
babeld=no
sharpd=no
pbrd=no
bfdd=yes
fabricd=no
vrrpd=no
pathd=no

vtysh_enable=yes
zebra_options="  -A 127.0.0.1 -s 90000000"
bgpd_options="   -A 127.0.0.1"
bfdd_options="   -A 127.0.0.1"
`
	if err := os.WriteFile("/etc/frr/daemons", []byte(daemons), 0o640); err != nil {
		return fmt.Errorf("write daemons config: %w", err)
	}
	return nil
}

const (
	frrStartSystemctl = "systemctl"
	frrStartInit      = "frrinit"
	frrStartDirect    = "direct"
)

var (
	runFRRDaemonCommand = runDaemonCmd
	frrRestartInterval  = 120 * time.Second
	frrDaemonStartDelay = 500 * time.Millisecond
	frrDaemonStopWait   = 3 * time.Second
	frrDaemonKillWait   = time.Second
	findFRRDaemonPIDs   = findFRRDaemonPIDsFromProc
	signalFRRProcess    = syscall.Kill
	frrInitScriptPath   = "/usr/lib/frr/frrinit.sh"
)

// startFRR launches FRR daemons using the best available method.
// All methods avoid CombinedOutput() because FRR daemons fork with -d,
// and child processes inherit pipes, blocking CombinedOutput() indefinitely.
func (m *Manager) startFRR(ctx context.Context) error {
	m.frrStartMethod = ""
	m.directDaemonList = nil

	if err := runFRRDaemonCommand(ctx, "systemctl", "restart", "frr"); err == nil {
		m.frrStartMethod = frrStartSystemctl
		m.log.Info("FRR daemons started via systemctl")
		return nil
	}

	if _, statErr := os.Stat(frrInitScriptPath); statErr == nil {
		if err := runFRRDaemonCommand(ctx, frrInitScriptPath, "start"); err == nil {
			m.frrStartMethod = frrStartInit
			m.log.Info("FRR daemons started via frrinit.sh")
			return nil
		}
		m.log.Warn("frrinit.sh start failed, falling back to direct daemon start")
	}

	return m.startDaemonsDirect(ctx)
}

func (m *Manager) stopFRR(ctx context.Context) error {
	switch m.frrStartMethod {
	case frrStartSystemctl:
		return runFRRDaemonCommand(ctx, "systemctl", "stop", "frr")
	case frrStartInit:
		return runFRRDaemonCommand(ctx, frrInitScriptPath, "stop")
	case frrStartDirect:
		return m.stopDaemonsDirect(ctx)
	default:
		if len(m.directDaemonList) > 0 {
			return m.stopDaemonsDirect(ctx)
		}
		return stopFRRBestEffort(ctx)
	}
}

func (m *Manager) restartFRR(ctx context.Context) error {
	switch m.frrStartMethod {
	case frrStartSystemctl:
		return runFRRDaemonCommand(ctx, "systemctl", "restart", "frr")
	case frrStartInit:
		return runFRRDaemonCommand(ctx, frrInitScriptPath, "restart")
	case frrStartDirect:
		if err := m.stopDaemonsDirect(ctx); err != nil {
			return err
		}
		return m.startDaemonsDirect(ctx)
	default:
		return restartFRRBestEffort(ctx)
	}
}

func restartFRRBestEffort(ctx context.Context) error {
	if err := runFRRDaemonCommand(ctx, "systemctl", "restart", "frr"); err == nil {
		return nil
	}
	if _, statErr := os.Stat(frrInitScriptPath); statErr == nil {
		return runFRRDaemonCommand(ctx, frrInitScriptPath, "restart")
	}
	return fmt.Errorf("no FRR restart method available")
}

func stopFRRBestEffort(ctx context.Context) error {
	if err := runFRRDaemonCommand(ctx, "systemctl", "stop", "frr"); err == nil {
		return nil
	}
	if _, statErr := os.Stat(frrInitScriptPath); statErr == nil {
		return runFRRDaemonCommand(ctx, frrInitScriptPath, "stop")
	}
	return nil
}

// runDaemonCmd runs a command that may fork long-lived daemons.
// It uses os.Stderr directly (no pipes) so cmd.Wait returns when the
// parent process exits, not when all child pipe holders close.
func runDaemonCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s: %w", name, err)
	}

	return nil
}

type frrDaemonSpec struct {
	name     string
	args     []string
	required bool
}

var frrDaemonDirs = []string{"/usr/lib/frr", "/sbin"}

func frrDaemonSpecs() []frrDaemonSpec {
	// FRR 10.x daemon startup order: mgmtd → zebra → staticd → bgpd → bfdd.
	// bgpd always uses -f to read its config directly — peer-group property
	// inheritance is unreliable when config is pushed by mgmtd alone.
	return []frrDaemonSpec{
		{"mgmtd", []string{"-d", "-A", "127.0.0.1"}, false},
		{"zebra", []string{"-d", "-A", "127.0.0.1", "-s", "90000000"}, true},
		{"staticd", []string{"-d", "-A", "127.0.0.1"}, false},
		{"bgpd", []string{"-d", "-A", "127.0.0.1", "-f", "/etc/frr/frr.conf"}, true},
		{"bfdd", []string{"-d", "-A", "127.0.0.1"}, true},
	}
}

func resolveFRRDaemonPath(name string) (path string, ok bool, err error) {
	for _, dir := range frrDaemonDirs {
		candidate := dir + "/" + name
		if _, err := os.Stat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", false, fmt.Errorf("stat FRR daemon %s: %w", candidate, err)
		}
		return candidate, true, nil
	}
	return "", false, nil
}

func resolveFRRDaemons(daemons []frrDaemonSpec) (paths map[string]string, missing []string, err error) {
	paths = make(map[string]string, len(daemons))
	for _, d := range daemons {
		path, ok, err := resolveFRRDaemonPath(d.name)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			if d.required {
				missing = append(missing, d.name)
			}
			continue
		}
		paths[d.name] = path
	}
	return paths, missing, nil
}

func (m *Manager) startDaemonsDirect(ctx context.Context) error {
	daemons := frrDaemonSpecs()
	paths, missing, err := resolveFRRDaemons(daemons)
	if err != nil {
		return fmt.Errorf("resolve FRR daemons: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("required FRR daemons not found: %s", strings.Join(missing, ", "))
	}
	for _, d := range daemons {
		path, ok := paths[d.name]
		if !ok {
			m.log.Debug("Daemon not found, skipping", "daemon", d.name)
			continue
		}
		if err := runFRRDaemonCommand(ctx, path, d.args...); err != nil {
			if d.required {
				startErr := fmt.Errorf("start FRR daemon %s: %w", d.name, err)
				if stopErr := m.stopDaemonsDirect(ctx); stopErr != nil {
					return errors.Join(startErr, fmt.Errorf("rollback direct FRR daemons: %w", stopErr))
				}
				return startErr
			}
			m.log.Warn("Failed to start daemon", "daemon", d.name, "error", err)
		} else {
			m.trackDirectDaemon(d.name)
			m.log.Info("Started FRR daemon", "daemon", d.name)
		}
		time.Sleep(frrDaemonStartDelay)
	}
	m.frrStartMethod = frrStartDirect
	return nil
}

func (m *Manager) trackDirectDaemon(name string) {
	for _, existing := range m.directDaemonList {
		if existing == name {
			return
		}
	}
	m.directDaemonList = append(m.directDaemonList, name)
}

func (m *Manager) stopDaemonsDirect(ctx context.Context) error {
	names := reverseStrings(m.directDaemonList)
	if len(names) == 0 {
		return nil
	}
	if err := signalDaemons(ctx, names, syscall.SIGTERM); err != nil {
		return err
	}
	if err := waitForDaemonsExit(ctx, names, frrDaemonStopWait); err != nil {
		m.log.Warn("FRR daemons still running after SIGTERM; sending SIGKILL", "error", err)
		if killErr := signalDaemons(ctx, names, syscall.SIGKILL); killErr != nil {
			return errors.Join(err, killErr)
		}
		if waitErr := waitForDaemonsExit(ctx, names, frrDaemonKillWait); waitErr != nil {
			return errors.Join(err, waitErr)
		}
	}
	m.directDaemonList = nil
	m.frrStartMethod = ""
	return nil
}

func reverseStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}

func signalDaemons(ctx context.Context, names []string, sig syscall.Signal) error {
	pids, err := findFRRDaemonPIDs(names)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		for _, pid := range pids[name] {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("signal FRR daemons canceled: %w", ctxErr)
			}
			if err := signalFRRProcess(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("signal %s pid %d: %w", name, pid, err))
			}
		}
	}
	return errors.Join(errs...)
}

func waitForDaemonsExit(ctx context.Context, names []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pids, err := findFRRDaemonPIDs(names)
		if err != nil {
			return err
		}
		if daemonPIDCount(pids) == 0 {
			return nil
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return fmt.Errorf("frr daemon pids still running: %v", pids)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("wait for FRR daemon exit canceled: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func daemonPIDCount(pids map[string][]int) int {
	count := 0
	for _, ids := range pids {
		count += len(ids)
	}
	return count
}

func findFRRDaemonPIDsFromProc(names []string) (map[string][]int, error) {
	want := make(map[string]struct{}, len(names))
	for _, name := range names {
		want[name] = struct{}{}
	}
	pids := make(map[string][]int, len(names))
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	for _, entry := range entries {
		pid, ok := procPID(entry.Name())
		if !ok {
			continue
		}
		name, readErr := procComm(pid)
		if readErr != nil {
			continue
		}
		if _, found := want[name]; found {
			pids[name] = append(pids[name], pid)
		}
	}
	return pids, nil
}

func procPID(name string) (int, bool) {
	pid, err := strconv.Atoi(name)
	return pid, err == nil
}

func procComm(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "", fmt.Errorf("read process comm for pid %d: %w", pid, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func (m *Manager) addBGPPeer(ctx context.Context, vrfName string, asn uint32, nic string) error {
	bgpCmd := fmt.Sprintf("router bgp %d", asn)
	if vrfName != "" {
		bgpCmd = fmt.Sprintf("router bgp %d vrf %s", asn, vrfName)
	}
	out, err := m.commander.Run(ctx, "vtysh",
		"-c", "conf t",
		"-c", bgpCmd,
		"-c", fmt.Sprintf("neighbor %s interface peer-group fabric", nic),
	)
	if err != nil {
		return fmt.Errorf("vtysh add peer %s: %w (output: %s)", nic, err, string(out))
	}
	m.log.Info("Added BGP peer", "nic", nic, "vrf", vrfName)
	return nil
}

// waitForHTTPWithFRR polls target, restarting FRR every 120s if needed.
func waitForHTTPWithFRR(ctx context.Context, target string, timeout time.Duration, mgr *Manager) error {
	log := slog.Default().With("component", "frr")
	if mgr != nil {
		log = mgr.log
	}
	if target == "" {
		return fmt.Errorf("empty connectivity target URL")
	}

	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 10 * time.Second}
	attempt := 0
	lastRestart := time.Now()
	restartInterval := frrRestartInterval

	for time.Now().Before(deadline) {
		attempt++
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, http.NoBody)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		resp, err := client.Do(req) //nolint:gosec // target URL from trusted config, not user input
		if err == nil {
			_ = resp.Body.Close()
			log.Info("Network connectivity established",
				"target", target, "attempt", attempt)
			return nil
		}

		log.Debug("Connectivity check failed",
			"target", target, "attempt", attempt, "error", err)

		if time.Since(lastRestart) >= restartInterval {
			log.Info("Restarting FRR daemons for connectivity recovery")
			if mgr != nil {
				if err := mgr.restartFRR(ctx); err != nil {
					log.Warn("failed to restart FRR daemons", "error", err)
				}
			} else if err := restartFRRBestEffort(ctx); err != nil {
				log.Warn("failed to restart FRR daemons", "error", err)
			}
			lastRestart = time.Now()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("connectivity check canceled: %w", ctx.Err())
		case <-time.After(1 * time.Second):
		}
	}

	if mgr != nil {
		log.Warn("connectivity timeout — dumping FRR state for diagnostics")
		mgr.DumpFRRState()
	}
	return fmt.Errorf("network connectivity timeout after %s (%d attempts)", timeout, attempt)
}
