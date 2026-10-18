
SHELL := /bin/sh

TARGET := booty
.DEFAULT_GOAL := build

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0)
BUILD := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
# Committer time of HEAD, the earliest time the time sync accepts.
COMMIT_TIME := $(shell git log -1 --format=%cI 2>/dev/null)

TARGETOS=linux
TARGETARCH ?= $(shell go env GOARCH)

GO_LDFLAGS = -s -w -X=main.Version=$$VERSION -X=main.Build=$$BUILD -X=github.com/telekom/BOOTy/pkg/buildinfo.commitTime=$(COMMIT_TIME) -extldflags -static
# grpcnotrace drops gRPC's x/net/trace event log. GoBGP sets
# grpc.EnableTracing = false, so the traced code never runs.
GO_TAGS = grpcnotrace

SRC = $(shell find . -type f -name '*.go' -not -path "./vendor/*")

DOCKERTAG ?= $(VERSION)
REPOSITORY = ghcr.io/telekom/booty

COVERAGE_PROFILE ?= coverage.out
COVERAGE_HTML ?= coverage.html
COVERAGE_THRESHOLD ?= 40.0

CLEAN_FILES := \
	$(COVERAGE_PROFILE) \
	$(COVERAGE_HTML) \
	booty.iso \
	booty-gobgp.iso \
	initramfs.cpio.gz \
	initramfs.cpio.gz.sha256 \
	initramfs.cpio.zst \
	initramfs.cpio.zst.sha256

# Keep the version in sync with the golangci-lint step in .github/workflows/ci.yml.
# CI lints with the Go version from go.mod. golangci-lint and staticcheck can
# crash with a newer local Go, so build and run the linter with that version.
GOLANGCI_LINT_VERSION ?= v2.14.0
LINT_GOTOOLCHAIN ?= go$(shell sed -n 's/^go //p' go.mod)
LINT_HOSTOS := $(shell go env GOHOSTOS)
LINT_EXE := $(if $(filter windows,$(LINT_HOSTOS)),.exe)
GOLANGCI_LINT ?= bin/golangci-lint-$(GOLANGCI_LINT_VERSION)-$(LINT_GOTOOLCHAIN)-$(LINT_HOSTOS)-$(shell go env GOHOSTARCH)$(LINT_EXE)

CLAB_TEST_IMAGE ?= test/e2e/clab/images/test.img.gz

.PHONY: all build build-all build-esp-sync clean install uninstall fmt fmt-check lint test docker dockerx86 iso slim micro gobgp gobgp-iso dockerx86slim dockerx86micro dockerx86gobgp arm64 arm64-slim arm64-gobgp test-iso getramdisk getramdisk-arm64 test-kvm test-e2e clab-test-image clab-up clab-down test-e2e-integration clab-boot-up clab-boot-down test-e2e-boot booty-test-image booty-vrnetlab-image clab-vrnetlab-up clab-vrnetlab-down test-e2e-vrnetlab booty-gobgp-test-image clab-gobgp-up clab-gobgp-down test-e2e-gobgp clab-gobgp-vrnetlab-up clab-gobgp-vrnetlab-down test-e2e-gobgp-vrnetlab clab-type5-up clab-type5-down test-e2e-type5 clab-prodtpl-up clab-prodtpl-down test-e2e-prodtpl clab-production-up clab-production-down test-e2e-production test-e2e-production-full booty-dhcpd-test-image clab-dhcp-up clab-dhcp-down test-e2e-dhcp clab-bond-up clab-bond-down test-e2e-bond clab-lacp-up clab-lacp-down test-e2e-lacp clab-static-up clab-static-down test-e2e-static clab-multi-nic-up clab-multi-nic-down test-e2e-multi-nic booty-lldpd-test-image clab-cablecheck-up clab-cablecheck-down test-e2e-cablecheck check check-build-vars check-docker-vars check-oci-vars oci-push oci-push-initramfs oci-push-binary changelog changelog-release

export TARGET VERSION BUILD TARGETOS TARGETARCH DOCKERTAG REPOSITORY

all: lint test install

