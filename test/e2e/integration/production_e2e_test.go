//go:build e2e_production

// Package integration contains E2E tests for the production-realistic topology.
// These tests verify that BOOTy correctly handles production networking patterns:
//
//   - VRF isolation (Vrf_underlay, table 10)
//   - eBGP unnumbered to leaf with local-as override
//   - iBGP to DCGW route reflector with BFD (150ms)
//   - EVPN VXLAN with production-scale VNI (2002002)
//   - BGP timers (keepalive 30s, hold 90s)
//
// Prerequisites:
//
//	make clab-production-up   # deploys topology-production.clab.yml
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	prodLabPrefix = "clab-booty-production-lab"

	prodSpineContainer  = prodLabPrefix + "-spine01"
	prodDCGWContainer   = prodLabPrefix + "-dcgw01"
	prodBootyContainer  = prodLabPrefix + "-booty-prod"
	prodClientContainer = prodLabPrefix + "-client"
	prodNginxContainer  = prodLabPrefix + "-nginx"
	prodCAPRFContainer  = prodLabPrefix + "-caprf-mock"

	prodBGPConvergeTimeout  = 90 * time.Second
	prodBGPConvergeInterval = 2 * time.Second
)

// requireProductionLab fails if the production topology is not deployed.
func requireProductionLab(t *testing.T) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker not available: %v", err)
	}
	if !strings.Contains(string(out), prodSpineContainer) {
		t.Fatal("Production topology not deployed (run: make clab-production-up)")
	}
}

