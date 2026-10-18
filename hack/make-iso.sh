#!/bin/sh
# Pack a BOOTy ISO that boots on BIOS (ISOLINUX) and UEFI (shim + GRUB),
# from a CD or virtual media and from a USB stick or disk.
#
# Layout: xorriso's hybrid GPT with an appended EFI partition, as Ubuntu's
# ISOs use it. El Torito BIOS entry with ISOLINUX, El Torito EFI entry that
# points at the appended EFI system partition, an isohybrid MBR (marked
# bootable for older BIOSes) for BIOS USB boot, and a GPT with the ESP as
# partition 2.
#
# usage: make-iso.sh <iso-root> <efiboot.img> <out.iso> [volid]
#   iso-root must contain boot/vmlinuz and boot/initrd.img.
#   efiboot.img comes from make-efiboot.sh.
# Needs: xorriso, isolinux, syslinux-common. SOURCE_DATE_EPOCH (default
# 315532800, 1980-01-01) fixes the timestamps, the volume UUID and the GPT
# GUIDs. CI and releases set it to the commit time.
set -eu
root=${1:?usage: make-iso.sh <iso-root> <efiboot.img> <out.iso> [volid]}
efiboot=${2:?usage: make-iso.sh <iso-root> <efiboot.img> <out.iso> [volid]}
out=${3:?usage: make-iso.sh <iso-root> <efiboot.img> <out.iso> [volid]}
volid=${4:-BOOTY}
: "${SOURCE_DATE_EPOCH:=315532800}"
export SOURCE_DATE_EPOCH
cmdline="console=tty0 console=ttyS0,115200n8"

for f in boot/vmlinuz boot/initrd.img; do
	[ -s "$root/$f" ] || { echo "make-iso: $root/$f is missing" >&2; exit 1; }
done

mkdir -p "$root/isolinux" "$root/boot/grub" "$root/EFI/debian"
cp /usr/lib/ISOLINUX/isolinux.bin /usr/lib/syslinux/modules/bios/ldlinux.c32 "$root/isolinux/"

# SERIAL makes ISOLINUX usable on headless machines (serial console only).
# Without PROMPT and TIMEOUT, ISOLINUX boots the DEFAULT entry at once.
printf 'SERIAL 0 115200\nDEFAULT booty\nLABEL booty\n  KERNEL /boot/vmlinuz\n  APPEND initrd=/boot/initrd.img %s\n' \
	"$cmdline" > "$root/isolinux/isolinux.cfg"

# No menu and no wait: GRUB boots at once. Esc held while GRUB starts
# (it checks once for a key) or Shift (keystatus, where the firmware
# reports it) opens the menu, where e edits the kernel command line.
cat > "$root/boot/grub/grub.cfg" <<GRUBCFG
set default=0
set timeout_style=hidden
set timeout=0
serial --unit=0 --speed=115200
terminal_input console serial
terminal_output console serial
if keystatus --shift; then
  set timeout_style=menu
  set timeout=-1
fi
menuentry "BOOTy" {
  linux /boot/vmlinuz $cmdline
  initrd /boot/initrd.img
}
GRUBCFG

# Booted from a CD, Debian's GRUB has the ISO9660 filesystem as $root and
# reads $prefix/grub.cfg = /EFI/debian/grub.cfg from it. $root is already
# this ISO, so no search: a search could pick another disk.
cat > "$root/EFI/debian/grub.cfg" <<'GRUBCFG'
set prefix=($root)/boot/grub
configfile ($root)/boot/grub/grub.cfg
GRUBCFG

# With SOURCE_DATE_EPOCH, xorriso fixes the volume dates and UUID and sets
# every file date to its mtime; fixing the mtimes makes the ISO reproducible.
find "$root" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

xorriso -as mkisofs \
	-o "$out" \
	-V "$volid" \
	-r -J \
	-partition_offset 16 \
	-isohybrid-mbr /usr/lib/ISOLINUX/isohdpfx.bin \
	--mbr-force-bootable \
	-b isolinux/isolinux.bin \
	-c isolinux/boot.cat \
	-no-emul-boot -boot-load-size 4 -boot-info-table \
	-append_partition 2 0xef "$efiboot" \
	-appended_part_as_gpt \
	-eltorito-alt-boot \
	-e --interval:appended_partition_2:all:: \
	-no-emul-boot \
	"$root"
