# AgentRouter Spoof Proxy — Go rewrite build tooling
GO       ?= go
BINARY   ?= dist/proxy
LDFLAGS  ?= -s -w

.PHONY: build test test-race vet lint fmt check image cross install-lint

build:
	$(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/proxy

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run

fmt:
	gofmt -l -w cmd internal e2e testutil

check: vet lint test

install-lint:
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

# Cross-compile matrix (from any host, pure Go)
cross:
	GOOS=linux   GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/proxy-linux-amd64  ./cmd/proxy
	GOOS=linux   GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/proxy-linux-arm64  ./cmd/proxy
	GOOS=darwin  GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/proxy-darwin-arm64 ./cmd/proxy
	GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o dist/proxy.exe          ./cmd/proxy

# Multi-arch Docker image built from any host (native cross-compile, no QEMU)
image:
	docker buildx build --platform linux/amd64,linux/arm64 -t agentrouter-proxy:go --push .
