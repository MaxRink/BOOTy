#!/bin/sh
# Remove the shared libraries that no executable in an initramfs tree loads.
#
# The builder stages copy the library sets that the tools stage collected for
# every tool. A flavor that ships only some of the tools also gets the
# libraries of the tools it leaves out. This script runs ldd on every
# executable in the tree, keeps the resolved libraries (and the dynamic
# loader), and deletes the other shared libraries.
#
# usage: prune-initramfs-libs.sh <initramfs-root>
set -eu
export LC_ALL=C

root=${1:?usage: prune-initramfs-libs.sh <initramfs-root>}
root=$(cd "$root" && pwd)
work=$(mktemp -d)

find "$root" -type f -name '*.so*' | sort > "$work/libs"
LD_LIBRARY_PATH=$(sed 's|/[^/]*$||' "$work/libs" | sort -u | paste -sd: -)
export LD_LIBRARY_PATH

# ldd prints a library either at its path in the tree (found through
# LD_LIBRARY_PATH) or at its path in the build container (dynamic loader,
# RPATH). The tools stage copies libraries with cp --parents, so both map to
# the same path under the root.
find "$root" -type f -perm -u+x ! -name '*.so*' ! -name '*.ko' | while read -r exe; do
	ldd "$exe" 2>/dev/null || true
done | awk -v root="$root" '{
	for (i = 1; i <= NF; i++) {
		if ($i !~ /^\//) continue
		p = $i
		if (index(p, root "/") != 1) p = root p
		print p
	}
}' | sort -u > "$work/needed"

# An empty closure means that ldd did not run; never delete every library.
if ! grep -q '/libc\.so\.6$' "$work/needed"; then
	echo "prune-initramfs-libs: libc.so.6 is not in the ldd closure of $root" >&2
	exit 1
fi

comm -23 "$work/libs" "$work/needed" > "$work/unused"
while read -r lib; do
	echo "prune-initramfs-libs: removing unused ${lib#"$root"/}"
	rm -f "$lib"
done < "$work/unused"
rm -f "$work/libs" "$work/needed" "$work/unused"
rmdir "$work"
