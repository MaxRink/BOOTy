//go:build e2e

package kvm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAgentCycleTimingQEMU measures one deprovision and provision cycle of
// a reused host twice, and writes the phase durations as a cycle report:
//
//   - cold: the CAPRF flow today. A deprovision VM boots the ISO, wipes
//     and powers off, then a provision VM boots the ISO again, streams the
//     image and powers off.
//   - agent: one VM boots the ISO in standby mode. The test ejects the CD
//     after the first heartbeat (BOOTy runs from RAM), then queues a
//     deprovision command and, at its ACK, a provision command. No reboot
//     between the commands.
//
// Both boot a CAPRF-style ISO (ISOLINUX, deploy vars in the initramfs) on
// q35 with SeaBIOS, so the boot phase includes firmware and the ISO read.
// BOOTY_CYCLE_VMEDIA_BPS throttles the CD reads to model a BMC virtual
// media link. See docs/agent-fast-cycle.md.
func TestAgentCycleTimingQEMU(t *testing.T) {
	requireRoot(t)
	qemuAvailable(t)
	requireProvisionTools(t)
	requireXorrisofs(t)

	rawDisk := createChrootCapableTestDiskImage(t, 512)
	image := compressGzip(t, rawDisk)
	kernel := findKernel(t)

	report := cycleReport{Accel: accel(), Commit: os.Getenv("GITHUB_SHA")}
	report.Cycles = append(report.Cycles, measureColdCycle(t, kernel, image))
	agentBoot, agent := measureAgentCycle(t, kernel, image)
	report.Cycles = append(report.Cycles, agent, agentBoot)
	publishCycleReport(t, report)
}

// usedTargetDisk returns a 2 GiB qcow2 disk with a partition table and file
// systems, like a host that ran a cluster node before.
func usedTargetDisk(t *testing.T) string {
	t.Helper()
	disk := filepath.Join(t.TempDir(), "cycle-target.qcow2")
	run(t, "convert disk to qcow2", "qemu-img", "convert", "-O", "qcow2", createTestDiskImage(t, 256), disk)
	run(t, "grow target disk", "qemu-img", "resize", disk, "2G")
	return disk
}

func provisionVars(f *fakeCAPRF, vars map[string]string) map[string]string {
	vars["IMAGE"] = f.baseURL + "/image.gz"
	vars["IMAGE_ALLOW_INSECURE_HTTP"] = "true"
	vars["DISK_DEVICE"] = "/dev/vda"
	vars["PROVISION_TARGET_OS"] = "linux"
	vars["SUCCESS_ACTION"] = "poweroff"
	return vars
}

func measureColdCycle(t *testing.T, kernel, image string) cycleRun {
	t.Helper()
	disk := usedTargetDisk(t)
	cold := cycleRun{Name: "cold"}
	for _, mode := range []string{"deprovision", "provision"} {
		caprf := startFakeCAPRF(t, image)
		vars := provisionVars(caprf, caprf.nativeVars(mode))
		iso := buildCycleISO(t, kernel, buildProvisionInitramfs(t, vars))
		vm := runTimedQEMU(t, cycleQEMUArgs(iso, disk, ""), 8*time.Minute, nil)
		t.Logf("cold %s VM: %.1fs, tail:\n%s", mode, vm.exit.Seconds(), tail(vm.out, 2000))
		caprf.requireStatuses(t, "init", "success")
		requirePowerOff(t, vm.out)
		if mode == "provision" {
			assertProvisionSucceeded(t, vm.out)
		}
		cold.TotalSeconds += vm.exit.Seconds()
		cold.Phases = append(cold.Phases, phasesBetween(vm.lines, 0, vm.exit)...)
	}
	return cold
}