check-build-vars:
	@reject_multiline() { [ "$$(printf '%s' "$$2" | wc -l | tr -d ' ')" = 0 ] || { printf 'ERROR: invalid %s: %s\n' "$$1" "$$2"; exit 2; }; }; \
	reject_multiline TARGET "$$TARGET"; \
	reject_multiline VERSION "$$VERSION"; \
	reject_multiline BUILD "$$BUILD"; \
	reject_multiline TARGETOS "$$TARGETOS"; \
	reject_multiline TARGETARCH "$$TARGETARCH"
	@printf '%s\n' "$$TARGET" | grep -Eq '^[A-Za-z0-9_./][A-Za-z0-9_./ -]*$$' || { printf 'ERROR: invalid TARGET: %s\n' "$$TARGET"; exit 2; }
	@case "$$TARGET" in *..*) printf 'ERROR: invalid TARGET: %s\n' "$$TARGET"; exit 2 ;; esac
	@printf '%s\n' "$$VERSION" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._+~-]*$$' || { printf 'ERROR: invalid VERSION: %s\n' "$$VERSION"; exit 2; }
	@printf '%s\n' "$$BUILD" | grep -Eq '^([A-Fa-f0-9]{7,64}|unknown)$$' || { printf 'ERROR: invalid BUILD: %s\n' "$$BUILD"; exit 2; }
	@printf '%s\n' "$$TARGETOS" | grep -Eq '^[A-Za-z0-9_.-]+$$' || { printf 'ERROR: invalid TARGETOS: %s\n' "$$TARGETOS"; exit 2; }
	@printf '%s\n' "$$TARGETARCH" | grep -Eq '^[A-Za-z0-9_.-]+$$' || { printf 'ERROR: invalid TARGETARCH: %s\n' "$$TARGETARCH"; exit 2; }

check-docker-vars:
	@reject_multiline() { [ "$$(printf '%s' "$$2" | wc -l | tr -d ' ')" = 0 ] || { printf 'ERROR: invalid %s: %s\n' "$$1" "$$2"; exit 2; }; }; \
	reject_multiline REPOSITORY "$$REPOSITORY"; \
	reject_multiline DOCKERTAG "$$DOCKERTAG"
	@printf '%s\n' "$$REPOSITORY" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)+$$' || { printf 'ERROR: invalid REPOSITORY: %s\n' "$$REPOSITORY"; exit 2; }
	@printf '%s\n' "$$DOCKERTAG" | grep -Eq '^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$$' || { printf 'ERROR: invalid DOCKERTAG: %s\n' "$$DOCKERTAG"; exit 2; }

check-oci-vars: check-docker-vars check-build-vars
	@reject_multiline() { [ "$$(printf '%s' "$$2" | wc -l | tr -d ' ')" = 0 ] || { printf 'ERROR: invalid %s: %s\n' "$$1" "$$2"; exit 2; }; }; \
	reject_multiline OCI_FLAVOR "$$OCI_FLAVOR"; \
	reject_multiline OCI_ARCH "$$OCI_ARCH"
	@case "$$OCI_FLAVOR" in default|slim|micro|gobgp) ;; *) printf 'ERROR: invalid OCI_FLAVOR: %s\n' "$$OCI_FLAVOR"; exit 2 ;; esac
	@case "$$OCI_ARCH" in amd64|arm64) ;; *) printf 'ERROR: invalid OCI_ARCH: %s\n' "$$OCI_ARCH"; exit 2 ;; esac

build: check-build-vars
	@mkdir -p .build
	@build_vars="TARGET=$$TARGET TARGETOS=$$TARGETOS TARGETARCH=$$TARGETARCH VERSION=$$VERSION BUILD=$$BUILD"; \
	vars_file=.build/build.vars; \
	if [ -f "$$TARGET" ] && [ -f "$$vars_file" ] && [ "$$(cat "$$vars_file")" = "$$build_vars" ] && ! find . -type f -name '*.go' -not -path "./vendor/*" -newer "$$TARGET" | grep -q .; then \
		printf '%s\n' "$$TARGET is up to date"; \
		exit 0; \
	fi; \
	GOOS="$$TARGETOS" GOARCH="$$TARGETARCH" go build -trimpath -tags "$(GO_TAGS)" -ldflags "$(GO_LDFLAGS)" -o "$$TARGET"; \
	printf '%s\n' "$$build_vars" > "$$vars_file"

