# Makefile — developer targets. CI runs the same commands (see .github/workflows/ci.yml).
# Local toolchain note: Go >= 1.24.5 must be on PATH (prefer GOTOOLCHAIN=local in
# environments with multiple toolchains so builds resolve against go 1.24 deps).

BINARY := topctl
GO     ?= go

help:
	@echo "TOP developer targets (CI runs the same commands):"
	@echo "  make build           build ./cmd/topctl -> ./topctl"
	@echo "  make test            tier-1 unit tests (go test ./... -race)"
	@echo "  make test-shim       tier-2 dokku-shim integration (toptests tag)"
	@echo "  make test-conformance  T-111 contract conformance probes (tag-gated)"
	@echo "  make vet             go vet ./..."
	@echo "  make fmt             gofumpt -l -w ."
	@echo "  make lint            golangci-lint run (skips if not installed)"
	@echo "  make hygiene         ADR-0014 public/private grep"

.PHONY: build test test-conformance vet fmt lint hygiene test-shim clean help

build:
	CGO_ENABLED=0 $(GO) build -o $(BINARY) ./cmd/topctl

test:
	$(GO) test ./... -race -count=1

# T-111 contract conformance harness: compile-level asserts (always built)
# plus tag-gated behavior probes. Run explicitly — NOT wired into `test` or
# CI; the T-112 review decides when conformance joins pr CI (recommended
# post-M2). See conformance_check/README.md.
test-conformance:
	GOTOOLCHAIN=local $(GO) test -tags conformance ./conformance_check/... -race -count=1

# Tier 2 (ADR-0012): shim integration tests, guarded by the `toptests` build
# tag so they stay out of `test` and CI's `go test ./...`. Exercises the
# exec.Runner seam against a fake dokku script on PATH — see test/shim/README.md.
test-shim:
	$(GO) test -tags toptests ./test/fakeexec ./test/shim -race -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) run mvdan.cc/gofumpt@v0.7.0 -l -w .

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed; skipping (CI runs it). Install: see https://golangci-lint.run/welcome/install/"; exit 0; }
	golangci-lint run

hygiene:
	./test/hygiene.sh

clean:
	rm -f $(BINARY)
