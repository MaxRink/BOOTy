# Test container for GoBGP mode inside containerlab.
# Unlike booty-test.Dockerfile, this does NOT install FRR — GoBGP is compiled
# directly into the BOOTy binary and runs in-process.
FROM golang:1.27-alpine AS builder
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
    until apk add --no-cache ca-certificates iproute2 iptables; do \
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
RUN mkdir -p /deploy /tmp

CMD ["/bin/sh", "-c", "/usr/local/bin/booty 2>&1; sleep infinity"]