build-all: check-build-vars $(SRC)
	@mkdir -p dist/amd64 dist/arm64
	@GOOS="$$TARGETOS" GOARCH=amd64 go build -trimpath -tags "$(GO_TAGS)" -ldflags "$(GO_LDFLAGS)" -o "dist/amd64/$$TARGET"
	@GOOS="$$TARGETOS" GOARCH=arm64 go build -trimpath -tags "$(GO_TAGS)" -ldflags "$(GO_LDFLAGS)" -o "dist/arm64/$$TARGET"
	@CGO_ENABLED=0 GOOS="$$TARGETOS" GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/amd64/booty-esp-sync ./cmd/booty-esp-sync
	@CGO_ENABLED=0 GOOS="$$TARGETOS" GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/arm64/booty-esp-sync ./cmd/booty-esp-sync

build-esp-sync: check-build-vars
	@CGO_ENABLED=0 GOOS="$$TARGETOS" GOARCH="$$TARGETARCH" go build -trimpath -ldflags "-s -w" -o booty-esp-sync ./cmd/booty-esp-sync

clean: check-build-vars
	@rm -f -- "$$TARGET" "$${TARGET}.sha256"
	@rm -f -- $(CLEAN_FILES)
	@rm -rf dist .build

install: check-build-vars
	@echo Building and Installing project
	@go install -trimpath -tags "$(GO_TAGS)" -ldflags "$(GO_LDFLAGS)"

uninstall: check-build-vars clean
	@target_path=$$(command -v -- "$$TARGET" 2>/dev/null || true); \
	if [ -n "$$target_path" ]; then rm -f -- "$$target_path"; fi

$(GOLANGCI_LINT):
	@mkdir -p $(@D)
	@tmp=$$(mktemp -d) && \
	GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) GOBIN="$$tmp" go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) && \
	mv "$$tmp/golangci-lint$(LINT_EXE)" "$@" && rmdir "$$tmp"

fmt: $(GOLANGCI_LINT)
	@gofmt -l -w $(SRC)
	@GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) $(GOLANGCI_LINT) fmt

fmt-check: $(GOLANGCI_LINT)
	@files=$$(gofmt -l $(SRC)); \
	if [ -n "$$files" ]; then \
		printf '%s\n' "$$files"; \
		exit 1; \
	fi
	@GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) $(GOLANGCI_LINT) fmt --diff

# CHANGELOG.md sections are generated by hack/changelog.py with git-cliff.
# CI uses git-cliff GIT_CLIFF_VERSION from .github/workflows/changelog.yml.
GIT_CLIFF ?= git-cliff

# Print the next release section: commits since the last tag plus changes/*.md.
changelog:
	@GIT_CLIFF="$(GIT_CLIFF)" python3 -B hack/changelog.py preview

# Write the section into CHANGELOG.md as RELEASE and delete changes/*.md.
changelog-release:
	@test -n "$(RELEASE)" || { echo "usage: make changelog-release RELEASE=0.2.0-rc.16"; exit 1; }
	@GIT_CLIFF="$(GIT_CLIFF)" python3 -B hack/changelog.py release "$(RELEASE)"

# BOOTy runs on Linux only, so lint the Linux build like CI does, also on macOS.
lint: $(GOLANGCI_LINT)
	@GOTOOLCHAIN=$(LINT_GOTOOLCHAIN) GOOS=$(TARGETOS) $(GOLANGCI_LINT) run ./...

test:
	@go test -race -coverprofile=$(COVERAGE_PROFILE) ./...
	@go tool cover -func=$(COVERAGE_PROFILE) | awk -v min="$(COVERAGE_THRESHOLD)" '\
		BEGIN { \
			threshold = min; \
			sub(/%$$/, "", threshold); \
			if (threshold !~ /^[0-9]+([.][0-9]+)?$$/) { \
				printf "invalid COVERAGE_THRESHOLD: %s\n", min; \
				invalid = 1; \
				exit 2; \
			} \
		} \
		{ print } \
		/^total:/ { coverage = $$3; sub(/%$$/, "", coverage); found = 1 } \
		END { \
			if (invalid) { exit 2 } \
			if (!found) { print "coverage check failed: total coverage line not found"; exit 1 } \
			if (coverage + 0 < threshold + 0) { \
				printf "coverage %s%% is below %s%% threshold\n", coverage, threshold; \
				exit 1; \
			} \
			printf "coverage %s%% meets %s%% threshold\n", coverage, threshold; \
		}'
	@go tool cover -html=$(COVERAGE_PROFILE) -o $(COVERAGE_HTML)
	@echo "Coverage report: $(COVERAGE_HTML)"