// prodDockerExec runs a command inside a production lab container.
func prodDockerExec(t *testing.T, container string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmdArgs := append([]string{"exec", container}, args...)
	out, err := exec.CommandContext(ctx, "docker", cmdArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s %s failed: %v\n%s",
			container, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// prodDockerExecRaw runs docker exec and returns output + error without failing.
func prodDockerExecRaw(t *testing.T, container string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmdArgs := append([]string{"exec", container}, args...)
	out, err := exec.CommandContext(ctx, "docker", cmdArgs...).CombinedOutput()
	return string(out), err
}

// prodGetBootyLogs retrieves BOOTy log output from the booty-prod container.
func prodGetBootyLogs(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "logs", prodBootyContainer).CombinedOutput()
	if err != nil {
		t.Logf("Warning: could not get logs for %s: %v", prodBootyContainer, err)
		return ""
	}
	return string(out)
}

// prodWaitForLogEntry waits for a log line in the booty-prod container.
// Fetches the full log each poll to avoid missing entries that scroll past a
// --tail window — same approach as the boot E2E helper.
func prodWaitForLogEntry(t *testing.T, entry string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, err := exec.CommandContext(ctx, "docker", "logs", prodBootyContainer).CombinedOutput()
		cancel()
		if err == nil && strings.Contains(string(out), entry) {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// prodDumpDebugState dumps network and BGP state from all containers on failure.
func prodDumpDebugState(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}

	t.Log("=== PRODUCTION DEBUG STATE DUMP ===")

	// Spine BGP state.
	for _, cmd := range []string{
		"show bgp summary json",
		"show bgp l2vpn evpn summary json",
		"show bgp l2vpn evpn",
		"show ip route",
		"show evpn vni",
	} {
		out, _ := prodDockerExecRaw(t, prodSpineContainer, "vtysh", "-c", cmd)
		t.Logf("[spine01] %s:\n%s", cmd, out)
	}

	// DCGW BGP state.
	for _, cmd := range []string{
		"show bgp summary json",
		"show bgp neighbors 10.50.0.140 json",
		"show bfd peers",
		"show bgp l2vpn evpn",
	} {
		out, _ := prodDockerExecRaw(t, prodDCGWContainer, "vtysh", "-c", cmd)
		t.Logf("[dcgw01] %s:\n%s", cmd, out)
	}

	// BOOTy network state.
	for _, cmd := range [][]string{
		{"ip", "addr", "show"},
		{"ip", "link", "show", "type", "vrf"},
		{"ip", "-d", "link", "show", "type", "vxlan"},
		{"ip", "route", "show", "table", "10"},
		{"bridge", "fdb", "show"},
		{"vtysh", "-c", "show bgp summary"},
		{"vtysh", "-c", "show bgp l2vpn evpn"},
		{"vtysh", "-c", "show bfd peers"},
	} {
		out, _ := prodDockerExecRaw(t, prodBootyContainer, cmd...)
		t.Logf("[booty-prod] %s:\n%s", strings.Join(cmd, " "), out)
	}

	// Full BOOTy logs.
	logs := prodGetBootyLogs(t)
	t.Logf("[booty-prod] BOOTy logs:\n%s", logs)
}

// --- Boot and mode detection tests ---

func TestProductionBootyStartsSuccessfully(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	if !prodWaitForLogEntry(t, "starting BOOTy", 60*time.Second) {
		t.Fatal("BOOTy did not start within 60s")
	}
	t.Log("BOOTy started successfully")
}

func TestProductionCAPRFModeDetected(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	if !prodWaitForLogEntry(t, "CAPRF mode active", 60*time.Second) {
		t.Fatal("CAPRF mode not detected within 60s")
	}
	t.Log("CAPRF mode correctly detected")
}

func TestProductionFRRNetworkModeSelected(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	if !prodWaitForLogEntry(t, "using FRR/EVPN network mode", 60*time.Second) {
		t.Fatal("FRR/EVPN network mode not selected")
	}
	t.Log("FRR/EVPN network mode correctly selected")
}

// --- VRF isolation tests ---

func TestProductionVRFCreated(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// Wait for BOOTy to complete network setup.
	if !prodWaitForLogEntry(t, "network setup complete", prodBGPConvergeTimeout) {
		// Check if FRR started — network may be up even without explicit log.
		if !prodWaitForLogEntry(t, "FRR", 30*time.Second) {
			t.Fatal("network setup did not complete")
		}
	}

	// Verify VRF device exists with table 10.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, err := prodDockerExecRaw(t, prodBootyContainer, "ip", "link", "show", "type", "vrf")
		if err == nil && strings.Contains(out, "Vrf_underlay") {
			t.Log("VRF Vrf_underlay created on booty-prod")
			// Verify table ID.
			detailOut, _ := prodDockerExecRaw(t, prodBootyContainer, "ip", "-d", "link", "show", "type", "vrf")
			if strings.Contains(detailOut, "table 10") {
				t.Log("VRF Vrf_underlay has correct routing table 10")
				return
			}
			t.Logf("VRF detail: %s", detailOut)
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("VRF Vrf_underlay not created or incorrect table ID")
}

// --- BGP peering tests ---

func TestProductionSpineBGPEstablished(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy peers with spine01 via eBGP unnumbered on eth1. The spine must
	// see the peer with ASN 65501 (BOOTy's local-as). A first session can
	// come up with the real ASN before the local-as applies and is then
	// reset, so poll until the session is Established with 65501.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	last := "no answer"
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodSpineContainer,
			"vtysh", "-c", "show bgp neighbors eth1 json")
		var neighbors map[string]struct {
			State    string `json:"bgpState"`
			RemoteAS uint32 `json:"remoteAs"`
		}
		// vtysh can print a warning before the JSON.
		if i := strings.Index(out, "{"); i > 0 {
			out = out[i:]
		}
		if err := json.Unmarshal([]byte(out), &neighbors); err != nil {
			last = fmt.Sprintf("no JSON: %v", err)
		} else {
			for name, n := range neighbors {
				last = fmt.Sprintf("%s: state %q remote AS %d", name, n.State, n.RemoteAS)
				if n.State == "Established" && n.RemoteAS == 65501 {
					t.Log("Spine sees BOOTy ESTABLISHED with local-as 65501")
					return
				}
			}
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatalf("Spine <-> BOOTy session not ESTABLISHED with remote AS 65501 (local-as) within %s: last %s", prodBGPConvergeTimeout, last)
}

func TestProductionDCGWBGPEstablished(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy peers with dcgw01 via iBGP (AS 65188) from underlay IP 10.50.0.140.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodDCGWContainer,
			"vtysh", "-c", "show bgp neighbors 10.50.0.140 json")
		if strings.Contains(out, "Established") {
			t.Log("DCGW ↔ BOOTy iBGP session ESTABLISHED")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("DCGW ↔ BOOTy iBGP session not ESTABLISHED")
}

func TestProductionSpineDCGWBGPEstablished(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// Spine ↔ DCGW eBGP session (65500 ↔ 65188) over 10.0.1.0/30.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodSpineContainer,
			"vtysh", "-c", "show bgp neighbors 10.0.1.2 json")
		if strings.Contains(out, "Established") {
			t.Log("Spine ↔ DCGW eBGP session ESTABLISHED")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Spine ↔ DCGW BGP session not ESTABLISHED")
}

// --- BFD tests ---

func TestProductionBFDActiveOnDCGW(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// Wait for BGP to establish first.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodDCGWContainer,
			"vtysh", "-c", "show bgp neighbors 10.50.0.140 json")
		if strings.Contains(out, "Established") {
			break
		}
		time.Sleep(prodBGPConvergeInterval)
	}

	// Verify BFD session is up on the DCGW toward BOOTy.
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodDCGWContainer, "vtysh", "-c", "show bfd peers")
		if strings.Contains(out, "10.50.0.140") && strings.Contains(out, "up") {
			t.Log("BFD session to BOOTy (10.50.0.140) is UP on DCGW")
			return
		}
		time.Sleep(2 * time.Second)
	}
	out, _ := prodDockerExecRaw(t, prodDCGWContainer, "vtysh", "-c", "show bfd peers")
	t.Fatalf("BFD session to BOOTy (10.50.0.140) not UP on DCGW after 30s:\n%s", out)
}