func measureAgentCycle(t *testing.T, kernel, image string) (boot, agent cycleRun) {
	t.Helper()
	disk := usedTargetDisk(t)
	caprf := startFakeCAPRF(t, image)
	vars := caprf.standbyVars()
	vars["STANDBY_DEPROVISION_SUCCESS_ACTION"] = "standby"
	iso := buildCycleISO(t, kernel, buildProvisionInitramfs(t, vars))
	monitor := filepath.Join(t.TempDir(), "mon.sock")

	var cycleStart time.Duration
	vm := runTimedQEMU(t, cycleQEMUArgs(iso, disk, monitor), 10*time.Minute, func(vm *timedQEMU) error {
		if err := waitUntil(5*time.Minute, func() bool { return caprf.heartbeatCount() > 0 }); err != nil {
			return fmt.Errorf("standby ready: %w", err)
		}
		// BOOTy reported ready: from now on it must not need the ISO.
		if err := ejectCD(monitor); err != nil {
			return err
		}
		cycleStart = vm.elapsed()
		caprf.queueCommand(agentCommand{ID: "d1", Type: "deprovision"})
		if err := waitUntil(5*time.Minute, func() bool { return len(caprf.ackList()) > 0 }); err != nil {
			return fmt.Errorf("deprovision ACK: %w", err)
		}
		caprf.queueCommand(agentCommand{ID: "p1", Type: "provision"})
		return nil
	})
	t.Logf("agent VM: %.1fs, tail:\n%s", vm.exit.Seconds(), tail(vm.out, 3000))
	assertProvisionSucceeded(t, vm.out)
	requirePowerOff(t, vm.out)
	if n := strings.Count(string(vm.out), bootyStartMarker); n != 1 {
		t.Fatalf("guest booted %d times, want one boot for the agent cycle", n)
	}
	acks := caprf.ackList()
	if len(acks) != 2 || acks[0].Status != "completed" || acks[1].Status != "completed" {
		t.Fatalf("acks = %+v, want d1 and p1 completed", acks)
	}
	for _, l := range vm.lines {
		if strings.Contains(l.text, "memAvailableMiB") {
			t.Logf("RAM footprint: %s", l.text)
		}
	}
	boot = cycleRun{Name: "agent-boot", TotalSeconds: cycleStart.Seconds(), Phases: phasesBetween(vm.lines, 0, cycleStart)}
	agent = cycleRun{Name: "agent", TotalSeconds: (vm.exit - cycleStart).Seconds(), Phases: phasesBetween(vm.lines, cycleStart, vm.exit)}
	return boot, agent
}

// cycleQEMUArgs boots iso from a SCSI CD like the kindmetal VMs. With a
// monitor path, the HMP monitor listens there for the eject.
func cycleQEMUArgs(iso, disk, monitor string) []string {
	cd := fmt.Sprintf("file=%s,format=raw,if=none,id=cd0,readonly=on", iso)
	if bps := os.Getenv("BOOTY_CYCLE_VMEDIA_BPS"); bps != "" {
		cd += ",throttling.bps-read=" + bps
	}
	args := []string{
		"-machine", "q35,usb=off", "-accel", "kvm", "-accel", "tcg",
		"-m", "1024", "-display", "none", "-nodefaults", "-no-reboot",
		"-chardev", "stdio,id=charserial0", "-device", "isa-serial,chardev=charserial0",
		"-device", "virtio-scsi-pci,id=scsi0", "-drive", cd, "-device", "scsi-cd,drive=cd0,bootindex=1",
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", disk),
		"-net", "nic,model=virtio,macaddr=52:54:00:12:34:56", "-net", "user",
	}
	if monitor != "" {
		args = append(args, "-monitor", "unix:"+monitor+",server=on,wait=off")
	}
	return append(args, splitExtraArgs(os.Getenv("QEMU_EXTRA_ARGS"))...)
}

// buildCycleISO packs kernel and initramfs like the CAPRF ramdisk builder,
// without the stub deploy.cpio: the initramfs carries the deploy vars.
func buildCycleISO(t *testing.T, kernel, initramfs string) string {
	t.Helper()
	isolinuxBin, ldlinuxC32 := requireISOLINUX(t)
	root := t.TempDir()
	for _, dir := range []string{"boot", "isolinux"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	copyFile(t, kernel, filepath.Join(root, "boot", "vmlinuz"))
	copyFile(t, initramfs, filepath.Join(root, "boot", "initrd.img"))
	copyFile(t, isolinuxBin, filepath.Join(root, "isolinux", "isolinux.bin"))
	copyFile(t, ldlinuxC32, filepath.Join(root, "isolinux", "ldlinux.c32"))
	writeFile(t, filepath.Join(root, "efiboot.img"), strings.Repeat("\x00", 1024))
	writeFile(t, filepath.Join(root, "isolinux", "isolinux.cfg"), `SERIAL 0 115200
PROMPT 0
TIMEOUT 1
DEFAULT booty
LABEL booty
  KERNEL /boot/vmlinuz
  APPEND initrd=/boot/initrd.img console=ttyS0,115200n8 panic=1 net.ifnames=0
`)
	iso := filepath.Join(t.TempDir(), "cycle.iso")
	runXorrisofs(t, root, iso)
	return iso
}

// timedQEMU is a QEMU run whose serial lines carry the host time since start.
type timedQEMU struct {
	start time.Time
	lines []timedLine
	out   []byte
	exit  time.Duration
}

func (q *timedQEMU) elapsed() time.Duration { return time.Since(q.start) }

// runTimedQEMU runs QEMU until it exits. during runs beside it, for example
// to queue commands; its error fails the test after QEMU ended.
func runTimedQEMU(t *testing.T, args []string, timeout time.Duration, during func(*timedQEMU) error) *timedQEMU {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	q := &timedQEMU{start: time.Now()}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start QEMU: %v", err)
	}
	read := make(chan struct{})
	go func() {
		defer close(read)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			text := strings.TrimRight(sc.Text(), "\r")
			q.lines = append(q.lines, timedLine{at: q.elapsed(), text: text})
			q.out = append(q.out, text+"\n"...)
		}
	}()
	var duringErr error
	var wg sync.WaitGroup
	if during != nil {
		wg.Go(func() { duringErr = during(q) })
	}
	waitErr := cmd.Wait()
	q.exit = q.elapsed()
	_ = pw.Close()
	<-read
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatalf("QEMU timed out after %v. tail:\n%s", timeout, tail(q.out, 3000))
	}
	if waitErr != nil {
		t.Logf("QEMU exited: %v (expected on power off)", waitErr)
	}
	if duringErr != nil {
		t.Fatalf("%v. tail:\n%s", duringErr, tail(q.out, 3000))
	}
	return q
}

