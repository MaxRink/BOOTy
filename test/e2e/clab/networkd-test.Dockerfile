# systemd-networkd as the LLDP reference of the production template lab,
# one image per Ubuntu LTS release in standard support (UBUNTU selects it).
# 22.04 (systemd 249) is the default: what the legacy ramdisk sends with
# netplan "emit-lldp: true". It sends and receives LLDP on its lldp* links
# and does nothing else (no addresses, no RA).
# Keep 22.04 the first FROM: .github/dependabot.yml relies on it.
ARG UBUNTU=22.04
FROM ubuntu:22.04@sha256:5ec03bb3441e8b0bf3b4f9cd4629a1ae763010dc3035bb8da3ae6cf026486401 AS ubuntu-22.04
FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55 AS ubuntu-24.04
FROM ubuntu:26.04@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7 AS ubuntu-26.04

FROM ubuntu-${UBUNTU}
RUN set -eu; \
    n=0; \
    until apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "apt-get failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done; \
    if [ "$(dpkg-query -W -f '${Version}' systemd | cut -d. -f1)" -gt 255 ]; then \
      DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends udev; \
    fi; \
    rm -rf /var/lib/apt/lists/*; \
    printf '[Match]\nName=lldp*\n\n[Network]\nEmitLLDP=yes\nLLDP=yes\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n' \
      > /etc/systemd/network/10-lldp.network
# systemd creates the machine ID at boot; networkd uses it as chassis ID.
# networkd starts after containerlab has renamed the lab links lldp0 and
# lldp1: it does not match a link again after a rename.
# networkd newer than 255 (26.04: 259) sees the writable /sys of the
# privileged container, waits for udev ("link pending udev initialization")
# and sends nothing, so udev (installed above for those releases only)
# announces the links first.
CMD ["/bin/sh", "-c", "systemd-machine-id-setup >/dev/null 2>&1; mkdir -p /run/systemd/netif; until [ -e /sys/class/net/lldp0 ] && [ -e /sys/class/net/lldp1 ]; do sleep 1; done; if command -v udevadm >/dev/null; then /lib/systemd/systemd-udevd --daemon && udevadm trigger --action=add --subsystem-match=net && udevadm settle; fi; exec /lib/systemd/systemd-networkd"]
