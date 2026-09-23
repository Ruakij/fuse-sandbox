VERSION ?= dev
LDFLAGS = -s -w -X main.version=$(VERSION)
# Everything x/sys covers for the new mount API; the binary is static, so any of
# these runs on any distro with a 5.12+ kernel.
ARCHES ?= amd64 arm64 arm 386 riscv64 ppc64le s390x

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "$(LDFLAGS)" -o bin/fuse-sandbox ./cmd/fuse-sandbox

# Release binaries plus their checksums, as the release workflow publishes them.
.PHONY: dist
dist:
	rm -rf dist && mkdir dist
	for arch in $(ARCHES); do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/fuse-sandbox-linux-$$arch ./cmd/fuse-sandbox || exit 1; \
	done
	cd dist && sha256sum fuse-sandbox-* > SHA256SUMS

FUZZTIME ?= 30s

.PHONY: fuzz
fuzz:
	go test -run '^$$' -fuzz FuzzParse -fuzztime $(FUZZTIME) ./cmd/fuse-sandbox

# Mount tests need root, /dev/fuse and a Linux kernel, so they run in a privileged
# container. On a non-Linux machine that container is inside colima or another VM.
MOUNTTEST = docker run --rm --privileged -v $(CURDIR):/src -w /src -e GOFLAGS=-buildvcs=false \
	golang:1.27-alpine go test -tags mounttest

.PHONY: test-mount
test-mount:
	$(MOUNTTEST) -count=1 -v ./...

# Where the container keeps its corpus, so the next run builds on it. The default
# is Go's own on Linux.
FUZZCACHE ?= $(HOME)/.cache/go-build/fuzz

.PHONY: fuzz-mount
fuzz-mount:
	mkdir -p $(FUZZCACHE)
	docker run --rm --privileged -v $(CURDIR):/src -v $(FUZZCACHE):/root/.cache/go-build/fuzz -w /src \
		-e GOFLAGS=-buildvcs=false golang:1.27-alpine \
		go test -tags mounttest -run '^$$' -fuzz FuzzSandbox -fuzztime $(FUZZTIME) ./pkg/sandbox
