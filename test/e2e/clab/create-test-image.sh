#!/usr/bin/env bash
# create-test-image.sh — Generate a minimal GPT disk image for E2E boot tests.
#
# Creates a ~256 MiB raw disk image with:
#   Partition 1: 64 MiB  EFI System Partition (FAT32)
#   Partition 2: ~180 MiB Linux root filesystem (ext4)
#
# The root partition includes a minimal directory skeleton so BOOTy's
# provisioning pipeline can proceed through mount, chroot, and grow steps.
# Provisioning will eventually fail at configure-kubelet (no real OS),
# giving a deterministic and meaningful E2E failure point.
#
# Usage:
#   ./create-test-image.sh [output-dir]
#   Default output: test/e2e/clab/images/
#
# Requires: dd, sgdisk (gdisk), losetup, mkfs.vfat, mkfs.ext4, mount (root)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
OUTPUT_DIR="${1:-${SCRIPT_DIR}/images}"
IMAGE_RAW="${OUTPUT_DIR}/test.img"
IMAGE_GZ="${OUTPUT_DIR}/test.img.gz"
IMAGE_SIZE_MB=256
LOOP_DEV=""
MOUNT_DIR=""

cleanup() {
    local status=$?

    if [ -n "${MOUNT_DIR}" ]; then
        umount "${MOUNT_DIR}/boot/efi" 2>/dev/null || true
        umount "${MOUNT_DIR}" 2>/dev/null || true
        rmdir "${MOUNT_DIR}" 2>/dev/null || true
    fi

    if [ -n "${LOOP_DEV}" ]; then
        losetup -d "${LOOP_DEV}" 2>/dev/null || true
    fi

    exit "${status}"
}

trap cleanup EXIT

mkdir -p "${OUTPUT_DIR}"

echo "==> Creating ${IMAGE_SIZE_MB} MiB raw disk image"
dd if=/dev/zero of="${IMAGE_RAW}" bs=1M count="${IMAGE_SIZE_MB}" status=progress 2>&1

echo "==> Creating GPT partition table"
sgdisk --zap-all "${IMAGE_RAW}"
# Partition 1: EFI System Partition, 64 MiB
sgdisk --new=1:2048:+64M --typecode=1:EF00 --change-name=1:EFI "${IMAGE_RAW}"
# Partition 2: Linux filesystem, remaining space
sgdisk --new=2:0:0 --typecode=2:8300 --change-name=2:root "${IMAGE_RAW}"
sgdisk --print "${IMAGE_RAW}"

echo "==> Setting up loop device with partition scanning"
LOOP_DEV=$(losetup --find --show --partscan "${IMAGE_RAW}")
echo "    Loop device: ${LOOP_DEV}"

# Wait for the partition nodes. --partscan returns before the kernel and
# udev create them, and without udev (containers) they never appear in /dev:
# re-read the table while sysfs lacks a partition and (re)create the node from
# its sysfs major:minor.
for iter in $(seq 1 100); do
    missing=0
    for n in 1 2; do
        part="${LOOP_DEV}p${n}"
        sysdev="/sys/class/block/$(basename "${part}")/dev"
        if [ ! -r "${sysdev}" ]; then
            missing=1
            continue
        fi
        want="$(cat "${sysdev}" 2>/dev/null)" || { missing=1; continue; }
        # A node left from an earlier loop device can point elsewhere. Give
        # udev and devtmpfs about a second before creating the node here.
        if [ "${iter}" -gt 3 ] && { [ "$(stat -c '%Hr:%Lr' "${part}" 2>/dev/null)" != "${want}" ] || [ ! -b "${part}" ]; }; then
            rm -f "${part}"
            mknod "${part}" b "${want%%:*}" "${want##*:}" 2>/dev/null || true
        fi
        # The node must match sysfs, and a partition that the kernel still
        # adds or removes cannot be read.
        if [ "$(stat -c '%Hr:%Lr' "${part}" 2>/dev/null)" != "${want}" ] ||
            ! blockdev --getsize64 "${part}" >/dev/null 2>&1; then
            missing=1
        fi
    done
    [ "${missing}" -eq 0 ] && break
    partx --update "${LOOP_DEV}" 2>/dev/null || true
    partprobe "${LOOP_DEV}" 2>/dev/null || true
    udevadm settle --timeout=2 2>/dev/null || true
    sleep 0.3
done

if [ ! -b "${LOOP_DEV}p1" ] || [ ! -b "${LOOP_DEV}p2" ]; then
    echo "ERROR: partition devices not found (${LOOP_DEV}p1, ${LOOP_DEV}p2)"
    exit 1
fi

echo "==> Formatting EFI partition (FAT32)"
mkfs.vfat -F 32 -n EFI "${LOOP_DEV}p1"

echo "==> Formatting root partition (ext4)"
mkfs.ext4 -L root -q "${LOOP_DEV}p2"

echo "==> Populating root filesystem skeleton"
MOUNT_DIR=$(mktemp -d)
mount "${LOOP_DEV}p2" "${MOUNT_DIR}"

# Minimal directory structure expected by BOOTy provisioning steps.
mkdir -p "${MOUNT_DIR}/boot/grub"
mkdir -p "${MOUNT_DIR}/boot/efi"
mkdir -p "${MOUNT_DIR}/etc/kubernetes"
mkdir -p "${MOUNT_DIR}/etc/default"
mkdir -p "${MOUNT_DIR}/usr/bin"
mkdir -p "${MOUNT_DIR}/var/log"

# Create a minimal grub.cfg so configure-grub doesn't fail.
cat > "${MOUNT_DIR}/boot/grub/grub.cfg" <<'GRUB'
set default=0
set timeout=5
menuentry "Linux" {
    linux /vmlinuz root=LABEL=root ro
    initrd /initrd.img
}
GRUB

# Mount and populate EFI partition.
mount "${LOOP_DEV}p1" "${MOUNT_DIR}/boot/efi"
mkdir -p "${MOUNT_DIR}/boot/efi/EFI/BOOT"
echo "# EFI stub" > "${MOUNT_DIR}/boot/efi/EFI/BOOT/BOOTX64.CSV"
umount "${MOUNT_DIR}/boot/efi"

umount "${MOUNT_DIR}"
rmdir "${MOUNT_DIR}"
MOUNT_DIR=""

echo "==> Detaching loop device"
losetup -d "${LOOP_DEV}"
LOOP_DEV=""

echo "==> Compressing image with gzip"
gzip -f -k "${IMAGE_RAW}"
ls -lh "${IMAGE_RAW}" "${IMAGE_GZ}"

echo "==> Test image ready: ${IMAGE_GZ}"
echo "    Raw: $(du -h "${IMAGE_RAW}" | cut -f1)"
echo "    Gzip: $(du -h "${IMAGE_GZ}" | cut -f1)"
