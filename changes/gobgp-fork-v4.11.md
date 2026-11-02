---
section: Behaviour changes in all modes
---

- GoBGP mode: the GoBGP fork moves to upstream master 629e5ae8, after
  v4.11.0 (fork tag v4.11.0-booty.1) (#874). The fork drops the fixes that
  upstream has merged: EVPN NLRIs of unknown route types, the EVPN
  ARP/ND extended community, the IPv6 zone of a neighbor set entry, the
  admin state idempotence and request queue, a new connection from a
  restarting peer, a connection that arrives while the idle hold time is
  0, out-of-range RD/RT and community values, and per-bind-interface BFD
  listeners. It keeps the FRR idle hold backoff, `BfdDown`,
  `ConnectAtOnce`, Enhanced Route Refresh, Paths-Limit and the collision
  cases that upstream does not cover.

  GoBGP now accepts a connection that arrives right after `AddPeer`
  instead of closing it at random, takes a new connection from a
  restarting peer as a graceful restart, and sends Cease/BFD Down and
  Administrative Reset as a Hard Reset when the N bit was exchanged, like
  FRR. An EVPN MAC/IP route with a MAC length other than 48 is malformed
  and resets the session like FRR and BIRD. BOOTy keeps the graceful
  restart stale routes when it disables a peer on carrier loss
  (v4.11.0-booty.1), as before.