dockerx86: check-docker-vars
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --load -t "$${REPOSITORY}:$${DOCKERTAG}" -f initrd.Dockerfile .

docker: check-docker-vars
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64,linux/arm64 --push -t "$${REPOSITORY}:$${DOCKERTAG}" -f initrd.Dockerfile .

iso:
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target iso --output type=local,dest=. -f initrd.Dockerfile .
	@echo ISO built: booty.iso

slim:
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target slim --output type=local,dest=. -f initrd.Dockerfile .
	@echo Slim initramfs built: initramfs.cpio.zst

micro:
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target micro --output type=local,dest=. -f initrd.Dockerfile .
	@echo Micro initramfs built: initramfs.cpio.gz

gobgp:
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target gobgp --output type=local,dest=. -f initrd.Dockerfile .
	@echo GoBGP initramfs built: initramfs.cpio.zst

gobgp-iso:
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target gobgp-iso --output type=local,dest=. -f initrd.Dockerfile .
	@echo GoBGP ISO built: booty-gobgp.iso

dockerx86slim: check-docker-vars
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target slim --load -t "$${REPOSITORY}:$${DOCKERTAG}-slim" -f initrd.Dockerfile .

dockerx86micro: check-docker-vars
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target micro --load -t "$${REPOSITORY}:$${DOCKERTAG}-micro" -f initrd.Dockerfile .

dockerx86gobgp: check-docker-vars
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/amd64 --target gobgp --load -t "$${REPOSITORY}:$${DOCKERTAG}-gobgp" -f initrd.Dockerfile .

arm64: check-docker-vars
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/arm64 --load -t "$${REPOSITORY}:$${DOCKERTAG}-arm64" -f initrd.Dockerfile .

arm64-slim:
	@mkdir -p dist/arm64
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/arm64 --target slim --output type=local,dest=dist/arm64 -f initrd.Dockerfile .
	@echo ARM64 slim initramfs built: dist/arm64/initramfs.cpio.zst

arm64-gobgp:
	@mkdir -p dist/arm64
	@docker buildx build --build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" --platform linux/arm64 --target gobgp --output type=local,dest=dist/arm64 -f initrd.Dockerfile .
	@echo ARM64 GoBGP initramfs built: dist/arm64/initramfs.cpio.zst

test-iso:
	@echo Verifying ISO hybrid boot record
	@file booty.iso | grep -q "ISO 9660" || (echo "FAIL: not a valid ISO"; exit 1)
	@echo PASS

# This is typically only for quick testing
getramdisk: check-docker-vars

	@ID=$$(docker create "$${REPOSITORY}:$${DOCKERTAG}" null); \
	trap 'docker rm "$$ID" >/dev/null 2>&1 || true' EXIT; \
	docker cp "$$ID:/initramfs.cpio.zst" initramfs.cpio.zst
	@echo Extracted ramdisk

getramdisk-arm64: check-docker-vars
	@mkdir -p dist/arm64
	@ID=$$(docker create "$${REPOSITORY}:$${DOCKERTAG}-arm64" null); \
	trap 'docker rm "$$ID" >/dev/null 2>&1 || true' EXIT; \
	docker cp "$$ID:/initramfs.cpio.zst" dist/arm64/initramfs.cpio.zst
	@echo Extracted ARM64 ramdisk to dist/arm64/

simplify:
	@gofmt -s -l -w $(SRC)

test-e2e:
	@echo Running E2E tests
	@packages=$$(go list -tags e2e ./test/e2e/...) || exit $$?; \
	packages=$$(printf '%s\n' "$$packages" | awk '$$0 !~ /\/kvm$$/'); \
	if [ -z "$$packages" ]; then \
		printf '%s\n' 'no non-KVM e2e packages discovered' >&2; \
		exit 1; \
	fi; \
	printf '%s\n' "$$packages" | xargs go test -tags e2e -race -v -timeout 20m

test-kvm:
	@printf '%s\n' 'Running KVM E2E tests (requires QEMU, root, and KVM assets)'
	@go test -tags e2e -race -count=1 -v -timeout 15m ./test/e2e/kvm/...

