#!/usr/bin/env bash
# usage: CYCLES=n test/e2e/bfd/frr-interop.sh [FRR_IMAGE]
# Runs pkg/network/bfd TestFRRInterop against a real FRR bfdd peer on a
# docker network with the fabric timers 150/150/3: 20 s after the session
# is Up, CYCLES times (default 1) the FRR peer is shut down for 3 s and
# stays up for 10 s after no shutdown. A session that goes Down within 2 s
# of an Up fails the test. Needs docker.
set -euo pipefail
image=${1:-quay.io/frrouting/frr:10.3.1}
cycles=${CYCLES:-1}
root=$(cd "$(dirname "$0")/../../.." && pwd)
name=booty-bfd-interop-$$
net=$name
tmp=$(mktemp -d)
cleanup() { docker rm -f "$name-frr" >/dev/null 2>&1 || true; docker network rm "$net" >/dev/null 2>&1 || true; rm -r "$tmp"; }
trap cleanup EXIT
docker network create --subnet 172.30.99.0/24 "$net" >/dev/null
printf 'zebra=yes\nbfdd=yes\nvtysh_enable=yes\n' > "$tmp/daemons"
cat > "$tmp/frr.conf" <<CONF
frr defaults datacenter
bfd
 peer 172.30.99.3
  transmit-interval 150
  receive-interval 150
  detect-multiplier 3
CONF
docker run -d --name "$name-frr" --network "$net" --ip 172.30.99.2 --privileged \
	-v "$tmp/daemons:/etc/frr/daemons:ro" -v "$tmp/frr.conf:/etc/frr/frr.conf:ro" "$image" >/dev/null
sleep 5
(
	# 20 s after FRR has the session Up, shut the peer down for 3 s.
	until docker exec "$name-frr" vtysh -c 'show bfd peer 172.30.99.3 json' 2>/dev/null | grep -q '"status":"up"'; do sleep 1; done
	sleep 20
	for _ in $(seq "$cycles"); do
		docker exec "$name-frr" vtysh -c 'configure terminal' -c 'bfd' -c 'peer 172.30.99.3' -c 'shutdown' >/dev/null
		sleep 3
		docker exec "$name-frr" vtysh -c 'configure terminal' -c 'bfd' -c 'peer 172.30.99.3' -c 'no shutdown' >/dev/null
		sleep 10
	done
) &
docker run --rm --network "$net" --ip 172.30.99.3 -v "$root":/src:ro -v "${GOMODCACHE:-$HOME/go/pkg/mod}":/go/pkg/mod -w /src \
	-e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=local -e BFD_FRR_PEER=172.30.99.2 -e BFD_FRR_DURATION=$((45 + cycles * 13))s \
	golang:1.27.2 go test -tags bfd_frr -count=1 -v -run TestFRRInterop ./pkg/network/bfd/
wait
docker exec "$name-frr" vtysh -c 'show bfd peers counters' | grep -E 'peer|Session up|Session down' || true
