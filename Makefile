# Makefile — developer targets. CI runs the same commands (see .github/workflows/ci.yml).
# Local toolchain note: Go >= 1.24.5 must be on PATH (prefer GOTOOLCHAIN=local in
# environments with multiple toolchains so builds resolve against go 1.24 deps).

BINARY := topctl
GO     ?= go

.PHONY: build test vet fmt lint hygiene clean

build:
	CGO_ENABLED=0 $(GO) build -o $(BINARY) ./cmd/topctl

test:
	$(GO) test ./... -race -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) run mvdan.cc/gofumpt@latest -l -w .

lint:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not installed; skipping (CI runs it). Install: see https://golangci-lint.run/welcome/install/"; exit 0; }
	golangci-lint run

hygiene:
	./test/hygiene.sh

clean:
	rm -f $(BINARY)