clab-test-image: $(CLAB_TEST_IMAGE)

$(CLAB_TEST_IMAGE): test/e2e/clab/create-test-image.sh
	@printf '%s\n' 'Generating test disk image (requires root)'
	@sudo test/e2e/clab/create-test-image.sh $(dir $@)

clab-up: $(CLAB_TEST_IMAGE)
	@echo Deploying ContainerLab topology
	@cd test/e2e/clab && sudo clab deploy --topo topology.clab.yml

clab-down:
	@echo Destroying ContainerLab topology
	@cd test/e2e/clab && sudo clab destroy --topo topology.clab.yml --cleanup

test-e2e-integration:
	@printf '%s\n' 'Running E2E integration tests (requires clab-up)'
	@BOOTY_TOPOLOGY=$${BOOTY_TOPOLOGY:-lab} go test -tags e2e_integration -race -v -timeout 120s ./test/e2e/integration/...

booty-test-image:
	@echo Building BOOTy test container image
	@docker build -t booty-test:latest -f test/e2e/clab/booty-test.Dockerfile .

clab-boot-up: booty-test-image $(CLAB_TEST_IMAGE)
	@printf '%s\n' 'Deploying boot test topology (includes BOOTy nodes)'
	@cd test/e2e/clab && sudo clab deploy --topo topology-boot.clab.yml

clab-boot-down:
	@echo Destroying boot test topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-boot.clab.yml --cleanup

test-e2e-boot:
	@printf '%s\n' 'Running BOOTy boot E2E tests (requires clab-boot-up)'
	@go test -tags e2e_boot -race -v -timeout 30m ./test/e2e/integration/...

booty-vrnetlab-image:
	@echo Building BOOTy vrnetlab VM image
	@docker build -t booty-vrnetlab:latest -f test/e2e/clab/vrnetlab/Dockerfile .

clab-vrnetlab-up: booty-vrnetlab-image $(CLAB_TEST_IMAGE)
	@printf '%s\n' 'Deploying vrnetlab EVPN topology (QEMU VMs + EVPN fabric)'
	@cd test/e2e/clab && sudo clab deploy --topo topology-vrnetlab.clab.yml

clab-vrnetlab-down:
	@echo Destroying vrnetlab EVPN topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-vrnetlab.clab.yml --cleanup

test-e2e-vrnetlab:
	@printf '%s\n' 'Running vrnetlab EVPN E2E tests (requires clab-vrnetlab-up)'
	@go test -tags e2e_vrnetlab -race -v -timeout 600s ./test/e2e/integration/...

# ── GoBGP e2e targets ──────────────────────────────────────────────────────

booty-gobgp-test-image:
	@printf '%s\n' 'Building BOOTy GoBGP test container image (no FRR)'
	@docker build -t booty-gobgp-test:latest -f test/e2e/clab/booty-gobgp-test.Dockerfile .

clab-gobgp-up: booty-gobgp-test-image $(CLAB_TEST_IMAGE)
	@printf '%s\n' 'Deploying GoBGP test topology (unnumbered + dual + numbered)'
	@cd test/e2e/clab && sudo clab deploy --topo topology-gobgp.clab.yml

clab-gobgp-down:
	@echo Destroying GoBGP test topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-gobgp.clab.yml --cleanup

test-e2e-gobgp:
	@printf '%s\n' 'Running GoBGP E2E tests (requires clab-gobgp-up)'
	@go test -tags e2e_gobgp -race -v -timeout 300s ./test/e2e/integration/...

clab-gobgp-vrnetlab-up: booty-vrnetlab-image $(CLAB_TEST_IMAGE)
	@printf '%s\n' 'Deploying GoBGP vrnetlab topology (QEMU VMs, all PeerModes)'
	@cd test/e2e/clab && sudo clab deploy --topo topology-gobgp-vrnetlab.clab.yml

clab-gobgp-vrnetlab-down:
	@echo Destroying GoBGP vrnetlab topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-gobgp-vrnetlab.clab.yml --cleanup

test-e2e-gobgp-vrnetlab:
	@printf '%s\n' 'Running GoBGP vrnetlab E2E tests (requires clab-gobgp-vrnetlab-up)'
	@go test -tags e2e_gobgp_vrnetlab -race -v -timeout 600s ./test/e2e/integration/...

