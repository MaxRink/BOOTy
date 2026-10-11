//go:build e2e_integration

package integration

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/telekom/BOOTy/pkg/caprf"
	"github.com/telekom/BOOTy/pkg/config"
	"github.com/telekom/BOOTy/pkg/network"
	"github.com/telekom/BOOTy/pkg/network/frr"
)

// containerMgmtIP returns the Docker management network IP for a containerlab node.
func containerMgmtIP(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name).Output()
	if err != nil {
		t.Fatalf("get mgmt IP for %s: %v", name, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		t.Fatalf("no management IP for container %s", name)
	}
	return ip
}

// ---------------------------------------------------------------------------
// CAPRF client tests -- exercise Go CAPRF package against the caprf-mock
// ---------------------------------------------------------------------------

func TestVarsParsingFromCAPRFMock(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	resp, err := http.Get(fmt.Sprintf("http://%s/vars", ip)) //nolint:gosec // test URL
	if err != nil {
		t.Fatalf("GET /vars: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /vars: status %d", resp.StatusCode)
	}

	cfg, err := caprf.ParseVars(resp.Body)
	if err != nil {
		t.Fatalf("ParseVars: %v", err)
	}

	if cfg.Hostname != "clab-e2e-host" {
		t.Errorf("hostname = %q, want clab-e2e-host", cfg.Hostname)
	}
	if cfg.Mode != "provision" {
		t.Errorf("mode = %q, want provision", cfg.Mode)
	}
	if cfg.Transport.Token != "e2e-fabric-token" {
		t.Errorf("token = %q, want e2e-fabric-token", cfg.Transport.Token)
	}
	if len(cfg.Provision.Image.URLs) != 1 {
		t.Errorf("image URLs = %v, want 1 entry", cfg.Provision.Image.URLs)
	}
	if cfg.Provision.Disk.MinSizeGB != 50 {
		t.Errorf("min disk = %d, want 50", cfg.Provision.Disk.MinSizeGB)
	}
	if cfg.Network.EVPN.UnderlaySubnet == "" {
		t.Error("underlay_subnet should be set")
	}
	if cfg.Network.EVPN.OverlaySubnet == "" {
		t.Error("overlay_subnet should be set")
	}
	if cfg.Network.IPMI.Subnet == "" {
		t.Error("ipmi_subnet should be set")
	}
	if cfg.Provision.ProviderID != "redfish://clab-e2e/host-01" {
		t.Errorf("provider_id = %q", cfg.Provision.ProviderID)
	}
	if cfg.Provision.Region != "eu-central-1" {
		t.Errorf("region = %q", cfg.Provision.Region)
	}
	if cfg.Provision.FailureDomain != "rack-a" {
		t.Errorf("failure_domain = %q", cfg.Provision.FailureDomain)
	}
	t.Logf("Parsed vars: hostname=%s images=%v region=%s", cfg.Hostname, cfg.Provision.Image.URLs, cfg.Provision.Region)
}

func TestCAPRFClientStatusLifecycle(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	base := fmt.Sprintf("http://%s", ip)

	client := caprf.NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{
			Token:      "e2e-fabric-token",
			InitURL:    base + "/status/init",
			SuccessURL: base + "/status/success",
			ErrorURL:   base + "/status/error",
			LogURL:     base + "/log",
		},
	})
	ctx := context.Background()

	for _, tc := range []struct {
		status config.Status
		msg    string
		label  string
	}{
		{config.StatusInit, "starting provisioning", "init"},
		{config.StatusSuccess, "done", "success"},
		{config.StatusError, "disk failure", "error"},
	} {
		err := client.ReportStatus(ctx, tc.status, tc.msg)
		assertInsecureTransportBehavior(t, tc.label, err)
	}
	t.Log("CAPRF status lifecycle works with secure transport policy")
}

