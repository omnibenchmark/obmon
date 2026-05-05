# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`obmon` — monitoring utilities for omnibenchmark. Modern Go codebase (Go 1.22+).

The main entrypoint is the `obmon` CLI. Subcommands map to modules:

- **stream** — first module; streams `telemetry.jsonl` from a remote server to a local [Aspire](https://learn.microsoft.com/en-us/dotnet/aspire/) OTel dashboard via OTLP gRPC. Uses a lightweight agent binary deployed to the remote host.

## Commands

```bash
go build ./...               # build all binaries
go test ./...                # run all tests
go test ./internal/agent/... # run tests for a specific package
go vet ./...                 # lint
```

Build and run:

```bash
go build -o obmon ./cmd/obmon
go build -o obmon-agent ./cmd/obmon-agent

./obmon stream \
  --host remote.example.com \
  --user alice \
  --identity ~/.ssh/id_ed25519 \
  --remote-file /data/omnibenchmark/telemetry.jsonl \
  --aspire localhost:4317
```

## Architecture

No FUSE. No shell execution on remote. Two binaries:

```
Remote host                              Local machine
────────────────────────────────────────────────────────
obmon-agent                              obmon stream
  ├── opens + tails telemetry.jsonl        ├── SSH dials remote
  └── streams raw JSONL over TCP           ├── SSH exec: ~/.obmon/bin/obmon-agent
       └──── SSH port forward ────────────→├── reads {"port":N} from agent stdout
                                           ├── SSH local port forward → agent port
                                           ├── reads raw JSONL from tunnel
                                           ├── parses each line → OTLP LogRecord
                                           └── gRPC → Aspire :4317
```

**Key properties:**
- Agent is a dumb JSONL forwarder (~minimal Go, rarely changes)
- JSONL→OTLP conversion lives in `obmon` (local, easy to iterate)
- Agent deployed once via SFTP upload; future versions auto-downloaded from GitHub Releases + SHA256 verified
- SSH exec uses absolute binary path — no shell metacharacter risk on the agent itself; remote file path is single-quote escaped

**Key dependencies:**
- `golang.org/x/crypto/ssh` — SSH client, port forwarding, exec
- `github.com/pkg/sftp` — agent binary upload
- `go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc` — OTLP gRPC log exporter
- `github.com/gliderlabs/ssh` — in-process SSH server for tests

**Package layout:**
```
cmd/obmon/main.go          # CLI: stream subcommand
cmd/obmon-agent/main.go    # Agent: listen on random TCP port, tail + stream JSONL
internal/agent/server.go   # agent.Serve(ctx, filePath, listener) — core tail+stream logic
internal/sshconn/connect.go # SSH dial, agent exec, port forward → returns io.ReadCloser
internal/otlp/forward.go   # Forward(ctx, reader, endpoint) — JSONL lines → OTLP gRPC
```

**Testing strategy:** `internal/agent` and `internal/otlp` are tested independently. SSH integration test uses `gliderlabs/ssh` in-process server whose handler calls `agent.Serve()` directly — no real binary needed. Mock OTLP gRPC receiver validates exported log records.