# ── Pure Type-5 e2e targets (CAPRF per-machine leaf fabric) ───────────────

clab-type5-up: booty-gobgp-test-image
	@printf '%s\n' 'Deploying pure Type-5 topology (spine + per-machine leaf, VNI 1000)'
	@cd test/e2e/clab && sudo clab deploy --topo topology-type5.clab.yml

clab-type5-down:
	@echo Destroying pure Type-5 topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-type5.clab.yml --cleanup

test-e2e-type5:
	@printf '%s\n' 'Running pure Type-5 E2E tests (requires clab-type5-up)'
	@go test -tags e2e_gobgp_type5 -race -v -timeout 300s ./test/e2e/integration/...

# ── Production template e2e targets (#630 slice b) ─────────────────────────

clab-prodtpl-up: booty-gobgp-test-image
	@printf '%s\n' 'Deploying the production template topology (BOOTy and shadow FRR)'
	@cd test/e2e/clab && sudo clab deploy --topo topology-production-templates.clab.yml

clab-prodtpl-down:
	@echo Destroying the production template topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-production-templates.clab.yml --cleanup

test-e2e-prodtpl:
	@printf '%s\n' 'Running production template E2E tests (requires clab-prodtpl-up)'
	@go test -tags e2e_prodtpl -race -v -count=1 -timeout 15m ./test/e2e/integration/...

# ── Production-realistic e2e targets ───────────────────────────────────────

PRODUCTION_CI_TESTS := ^(TestProductionBootyStartsSuccessfully|TestProductionCAPRFModeDetected|TestProductionFRRNetworkModeSelected|TestProductionSpineBGPEstablished|TestProductionDCGWBGPEstablished|TestProductionSpineDCGWBGPEstablished|TestProductionEVPNAddressFamilyOnSpine|TestProductionEVPNAddressFamilyOnDCGW|TestProductionVXLANInterfaceCreated|TestProductionProvisionBridgeIP|TestProductionUnderlayRouteOnSpine|TestProductionUnderlayRouteOnDCGW|TestProductionOverlayReachClient|TestProductionOverlayReachNginx|TestProductionOverlayReachCAPRF|TestProductionGatewayFDB|TestProductionGatewayRoute)$$

clab-production-up: booty-test-image $(CLAB_TEST_IMAGE)
	@printf '%s\n' 'Deploying production-realistic topology'
	@cd test/e2e/clab && sudo clab deploy --topo topology-production.clab.yml

clab-production-down:
	@echo Destroying production-realistic topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-production.clab.yml --cleanup

test-e2e-production:
	@printf '%s\n' 'Running CI-proven production E2E smoke tests (requires clab-production-up)'
	@go test -tags e2e_production -race -v -run '$(PRODUCTION_CI_TESTS)' -timeout 600s ./test/e2e/integration/...

test-e2e-production-full:
	@printf '%s\n' 'Running full production E2E tests, including known unproven limitations (requires clab-production-up)'
	@go test -tags e2e_production -race -v -timeout 600s ./test/e2e/integration/...

# ── DHCP lab targets ───────────────────────────────────────────────────────

booty-dhcpd-test-image:
	@printf '%s\n' 'Building DHCP server test image'
	@docker build -t booty-dhcpd-test:latest -f test/e2e/clab/dhcpd-test.Dockerfile test/e2e/clab

clab-dhcp-up: booty-dhcpd-test-image
	@echo Deploying DHCP test topology
	@cd test/e2e/clab && sudo clab deploy --topo topology-dhcp.clab.yml

clab-dhcp-down:
	@echo Destroying DHCP test topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-dhcp.clab.yml --cleanup

test-e2e-dhcp:
	@printf '%s\n' 'Running DHCP E2E tests (requires clab-dhcp-up)'
	@BOOTY_TOPOLOGY=dhcp go test -tags e2e_integration -race -v -timeout 120s ./test/e2e/integration/... -run TestContainerLabTopologySmoke

# ── Bonding (non-LACP) lab targets ───────────────────────────────────────

clab-bond-up:
	@printf '%s\n' 'Deploying bond-mode (non-LACP) test topology'
	@cd test/e2e/clab && sudo clab deploy --topo topology-lacp.clab.yml