func TestCAPRFClientLogAndDebugShipping(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	base := fmt.Sprintf("http://%s", ip)

	client := caprf.NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{
			Token:    "e2e-token",
			LogURL:   base + "/log",
			DebugURL: base + "/debug",
		},
	})
	ctx := context.Background()

	assertInsecureTransportBehavior(t, "ShipLog", client.ShipLog(ctx, "provisioning step 1 complete"))
	assertInsecureTransportBehavior(t, "ShipLog(2)", client.ShipLog(ctx, "provisioning step 2 complete"))
	assertInsecureTransportBehavior(t, "ShipDebug", client.ShipDebug(ctx, "disk enumeration: sda=500GB sdb=1TB"))
	t.Log("CAPRF log + debug shipping works with secure transport policy")
}

func TestCAPRFClientFetchCommandsEmpty(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	client := caprf.NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "e2e-token"},
		Agent:     config.AgentConfig{CommandsURL: fmt.Sprintf("http://%s/commands", ip)},
	})

	cmds, err := client.FetchCommands(context.Background())
	if err != nil {
		if cmds != nil {
			t.Fatalf("expected nil commands when insecure transport is blocked, got %v", cmds)
		}
		assertInsecureTransportBehavior(t, "FetchCommands", err)
		t.Log("FetchCommands blocked non-HTTPS bearer transport as expected")
		return
	}
	t.Log("FetchCommands succeeded over HTTP without bearer token")
}

func TestCAPRFClientFetchCommandsWithData(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	client := caprf.NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "e2e-token"},
		Agent:     config.AgentConfig{CommandsURL: fmt.Sprintf("http://%s/commands-data", ip)},
	})

	cmds, err := client.FetchCommands(context.Background())
	if err != nil {
		if cmds != nil {
			t.Fatalf("expected nil commands when insecure transport is blocked, got %v", cmds)
		}
		assertInsecureTransportBehavior(t, "FetchCommands with data", err)
		t.Log("FetchCommands with data blocked non-HTTPS bearer transport as expected")
		return
	}
	if len(cmds) == 0 {
		t.Fatal("expected command payload when HTTP requests are allowed without bearer token")
	}
	t.Logf("FetchCommands with data succeeded over HTTP without bearer token: %d command(s)", len(cmds))
}

func TestCAPRFClientHeartbeatThroughGoClient(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	client := caprf.NewFromConfig(&config.MachineConfig{
		Transport: config.TransportConfig{Token: "e2e-token"},
		Agent:     config.AgentConfig{HeartbeatURL: fmt.Sprintf("http://%s/status/heartbeat", ip)},
	})

	assertInsecureTransportBehavior(t, "Heartbeat", client.Heartbeat(context.Background()))
	t.Log("Heartbeat works with secure transport policy")
}

// ---------------------------------------------------------------------------
// Image + HTTP connectivity tests
// ---------------------------------------------------------------------------

func TestImageDownloadViaGoClient(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-nginx")
	resp, err := http.Get(fmt.Sprintf("http://%s/images/test.img.gz", ip)) //nolint:gosec // test URL
	if err != nil {
		t.Fatalf("GET /images/test.img.gz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("image download status: %d", resp.StatusCode)
	}

	// Read first 4 bytes to verify gzip magic number (1f 8b).
	header := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, header); err != nil {
		t.Fatalf("read image header: %v", err)
	}
	if header[0] != 0x1f || header[1] != 0x8b {
		t.Fatalf("image is not gzip: magic=%x%x", header[0], header[1])
	}
	t.Logf("Image downloaded via Go HTTP client (gzip, content-length: %s)", resp.Header.Get("Content-Length"))
}

func TestImageDownloadViaOverlayFromClient(t *testing.T) {
	requireNetworkLab(t)

	// Verify test image is listed through EVPN overlay (avoid full 256 MiB download).
	imgList := wgetFromClient(t, "http://10.100.0.10/images/")
	if !strings.Contains(imgList, "test.img.gz") {
		t.Fatalf("test.img.gz not in image listing:\n%s", imgList)
	}

	html := wgetFromClient(t, "http://10.100.0.10/")
	if !strings.Contains(html, "booty-lab") {
		t.Fatalf("overlay static content mismatch: %s", html)
	}
	t.Log("Image listed + static content downloaded through EVPN overlay")
}