// --- EVPN tests ---

func TestProductionEVPNAddressFamilyOnSpine(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodSpineContainer,
			"vtysh", "-c", "show bgp l2vpn evpn summary")
		// Established peer should show a numeric PfxRcd, not "Active"/"Connect".
		if strings.Contains(out, "65501") &&
			!strings.Contains(out, "Active") &&
			!strings.Contains(out, "Connect") {
			t.Log("L2VPN-EVPN address family active on spine toward BOOTy")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("L2VPN-EVPN address family not established on spine")
}

func TestProductionEVPNAddressFamilyOnDCGW(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodDCGWContainer,
			"vtysh", "-c", "show bgp neighbors 10.50.0.140 json")
		if strings.Contains(out, "Established") &&
			strings.Contains(strings.ToLower(out), "l2vpnevpn") {
			t.Log("L2VPN-EVPN address family active on DCGW toward BOOTy")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("L2VPN-EVPN not active on DCGW toward BOOTy")
}

// --- VXLAN / overlay tests ---

func TestProductionVXLANInterfaceCreated(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// Wait for network setup.
	if !prodWaitForLogEntry(t, "FRR", prodBGPConvergeTimeout) {
		t.Log("Warning: FRR log not found, checking VXLAN anyway")
	}

	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodBootyContainer, "ip", "-d", "link", "show", "type", "vxlan")
		if strings.Contains(out, "id 2002002") {
			t.Log("VXLAN interface with VNI 2002002 present on booty-prod")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("VXLAN interface with VNI 2002002 not found on booty-prod")
}

func TestProductionProvisionBridgeIP(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodBootyContainer, "ip", "addr", "show", "dev", "br.provision")
		if strings.Contains(out, "10.100.0.20") {
			t.Log("Provision IP 10.100.0.20 assigned on br.provision")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Provision IP 10.100.0.20 not found on br.provision")
}

// --- Underlay route advertisement tests ---

func TestProductionUnderlayRouteOnSpine(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy should advertise its underlay IP (10.50.0.140/32) via the
	// eBGP unnumbered session to the spine.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodSpineContainer,
			"vtysh", "-c", "show ip route 10.50.0.140/32")
		if strings.Contains(out, "10.50.0.140") {
			t.Log("Underlay route 10.50.0.140/32 present on spine")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Underlay route 10.50.0.140/32 not learned on spine")
}

