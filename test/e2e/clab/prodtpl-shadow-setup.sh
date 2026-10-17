#!/bin/sh
# What netplan/networkd makes of the production template on the shadow
# node: link-local: [] is addrgenmode none, accept-ra: false is accept_ra 0,
# hairpin and port-mac-learning are bridge port flags.
set -e
ip link add Vrf_underlay type vrf table 10
ip link set Vrf_underlay up
ip link set ens1f0np0 master Vrf_underlay mtu 9100 up
ip link set ens1f1np1 master Vrf_underlay mtu 9100 up
ip link set ens1f2np2 master Vrf_underlay mtu 9100 up
ip link set ens1f3np3 mtu 9100 up
ip link add dum.underlay type dummy
ip link set dum.underlay master Vrf_underlay mtu 9100 up
ip addr add 192.0.2.18/32 dev dum.underlay
ip -6 addr add 2001:db8:40a:5481::18/128 dev lo
for d in ens1f0np0 ens1f1np1 ens1f2np2 ens1f3np3; do sysctl -qw net.ipv6.conf.$d.accept_ra=0; done
ip link add vx.3000 type vxlan id 3000 local 192.0.2.18 dstport 4789 nolearning noproxy dev dum.underlay
ip link add br.provision address 02:54:c0:00:02:12 type bridge stp_state 0
ip link set vx.3000 addrgenmode none
ip link set br.provision addrgenmode none
for d in vx.3000 br.provision; do echo 0 > /proc/sys/net/ipv6/conf/$d/accept_ra; done
ip link set vx.3000 address 02:54:c0:00:02:12 master br.provision mtu 9000 up
bridge link set dev vx.3000 hairpin on learning off
ip link set br.provision mtu 9000 up
