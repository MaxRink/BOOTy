//go:build linux

package image

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTargetPartitionNode(t *testing.T) {
	tests := []struct {
		name    string
		disk    string
		partNum int
		want    string
	}{
		{"sda partition 1", "/dev/sda", 1, "/dev/sda1"},
		{"sda partition 2", "/dev/sda", 2, "/dev/sda2"},
		{"nvme partition 1", "/dev/nvme0n1", 1, "/dev/nvme0n1p1"},
		{"nvme partition 3", "/dev/nvme0n1", 3, "/dev/nvme0n1p3"},
		{"loop partition 1", "/dev/loop0", 1, "/dev/loop0p1"},
		{"vda partition 2", "/dev/vda", 2, "/dev/vda2"},
		{"mmcblk partition 1", "/dev/mmcblk0", 1, "/dev/mmcblk0p1"},
		{"mmcblk partition 2", "/dev/mmcblk0", 2, "/dev/mmcblk0p2"},
		{"mmcblk1 partition 3", "/dev/mmcblk1", 3, "/dev/mmcblk1p3"},
		{"md partition 1", "/dev/md0", 1, "/dev/md0p1"},
		{"nbd partition 2", "/dev/nbd0", 2, "/dev/nbd0p2"},
		{"by-id partition 1", "/dev/disk/by-id/nvme-eui.0011223344556677", 1, "/dev/disk/by-id/nvme-eui.0011223344556677-part1"},
		{"by-path partition 2", "/dev/disk/by-path/pci-0000:00:1f.2-ata-1", 2, "/dev/disk/by-path/pci-0000:00:1f.2-ata-1-part2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetPartitionNode(tt.disk, tt.partNum)
			if got != tt.want {
				t.Errorf("targetPartitionNode(%q, %d) = %q, want %q", tt.disk, tt.partNum, got, tt.want)
			}
		})
	}
}

func TestTargetPartitionNodeForSourceUsesParsedPartitionNumber(t *testing.T) {
	source := sfdiskPartition{Node: "/dev/loop0p7", Number: 7}
	if got, partNum := targetPartitionNodeForSource("/dev/sda", source, 1); got != "/dev/sda7" || partNum != 7 {
		t.Fatalf("targetPartitionNodeForSource sparse source = (%q, %d), want (/dev/sda7, 7)", got, partNum)
	}

	source.Number = 0
	if got, partNum := targetPartitionNodeForSource("/dev/nvme0n1", source, 2); got != "/dev/nvme0n1p2" || partNum != 2 {
		t.Fatalf("targetPartitionNodeForSource fallback = (%q, %d), want (/dev/nvme0n1p2, 2)", got, partNum)
	}
}

func TestConvertQCOW2HookRegistered(t *testing.T) {
	// On linux, the init() in qcow2.go should have set the hook.
	if convertQCOW2Hook == nil {
		t.Fatal("convertQCOW2Hook is nil on linux")
	}
}

func TestVerifyRawImageChecksumPasses(t *testing.T) {
	raw := []byte("partition-mode raw image")
	path := writeRawChecksumFixture(t, raw)
	sum := sha256.Sum256(raw)

	err := verifyRawImageChecksum(path, StreamOpts{
		Checksum:     hex.EncodeToString(sum[:]),
		ChecksumType: "sha256",
	})
	if err != nil {
		t.Fatalf("verifyRawImageChecksum: %v", err)
	}
}

func TestVerifyRawImageChecksumMismatch(t *testing.T) {
	path := writeRawChecksumFixture(t, []byte("partition-mode raw image"))

	err := verifyRawImageChecksum(path, StreamOpts{
		Checksum:     strings.Repeat("0", sha256.Size*2),
		ChecksumType: "sha256",
	})
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error = %q, want checksum mismatch", err.Error())
	}
}