func TestProductionUnderlayRouteOnDCGW(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// DCGW should learn BOOTy's underlay via its iBGP session or spine.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodDCGWContainer,
			"vtysh", "-c", "show ip route 10.50.0.140/32")
		if strings.Contains(out, "10.50.0.140") {
			t.Log("Underlay route 10.50.0.140/32 present on DCGW")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Underlay route 10.50.0.140/32 not learned on DCGW")
}

// --- EVPN Type-5 route tests ---

func TestProductionEVPNType5OnSpine(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy should advertise its provision IP as an EVPN Type-5 route.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodSpineContainer,
			"vtysh", "-c", "show bgp l2vpn evpn")
		if strings.Contains(out, "10.100.0.20") {
			t.Log("EVPN Type-5 route for 10.100.0.20 visible on spine")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("EVPN Type-5 route for provision IP not visible on spine")
}

func TestProductionEVPNType5OnDCGW(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// DCGW should receive the EVPN Type-5 from BOOTy via iBGP.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodDCGWContainer,
			"vtysh", "-c", "show bgp l2vpn evpn")
		if strings.Contains(out, "10.100.0.20") {
			t.Log("EVPN Type-5 route for 10.100.0.20 visible on DCGW")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("EVPN Type-5 route for provision IP not visible on DCGW")
}

// --- End-to-end overlay connectivity tests ---

func TestProductionOverlayReachClient(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy should be able to reach the client (10.100.0.100) through
	// the VXLAN overlay via the spine's bridge.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, err := prodDockerExecRaw(t, prodBootyContainer,
			"ping", "-c", "1", "-W", "2", "-I", "br.provision", "10.100.0.100")
		if err == nil && strings.Contains(out, "1 packets received") {
			t.Log("Overlay connectivity to client 10.100.0.100 verified")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Cannot reach client 10.100.0.100 from booty-prod through overlay")
}

func TestProductionOverlayReachNginx(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy should reach nginx (10.100.0.10) through VXLAN overlay.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, err := prodDockerExecRaw(t, prodBootyContainer,
			"ping", "-c", "1", "-W", "2", "-I", "br.provision", "10.100.0.10")
		if err == nil && strings.Contains(out, "1 packets received") {
			t.Log("Overlay connectivity to nginx 10.100.0.10 verified")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Cannot reach nginx 10.100.0.10 from booty-prod through overlay")
}

func TestProductionOverlayReachCAPRF(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy should reach the CAPRF mock (10.100.0.11) through VXLAN overlay.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, err := prodDockerExecRaw(t, prodBootyContainer,
			"ping", "-c", "1", "-W", "2", "-I", "br.provision", "10.100.0.11")
		if err == nil && strings.Contains(out, "1 packets received") {
			t.Log("Overlay connectivity to CAPRF mock 10.100.0.11 verified")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Cannot reach CAPRF mock 10.100.0.11 from booty-prod through overlay")
}

// --- Gateway route and FDB tests ---

func TestProductionGatewayFDB(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy should install a BUM FDB entry for the provision_gateway (10.0.0.1).
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodBootyContainer, "bridge", "fdb", "show")
		if strings.Contains(out, "00:00:00:00:00:00") && strings.Contains(out, "10.0.0.1") {
			t.Log("Gateway BUM FDB entry present: 00:00:00:00:00:00 → 10.0.0.1")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Gateway BUM FDB entry for 10.0.0.1 not found")
}

func TestProductionGatewayRoute(t *testing.T) {
	requireProductionLab(t)
	t.Cleanup(func() { prodDumpDebugState(t) })

	// BOOTy should install a /32 route to the gateway VTEP via the first NIC.
	deadline := time.Now().Add(prodBGPConvergeTimeout)
	for time.Now().Before(deadline) {
		out, _ := prodDockerExecRaw(t, prodBootyContainer, "ip", "route", "show", "10.0.0.1/32")
		if strings.Contains(out, "10.0.0.1") {
			t.Log("Gateway route 10.0.0.1/32 present in kernel")
			return
		}
		time.Sleep(prodBGPConvergeInterval)
	}
	t.Fatal("Gateway /32 route to 10.0.0.1 not in kernel")
}
