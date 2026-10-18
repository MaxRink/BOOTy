# systemd-networkd (systemd 249, Ubuntu 22.04) as the LLDP reference of
# the production template lab: what the legacy ramdisk sends with netplan
# "emit-lldp: true". It sends and receives LLDP on its lldp* links and
# does nothing else (no addresses, no RA).
FROM ubuntu:22.04
RUN set -eu; \
    n=0; \
    until apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "apt-get failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done; \
    rm -rf /var/lib/apt/lists/*; \
    printf '[Match]\nName=lldp*\n\n[Network]\nEmitLLDP=yes\nLLDP=yes\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n' \
      > /etc/systemd/network/10-lldp.network
# systemd creates the machine ID at boot; networkd uses it as chassis ID.
# networkd starts after containerlab has renamed the lab links lldp0 and
# lldp1: it does not match a link again after a rename.
CMD ["/bin/sh", "-c", "systemd-machine-id-setup >/dev/null 2>&1; mkdir -p /run/systemd/netif; until [ -e /sys/class/net/lldp0 ] && [ -e /sys/class/net/lldp1 ]; do sleep 1; done; exec /lib/systemd/systemd-networkd"]
