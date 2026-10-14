//go:build e2e

package kvm

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file run BOOTy in standby (agent) mode against the
// fake CAPRF agent endpoints: GET /agent/commands, POST
// /agent/commands/ack and POST /agent/heartbeat. See
// docs/caprf-integration.md#agent-mode-contract.

// agentCommand is one queued command. Payload is base64 in JSON, like a
// []byte field of the CAPRF AgentCommand.
type agentCommand struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Payload []byte `json:"payload,omitempty"`
}

type agentAck struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

func (f *fakeCAPRF) agentRoutes(mux *http.ServeMux) {
	authorized := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+fakeCAPRFToken {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("GET /agent/commands", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		f.mu.Lock()
		cmds := f.commands
		f.commands = nil
		f.mu.Unlock()
		if len(cmds) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cmds)
	})
	mux.HandleFunc("POST /agent/commands/ack", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		var ack agentAck
		if err := json.NewDecoder(r.Body).Decode(&ack); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.acks = append(f.acks, ack)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(w, r) {
			return
		}
		f.mu.Lock()
		f.heartbeats++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
}

func (f *fakeCAPRF) queueCommand(cmd agentCommand) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmd)
}

// standbyVars returns native standby vars with the agent endpoints.
func (f *fakeCAPRF) standbyVars() map[string]string {
	vars := f.nativeVars("standby")
	vars["HEARTBEAT_URL"] = f.baseURL + "/agent/heartbeat"
	vars["COMMANDS_URL"] = f.baseURL + "/agent/commands"
	vars["IMAGE"] = f.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["PROVISION_TARGET_OS"] = "linux"
	// Power off after the provision command, so the test can read the disk.
	vars["SUCCESS_ACTION"] = "poweroff"
	return vars
}

func (f *fakeCAPRF) ackList() []agentAck {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentAck(nil), f.acks...)
}

// Lab finding (TCAAS-862): a standby provision command ignored its payload
// and provisioned with HOSTNAME and EXTRA_KERNEL_PARAMS of /deploy/vars.
// The payload must win for that command.
func TestStandbyProvisionPayloadWinsQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))
	targetDisk := filepath.Join(t.TempDir(), "standby-payload.qcow2")
	run(t, "create target disk", "qemu-img", "create", "-f", "qcow2", targetDisk, "2G")

	vars := caprf.standbyVars()
	vars["HOSTNAME"] = "t1"
	vars["EXTRA_KERNEL_PARAMS"] = "fromvars=t1"
	caprf.queueCommand(agentCommand{
		ID: "p1", Type: "provision",
		Payload: []byte("HOSTNAME=t1-payload\nEXTRA_KERNEL_PARAMS=frompayload=t1\n"),
	})

	output := runQEMUProvision(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 6*time.Minute)
	t.Logf("standby payload output tail:\n%s", tail(output, 3000))
	assertProvisionSucceeded(t, output)
	requirePowerOff(t, output)

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()
	hostname := strings.TrimSpace(readProvisionedFile(t, rootMount, "etc/hostname"))
	grubCfg := readProvisionedFile(t, rootMount, "etc/default/grub.d/10-caprf-kernel-params.cfg")
	if hostname != "t1-payload" || !strings.Contains(grubCfg, "frompayload=t1") || strings.Contains(grubCfg, "fromvars=t1") {
		t.Fatalf("/etc/hostname = %q, kernel params drop-in = %q; want the payload values t1-payload and frompayload=t1", hostname, grubCfg)
	}
	acks := caprf.ackList()
	if len(acks) != 1 || acks[0].ID != "p1" || acks[0].Status != "completed" ||
		!strings.Contains(acks[0].Message, "EXTRA_KERNEL_PARAMS,HOSTNAME") {
		t.Fatalf("acks = %+v, want p1 completed with the payload keys", acks)
	}
}

// With STANDBY_DEPROVISION_SUCCESS_ACTION=standby a deprovision command
// returns to standby in the same boot: heartbeats go on, and a later
// provision command succeeds without a reboot or power cycle.
func TestStandbyDeprovisionReturnsToStandbyQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireDiskInspectTools(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	caprf := startFakeCAPRF(t, compressGzip(t, rawDisk))
	// Start from a disk with a partition table and file systems.
	targetDisk := filepath.Join(t.TempDir(), "standby-deprovision.qcow2")
	run(t, "convert disk to qcow2", "qemu-img", "convert", "-O", "qcow2", createTestDiskImage(t, 256), targetDisk)
	run(t, "grow target disk", "qemu-img", "resize", targetDisk, "2G")

	vars := caprf.standbyVars()
	vars["HOSTNAME"] = "standby-redeploy"
	vars["STANDBY_DEPROVISION_SUCCESS_ACTION"] = "standby"
	caprf.queueCommand(agentCommand{ID: "d1", Type: "deprovision"})

	// Queue the provision command only after the deprovision ACK and a
	// later heartbeat, so it can only run when BOOTy is back in standby.
	queued := make(chan int, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Minute)
		beatsAtAck := -1
		for time.Now().Before(deadline) {
			caprf.mu.Lock()
			acked, beats := len(caprf.acks) > 0, caprf.heartbeats
			caprf.mu.Unlock()
			if acked && beatsAtAck < 0 {
				beatsAtAck = beats
			}
			if acked && beats > beatsAtAck {
				caprf.queueCommand(agentCommand{ID: "p1", Type: "provision"})
				queued <- beats
				return
			}
			<-time.After(time.Second)
		}
		close(queued)
	}()

	output := runQEMUProvision(t, findKernel(t), buildProvisionInitramfs(t, vars), targetDisk, 9*time.Minute)
	t.Logf("standby deprovision output tail:\n%s", tail(output, 4000))
	if _, ok := <-queued; !ok {
		t.Fatalf("no heartbeat after the deprovision ACK, BOOTy did not return to standby. acks %+v", caprf.ackList())
	}
	assertProvisionSucceeded(t, output)
	requirePowerOff(t, output)
	if n := strings.Count(string(output), "Linux version "); n != 1 {
		t.Fatalf("guest booted %d times, want one boot for deprovision and provision", n)
	}
	acks := caprf.ackList()
	if len(acks) != 2 || acks[0].ID != "d1" || acks[0].Status != "completed" || acks[1].ID != "p1" || acks[1].Status != "completed" {
		t.Fatalf("acks = %+v, want d1 and p1 completed", acks)
	}
	caprf.requireStatuses(t, "init", "init", "success", "init", "init", "success")

	rootMount, cleanup := mountQcow2(t, targetDisk)
	defer cleanup()
	if got := strings.TrimSpace(readProvisionedFile(t, rootMount, "etc/hostname")); got != "standby-redeploy" {
		t.Fatalf("/etc/hostname = %q, want the provisioned hostname", got)
	}
}
