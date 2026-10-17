#!/bin/sh
# One-uplink template: the second server ports are not cabled, so the leaf
# does not send router advertisements there (a kernel RA default route on
# the unused NIC would win over the Type-5 default in zebra).
set -e
for i in 1 3 5 6 7 9; do ip link set dev eth$i mtu 9100 up; done
for i in 2 4 8 10; do ip link set dev eth$i down; done