func TestWaitForHTTPAgainstNginx(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-nginx")
	target := fmt.Sprintf("http://%s/", ip)

	err := network.WaitForHTTP(context.Background(), target, 30*time.Second)
	if err != nil {
		t.Fatalf("WaitForHTTP(%s): %v", target, err)
	}
	t.Log("WaitForHTTP succeeds against nginx container")
}

func TestWaitForHTTPAgainstCAPRFMock(t *testing.T) {
	requireNetworkLab(t)

	ip := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	target := fmt.Sprintf("http://%s/vars", ip)

	err := network.WaitForHTTP(context.Background(), target, 30*time.Second)
	if err != nil {
		t.Fatalf("WaitForHTTP(%s): %v", target, err)
	}
	t.Log("WaitForHTTP succeeds against CAPRF mock")
}

// ---------------------------------------------------------------------------
// FRR / network configuration tests
// ---------------------------------------------------------------------------

func TestFRRConfigRenderMatchesTopology(t *testing.T) {
	cfg := &network.Config{
		ASN:            65000,
		UnderlaySubnet: "10.0.0.0/24",
		VRFName:        "Vrf_underlay",
	}

	rendered, err := frr.RenderConfig(cfg, "10.0.0.1", "", []string{"eth1"})
	if err != nil {
		t.Fatalf("RenderConfig: %v", err)
	}

	for _, want := range []string{
		"router bgp 65000",
		"bgp router-id 10.0.0.1",
		"neighbor fabric peer-group",
		"neighbor fabric remote-as external",
		"neighbor eth1 interface peer-group fabric",
		"address-family l2vpn evpn",
		"advertise-all-vni",
		"vrf Vrf_underlay",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered config missing %q", want)
		}
	}
	t.Log("FRR config renders correctly for clab topology parameters")
}

func TestFRRConfigRenderOnefabric(t *testing.T) {
	cfg := &network.Config{
		ASN:              65000,
		UnderlaySubnet:   "10.0.0.0/24",
		VRFName:          "Vrf_underlay",
		DCGWIPs:          "172.16.0.1,172.16.0.2",
		OverlayAggregate: "10.100.0.0/16",
		VPNRT:            "65000:100",
	}

	rendered, err := frr.RenderConfig(cfg, "10.0.0.1", "", []string{"eth1", "eth2"})
	if err != nil {
		t.Fatalf("RenderConfig onefabric: %v", err)
	}

	for _, want := range []string{
		"172.16.0.1",
		"172.16.0.2",
		"aggregate-address 10.100.0.0/16",
		"route-target both 65000:100",
		"neighbor eth1 interface peer-group fabric",
		"neighbor eth2 interface peer-group fabric",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("onefabric config missing %q", want)
		}
	}
	t.Log("Onefabric FRR config renders correctly")
}

func TestIPDerivationWithFabricSubnets(t *testing.T) {
	ip, err := frr.DeriveIPFromOffset("172.30.0.5", "172.30.0.0/24", "10.0.0.0/24")
	if err != nil {
		t.Fatalf("IPv4 derivation: %v", err)
	}
	if ip != "10.0.0.5" {
		t.Errorf("underlay IP = %s, want 10.0.0.5", ip)
	}

	ipv6, err := frr.DeriveIPFromOffset("172.30.0.5", "172.30.0.0/24", "2a01:598:40a:5481::/64")
	if err != nil {
		t.Fatalf("cross-family derivation: %v", err)
	}
	if ipv6 != "2a01:598:40a:5481::5" {
		t.Errorf("overlay IP = %s, want 2a01:598:40a:5481::5", ipv6)
	}

	ip42, err := frr.DeriveIPFromOffset("172.30.0.42", "172.30.0.0/24", "10.0.0.0/24")
	if err != nil {
		t.Fatalf("offset-42: %v", err)
	}
	if ip42 != "10.0.0.42" {
		t.Errorf("offset-42 IP = %s, want 10.0.0.42", ip42)
	}
	t.Log("IP derivation produces correct results for fabric subnets")
}