func TestDownloadAndPrepareRawRejectsUnsupportedVMwareContainer(t *testing.T) {
	if err := os.RemoveAll(ramdiskPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ramdiskPath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ramdiskPath) })

	data := append([]byte{'K', 'D', 'M', 'V'}, []byte("vmdk payload")...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	_, err := downloadAndPrepareRaw(context.Background(), srv.URL+"/image.vmdk", nil)
	if err == nil {
		t.Fatal("expected unsupported VMDK error")
	}
	if !strings.Contains(err.Error(), "unsupported image format vmdk") {
		t.Fatalf("error = %q, want unsupported VMDK format", err.Error())
	}
}

func TestConvertPreparedQCOW2RequiresQemuImg(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := convertPreparedQCOW2(context.Background(), writeRawChecksumFixture(
		t,
		append([]byte{0x51, 0x46, 0x49, 0xfb}, []byte("qcow2 payload")...),
	))
	if err == nil {
		t.Fatal("expected qemu-img prerequisite error")
	}
	if !strings.Contains(err.Error(), "qemu-img") {
		t.Fatalf("error = %q, want qemu-img context", err.Error())
	}
}

func TestConvertPreparedQCOW2UsesQemuImg(t *testing.T) {
	dir := t.TempDir()
	qemuImg := filepath.Join(dir, "qemu-img")
	script := `#!/bin/sh
src=
dst=
shift
while [ "$#" -gt 0 ]; do
	case "$1" in
		-f|-O)
			shift 2
			;;
		-*)
			shift
			;;
		*)
			if [ -z "$src" ]; then
				src="$1"
			else
				dst="$1"
			fi
			shift
			;;
	esac
done
cp "$src" "$dst"
`
	if err := os.WriteFile(qemuImg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake qemu-img: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// A v3 qcow2 header without backing or external data file.
	payload := append([]byte{0x51, 0x46, 0x49, 0xfb, 0, 0, 0, 3}, make([]byte, 96)...)
	payload = append(payload, []byte("qcow2 payload")...)
	source := writeRawChecksumFixture(t, payload)
	converted, err := convertPreparedQCOW2(context.Background(), source)
	if err != nil {
		t.Fatalf("convertPreparedQCOW2: %v", err)
	}
	if converted == source {
		t.Fatal("expected converted raw path to differ from qcow2 source")
	}
	if strings.HasSuffix(converted, ".raw.raw") {
		t.Fatalf("converted path = %q, want no .raw.raw suffix", converted)
	}
	if want := strings.TrimSuffix(source, filepath.Ext(source)) + ".converted.raw"; converted != want {
		t.Fatalf("converted path = %q, want %q", converted, want)
	}
	got, err := os.ReadFile(converted)
	if err != nil {
		t.Fatalf("read converted fixture: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("converted content = %q, want %q", got, payload)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source was not removed after conversion: %v", err)
	}
}

func writeRawChecksumFixture(t *testing.T, data []byte) string {
	t.Helper()
	path := t.TempDir() + "/image.raw"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write raw fixture: %v", err)
	}
	return path
}

func TestReadSfdiskPartitionsDerivesNumbersFromDeviceNodes(t *testing.T) {
	previous := runCmd
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "sfdisk" || len(args) != 2 || args[0] != "--json" || args[1] != "/dev/loop0" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return []byte(`sfdisk noise
{"partitiontable":{"partitions":[
  {"node":"/dev/loop0p5","start":2048,"size":4096,"type":"83"},
  {"node":"/dev/loop0p7","start":6144,"size":4096,"type":"83"},
  {"node":"/dev/mapper/root","start":10240,"size":4096,"type":"83"}
]}}`), nil
	}
	t.Cleanup(func() { runCmd = previous })

	parts, err := readSfdiskPartitions(context.Background(), "/dev/loop0")
	if err != nil {
		t.Fatalf("readSfdiskPartitions: %v", err)
	}
	got := []int{parts[0].Number, parts[1].Number, parts[2].Number}
	want := []int{5, 7, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("partition numbers = %v, want %v", got, want)
		}
	}
}