func waitUntil(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return fmt.Errorf("not reached within %v", timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// ejectCD ejects the CD through the HMP monitor, like a CAPRF
// VirtualMedia.EjectMedia, and checks that the drive is empty.
func ejectCD(monitor string) error {
	conn, err := net.DialTimeout("unix", monitor, 5*time.Second)
	if err != nil {
		return fmt.Errorf("QEMU monitor: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprint(conn, "eject -f cd0\ninfo block cd0\n"); err != nil {
		return fmt.Errorf("eject CD: %w", err)
	}
	var seen strings.Builder
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		seen.WriteString(sc.Text() + "\n")
		if strings.Contains(sc.Text(), "not inserted") {
			return nil
		}
	}
	return fmt.Errorf("CD still inserted after eject, monitor output:\n%s", seen.String())
}

func (f *fakeCAPRF) heartbeatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeats
}

func accel() string {
	if _, err := os.Stat("/dev/kvm"); err == nil {
		return "kvm"
	}
	return "tcg"
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return def
}

// publishCycleReport writes the report to BOOTY_CYCLE_REPORT and the job
// summary, and compares it with the report in BOOTY_CYCLE_BASELINE.
// BOOTY_CYCLE_REGRESSION_MODE=fail fails the test on a regression, else the
// test only warns: shared runners are noisy.
func publishCycleReport(t *testing.T, r cycleReport) {
	t.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal cycle report: %v", err)
	}
	if p := os.Getenv("BOOTY_CYCLE_REPORT"); p != "" {
		writeFile(t, p, string(data)+"\n")
	}
	var base *cycleReport
	if p := os.Getenv("BOOTY_CYCLE_BASELINE"); p != "" {
		var b cycleReport
		if raw, err := os.ReadFile(p); err != nil || json.Unmarshal(raw, &b) != nil {
			t.Logf("no usable baseline at %s, reporting without comparison", p)
		} else {
			base = &b
		}
	}
	md := markdown(r, base)
	var regs []string
	if base != nil {
		md += fmt.Sprintf("\nBaseline: main %s, accel %s. Delta in parentheses.\n", base.Commit, base.Accel)
		if !comparable(r, *base) {
			md += fmt.Sprintf("\nNot compared: this run used %s, the baseline %s.\n", r.Accel, base.Accel)
		}
		regs = regressions(r, *base, regressionPolicy{
			Percent:        envFloat("BOOTY_CYCLE_REGRESSION_PCT", 25),
			MinSeconds:     envFloat("BOOTY_CYCLE_REGRESSION_MIN_SECONDS", 10),
			AgentMinSecond: envFloat("BOOTY_CYCLE_REGRESSION_AGENT_MIN_SECONDS", 2),
		})
		for _, reg := range regs {
			md += "\n**Regression:** " + reg + "\n"
		}
	}
	t.Logf("cycle report:\n%s\n%s", data, md)
	if p := os.Getenv("GITHUB_STEP_SUMMARY"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err == nil {
			_, _ = f.WriteString(md + "\n")
			_ = f.Close()
		}
	}
	for _, reg := range regs {
		if os.Getenv("BOOTY_CYCLE_REGRESSION_MODE") == "fail" {
			t.Errorf("cycle time regression: %s", reg)
		} else {
			fmt.Printf("::warning title=Cycle time regression::%s\n", reg)
		}
	}
}
