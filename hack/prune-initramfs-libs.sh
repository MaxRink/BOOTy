#!/bin/sh
# Remove the shared libraries that no executable in an initramfs tree loads,
# then prove that every dynamic executable still resolves all its libraries
# from the tree alone.
#
# The builder stages copy the library sets that the tools stage collected for
# every tool. A flavor that ships only some of the tools also gets the
# libraries of the tools it leaves out. This script runs ldd on every
# dynamic executable in the tree, keeps the resolved libraries (and the
# dynamic loader), and deletes the other shared libraries. It then runs the
# executable's own loader with --list inside a chroot of the tree, so a
# library that exists only in the build container cannot hide a gap.
#
# usage: prune-initramfs-libs.sh <initramfs-root>
set -eu
export LC_ALL=C

root=${1:?usage: prune-initramfs-libs.sh <initramfs-root>}
root=$(cd "$root" && pwd)
work=$(mktemp -d)

fail() {
	echo "prune-initramfs-libs: $*" >&2
	exit 1
}

# Shared libraries, without the loader cache and its configuration.
find_libs() {
	find "$root" -type f \( -name '*.so' -o -name '*.so.*' \) \
		! -name 'ld.so.cache' ! -name 'ld.so.conf'
}

# Print the PT_INTERP path of an ELF64 little-endian file, or nothing for a
# static ELF. The builder images have no readelf, so read the program
# headers with od.
elf_interp() {
	[ "$(od -An -c -j 1 -N 3 "$1" | tr -d ' ')" = ELF ] || return 0
	[ "$(od -An -t u1 -j 4 -N 2 "$1" | tr -s ' ')" = " 2 1" ] ||
		fail "${1#"$root"/}: only ELF64 little-endian files are supported"
	phoff=$(od -An -t u8 -j 32 -N 8 "$1" | tr -d ' ')
	phentsize=$(od -An -t u2 -j 54 -N 2 "$1" | tr -d ' ')
	phnum=$(od -An -t u2 -j 56 -N 2 "$1" | tr -d ' ')
	i=0
	while [ "$i" -lt "$phnum" ]; do
		ph=$((phoff + i * phentsize))
		if [ "$(od -An -t u4 -j "$ph" -N 4 "$1" | tr -d ' ')" = 3 ]; then
			off=$(od -An -t u8 -j $((ph + 8)) -N 8 "$1" | tr -d ' ')
			len=$(od -An -t u8 -j $((ph + 32)) -N 8 "$1" | tr -d ' ')
			tail -c +$((off + 1)) "$1" | head -c $((len - 1))
			return 0
		fi
		i=$((i + 1))
	done
}

# Record "<executable> <interpreter>" for every dynamic executable. The loops
# read from files, not pipes, so that fail exits the script.
find "$root" -type f -perm -u+x ! -name '*.so' ! -name '*.so.*' ! -name '*.ko' |
	sort > "$work/exes"
while read -r exe; do
	interp=$(elf_interp "$exe") || exit 1
	[ -z "$interp" ] || echo "$exe $interp"
done < "$work/exes" > "$work/dynamic"

find_libs | sort > "$work/libs"
LD_LIBRARY_PATH=$(sed 's|/[^/]*$||' "$work/libs" | sort -u | paste -sd: -)
export LD_LIBRARY_PATH

# ldd prints a library either at its path in the tree (found through
# LD_LIBRARY_PATH) or at its path in the build container (dynamic loader,
# RPATH). The tools stage copies libraries with cp --parents, so both map to
# the same path under the root.
while read -r exe interp; do
	out=$(ldd "$exe" 2>&1) || fail "ldd ${exe#"$root"/} failed: $out"
	case "$out" in *"not found"*) fail "ldd ${exe#"$root"/}: $out" ;; esac
	echo "$out"
done < "$work/dynamic" > "$work/ldd"
awk -v root="$root" '{
	for (i = 1; i <= NF; i++) {
		if ($i !~ /^\//) continue
		p = $i
		if (index(p, root "/") != 1) p = root p
		print p
	}
}' "$work/ldd" | sort -u > "$work/needed"

# An empty closure means that ldd did not run; never delete every library.
grep -q '/libc\.so\.6$' "$work/needed" ||
	fail "libc.so.6 is not in the ldd closure of $root"

comm -23 "$work/libs" "$work/needed" > "$work/unused"
while read -r lib; do
	echo "prune-initramfs-libs: removing unused ${lib#"$root"/}"
	rm -f "$lib"
done < "$work/unused"

# Resolve every dynamic executable with its own loader inside the tree.
checked=0
while read -r exe interp; do
	rel=/${exe#"$root"/}
	[ -f "$root$interp" ] || fail "$rel: interpreter $interp is not in the tree"
	out=$(unset LD_LIBRARY_PATH; chroot "$root" "$interp" --list "$rel" 2>&1) ||
		fail "chroot $interp --list $rel failed: $out"
	case "$out" in *"not found"*) fail "chroot $interp --list $rel: $out" ;; esac
	checked=$((checked + 1))
done < "$work/dynamic"
echo "prune-initramfs-libs: $checked dynamic executables resolve inside the tree"

rm -f "$work/exes" "$work/dynamic" "$work/ldd" "$work/libs" "$work/needed" "$work/unused"
rmdir "$work"