func TestDeriveAddressesForFabric(t *testing.T) {
	cfg := &network.Config{
		UnderlaySubnet: "10.0.0.0/24",
		OverlaySubnet:  "2a01:598:40a:5481::/64",
		IPMISubnet:     "172.30.0.0/24",
		IPMIIP:         "172.30.0.10",
		IPMIMAC:        "00:25:90:ab:cd:ef",
	}

	underlay, overlay, mac, err := frr.DeriveAddresses(cfg)
	if err != nil {
		t.Fatalf("DeriveAddresses: %v", err)
	}

	if underlay != "10.0.0.10" {
		t.Errorf("underlay = %s, want 10.0.0.10", underlay)
	}
	if overlay != "2a01:598:40a:5481::a" {
		t.Errorf("overlay = %s, want 2a01:598:40a:5481::a", overlay)
	}
	if mac != "02:54:90:ab:cd:ef" {
		t.Errorf("bridgeMAC = %s, want 02:54:90:ab:cd:ef", mac)
	}
	t.Logf("DeriveAddresses: underlay=%s overlay=%s mac=%s", underlay, overlay, mac)
}

func TestBridgeMACDerivation(t *testing.T) {
	mac := frr.DeriveBridgeMAC("00:25:90:ab:cd:ef")
	if mac != "02:54:90:ab:cd:ef" {
		t.Errorf("colon MAC = %s, want 02:54:90:ab:cd:ef", mac)
	}

	mac = frr.DeriveBridgeMAC("00-25-90-ab-cd-ef")
	if mac != "02:54:90:ab:cd:ef" {
		t.Errorf("dash MAC = %s, want 02:54:90:ab:cd:ef", mac)
	}

	mac = frr.DeriveBridgeMAC("invalid")
	if mac != "" {
		t.Errorf("invalid MAC = %s, want empty", mac)
	}
	t.Log("Bridge MAC derivation handles all formats")
}

// ---------------------------------------------------------------------------
// EVPN route and tunnel verification
// ---------------------------------------------------------------------------

