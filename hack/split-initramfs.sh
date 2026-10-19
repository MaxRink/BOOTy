#!/bin/sh
# Split a full ramdisk tree into a base ramdisk and one archive per optional
# payload (docs/proposal-ramdisk-split.md, option 2). The boot loader loads
# the base and, after it, the payload archives that the run needs; the
# kernel unpacks them over each other.
#
#   base.cpio.zst      the tree without the payload executables and the
#                      libraries that only they load; it lists the left-out
#                      payloads in etc/booty/payloads/split
#   <name>.cpio.zst    the executables of one payload, the libraries that
#                      the base lacks for them, and the manifest
#                      etc/booty/payloads/<name> with build=<build> and
#                      arch=<debian arch>
#
# prune-initramfs-libs.sh proves for the base alone and for the base plus
# each payload that every dynamic executable resolves its libraries.
#
# usage: split-initramfs.sh <full-root> <out-dir> <build>
set -eu
export LC_ALL=C

full=${1:?usage: split-initramfs.sh <full-root> <out-dir> <build>}
out=${2:?usage: split-initramfs.sh <full-root> <out-dir> <build>}
build=${3:?usage: split-initramfs.sh <full-root> <out-dir> <build>}

# Payload executables, relative to the root. Keep in sync with pkg/payload.
payload_files() {
	case "$1" in
	frr) echo sbin/bgpd sbin/zebra sbin/bfdd bin/vtysh sbin/watchfrr ;;
	ipmitool) echo bin/ipmitool ;;
	*) echo "split-initramfs: unknown payload $1" >&2; exit 1 ;;
	esac
}
payloads="frr ipmitool"

# The Debian architecture name is the GOARCH of BOOTy (amd64, arm64).
arch=$(dpkg --print-architecture)
work=$(mktemp -d)
mkdir -p "$out"

# Base: the full tree without the payload executables, pruned.
cp -a "$full" "$work/base"
for p in $payloads; do
	for f in $(payload_files "$p"); do
		[ -f "$full/$f" ] || { echo "split-initramfs: $f of $p is not in $full" >&2; exit 1; }
		rm -f "$work/base/$f"
	done
done
mkdir -p "$work/base/etc/booty/payloads"
echo "$payloads" > "$work/base/etc/booty/payloads/split"
prune-initramfs-libs.sh "$work/base"
(cd "$work/base" && find . | sort) > "$work/base.list"
pack-initramfs.sh "$work/base" "$out/base.cpio.zst"

# Each payload: the full tree without the other payloads, pruned. Its
# archive holds what the base does not have.
for p in $payloads; do
	cp -a "$full" "$work/with"
	for o in $payloads; do
		[ "$o" = "$p" ] && continue
		for f in $(payload_files "$o"); do rm -f "$work/with/$f"; done
	done
	mkdir -p "$work/with/etc/booty/payloads"
	printf 'name=%s\nbuild=%s\narch=%s\n' "$p" "$build" "$arch" > "$work/with/etc/booty/payloads/$p"
	prune-initramfs-libs.sh "$work/with"
	(cd "$work/with" && find . ! -type d | sort) | comm -23 - "$work/base.list" > "$work/$p.list"
	mkdir -p "$work/p"
	(cd "$work/with" && while read -r f; do
		mkdir -p "$work/p/${f%/*}"
		cp -a "$f" "$work/p/$f"
	done < "$work/$p.list")
	pack-initramfs.sh "$work/p" "$out/$p.cpio.zst"
	rm -rf "$work/with" "$work/p"
done

rm -rf "$work"
ls -l "$out"
