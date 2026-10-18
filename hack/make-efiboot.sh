#!/bin/sh
# Build the UEFI boot image (a FAT EFI system partition) for a BOOTy ISO.
#
# The image boots Debian's Secure Boot chain: shim (signed by the Microsoft
# UEFI CA) loads GRUB (signed by Debian), and GRUB verifies the kernel
# through shim. Debian's signed GRUB cannot carry a custom embedded config.
# It reads $prefix/grub.cfg, and its prefix is /EFI/debian on the device it
# was loaded from:
#   - from a CD (El Torito), that device is the ISO9660 filesystem, so the
#     ISO must ship /EFI/debian/grub.cfg itself (make-iso.sh writes it);
#   - from a USB stick or disk, that device is this partition, so the
#     /EFI/debian/grub.cfg below looks for the ISO9660 filesystem on the same
#     disk only. It never searches other disks, so another disk with the
#     same label or files cannot take over the boot.
#
# usage: make-efiboot.sh <out.img> <manifest>
#   manifest: text file that receives the package versions and the SHA-256
#             of the EFI binaries.
# Needs: shim-signed, shim-helpers-amd64-signed, grub-efi-amd64-signed,
# dosfstools, mtools. SOURCE_DATE_EPOCH (default 0) fixes the timestamps.
set -eu
out=${1:?usage: make-efiboot.sh <out.img> <manifest>}
manifest=${2:?usage: make-efiboot.sh <out.img> <manifest>}
: "${SOURCE_DATE_EPOCH:=0}"
export SOURCE_DATE_EPOCH

shim=/usr/lib/shim/shimx64.efi.signed
mm=/usr/lib/shim/mmx64.efi.signed
grub=/usr/lib/grub/x86_64-efi-signed/grubx64.efi.signed

work=$(mktemp -d)
trap 'rm -r "$work"' EXIT

cat > "$work/grub.cfg" <<'GRUBCFG'
# Booted from a USB stick or disk: $root is this EFI partition. Use the
# ISO9660 filesystem of the same disk only.
# $root is "hd0,gpt2" (no parentheses); keep the disk part.
regexp --set=1:disk '^\(?([^,)]+)' "$root"
for dev in "($disk)" "($disk,gpt1)" "($disk,msdos1)"; do
  if [ -f "$dev/boot/grub/grub.cfg" ]; then
    set root="$dev"
    set prefix="$dev/boot/grub"
    configfile "$dev/boot/grub/grub.cfg"
  fi
done
echo "BOOTy: no /boot/grub/grub.cfg on the boot disk ($disk)"
sleep 30
exit
GRUBCFG

cp "$shim" "$work/BOOTX64.EFI"
cp "$mm" "$work/mmx64.efi"
cp "$grub" "$work/grubx64.efi"
touch -d "@$SOURCE_DATE_EPOCH" "$work/BOOTX64.EFI" "$work/mmx64.efi" "$work/grubx64.efi" "$work/grub.cfg"

# Size the FAT image to its content plus 1 MiB for FAT metadata and slack.
kib=$(du -kc "$shim" "$mm" "$grub" | tail -n 1 | cut -f 1)
rm -f "$out"
# A fixed volume serial and SOURCE_DATE_EPOCH make the image reproducible.
mkfs.vfat -C -i 80075eed -n BOOTY_ESP "$out" $((kib + 1024)) >/dev/null
mmd -i "$out" ::/EFI ::/EFI/BOOT ::/EFI/debian
mcopy -m -i "$out" "$work/BOOTX64.EFI" "$work/mmx64.efi" "$work/grubx64.efi" ::/EFI/BOOT/
mcopy -m -i "$out" "$work/grub.cfg" ::/EFI/debian/grub.cfg

{
	echo "# BOOTy UEFI boot chain"
	dpkg-query -W -f '${Package} ${Version}\n' shim-signed shim-helpers-amd64-signed grub-efi-amd64-signed
	sha256sum "$shim" "$mm" "$grub"
	echo "efiboot.img $(sha256sum "$out" | cut -d ' ' -f 1)"
} > "$manifest"
cat "$manifest"