func TestTargetPartitionNodeForSourceUsesParsedSourceNumber(t *testing.T) {
	tests := []struct {
		name     string
		disk     string
		part     sfdiskPartition
		fallback int
		wantNode string
		wantNum  int
	}{
		{
			name:     "sparse source partition",
			disk:     "/dev/sda",
			part:     sfdiskPartition{Node: "/dev/loop0p7", Number: 7},
			fallback: 2,
			wantNode: "/dev/sda7",
			wantNum:  7,
		},
		{
			name:     "nvme sparse source partition",
			disk:     "/dev/nvme0n1",
			part:     sfdiskPartition{Node: "/dev/loop0p5", Number: 5},
			fallback: 1,
			wantNode: "/dev/nvme0n1p5",
			wantNum:  5,
		},
		{
			name:     "fallback for missing parsed number",
			disk:     "/dev/sda",
			part:     sfdiskPartition{Node: "/dev/mapper/root"},
			fallback: 3,
			wantNode: "/dev/sda3",
			wantNum:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNode, gotNum := targetPartitionNodeForSource(tt.disk, tt.part, tt.fallback)
			if gotNode != tt.wantNode || gotNum != tt.wantNum {
				t.Fatalf("targetPartitionNodeForSource() = (%q, %d), want (%q, %d)",
					gotNode, gotNum, tt.wantNode, tt.wantNum)
			}
		})
	}
}

func TestSettlePartitionTableFallsBackAndRunsMdev(t *testing.T) {
	previous := runCmd
	var calls []string
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, commandString(name, args...))
		switch commandString(name, args...) {
		case "sync":
			return nil, nil
		case "partprobe /dev/sda":
			return []byte("busy"), errors.New("busy")
		case "blockdev --rereadpt /dev/sda":
			return nil, nil
		case "mdev -s":
			return nil, nil
		default:
			t.Fatalf("unexpected command %s %v", name, args)
			return nil, nil
		}
	}
	t.Cleanup(func() { runCmd = previous })

	if err := settlePartitionTable(context.Background(), "/dev/sda"); err != nil {
		t.Fatalf("settlePartitionTable: %v", err)
	}
	assertCommands(t, calls, []string{
		"sync",
		"partprobe /dev/sda",
		"blockdev --rereadpt /dev/sda",
		"mdev -s",
	})
}

func TestWaitForPartitionDeviceRunsMdevUntilNodeAppears(t *testing.T) {
	previous := runCmd
	device := t.TempDir() + "/sda1"
	var mdevCalls int
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if commandString(name, args...) != "mdev -s" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		mdevCalls++
		if err := os.WriteFile(device, []byte("node"), 0o600); err != nil {
			t.Fatalf("write device fixture: %v", err)
		}
		return nil, nil
	}
	t.Cleanup(func() { runCmd = previous })

	if err := waitForPartitionDevice(context.Background(), device); err != nil {
		t.Fatalf("waitForPartitionDevice: %v", err)
	}
	if mdevCalls != 1 {
		t.Fatalf("mdev calls = %d, want 1", mdevCalls)
	}
}

func TestWaitForPartitionDeviceStopsOnCanceledContext(t *testing.T) {
	previousRun := runCmd
	previousStat := statPath
	runCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if commandString(name, args...) != "mdev -s" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return nil, nil
	}
	statPath = func(string) (os.FileInfo, error) {
		return nil, os.ErrNotExist
	}
	t.Cleanup(func() {
		runCmd = previousRun
		statPath = previousStat
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := waitForPartitionDevice(ctx, "/dev/missing1")
	if err == nil {
		t.Fatal("expected canceled context error")
	}
	if !strings.Contains(err.Error(), "wait canceled") {
		t.Fatalf("error = %q, want wait canceled", err.Error())
	}
}

func TestWaitForPartitionDeviceReturnsNonMissingStatError(t *testing.T) {
	previousRun := runCmd
	previousStat := statPath
	var mdevCalls int
	runCmd = func(context.Context, string, ...string) ([]byte, error) {
		mdevCalls++
		return nil, nil
	}
	statPath = func(string) (os.FileInfo, error) {
		return nil, os.ErrPermission
	}
	t.Cleanup(func() {
		runCmd = previousRun
		statPath = previousStat
	})

	err := waitForPartitionDevice(context.Background(), "/dev/sda1")
	if err == nil {
		t.Fatal("expected stat error")
	}
	if !strings.Contains(err.Error(), "stat partition device /dev/sda1") ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %q, want stat permission context", err.Error())
	}
	if mdevCalls != 0 {
		t.Fatalf("mdev calls = %d, want 0", mdevCalls)
	}
}

