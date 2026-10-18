//go:build e2e_vrnetlab

// EVPN E2E tests for BOOTy running as a real QEMU VM inside containerlab.
// Three BOOTy VMs (provision, deprovision, standby) boot with real PID 1 init,
// real mount/device setup, and communicate with the CAPRF mock through a full
// EVPN fabric (spine01 ↔ leaf01, VXLAN VNI 100, eBGP).
//
// Requires: topology-vrnetlab.clab.yml deployed via `make clab-vrnetlab-up`
package integration

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const (
	vrnetlabPrefix = "clab-booty-vrnetlab-lab"

	vmProvision   = vrnetlabPrefix + "-booty-provision"
	vmDeprovision = vrnetlabPrefix + "-booty-deprovision"
	vmStandby     = vrnetlabPrefix + "-booty-standby"
	vmCAPRF       = vrnetlabPrefix + "-caprf-mock"
	vmSpine       = vrnetlabPrefix + "-spine01"
	vmLeaf        = vrnetlabPrefix + "-leaf01"
	vmClient      = vrnetlabPrefix + "-client"
	vmNginx       = vrnetlabPrefix + "-nginx"
)

// ─── Helpers ──────────────────────────────────────────────────────────

func requireVrnetlabLab(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("docker not available: %v", err)
	}
	if !strings.Contains(string(out), vmProvision) {
		t.Fatal("vrnetlab topology not deployed (" + vmProvision + " not found)")
	}
}

func vmDockerExec(t *testing.T, container string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return vmDockerExecContext(t, ctx, container, args...)
}

func vmDockerExecContext(t *testing.T, ctx context.Context, container string, args ...string) (string, error) {
	t.Helper()
	cmdArgs := append([]string{"exec", container}, args...)
	out, err := exec.CommandContext(ctx, "docker", cmdArgs...).CombinedOutput()
	return string(out), err
}

func vmDockerExecOrFail(t *testing.T, container string, args ...string) string {
	t.Helper()
	out, err := vmDockerExec(t, container, args...)
	if err != nil {
		t.Fatalf("docker exec %s %v failed: %v\n%s", container, args, err, out)
	}
	return out
}

// getVMSerialLog retrieves QEMU serial console output from docker logs.
func getVMSerialLog(t *testing.T, container string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "logs", container).CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Logf("timeout getting logs for %s", container)
			return string(out)
		}
		t.Logf("could not get logs for %s: %v", container, err)
		return ""
	}
	return string(out)
}

