# Test container that runs BOOTy in CAPRF mode inside containerlab.
# BOOTy is built from source and runs as a regular process (not PID 1 init).
# With a real GPT disk image (created by create-test-image.sh), provisioning
# progresses through stream-image, partprobe, parse-partitions, mount-root,
# and fails at grow-partition (growpart not available in Alpine).
FROM golang:1.27.2-alpine AS builder
RUN set -eu; \
    n=0; \
    until apk add --no-cache git ca-certificates gcc linux-headers musl-dev; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "apk add failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done
COPY go.mod go.sum /go/src/github.com/telekom/BOOTy/
WORKDIR /go/src/github.com/telekom/BOOTy
RUN set -eu; \
    n=0; \
    until go mod download; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "go mod download failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done
COPY . /go/src/github.com/telekom/BOOTy/
RUN CGO_ENABLED=1 GOOS=linux go build -a \
    -ldflags "-linkmode external -extldflags '-static' -s -w" \
    -o /booty

FROM alpine:3.24
RUN set -eu; \
    n=0; \
    until apk add --no-cache ca-certificates iproute2; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "apk add failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done
# Install FRR from the official Alpine repo for EVPN networking support.
RUN set -eu; \
    n=0; \
    until apk add --no-cache frr; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "apk add failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done
# Disk provisioning tools needed for full provisioning pipeline.
RUN set -eu; \
    n=0; \
    until apk add --no-cache e2fsprogs dosfstools sgdisk parted lvm2 util-linux; do \
      n=$((n+1)); \
      if [ "$n" -ge 5 ]; then echo "apk add failed after $n attempts" >&2; exit 1; fi; \
      sleep 10; \
    done
COPY --from=builder /booty /usr/local/bin/booty
RUN mkdir -p /deploy /tmp /etc/frr /var/run/frr && \
    chown -R frr:frr /etc/frr /var/run/frr

# Entrypoint: run BOOTy in CAPRF mode.
# BOOTy writes structured logs to stderr; direct output avoids pipe buffering.
CMD ["/bin/sh", "-c", "/usr/local/bin/booty 2>&1; sleep infinity"]

