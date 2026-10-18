#!/bin/sh
# Pack a ramdisk tree as a newc cpio archive compressed with zstd.
#
# usage: pack-initramfs.sh <root> <out.cpio.zst>
#
# The kernel decompresses the ramdisk in one thread before /init starts,
# and slow BMC virtual media streams every byte of it. zstd is the codec
# with the best trade-off: xz is 11% smaller but decompresses 9 times
# slower in the kernel, which costs more than it saves below about
# 1.2 MB/s (docs/size-optimization.md). --ultra -22 --long is 1.2%
# smaller than -19 --long with the same 128 MiB window and the same
# decompression speed; only the build gets slower.
#
# The archive lists directories first, then the other non-regular files,
# then the regular files grouped by kind: kernel modules, shared
# libraries, executables, the rest. Similar files next to each other
# compress about 1% better than the find order, and the order no longer
# depends on the directory order of the build file system.
set -eu
root=${1:?usage: pack-initramfs.sh <root> <out.cpio.zst>}
out=${2:?usage: pack-initramfs.sh <root> <out.cpio.zst>}

cd "$root"
{
	find . -type d -print0 | LC_ALL=C sort -z
	find . ! -type d ! -type f -print0 | LC_ALL=C sort -z
	find . -type f -name '*.ko' -print0 | LC_ALL=C sort -z
	find . -type f -name '*.so*' ! -name '*.ko' -print0 | LC_ALL=C sort -z
	find . -type f -perm /111 ! -name '*.ko' ! -name '*.so*' -print0 | LC_ALL=C sort -z
	find . -type f ! -perm /111 ! -name '*.ko' ! -name '*.so*' -print0 | LC_ALL=C sort -z
} > /tmp/initramfs.files

# Every entry once: the groups must cover the tree exactly.
listed=$(tr '\0' '\n' < /tmp/initramfs.files | sort | uniq | wc -l)
total=$(find . | wc -l)
if [ "$listed" -ne "$total" ] || [ "$(tr -cd '\0' < /tmp/initramfs.files | wc -c)" -ne "$total" ]; then
	echo "pack-initramfs: file list has $listed unique of $total entries" >&2
	exit 1
fi

cpio --null -o --quiet --format=newc < /tmp/initramfs.files > /tmp/initramfs.cpio
rm /tmp/initramfs.files
zstd -q --ultra -22 --long /tmp/initramfs.cpio -o "$out"
rm /tmp/initramfs.cpio