// waitForVMLog polls docker logs until entry appears or timeout.
func waitForVMLog(t *testing.T, container, entry string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		logs := getVMSerialLog(t, container)
		if strings.Contains(logs, entry) {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// waitForVMLogAny polls docker logs until any entry appears or timeout.
func waitForVMLogAny(t *testing.T, container string, entries []string, timeout time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var logs string
	for time.Now().Before(deadline) {
		logs = getVMSerialLog(t, container)
		for _, entry := range entries {
			if strings.Contains(logs, entry) {
				return logs, true
			}
		}
		time.Sleep(3 * time.Second)
	}
	return logs, false
}

// waitForVMAccessLog polls a container's file until it contains the expected string.
func waitForVMAccessLog(t *testing.T, container, logPath, entry string, timeout time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	for time.Now().Before(deadline) {
		out, err := vmDockerExec(t, container, "cat", logPath)
		if err == nil && strings.Contains(out, entry) {
			return out, true
		}
		lastOut = out
		time.Sleep(3 * time.Second)
	}
	return lastOut, false
}

// ═══════════════════════════════════════════════════════════════════════
// EVPN Fabric Validation
// ═══════════════════════════════════════════════════════════════════════

func TestVrnetlabBGPSessionsEstablished(t *testing.T) {
	requireVrnetlabLab(t)

	// Use JSON output for precise peer state checking. The text-format
	// "never" check fails when ANY peer hasn't connected (e.g. due to a
	// ContainerLab race on one VM), even though the fabric is functional.
	// Require at least 2 Established peers (leaf01 + at least one BOOTy VM).
	var established bool
	for i := 0; i < 300; i++ {
		out, err := vmDockerExec(t, vmSpine, "vtysh", "-c", "show bgp summary json")
		if err == nil {
			estabCount := strings.Count(out, "\"Established\"")
			if estabCount >= 2 {
				established = true
				t.Logf("BGP established (%d peers):\n%s", estabCount, out)
				break
			}
		}
		time.Sleep(1 * time.Second)
	}
	if !established {
		out, _ := vmDockerExec(t, vmSpine, "vtysh", "-c", "show bgp summary json")
		nbr, _ := vmDockerExec(t, vmSpine, "vtysh", "-c", "show bgp neighbor json")
		bfd, _ := vmDockerExec(t, vmSpine, "vtysh", "-c", "show bfd peers json")
		vmLogs := getVMSerialLog(t, vmProvision)
		vmTail := ""
		if len(vmLogs) > 2000 {
			vmTail = vmLogs[len(vmLogs)-2000:]
		} else {
			vmTail = vmLogs
		}
		t.Fatalf("BGP sessions not established on spine01 (need ≥2 Established)\nSummary JSON:\n%s\nNeighbor JSON:\n%s\nBFD JSON:\n%s\nVM serial (tail):\n%s", out, nbr, bfd, vmTail)
	}
}

func TestVrnetlabEVPNRoutesPresent(t *testing.T) {
	requireVrnetlabLab(t)

	out := waitForVrnetlabEVPNRoutes(t)
	t.Logf("EVPN routes present on spine01:\n%s", out)
}

func waitForVrnetlabEVPNRoutes(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var last string
	var lastErr error
	for {
		out, err := vmDockerExecContext(t, ctx, vmSpine, "vtysh", "-c", "show bgp l2vpn evpn")
		last = out
		lastErr = err
		if err == nil && (strings.Contains(out, "Route Distinguisher") || strings.Contains(out, "Network")) {
			return out
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				t.Fatalf("no EVPN routes found on spine01 after 60s; last docker exec error: %v\n%s", lastErr, last)
			}
			t.Fatalf("no EVPN routes found on spine01 after 60s\n%s", last)
		case <-ticker.C:
		}
	}
}

func TestVrnetlabLeafBGPEstablished(t *testing.T) {
	requireVrnetlabLab(t)

	var established bool
	for i := 0; i < 60; i++ {
		out, err := vmDockerExec(t, vmLeaf, "vtysh", "-c", "show bgp summary json")
		if err == nil && strings.Contains(out, "\"Established\"") {
			established = true
			t.Logf("Leaf BGP established:\n%s", out)
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !established {
		out, _ := vmDockerExec(t, vmLeaf, "vtysh", "-c", "show bgp summary json")
		t.Fatalf("BGP sessions not established on leaf01\n%s", out)
	}
}

func TestVrnetlabClientReachesNginxThroughEVPN(t *testing.T) {
	requireVrnetlabLab(t)

	var reachable bool
	for i := 0; i < 30; i++ {
		out, err := vmDockerExec(t, vmClient, "ping", "-c1", "-W1", "10.100.0.10")
		if err == nil && strings.Contains(out, "1 packets received") {
			reachable = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !reachable {
		t.Fatal("client cannot reach nginx (10.100.0.10) through EVPN fabric")
	}
	t.Log("client → nginx reachable through EVPN fabric")
}

func TestVrnetlabClientReachesCAPRFThroughEVPN(t *testing.T) {
	requireVrnetlabLab(t)

	var reachable bool
	for i := 0; i < 30; i++ {
		out, err := vmDockerExec(t, vmClient, "ping", "-c1", "-W1", "10.100.0.11")
		if err == nil && strings.Contains(out, "1 packets received") {
			reachable = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !reachable {
		t.Fatal("client cannot reach CAPRF mock (10.100.0.11) through EVPN fabric")
	}
	t.Log("client → CAPRF mock reachable through EVPN fabric")
}

// ═══════════════════════════════════════════════════════════════════════
// QEMU VM Boot Lifecycle
// ═══════════════════════════════════════════════════════════════════════

func TestVrnetlabVMBootStarted(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmProvision, "starting BOOTy", 120*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not start BOOTy within 120s\nSerial log:\n%s", logs)
	}
	t.Log("provision VM: BOOTy started as PID 1")
}

func TestVrnetlabVMMountsSuccessful(t *testing.T) {
	requireVrnetlabLab(t)

	// BOOTy logs "beginning provisioning process" after setupMountsAndDevices() and loadModules() complete
	if !waitForVMLog(t, vmProvision, "beginning provisioning process", 120*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not reach provisioning stage (mounts may have failed)\n%s", logs)
	}
	t.Log("provision VM: mount and device setup completed")
}

func TestVrnetlabVMCAPRFModeDetected(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmProvision, "CAPRF mode active", 150*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not enter CAPRF mode\n%s", logs)
	}
	t.Log("provision VM: CAPRF mode detected from /deploy/vars")
}

func TestVrnetlabVMFRRNetworkModeDetected(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmProvision, "using FRR/EVPN network mode", 150*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not enter FRR/EVPN network mode\n%s", logs)
	}
	t.Log("provision VM: FRR/EVPN network mode active")
}

func TestVrnetlabVMNetworkConnectivity(t *testing.T) {
	requireVrnetlabLab(t)

	// BOOTy logs "waiting for network connectivity" before polling
	if !waitForVMLog(t, vmProvision, "waiting for network connectivity", 150*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not reach connectivity check\n%s", logs)
	}
	t.Log("provision VM: network connectivity check initiated")
}

func TestVrnetlabVMProvisionReportsInit(t *testing.T) {
	requireVrnetlabLab(t)

	// report-init means: boot → mounts → DHCP goroutine → /deploy/vars → CAPRF →
	// FRR/EVPN network mode → connectivity OK → CAPRF init report
	if !waitForVMLog(t, vmProvision, "report-init", 180*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not reach report-init\n%s", logs)
	}
	t.Log("provision VM: report-init reached — full CAPRF lifecycle working through EVPN")
}

// ═══════════════════════════════════════════════════════════════════════
// EVPN: VM → CAPRF Mock Communication
// ═══════════════════════════════════════════════════════════════════════

func TestVrnetlabCAPRFMockReceivedInit(t *testing.T) {
	requireVrnetlabLab(t)

	out, ok := waitForVMAccessLog(t, vmCAPRF, "/var/log/nginx/access.log", "/status/init", 120*time.Second)
	if !ok {
		t.Logf("CAPRF access log:\n%s", out)
		t.Fatal("CAPRF mock did not receive /status/init — VM→EVPN→CAPRF path broken")
	}
	t.Logf("CAPRF mock received init from BOOTy VM:\n%s", out)
}

func TestVrnetlabCAPRFMockReceivedHeartbeat(t *testing.T) {
	requireVrnetlabLab(t)

	out, ok := waitForVMAccessLog(t, vmCAPRF, "/var/log/nginx/access.log", "/status/heartbeat", 120*time.Second)
	if !ok {
		t.Logf("CAPRF access log:\n%s", out)
		t.Fatal("CAPRF mock did not receive heartbeat from standby VM through EVPN")
	}
	t.Log("CAPRF mock received heartbeat from standby VM through EVPN")
}

// ═══════════════════════════════════════════════════════════════════════// Image Pull Through EVPN
// ═════════════════════════════════════════════════════════════════════

func TestVrnetlabProvisionAttemptsImageDownload(t *testing.T) {
	requireVrnetlabLab(t)

	// Wait for provisioning to start and attempt image streaming
	if !waitForVMLog(t, vmProvision, "report-init", 180*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not reach report-init\n%s", logs)
	}

	if logs, ok := waitForVMLogAny(t, vmProvision, []string{
		"Streaming image",
		"Beginning write",
		"stream-image",
	}, 120*time.Second); !ok {
		t.Logf("Serial log:\n%s", logs)
		t.Fatal("provision VM did not attempt image download through EVPN")
	}
	// The format probe read the image over EVPN, and the stream wrote it.
	for _, marker := range []string{"image prerequisites validated", `msg="image written"`} {
		if !waitForVMLog(t, vmProvision, marker, 60*time.Second) {
			t.Fatalf("provision VM log lacks %q: the image did not come over EVPN\n%s", marker, getVMSerialLog(t, vmProvision))
		}
	}
	t.Log("provision VM: image download attempted through EVPN")
}

func TestVrnetlabNginxReceivedImageRequest(t *testing.T) {
	requireVrnetlabLab(t)

	out, ok := waitForVMAccessLog(t, vmNginx, "/var/log/nginx/access.log", "/images/test.img", 120*time.Second)
	if !ok {
		t.Logf("Nginx access log:\n%s", out)
		t.Fatal("nginx did not receive /images/test.img request from BOOTy VM through EVPN")
	}
	t.Logf("Nginx received image request from BOOTy VM through EVPN:\n%s", out)
}

func TestVrnetlabCAPRFMockReceivedErrorFromProvision(t *testing.T) {
	requireVrnetlabLab(t)

	out, ok := waitForVMAccessLog(t, vmCAPRF, "/var/log/nginx/access.log", "/status/error", 180*time.Second)
	if !ok {
		serial := getVMSerialLog(t, vmProvision)
		if strings.Contains(serial, "insecure transport") ||
			strings.Contains(serial, "refusing request to non-HTTPS endpoint") ||
			strings.Contains(serial, "skipping bearer token on non-HTTPS request") {
			t.Logf("CAPRF access log:\n%s", out)
			t.Log("CAPRF /status/error not posted because non-HTTPS bearer transport was intentionally blocked")
			return
		}
		t.Logf("CAPRF access log:\n%s", out)
		t.Logf("Provision VM serial log:\n%s", serial)
		t.Fatal("CAPRF mock did not receive /status/error and no secure-transport refusal was observed")
	}
	t.Log("CAPRF mock received /status/error (full CAPRF error lifecycle through EVPN)")
}

// ═════════════════════════════════════════════════════════════════════
// Full Lifecycle Through EVPN (per mode)
// ═════════════════════════════════════════════════════════════════════

func TestVrnetlabProvisionFullLifecycleViaEVPN(t *testing.T) {
	requireVrnetlabLab(t)

	steps := []struct {
		entry   string
		timeout time.Duration
		desc    string
	}{
		{"starting BOOTy", 120 * time.Second, "BOOTy started as PID 1"},
		{"CAPRF mode active", 60 * time.Second, "CAPRF mode detected from /deploy/vars"},
		{"using FRR/EVPN network mode", 30 * time.Second, "FRR/EVPN network mode active"},
		{"report-init", 60 * time.Second, "init reported to CAPRF through EVPN"},
	}

	for _, step := range steps {
		if !waitForVMLog(t, vmProvision, step.entry, step.timeout) {
			logs := getVMSerialLog(t, vmProvision)
			t.Fatalf("provision VM did not reach: %s\n%s", step.desc, logs)
		}
		t.Logf("provision VM: %s", step.desc)
	}

	// Verify provisioning continues past report-init.
	if logs, ok := waitForVMLogAny(t, vmProvision, []string{
		"step=collect-inventory",
		"step=health-checks",
		"step=stop-raid",
		"step=detect-disk",
	}, 120*time.Second); !ok {
		t.Fatalf("provision VM did not execute provisioning steps after report-init\n%s", logs)
	}
	t.Log("provision VM: provisioning orchestrator executing steps through EVPN")

	// Verify CAPRF mock received the init POST through EVPN
	access, ok := waitForVMAccessLog(t, vmCAPRF, "/var/log/nginx/access.log", "/status/init", 120*time.Second)
	if !ok {
		t.Fatalf("provision VM: CAPRF init status missing from access log\n%s", access)
	}
	t.Log("provision VM: CAPRF init status received through EVPN")
}

func TestVrnetlabDeprovisionFullLifecycleViaEVPN(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmDeprovision, "starting BOOTy", 120*time.Second) {
		logs := getVMSerialLog(t, vmDeprovision)
		t.Fatalf("deprovision VM did not start BOOTy\n%s", logs)
	}
	t.Log("deprovision VM: BOOTy started as PID 1")

	if !waitForVMLog(t, vmDeprovision, "CAPRF mode active", 60*time.Second) {
		logs := getVMSerialLog(t, vmDeprovision)
		t.Fatalf("deprovision VM did not enter CAPRF mode\n%s", logs)
	}
	t.Log("deprovision VM: CAPRF mode active")

	if !waitForVMLog(t, vmDeprovision, "using FRR/EVPN network mode", 60*time.Second) {
		logs := getVMSerialLog(t, vmDeprovision)
		t.Fatalf("deprovision VM did not enter FRR/EVPN mode\n%s", logs)
	}
	t.Log("deprovision VM: FRR/EVPN network mode active")

	if logs, ok := waitForVMLogAny(t, vmDeprovision, []string{
		"Deprovisioning step",
		"starting deprovisioning",
		"report-init",
	}, 90*time.Second); !ok {
		t.Fatalf("deprovision VM did not execute deprovisioning lifecycle\n%s", logs)
	}
	t.Log("deprovision VM: deprovisioning lifecycle executing through EVPN")
}

func TestVrnetlabStandbyFullLifecycleViaEVPN(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmStandby, "starting BOOTy", 120*time.Second) {
		logs := getVMSerialLog(t, vmStandby)
		t.Fatalf("standby VM did not start BOOTy\n%s", logs)
	}
	t.Log("standby VM: BOOTy started as PID 1")

	if !waitForVMLog(t, vmStandby, "CAPRF mode active", 60*time.Second) {
		logs := getVMSerialLog(t, vmStandby)
		t.Fatalf("standby VM did not enter CAPRF mode\n%s", logs)
	}
	t.Log("standby VM: CAPRF mode active")

	if !waitForVMLog(t, vmStandby, "using FRR/EVPN network mode", 60*time.Second) {
		logs := getVMSerialLog(t, vmStandby)
		t.Fatalf("standby VM did not enter FRR/EVPN mode\n%s", logs)
	}
	t.Log("standby VM: FRR/EVPN network mode active")

	if !waitForVMLog(t, vmStandby, "standby", 60*time.Second) {
		logs := getVMSerialLog(t, vmStandby)
		t.Fatalf("standby VM did not enter standby mode\n%s", logs)
	}
	t.Log("standby VM: entered standby mode")

	// Verify heartbeat was sent to CAPRF through EVPN.
	access, ok := waitForVMAccessLog(t, vmCAPRF, "/var/log/nginx/access.log", "/status/heartbeat", 120*time.Second)
	if !ok {
		logs := getVMSerialLog(t, vmStandby)
		t.Fatalf("standby VM: heartbeat missing from CAPRF access log\nAccess log:\n%s\nSerial log:\n%s", access, logs)
	}
	t.Log("standby VM: heartbeat sent to CAPRF through EVPN")
}

// ═════════════════════════════════════════════════════════════════════// Multi-mode Validation
// ═══════════════════════════════════════════════════════════════════════

func TestVrnetlabProvisionMode(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmProvision, "CAPRF mode active", 150*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM did not enter CAPRF mode\n%s", logs)
	}

	logs := getVMSerialLog(t, vmProvision)
	if !strings.Contains(logs, "mode=provision") && !strings.Contains(logs, "\"mode\":\"provision\"") {
		// mode might be logged differently; check for provision-related activity
		if !strings.Contains(logs, "booty-provision-e2e") {
			t.Logf("Serial log:\n%s", logs)
			t.Fatal("provision VM not in provision mode")
		}
	}
	t.Log("provision VM: mode=provision confirmed")
}

func TestVrnetlabDeprovisionMode(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmDeprovision, "CAPRF mode active", 150*time.Second) {
		logs := getVMSerialLog(t, vmDeprovision)
		t.Fatalf("deprovision VM did not enter CAPRF mode\n%s", logs)
	}

	if !waitForVMLog(t, vmDeprovision, "deprovision", 30*time.Second) {
		logs := getVMSerialLog(t, vmDeprovision)
		t.Fatalf("deprovision VM not in deprovision mode\n%s", logs)
	}
	t.Log("deprovision VM: mode=deprovision confirmed")
}