func TestSelectSourcePartitionsForAB(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024, Number: 1},
		{Node: "/dev/loop0p2", Type: linuxFilesystemGUID, Size: 2048, Name: "rootfs", Number: 2},
		{Node: "/dev/loop0p3", Type: linuxFilesystemGUID, Size: 4096, Name: "data", Number: 3},
	}
	boot, ok := selectSourceBootPartition(parts)
	if !ok || boot.Node != "/dev/loop0p1" {
		t.Fatalf("boot = %#v, ok=%v", boot, ok)
	}
	root, err := selectSourceRootPartition(parts, "", 0)
	if err != nil || root.Node != "/dev/loop0p2" {
		t.Fatalf("root = %#v, err=%v", root, err)
	}
}

func TestSelectSourceRootPartitionRejectsAmbiguousLinuxPartitions(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024},
		{Node: "/dev/loop0p2", Type: linuxFilesystemGUID, Size: 2048},
		{Node: "/dev/loop0p3", Type: linuxFilesystemGUID, Size: 8192},
	}
	_, err := selectSourceRootPartition(parts, "", 0)
	if err == nil {
		t.Fatal("expected ambiguous Linux root selection to fail")
	}
	if !strings.Contains(err.Error(), "provision.image.sourceRootLabel/provision.image.sourceRootPartition") {
		t.Fatalf("error = %q, want provision.image selector hint", err.Error())
	}
	if !strings.Contains(err.Error(), "provision.ab.sourceRootLabel/provision.ab.sourceRootPartition") {
		t.Fatalf("error = %q, want provision.ab selector hint", err.Error())
	}
}

func TestSelectSourceRootPartitionSupportsExplicitSelectors(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024, Number: 1},
		{Node: "/dev/loop0p2", Type: linuxFilesystemGUID, Size: 2048, Name: "ROOT-A", Number: 2},
		{Node: "/dev/loop0p3", Type: linuxFilesystemGUID, Size: 8192, Name: "STATE", Number: 3},
	}

	root, err := selectSourceRootPartition(parts, "root-a", 0)
	if err != nil || root.Node != "/dev/loop0p2" {
		t.Fatalf("label root = %#v, err=%v", root, err)
	}

	root, err = selectSourceRootPartition(parts, "", 3)
	if err != nil || root.Node != "/dev/loop0p3" {
		t.Fatalf("partition root = %#v, err=%v", root, err)
	}
}

func TestSelectSourceRootPartitionSupportsFlatcarUsrSlots(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024, Name: "EFI-SYSTEM", Number: 1},
		{Node: "/dev/loop0p3", Type: linuxFilesystemGUID, Size: 2048, Name: "USR-A", Number: 3},
		{Node: "/dev/loop0p4", Type: linuxFilesystemGUID, Size: 2048, Name: "USR-B", Number: 4},
		{Node: "/dev/loop0p9", Type: linuxFilesystemGUID, Size: 8192, Name: "ROOT", Number: 9},
	}

	root, err := selectSourceRootPartition(parts, "USR-A", 0)
	if err != nil || root.Node != "/dev/loop0p3" {
		t.Fatalf("flatcar explicit USR-A root = %#v, err=%v", root, err)
	}

	_, err = selectSourceRootPartition(parts, "", 0)
	if err == nil {
		t.Fatal("expected Flatcar USR-A/USR-B source image to require an explicit selector")
	}
	if !strings.Contains(err.Error(), "Flatcar-like USR-A/USR-B") {
		t.Fatalf("error = %q, want Flatcar-like USR slot rejection", err.Error())
	}
	if !strings.Contains(err.Error(), "provision.image.sourceRootLabel/provision.image.sourceRootPartition") {
		t.Fatalf("error = %q, want partition-layout selector hint", err.Error())
	}
	if !strings.Contains(err.Error(), "provision.ab.sourceRootLabel/provision.ab.sourceRootPartition") {
		t.Fatalf("error = %q, want A/B selector hint", err.Error())
	}
}

