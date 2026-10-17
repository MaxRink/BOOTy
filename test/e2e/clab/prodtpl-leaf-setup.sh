#!/bin/sh
set -e
for i in 1 2 3 4 5 6 7 8 9 10; do ip link set dev eth$i mtu 9100 up; done