func TestVrnetlabStandbyMode(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmStandby, "CAPRF mode active", 150*time.Second) {
		logs := getVMSerialLog(t, vmStandby)
		t.Fatalf("standby VM did not enter CAPRF mode\n%s", logs)
	}

	if !waitForVMLog(t, vmStandby, "standby", 30*time.Second) {
		logs := getVMSerialLog(t, vmStandby)
		t.Fatalf("standby VM not in standby mode\n%s", logs)
	}
	t.Log("standby VM: mode=standby confirmed")
}

func TestVrnetlabProvisionShowsHostname(t *testing.T) {
	requireVrnetlabLab(t)

	if !waitForVMLog(t, vmProvision, "booty-provision-e2e", 150*time.Second) {
		logs := getVMSerialLog(t, vmProvision)
		t.Fatalf("provision VM logs missing hostname\n%s", logs)
	}
	t.Log("provision VM: hostname appears in serial output")
}

func TestVrnetlabAllVMsBootSuccessfully(t *testing.T) {
	requireVrnetlabLab(t)

	vms := []struct {
		container string
		desc      string
	}{
		{vmProvision, "provision"},
		{vmDeprovision, "deprovision"},
		{vmStandby, "standby"},
	}

	for _, vm := range vms {
		vm := vm
		t.Run(vm.desc, func(t *testing.T) {
			t.Parallel()
			if !waitForVMLog(t, vm.container, "starting BOOTy", 120*time.Second) {
				logs := getVMSerialLog(t, vm.container)
				t.Fatalf("%s VM did not start BOOTy\n%s", vm.desc, logs)
			}
			t.Logf("%s VM: BOOTy started", vm.desc)
		})
	}
}