func TestSelectSourceRootPartitionRejectsImplicitSingleFlatcarUsrSlot(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024, Name: "EFI-SYSTEM", Number: 1},
		{Node: "/dev/loop0p3", Type: linuxFilesystemGUID, Size: 2048, Name: "USR-A", Number: 3},
	}

	_, err := selectSourceRootPartition(parts, "", 0)
	if err == nil {
		t.Fatal("expected single Flatcar USR-A source image to require an explicit selector")
	}
	if !strings.Contains(err.Error(), "Flatcar-like USR-A/USR-B") {
		t.Fatalf("error = %q, want Flatcar-like USR slot rejection", err.Error())
	}
	if !strings.Contains(err.Error(), "provision.image.sourceRootLabel/provision.image.sourceRootPartition") {
		t.Fatalf("error = %q, want partition-layout selector hint", err.Error())
	}
	if !strings.Contains(err.Error(), "provision.ab.sourceRootLabel/provision.ab.sourceRootPartition") {
		t.Fatalf("error = %q, want A/B selector hint", err.Error())
	}
}

func TestSelectSourceRootPartitionRejectsExplicitEFI(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024, Number: 1},
		{Node: "/dev/loop0p2", Type: linuxFilesystemGUID, Size: 2048, Name: "rootfs", Number: 2},
	}

	_, err := selectSourceRootPartition(parts, "", 1)
	if err == nil {
		t.Fatal("expected explicit EFI partition selector to fail")
	}
	if !strings.Contains(err.Error(), "is EFI, not a root partition") {
		t.Fatalf("error = %q, want EFI rejection", err.Error())
	}
}

func TestSelectSourceRootPartitionRejectsImplicitUnknownNonEFI(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024},
		{Node: "/dev/loop0p2", Type: "unknown", Size: 8192},
	}
	_, err := selectSourceRootPartition(parts, "", 0)
	if err == nil {
		t.Fatal("expected unknown non-EFI partition to require an explicit selector")
	}
	if !strings.Contains(err.Error(), "no Linux root partition candidate") {
		t.Fatalf("error = %q, want Linux root candidate rejection", err.Error())
	}
	if !strings.Contains(err.Error(), "provision.image.sourceRootLabel/provision.image.sourceRootPartition") {
		t.Fatalf("error = %q, want provision.image selector hint", err.Error())
	}
}

func TestSelectSourceRootPartitionRejectsImplicitMicrosoftBasicData(t *testing.T) {
	parts := []sfdiskPartition{
		{Node: "/dev/loop0p1", Type: efiSystemPartitionGUID, Size: 1024},
		{Node: "/dev/loop0p2", Type: "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7", Size: 8192},
	}
	_, err := selectSourceRootPartition(parts, "", 0)
	if err == nil {
		t.Fatal("expected Microsoft Basic Data partition to require an explicit selector")
	}
	if !strings.Contains(err.Error(), "no Linux root partition candidate") {
		t.Fatalf("error = %q, want Linux root candidate rejection", err.Error())
	}
}

func commandString(name string, args ...string) string {
	parts := append([]string{name}, args...)
	return strings.Join(parts, " ")
}

func assertCommands(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commands = %v, want %v", got, want)
		}
	}
}