clab-bond-down:
	@printf '%s\n' 'Destroying bond-mode (non-LACP) test topology'
	@cd test/e2e/clab && sudo clab destroy --topo topology-lacp.clab.yml --cleanup

test-e2e-bond:
	@printf '%s\n' 'Running bond-mode (non-LACP) E2E tests (requires clab-bond-up)'
	@BOOTY_TOPOLOGY=bond go test -tags e2e_integration -race -v -timeout 120s ./test/e2e/integration/... -run TestContainerLabTopologySmoke

# Backward-compatible aliases for the historical clab-lacp-* target names.
# topology-lacp.clab.yml validates bond connectivity, not 802.3ad negotiation.
clab-lacp-up: clab-bond-up
clab-lacp-down: clab-bond-down
test-e2e-lacp: test-e2e-bond

# ── Static IP lab targets ─────────────────────────────────────────────────

clab-static-up:
	@echo Deploying static IP test topology
	@cd test/e2e/clab && sudo clab deploy --topo topology-static.clab.yml

clab-static-down:
	@echo Destroying static IP test topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-static.clab.yml --cleanup

test-e2e-static:
	@printf '%s\n' 'Running static IP E2E tests (requires clab-static-up)'
	@BOOTY_TOPOLOGY=static go test -tags e2e_integration -race -v -timeout 120s ./test/e2e/integration/... -run TestContainerLabTopologySmoke

# ── Cable check lab targets ───────────────────────────────────────────────

booty-lldpd-test-image:
	@docker build -t booty-lldpd-test:latest -f test/e2e/clab/lldpd-test.Dockerfile test/e2e/clab

clab-cablecheck-up: booty-gobgp-test-image booty-lldpd-test-image
	@printf '%s\n' 'Deploying the cable check topology (BOOTy and an lldpd leaf)'
	@cd test/e2e/clab && sudo clab deploy --topo topology-cablecheck.clab.yml

clab-cablecheck-down:
	@cd test/e2e/clab && sudo clab destroy --topo topology-cablecheck.clab.yml --cleanup

test-e2e-cablecheck:
	@printf '%s\n' 'Running cable check E2E tests (requires clab-cablecheck-up)'
	@go test -tags e2e_cablecheck -race -v -count=1 -timeout 10m ./test/e2e/integration/...

# ── Multi-NIC lab targets ─────────────────────────────────────────────────

clab-multi-nic-up:
	@echo Deploying multi-NIC test topology
	@cd test/e2e/clab && sudo clab deploy --topo topology-multi-nic.clab.yml

clab-multi-nic-down:
	@echo Destroying multi-NIC test topology
	@cd test/e2e/clab && sudo clab destroy --topo topology-multi-nic.clab.yml --cleanup

test-e2e-multi-nic:
	@printf '%s\n' 'Running multi-NIC E2E tests (requires clab-multi-nic-up)'
	@BOOTY_TOPOLOGY=multi-nic go test -tags e2e_integration -race -v -timeout 120s ./test/e2e/integration/... -run TestContainerLabTopologySmoke

check: fmt-check
	@go vet ./...

run: install

# --- OCI artifact publishing (requires: oras, ghcr.io login) ---

OCI_FLAVOR ?= default
OCI_ARCH ?= $(TARGETARCH)
# default, slim, and gobgp flavors use zstd compression (.zst); micro uses gzip (.gz).
ZSTD_INITRAMFS_FLAVORS := default slim gobgp
VALID_INITRAMFS_FLAVORS := default slim gobgp micro
override OCI_INITRAMFS_DIR := dist/oci/$(OCI_FLAVOR)-$(OCI_ARCH)
override OCI_INITRAMFS_BASENAME := $(if $(filter $(OCI_FLAVOR),$(ZSTD_INITRAMFS_FLAVORS)),initramfs.cpio.zst,initramfs.cpio.gz)
override INITRAMFS_PATH := $(OCI_INITRAMFS_DIR)/$(OCI_INITRAMFS_BASENAME)

oci-push: oci-push-initramfs oci-push-binary
	@printf 'Initramfs and binary OCI artifacts pushed for %s/%s\n' "$$OCI_FLAVOR" "$$OCI_ARCH"

