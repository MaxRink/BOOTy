# EVPN E2E Gap Analysis: GoBGP Data Plane

> **Status:** Historical record. Gaps 1 to 5 are closed on `main`. Gap 6 was
> not confirmed. See [Current state](#current-state).
> **Date of the analysis:** 2026-03-22.
> **Pipeline:** CAPRF pipeline 60178989 (CAPRF MR #69, branch
> `feat/e2e-virtual-bm4x`). `e2e-kindmetal` passed (12m17s).
> `e2e-kindmetal-evpn` failed (39m59s, test timeout).

This document records why the CAPRF EVPN e2e suite failed with the GoBGP
ISO and how BOOTy fixed it. For the current GoBGP behaviour, see
[networking.md](networking.md). The CAPRF suite details (topology, timeouts,
`setup-evpn.sh`) are from the CAPRF repository at that date. This repository
does not contain them.

## Current state

| # | Gap | State on `main` | Code | Test coverage |
|---|---|---|---|---|
| 1 | No kernel route to the remote VTEP | Closed (commit `fdafdb4`). BOOTy installs a `/32` route to `provision_gateway`. In Type-5-only mode without a gateway, BOOTy installs a `/32` route to each VTEP learned from a Type-5 next hop. In numbered mode, the routes use the numbered neighbor egress. | `installGatewayRoute`, `ensureVTEPRoute` in `pkg/network/gobgp/stack.go` | `TestGoBGPUnnumberedGatewayRoute`, `TestGoBGPNumberedGatewayRoute`, `TestType5VM0VTEPReachable` |
| 2 | No BUM FDB entry | Closed (commit `88086d4`). BOOTy installs `00:00:00:00:00:00 -> provision_gateway` on `vx<VNI>`, except in Type-5-only mode. | `addGatewayFDB` in `pkg/network/gobgp/overlay.go` | `TestGoBGPUnnumberedGatewayFDB` |
| 3 | `watchRoutes()` only logged routes | Closed (PR #85, commit `657ab6d`). Type-2 and Type-3 routes program FDB entries. Type-5 routes now also install kernel routes, a gateway neighbor and a router MAC FDB entry. | `processRouteUpdate` in `pkg/network/gobgp/overlay.go` | `TestType5VM0ImportsVM1`, `TestType5VM1ImportsVM0` |
| 4 | Only Type-5 routes advertised | Closed. BOOTy originates Type-2 and Type-3 routes in L2 mode (`EVPN_L2_ENABLED=true` or auto-detected from `frr.conf`). | `advertiseType2`, `advertiseType3` | `TestGoBGPRouteModeL2FromFRRConfig`, `TestGoBGPRouteModeL2FromVars` |
| 5 | Asymmetric VXLAN learning | Closed by design. BOOTy keeps MAC learning off and learns through EVPN (L2 mode) or the gateway BUM entry. | `newVXLAN` (`Learning: false`) | `TestGoBGPUnnumberedVXLANPingGateway` |
| 6 | Stale "leaf AS 65500" sessions on spine01 | Not confirmed. No BOOTy change. | None | None |

The e2e suites are described in [e2e-test-matrix.md](e2e-test-matrix.md).
The Type-5-only lab (`make clab-type5-up && make test-e2e-type5`) runs
without static spine FDB entries. BOOTy must program all VTEP reachability
from EVPN routes there.

## 1. The two CAPRF e2e suites

### Non-EVPN (`e2e-kindmetal`)

| Aspect | Detail |
|---|---|
| Topology | One Linux bridge. No ContainerLab, no BGP. |
| Network mode | DHCP (dnsmasq in the host pod) |
| Path | BOOTy, L2 bridge, NAT, Kind node, CAPRF NodePort |
| BOOTy ISO | `booty.iso` |
| Timeouts | Provision 15m, test 20m |

### EVPN (`e2e-kindmetal-evpn`)

| Aspect | Detail |
|---|---|
| Topology | ContainerLab: spine01 (FRR 10.3.1, AS 65000), leaf01 (AS 65001), client (10.100.0.100) |
| Network mode | `NETWORK_MODE=gobgp`, `BGP_PEER_MODE=unnumbered` |
| Path | BOOTy, VXLAN VNI 100, spine01 `br100`, DNAT, Kind node, CAPRF NodePort |
| BOOTy ISO | `booty-gobgp.iso` |
| Timeouts | Provision 25m, BGP converge 3m, test 30m |
| Diagnostics | AfterAll dumps the BGP summary, L2VPN EVPN table, bridge FDB, controller logs and serial console |

Every packet from BOOTy to CAPRF goes through VXLAN to the remote VTEP
(spine01, `10.0.0.1`). It is then decapsulated, DNATed and forwarded to
Kind.

### Per-VM EVPN configuration (from `setup-evpn.sh`)

| VM | ASN | Underlay IP | Provision IP | Provision gateway |
|---|---|---|---|---|
| vm-0 | 65100 | 192.168.4.10 | 10.100.0.20/24 | 10.0.0.1 |
| vm-1 | 65101 | 192.168.4.11 | 10.100.0.21/24 | 10.0.0.1 |
| vm-2 | 65102 | 192.168.4.12 | 10.100.0.22/24 | 10.0.0.1 |

## 2. Observed failure

- Test: `#12 "BOOTy calls back through the EVPN overlay"`.
- Assertion: a `ProvisionerStarted` event on the `RedfishMachine` objects.
- Result: no event within 30 minutes.

```
10:51:22  Test #11 "BOOTy establishes GoBGP overlay in VMs" PASSED (15.38s)
          Serial console showed "Assigned provision IP"
10:51:37  Test #12 "BOOTy calls back through the EVPN overlay" starts polling
11:21:11  panic: test timed out after 30m0s
```

Sequence in the VM at that time:

1. BOOTy reads `/deploy/vars` and selects GoBGP.
2. The unnumbered underlay session to spine01 reaches ESTABLISHED.
3. BOOTy creates `vx100` and `br.provision` and assigns `10.100.0.20/24`.
4. BOOTy adds the BUM FDB entry `00:00:00:00:00:00 -> 10.0.0.1`.
5. The connectivity check sends HTTP `HEAD` requests to CAPRF through the
   overlay.
6. ARP for the CAPRF-side address is encapsulated with outer destination
   `10.0.0.1`. The kernel has no route to `10.0.0.1`, so it drops the
   packet.
7. The check timed out after 5 minutes and BOOTy rebooted. The cycle
   repeated. (BOOTy now makes 3 attempts with a network rebuild before it
   fails. See [Connectivity check](networking.md#connectivity-check).)

## 3. Root cause and gaps

### Gap 1: no kernel route to the remote VTEP (critical)

GoBGP keeps learned routes in its RIB only. It does not install them in the
kernel. Unlike FRR, there is no zebra. spine01 advertised its loopback, but
the kernel had no route to `10.0.0.1`, so VXLAN packets could not leave the
host.

Fix: a `/32` route to the gateway VTEP through a fabric NIC with link scope.
For the current NIC and table selection, see
[Kernel state per mode](networking.md#kernel-state-per-mode).

### Gap 2: no BUM FDB entry

Without a BUM entry, the VXLAN device had no remote VTEP for broadcast
frames, so ARP requests were dropped. Commit `88086d4` added the entry. It
was not enough alone, because Gap 1 still blocked the packets.

### Gap 3: `watchRoutes()` only logged routes

The route watcher received EVPN routes but did not program the kernel. PR
#85 added FDB programming for Type-2 and Type-3 routes, withdraw handling
and own-route filtering. At that time, Type-5 kernel routes were deferred.
They are now implemented (see [Current state](#current-state)).

### Gap 4: no Type-2 or Type-3 origination

At that time, BOOTy originated only Type-5 routes. spine01 could not learn
the BOOTy bridge MAC or flood list through BGP. The GoBGP lab topology
(`topology-gobgp.clab.yml`) still adds static BUM FDB entries on spine01 for
`booty-unnumbered`, `booty-dual` and `booty-numbered`. BOOTy now originates
Type-2 and Type-3 routes in L2 mode.

### Gap 5: asymmetric VXLAN learning

spine01 `vxlan100` uses the kernel default (MAC learning on), so spine01
learns the BOOTy MAC from the data plane. BOOTy creates its VXLAN device
with learning off. BOOTy reaches spine01 through the BUM entry (Gap 2) and,
in L2 mode, through Type-2 and Type-3 routes.

### Gap 6: stale "leaf AS 65500" sessions

spine01 showed idle sessions on its VM ports:

```
leaf(eth3) AS 65500 Idle MsgRcvd=9 MsgSent=6
leaf(eth4) AS 65500 Idle MsgRcvd=9 MsgSent=6
leaf(eth5) AS 65500 Idle MsgRcvd=9 MsgSent=6
```

The VMs use AS 65100 to 65102. One hypothesis: the base image that the VMs
boot before CAPRF attaches the BOOTy ISO starts its own BGP stack. This was
not confirmed. It did not block the suite once Gap 1 was fixed.

## 4. Failure chain (before the fixes)

```
GoBGP underlay ESTABLISHED                      OK
VXLAN device and bridge created                 OK
BUM FDB 00:00:00:00:00:00 -> 10.0.0.1           OK
Provision IP 10.100.0.20/24 assigned            OK
HTTP HEAD to CAPRF through the overlay
  ARP on br.provision -> vx100 -> outer dst 10.0.0.1
  kernel route lookup for 10.0.0.1              NO ROUTE, packet dropped
connectivity timeout -> reboot -> repeat
```

## 5. Architecture

```
+------------------------------------------------------------------+
| Kind cluster: CAPRF controller (NodePort)                        |
|   POST /status/init -> ProvisionerStarted event                  |
+------------------------------------------------------------------+
                              ^ DNAT
+------------------- spine01 (FRR, AS 65000) ----------------------+
| lo: 10.0.0.1/32       eth0 -> Kind network                       |
| br100: 10.100.0.1/24  vxlan100 (VNI 100, learning on)            |
| eth3, eth4, eth5: eBGP underlay to the VMs (link-local)          |
| iptables DNAT 10.100.0.1 -> Kind node, MASQUERADE out of eth0    |
+------------------------------------------------------------------+
        eth3                 eth4                 eth5
          |                    |                    |
+------------------+ +------------------+ +------------------+
| VM-0, AS 65100   | | VM-1, AS 65101   | | VM-2, AS 65102   |
| BOOTy GoBGP      | | BOOTy GoBGP      | | BOOTy GoBGP      |
| vx100, learn off | | vx100, learn off | | vx100, learn off |
| br.provision     | | br.provision     | | br.provision     |
| 10.100.0.20/24   | | 10.100.0.21/24   | | 10.100.0.22/24   |
+------------------+ +------------------+ +------------------+

Before the Gap 1 fix:
  VM-0 br.provision -> vx100 encap (dst 10.0.0.1) -> no route -> drop

After the Gap 1 fix:
  VM-0 br.provision -> vx100 encap (dst 10.0.0.1)
    -> route 10.0.0.1/32 dev <fabric NIC> scope link
    -> spine01 vxlan100 decap -> br100 -> DNAT -> Kind -> CAPRF
```
