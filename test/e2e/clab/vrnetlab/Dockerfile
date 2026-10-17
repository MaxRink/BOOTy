# Build BOOTy VM image for containerlab vrnetlab-style testing.
# Packages QEMU + kernel + BOOTy initramfs into a container that boots
# a real VM inside containerlab's EVPN fabric.
#
# Build:  docker build -t booty-vrnetlab:latest -f test/e2e/clab/vrnetlab/Dockerfile .

# ── Stage 1: Build BOOTy static binary ────────────────────────────────
FROM golang:1.27.1-alpine AS builder
RUN set -eu; \
    n=0; \
    until apk add --no-cache git ca-certificates gcc linux-headers musl-dev; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "apk add failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done
COPY go.mod go.sum /src/
WORKDIR /src
RUN set -eu; \
    n=0; \
    until go mod download; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "go mod download failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done
COPY . /src
RUN CGO_ENABLED=1 GOOS=linux go build -a \
    -ldflags "-linkmode external -extldflags '-static' -s -w" \
    -o /booty

# ── Stage 2: Extract a bootable kernel ────────────────────────────────
FROM debian:trixie-slim AS kernel
# Same kernel as initrd.Dockerfile: the Debian-signed trixie kernel, or the
# KERNEL_SUITE one (trixie-backports splits it into linux-image/-binary/
# -modules/-base packages).
ARG KERNEL_SUITE=
RUN if [ -n "$KERNEL_SUITE" ]; then \
        echo "deb http://deb.debian.org/debian $KERNEL_SUITE main" > /etc/apt/sources.list.d/kernel-suite.list; \
    fi && \
    apt-get -o Acquire::Retries=5 update && \
    apt-get -o Acquire::Retries=5 install -y --no-install-recommends kmod xz-utils && \
    KERNEL_PKG=$(apt-cache depends ${KERNEL_SUITE:+-t "$KERNEL_SUITE"} linux-image-amd64 | awk '/Depends: linux-image-/{print $2; exit}') && \
    test -n "$KERNEL_PKG" && \
    KPKG_VER=${KERNEL_PKG#linux-image-} && \
    mkdir -p /tmp/debs && cd /tmp/debs && \
    for p in linux-image linux-binary linux-modules linux-base; do \
        if apt-cache show "$p-$KPKG_VER" >/dev/null 2>&1; then \
            apt-get -o Acquire::Retries=5 download "$p-$KPKG_VER"; \
        fi; \
    done && \
    for d in *.deb; do dpkg-deb -x "$d" /tmp/kernel; done && \
    cd / && \
    MROOT=/tmp/kernel/usr && { [ -d "$MROOT/lib/modules" ] || MROOT=/tmp/kernel; } && \
    KVER=$(ls "$MROOT/lib/modules/" | head -1) && \
    MDIR="$MROOT/lib/modules/$KVER" && \
    VMLINUZ=$(ls /tmp/kernel/boot/vmlinuz-* "$MDIR/vmlinuz" 2>/dev/null | head -1) && \
    cp "$VMLINUZ" /vmlinuz && \
    mkdir -p /modules && \
    depmod -b "$MROOT" "$KVER" && \
    for m in \
        virtio virtio_ring virtio_pci_modern_dev virtio_pci_legacy_dev \
        virtio_pci virtio_net failover net_failover \
        scsi_mod sd_mod virtio_blk virtio_scsi \
        ext4 jbd2 mbcache crc32c_generic crc32c-cryptoapi xfs btrfs fat vfat nls_cp437 nls_iso8859-1 \
        vrf dummy vxlan udp_tunnel ip6_udp_tunnel bridge stp llc \
        e1000e igb igc ixgbe i40e ice iavf \
        tg3 bnxt_en \
        mlx4_core mlx4_en mlx5_core mlxfw \
        be2net; do \
        modprobe --show-depends -d "$MROOT" -S "$KVER" "$m" 2>/dev/null \
            | awk '/^insmod /{print $2}' \
            | while read -r dep; do \
                [ -f "$dep" ] && cp -n "$dep" /modules/ 2>/dev/null || true; \
            done; \
        find "$MDIR" -name "${m}.ko*" -exec cp -n {} /modules/ \; 2>/dev/null || true; \
    done && \
    find /modules -name '*.ko.xz' -exec xz -d {} + && \
    if ls /modules/*.ko.* >/dev/null 2>&1; then echo "compressed modules left in /modules" >&2; exit 1; fi && \
    echo "Extracted $(ls /modules/*.ko 2>/dev/null | wc -l) kernel modules of $KVER" && \
    rm -rf /tmp/kernel /tmp/debs /var/lib/apt/lists/*

# ── Stage 3: Assemble initramfs ───────────────────────────────────────
FROM debian:trixie-slim AS initramfs
RUN apt-get -o Acquire::Retries=5 update && apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
    cpio busybox-static curl gnupg lsb-release ca-certificates \
    e2fsprogs dosfstools gdisk parted lvm2 fdisk util-linux mdadm \
    && rm -rf /var/lib/apt/lists/*

# Install FRR for EVPN networking support inside the VM.
RUN curl --fail --location --show-error --silent \
      --retry 5 --retry-delay 5 --retry-connrefused --connect-timeout 30 \
      https://deb.frrouting.org/frr/keys.gpg \
      -o /tmp/frrouting.downloaded.gpg && \
    printf '%s\n' \
      3D9968AC9AE7BE1169288DDB1FD5839895F57FDA \
      4A56C7738BB3F81595A805D2A832769908F13ED1 \
      A90FC36D9429409798E9C2D874DEED43AB194DBF \
      BBC9ACA9D13025A2C186FF7F741E92A1F6E3975B \
      > /tmp/frrouting.expected-fingerprints && \
    export GNUPGHOME="$(mktemp -d)" && \
    chmod 700 "$GNUPGHOME" && \
    gpg --batch --import /tmp/frrouting.downloaded.gpg && \
    gpg --batch --export $(cat /tmp/frrouting.expected-fingerprints) \
      > /usr/share/keyrings/frrouting.gpg && \
    gpg --show-keys --with-colons /usr/share/keyrings/frrouting.gpg \
      | awk -F: '$1 == "pub" { primary=1; next } $1 == "sub" { primary=0 } $1 == "fpr" && primary { print $10; primary=0 }' \
      | sort -u > /tmp/frrouting.fingerprints && \
    sort -u /tmp/frrouting.expected-fingerprints -o /tmp/frrouting.expected-fingerprints && \
    diff -u /tmp/frrouting.expected-fingerprints /tmp/frrouting.fingerprints && \
    rm -rf "$GNUPGHOME" /tmp/frrouting.downloaded.gpg \
      /tmp/frrouting.expected-fingerprints /tmp/frrouting.fingerprints && \
    echo "deb [signed-by=/usr/share/keyrings/frrouting.gpg] https://deb.frrouting.org/frr $(lsb_release -s -c) frr-stable" \
      > /etc/apt/sources.list.d/frr.list && \
    apt-get -o Acquire::Retries=5 update && apt-get -o Acquire::Retries=5 install -y --no-install-recommends frr && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /initramfs
RUN mkdir -p bin sbin dev proc run sys etc tmp deploy lib/modules mnt \
    usr/lib/frr usr/bin etc/frr etc/pam.d var/run/frr lib && \
    echo 'root:x:0:0:root:/root:/bin/sh' > etc/passwd && \
    echo 'frr:x:101:104::/var/run/frr:/bin/sh' >> etc/passwd && \
    echo 'root:x:0:' > etc/group && \
    echo 'frr:x:104:' >> etc/group && \
    echo 'frrvty:x:103:frr' >> etc/group && \
    echo 'passwd: files' > etc/nsswitch.conf && \
    echo 'group: files' >> etc/nsswitch.conf && \
    chown -R 101:104 etc/frr var/run/frr && \
    printf 'auth sufficient pam_permit.so\naccount sufficient pam_permit.so\n' > etc/pam.d/frr && \
    touch etc/frr/vtysh.conf

# Busybox: static binary + essential symlinks
RUN cp /bin/busybox bin/busybox && \
    cd bin && \
    for cmd in sh mount umount insmod find ip ping cat echo ls mkdir rm \
               chmod sleep ln test grep awk sed head tail stty chroot; do \
        ln -sf busybox "$cmd"; \
    done

# BOOTy binary (exec'd by init wrapper)
COPY --from=builder /booty /initramfs/booty

# Kernel modules for networking (flat directory, loaded via insmod)
COPY --from=kernel /modules/ /initramfs/modules/

# FRR daemons and dependencies for EVPN networking.
# mgmtd is required in FRR 10.5+ for proper integrated config loading.
# Also include disk provisioning tools (mdadm, sgdisk, mkfs, parted, lvm).
# Copy binaries and resolve all shared library dependencies.
RUN mkdir -p /initramfs/lib64 /initramfs/lib/x86_64-linux-gnu && \
    for bin in /usr/lib/frr/mgmtd /usr/lib/frr/zebra /usr/lib/frr/bgpd /usr/lib/frr/bfdd /usr/bin/vtysh \
               /usr/sbin/sgdisk /usr/sbin/mkfs.ext4 /usr/sbin/mkfs.vfat /usr/sbin/parted \
               /usr/sbin/pvcreate /usr/sbin/vgcreate /usr/sbin/lvcreate \
               /usr/sbin/mdadm /usr/sbin/wipefs /usr/sbin/sfdisk /usr/bin/lsblk /usr/sbin/blkid; do \
        dest="/initramfs$(dirname $bin)"; \
        mkdir -p "$dest"; \
        cp "$bin" "/initramfs${bin}"; \
        ldd "$bin" 2>/dev/null | grep -o '/[^ ]*' | while read -r lib; do \
            mkdir -p "/initramfs$(dirname $lib)"; \
            cp -n "$lib" "/initramfs${lib}" 2>/dev/null || true; \
        done; \
    done && \
    cp /lib64/ld-linux-x86-64.so.2 /initramfs/lib64/ 2>/dev/null || true && \
    cp /lib/x86_64-linux-gnu/libnss_files.so.2 /initramfs/lib/x86_64-linux-gnu/ 2>/dev/null || true && \
    mkdir -p /initramfs/lib/x86_64-linux-gnu/security && \
    cp /lib/x86_64-linux-gnu/security/pam_permit.so /initramfs/lib/x86_64-linux-gnu/security/ 2>/dev/null || true

# Init wrapper (PID 1 — sets up network then execs BOOTy)
COPY test/e2e/clab/vrnetlab/init-wrapper.sh /initramfs/init
RUN chmod +x /initramfs/init

# ── Stage 4: Runtime container with QEMU ──────────────────────────────
FROM debian:trixie-slim
RUN apt-get -o Acquire::Retries=5 update && apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
    qemu-system-x86 qemu-utils iproute2 bridge-utils cpio gzip procps \
    && rm -rf /var/lib/apt/lists/*

COPY --from=kernel /vmlinuz /opt/vmlinuz
COPY --from=initramfs /initramfs /opt/initramfs

COPY test/e2e/clab/vrnetlab/boot.sh /opt/boot.sh
RUN chmod +x /opt/boot.sh

ENTRYPOINT ["/opt/boot.sh"]