override INITRAMFS_MEDIA_TYPE := $(if $(filter $(OCI_FLAVOR),$(ZSTD_INITRAMFS_FLAVORS)),application/vnd.cncf.initramfs.layer.v1+zstd,application/vnd.cncf.initramfs.layer.v1+gzip)

export VERSION DOCKERTAG REPOSITORY TARGET OCI_FLAVOR OCI_ARCH OCI_INITRAMFS_DIR INITRAMFS_PATH INITRAMFS_MEDIA_TYPE

ensure_oci_ref_absent = @ref="$(1)"; err=$$(mktemp "$${TMPDIR:-/tmp}/oci-ref-check.XXXXXX") || { echo "ERROR: could not create temporary file for OCI ref check" >&2; exit 1; }; trap 'rm -f "$$err"' EXIT; if oras manifest fetch "$$ref" >/dev/null 2>"$$err"; then echo "ERROR: OCI artifact $$ref already exists; refusing to overwrite"; exit 1; fi; if ! grep -Eiq '(not found|manifest unknown|name unknown|404)' "$$err"; then echo "ERROR: could not verify OCI artifact $$ref is absent"; cat "$$err" >&2 || true; exit 1; fi

oci-push-initramfs: check-oci-vars
	$(call ensure_oci_ref_absent,$${REPOSITORY}/initramfs:$${DOCKERTAG}-$${OCI_FLAVOR}-$${OCI_ARCH})
	@mkdir -p "$$OCI_INITRAMFS_DIR"
	@rm -f "$$OCI_INITRAMFS_DIR"/initramfs.cpio.zst "$$OCI_INITRAMFS_DIR"/initramfs.cpio.gz \
		"$$OCI_INITRAMFS_DIR"/initramfs.cpio.zst.sha256 "$$OCI_INITRAMFS_DIR"/initramfs.cpio.gz.sha256
	@target_arg=""; \
	if [ "$$OCI_FLAVOR" != "default" ]; then target_arg="--target=$$OCI_FLAVOR"; fi; \
	docker buildx build --platform "linux/$$OCI_ARCH" $$target_arg \
		--build-arg "BOOTY_COMMIT_TIME=$(COMMIT_TIME)" \
		--output "type=local,dest=$$OCI_INITRAMFS_DIR" -f initrd.Dockerfile .
	@test -f "$$INITRAMFS_PATH" || (printf 'ERROR: expected %s/%s artifact %s was not produced\n' "$$OCI_FLAVOR" "$$OCI_ARCH" "$$INITRAMFS_PATH"; exit 1)
	@sha256sum "$$INITRAMFS_PATH" > "$${INITRAMFS_PATH}.sha256"
	@oras push "$${REPOSITORY}/initramfs:$${DOCKERTAG}-$${OCI_FLAVOR}-$${OCI_ARCH}" \
		--annotation "org.opencontainers.image.version=$$VERSION" \
		--annotation "io.booty.flavor=$$OCI_FLAVOR" \
		--annotation "io.booty.arch=$$OCI_ARCH" \
		"$${INITRAMFS_PATH}:$${INITRAMFS_MEDIA_TYPE}" \
		"$${INITRAMFS_PATH}.sha256:text/plain"
	@printf 'Pushed %s/initramfs:%s-%s-%s\n' "$$REPOSITORY" "$$DOCKERTAG" "$$OCI_FLAVOR" "$$OCI_ARCH"

oci-push-binary: check-oci-vars
	@test -f "$$TARGET" || (printf "ERROR: %s binary not found — run 'make build' first\n" "$$TARGET"; exit 1)
	$(call ensure_oci_ref_absent,$${REPOSITORY}/binary:$${DOCKERTAG}-$${OCI_ARCH})
	@sha256sum "$$TARGET" > "$${TARGET}.sha256"
	@oras push "$${REPOSITORY}/binary:$${DOCKERTAG}-$${OCI_ARCH}" \
		--annotation "org.opencontainers.image.version=$$VERSION" \
		--annotation "io.booty.arch=$$OCI_ARCH" \
		"$${TARGET}:application/vnd.cncf.binary.layer.v1" \
		"$${TARGET}.sha256:text/plain"
	@printf 'Pushed %s/binary:%s-%s\n' "$$REPOSITORY" "$$DOCKERTAG" "$$OCI_ARCH"