func TestVrnetlabAllVMsEnterCAPRF(t *testing.T) {
	requireVrnetlabLab(t)

	vms := []struct {
		container string
		desc      string
	}{
		{vmProvision, "provision"},
		{vmDeprovision, "deprovision"},
		{vmStandby, "standby"},
	}

	for _, vm := range vms {
		vm := vm
		t.Run(vm.desc, func(t *testing.T) {
			t.Parallel()
			if !waitForVMLog(t, vm.container, "CAPRF mode active", 150*time.Second) {
				logs := getVMSerialLog(t, vm.container)
				t.Fatalf("%s VM did not enter CAPRF mode\n%s", vm.desc, logs)
			}
			t.Logf("%s VM: CAPRF mode active", vm.desc)
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Initramfs Module Verification
// ═══════════════════════════════════════════════════════════════════════

// vrnetlabRequiredModules lists kernel modules that must be loaded for a
// successful boot. If the Dockerfile module copy loop breaks (e.g. shell
// comment parsing), this test catches it before a real deploy. virtio_pci is
// not listed: the Debian 6.12 kernel builds it in (CONFIG_VIRTIO_PCI=y).
var vrnetlabRequiredModules = []string{
	"ext4",          // root filesystem
	"xfs",           // common data filesystem
	"fat",           // FAT core used by vfat
	"vfat",          // EFI system partition
	"nls_cp437",     // default FAT codepage support
	"nls_iso8859-1", // default FAT iocharset support
	"scsi_mod",      // SCSI subsystem
	"sd_mod",        // SCSI disk driver
	"virtio_blk",    // virtio block storage (QEMU)
	"virtio_scsi",   // virtio SCSI controller
	"virtio_net",    // virtio NIC
	"vxlan",         // VXLAN overlay
}

func TestVrnetlabModulesLoaded(t *testing.T) {
	requireVrnetlabLab(t)

	logs := getVMSerialLog(t, vmProvision)

	if logs == "" {
		t.Fatalf("serial log for %s is empty; cannot verify required kernel modules (check docker logs / VM state)", vmProvision)
	}

	const provisioningMarker = "beginning provisioning"
	if !strings.Contains(logs, provisioningMarker) {
		t.Fatalf("serial log for %s does not contain %q; cannot verify required kernel modules\n%s",
			vmProvision, provisioningMarker, logs)
	}

	for _, mod := range vrnetlabRequiredModules {
		// loadModules logs: "loaded kernel module" with a "module=<name>.ko*" field.
		field := "module=" + mod + ".ko"
		if !strings.Contains(logs, "loaded kernel module") || !strings.Contains(logs, field) {
			t.Errorf("module %q not loaded according to serial log — expected log entry with 'loaded kernel module' and %q", mod, field)
		}
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Unexpected ERROR Detection
// ═══════════════════════════════════════════════════════════════════════

// vrnetlabAllowedErrorWrappers and vrnetlabAllowedErrorRootCauses list error
// messages expected in CI (no real disk, etc.).
// Debug dumps (DumpDebugState, DumpPATH, dumpConfig) log at WARN level and
// are invisible to this check — only genuine ERROR-level messages remain.
var vrnetlabAllowedErrorWrappers = []string{
	// Top-level provisioning/deprovisioning failure.
	"provisioning failed",
	"deprovisioning failed",
	// Individual step failures.
	"provisioning step failed",
	"Deprovisioning step failed",
}

var vrnetlabAllowedErrorRootCauses = []string{
	// Expected in CI without real disks or network.
	"no suitable disk found",
	"stop raid arrays",
	"Connectivity timeout",
	"Connecting to provisioning server",
	"network connectivity timeout",
	// Expected when CAPRF endpoints are HTTP-only and token auth is enforced.
	"failed to report error status",
	"insecure transport",
	// The vrnetlab ramdisk has no cloud-guest-utils: the provision ends in
	// grow-partition, after the image came over EVPN and was written
	// (TestVrnetlabProvisionAttemptsImageDownload requires both).
	`exec: "growpart": executable file not found`,
}

var vrnetlabDeniedErrorRootCauses = []string{
	`exec mdadm: exec: "mdadm": executable file not found`,
}

func vrnetlabAllowedErrorLine(line string) bool {
	lineLower := strings.ToLower(line)
	lineLower = strings.ReplaceAll(lineLower, `\"`, `"`)
	if strings.Contains(lineLower, `msg="mode exited with error"`) {
		return vrnetlabAllowedErrorRootCause(lineLower)
	}

	for _, pattern := range vrnetlabAllowedErrorWrappers {
		if strings.Contains(lineLower, strings.ToLower(pattern)) {
			return true
		}
	}
	return vrnetlabAllowedErrorRootCause(lineLower)
}

func vrnetlabAllowedErrorRootCause(lineLower string) bool {
	for _, pattern := range vrnetlabDeniedErrorRootCauses {
		if strings.Contains(lineLower, strings.ToLower(pattern)) {
			return false
		}
	}
	for _, pattern := range vrnetlabAllowedErrorRootCauses {
		if strings.Contains(lineLower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

func TestVrnetlabNoUnexpectedErrors(t *testing.T) {
	requireVrnetlabLab(t)

	time.Sleep(15 * time.Second)

	vms := []struct {
		name string
		desc string
	}{
		{vmProvision, "provision"},
		{vmDeprovision, "deprovision"},
		{vmStandby, "standby"},
	}

	for _, vm := range vms {
		logs := getVMSerialLog(t, vm.name)
		for _, line := range strings.Split(logs, "\n") {
			if !strings.Contains(line, "level=ERROR") {
				continue
			}
			if !vrnetlabAllowedErrorLine(line) {
				t.Errorf("%s: unexpected ERROR log: %s", vm.desc, line)
			}
		}
	}
}

func TestVrnetlabAllowedErrorLineRequiresExpectedModeFailureCause(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "direct allowed provisioning failure",
			line: `level=ERROR msg="provisioning failed" error="no suitable disk found"`,
			want: true,
		},
		{
			name: "mode exit allowed with wrapped expected cause",
			line: `level=ERROR msg="mode exited with error" error="provisioning failed: no suitable disk found"`,
			want: true,
		},
		{
			name: "mode exit rejects missing mdadm",
			line: `level=ERROR msg="mode exited with error" mode=deprovision error="deprovision step stop-raid: stop raid arrays: exec mdadm: exec: \"mdadm\": executable file not found in $PATH"`,
			want: false,
		},
		{
			name: "mode exit rejected with generic wrapper and unexpected cause",
			line: `level=ERROR msg="mode exited with error" error="provisioning failed: unexpected storage corruption"`,
			want: false,
		},
		{
			name: "mode exit rejected without expected cause",
			line: `level=ERROR msg="mode exited with error" error="unexpected storage corruption"`,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vrnetlabAllowedErrorLine(tc.line); got != tc.want {
				t.Fatalf("vrnetlabAllowedErrorLine() = %t, want %t", got, tc.want)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Full Log Dump (always runs last for debugging)
// ═══════════════════════════════════════════════════════════════════════

func TestVrnetlabZZZDumpAllLogs(t *testing.T) {
	requireVrnetlabLab(t)

	time.Sleep(10 * time.Second)

	vms := []struct {
		name string
		desc string
	}{
		{vmProvision, "PROVISION"},
		{vmDeprovision, "DEPROVISION"},
		{vmStandby, "STANDBY"},
	}

	for _, vm := range vms {
		logs := getVMSerialLog(t, vm.name)
		t.Logf("\n"+
			"════════════════════════════════════════\n"+
			"  %s VM SERIAL LOG\n"+
			"════════════════════════════════════════\n"+
			"%s\n"+
			"════════════════════════════════════════",
			vm.desc, logs)
	}

	// CAPRF access log
	accessLog, _ := vmDockerExec(t, vmCAPRF, "cat", "/var/log/nginx/access.log")
	t.Logf("\n"+
		"════════════════════════════════════════\n"+
		"  CAPRF MOCK ACCESS LOG\n"+
		"════════════════════════════════════════\n"+
		"%s\n"+
		"════════════════════════════════════════",
		accessLog)

	// BGP and EVPN state
	bgp := vmDockerExecOrFail(t, vmSpine, "vtysh", "-c", "show bgp summary")
	t.Logf("\nBGP Summary (spine01):\n%s", bgp)

	evpn, _ := vmDockerExec(t, vmSpine, "vtysh", "-c", "show bgp l2vpn evpn")
	t.Logf("\nEVPN State (spine01):\n%s", evpn)

	leafBgp, _ := vmDockerExec(t, vmLeaf, "vtysh", "-c", "show bgp summary")
	t.Logf("\nBGP Summary (leaf01):\n%s", leafBgp)

	fmt.Println("vrnetlab EVPN E2E: all logs dumped")
}
