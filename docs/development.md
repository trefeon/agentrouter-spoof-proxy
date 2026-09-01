# Development Guide

Public notes for anyone who forks or clones this repo and wants to build or contribute.

## Fork and Clone

```bash
git clone https://github.com/trefeon/agentrouter-spoof-proxy.git
cd agentrouter-spoof-proxy
cp .env.example .env
```

Edit `.env` only if you need to change the listen port or upstream. Defaults work for local use.

## Prerequisites

- Go 1.26 or newer (`go version`)
- Docker and Compose if you run the container build
- `golangci-lint` only if you run `make check`

## Build

```bash
go build ./...
go vet ./...
```

Single binary:

```bash
go build -trimpath -ldflags="-s -w" -o dist/proxy ./cmd/proxy
./dist/proxy
```

Docker:

```bash
docker compose up -d --build
curl http://localhost:8318/health
```

Systemd (host install):

```bash
bash scripts/install.sh --systemd
systemctl status agentrouter-proxy
```

## Test

```bash
go test ./...          # all tests, about 257 across 9 packages
go test ./internal/... # unit only
go test ./e2e/         # E2E with mock upstream
go test -race ./...    # race detector, needs cgo toolchain
```

> Windows sandbox note: when `go test` fails on `internal/checkin` with
> `fork/exec ... checkin.test.exe: Access is denied`, the MSYS2-gcc-linked
> cgo test binary is being blocked by the sandbox, not by the code. Run
> `CGO_ENABLED=0 go test ./...` instead (the Docker build is already
> CGO_ENABLED=0 static). The tests themselves are green.

`make check` runs vet, lint, and tests if `golangci-lint` is installed.

## Project Layout

- `cmd/proxy/main.go`: entry, config validation, signal handling
- `internal/config`: env config and validation
- `internal/auth`: spoof headers and WAF cookie handling
- `internal/proxy`: handler, SSE pump, and pure helpers
- `internal/models`: discovery, health, and stats
- `internal/resilience`: circuit breaker
- `internal/server`: routing and graceful shutdown
- `internal/logstore`: ring-buffer request/error log for the dashboard
- `internal/checkin`: check-in command runner and scheduler
- `internal/dashboard`: embedded admin web UI
- `testutil/mockupstream`: scripted mock upstream for tests
- `e2e/`: E2E and regression tests

## Contributing

Keep changes small and focused. Before opening a PR:

1. Run `go vet ./...` and `go test ./...`
2. Keep comments short and explain why, not what. One comment per block is enough.
3. Do not add em dashes in docs or comments. Use commas or periods.
4. Update `README.md` only if you change user-facing behavior.

Private local notes stay in `docs/dev.md` which is gitignored. Public dev docs live here.

## Troubleshooting

- `wafCookie: false` on `/health`: warmup is still running, wait 5 seconds and retry.
- 9Router cannot reach proxy on Docker: put both on the same Docker network or use `host.docker.internal` on Windows and Mac.
- SSE cuts early: check logs for `SLOW STREAM` and raise `SSE_CHUNK_TIMEOUT_MS` or `SSE_IDLE_TIMEOUT_MS`.