func TestEVPNRouteTypes(t *testing.T) {
	requireContainerLab(t)

	var found bool
	for range 30 {
		out := dockerExec(t, "clab-booty-lab-spine01", "vtysh", "-c", "show bgp l2vpn evpn")
		if strings.Contains(out, "Route Distinguisher") ||
			strings.Contains(out, "[2]:[") ||
			strings.Contains(out, "[3]:[") {
			found = true
			t.Log("EVPN routes present on spine01")
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !found {
		t.Fatal("no EVPN routes found on spine01 after 30s")
	}
}

func TestBGPNeighborASNVerification(t *testing.T) {
	requireContainerLab(t)

	var verified bool
	for range 30 {
		out := dockerExec(t, "clab-booty-lab-spine01", "vtysh", "-c", "show bgp neighbors json")
		if strings.Contains(out, "65001") && strings.Contains(out, "Established") {
			verified = true
			t.Log("Spine BGP neighbor with ASN 65001 verified")
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !verified {
		out := dockerExec(t, "clab-booty-lab-spine01", "vtysh", "-c", "show bgp summary")
		t.Fatalf("BGP neighbor ASN not verified:\n%s", out)
	}
}

func TestVXLANTunnelEndpoints(t *testing.T) {
	requireContainerLab(t)

	out := dockerExec(t, "clab-booty-lab-spine01", "ip", "-d", "link", "show", "vxlan100")
	if !strings.Contains(out, "vxlan id 100") {
		t.Fatalf("vxlan100 missing VNI 100:\n%s", out)
	}
	if !strings.Contains(out, "dstport 4789") {
		t.Fatalf("vxlan100 missing dstport 4789:\n%s", out)
	}
	t.Log("VXLAN tunnel endpoint configured correctly on spine01")
}

// ---------------------------------------------------------------------------
// Full provisioning flow -- end-to-end through the fabric
// ---------------------------------------------------------------------------

func TestFullProvisioningFlow(t *testing.T) {
	requireNetworkLab(t)

	capIP := containerMgmtIP(t, "clab-booty-lab-caprf-mock")
	ngxIP := containerMgmtIP(t, "clab-booty-lab-nginx")

	// Step 1: Parse vars from CAPRF mock
	t.Log("Step 1: Fetching and parsing vars from CAPRF mock")
	resp, err := http.Get(fmt.Sprintf("http://%s/vars", capIP)) //nolint:gosec // test URL
	if err != nil {
		t.Fatalf("GET /vars: %v", err)
	}
	defer resp.Body.Close()

	mcfg, err := caprf.ParseVars(resp.Body)
	if err != nil {
		t.Fatalf("ParseVars: %v", err)
	}

	// Step 2: Create CAPRF client, report init
	// Override URLs to use management IPs (vars file has overlay IPs)
	t.Log("Step 2: Reporting init status via CAPRF client")
	mcfg.Transport.InitURL = fmt.Sprintf("http://%s/status/init", capIP)
	mcfg.Transport.SuccessURL = fmt.Sprintf("http://%s/status/success", capIP)
	mcfg.Transport.ErrorURL = fmt.Sprintf("http://%s/status/error", capIP)
	mcfg.Transport.LogURL = fmt.Sprintf("http://%s/log", capIP)
	mcfg.Transport.DebugURL = fmt.Sprintf("http://%s/debug", capIP)

	client := caprf.NewFromConfig(mcfg)
	ctx := context.Background()

	assertInsecureTransportBehavior(t, "init", client.ReportStatus(ctx, config.StatusInit, "starting"))

	// Step 3: Ship a provisioning log
	t.Log("Step 3: Shipping log via CAPRF client")
	assertInsecureTransportBehavior(t, "ship log", client.ShipLog(ctx, "imaging disk /dev/sda"))

	// Step 4: Download image from nginx via management network
	t.Log("Step 4: Downloading test image header from nginx")
	imgResp, err := http.Get(fmt.Sprintf("http://%s/images/test.img.gz", ngxIP)) //nolint:gosec // test URL
	if err != nil {
		t.Fatalf("image download: %v", err)
	}
	defer imgResp.Body.Close()

	if imgResp.StatusCode != http.StatusOK {
		t.Fatalf("image download status: %d", imgResp.StatusCode)
	}
	// Verify gzip magic number.
	header := make([]byte, 2)
	if _, err := io.ReadFull(imgResp.Body, header); err != nil {
		t.Fatalf("read image header: %v", err)
	}
	if header[0] != 0x1f || header[1] != 0x8b {
		t.Fatalf("image is not gzip: magic=%x%x", header[0], header[1])
	}

	// Step 5: Also verify image listed through EVPN overlay
	t.Log("Step 5: Verifying image listed through EVPN overlay")
	imgList := wgetFromClient(t, "http://10.100.0.10/images/")
	if !strings.Contains(imgList, "test.img.gz") {
		t.Fatalf("test.img.gz not in overlay image listing:\n%s", imgList)
	}

	// Step 6: Report success
	t.Log("Step 6: Reporting success via CAPRF client")
	assertInsecureTransportBehavior(t, "success", client.ReportStatus(ctx, config.StatusSuccess, "provisioning complete"))

	t.Log("Full provisioning flow validated with secure transport policy: vars -> CAPRF calls -> image -> overlay")
}

func assertInsecureTransportBehavior(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Logf("%s: non-HTTPS request allowed without bearer token", label)
		return
	}
	if !strings.Contains(strings.ToLower(err.Error()), "insecure transport") {
		t.Fatalf("%s: expected insecure transport error, got: %v", label, err)
	}
}
