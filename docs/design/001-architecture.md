# obmon Architecture

## Overview

Two binaries. No FUSE. No shell execution of user-controlled input.

```
Remote host                              Local machine
────────────────────────────────────────────────────────────────────
obmon-agent                              obmon stream
  └── tails telemetry.jsonl                ├── SSH dials remote
       streams raw JSONL over TCP          ├── SSH exec: obmon-agent --daemon --file <path>
            │                              ├── reads {"port":N} or connects to agent.sock
            └──── SSH tunnel ─────────────→├── sends {"resume_line": N}
                                           ├── reads raw JSONL from tunnel
                                           ├── io.TeeReader → local cache file
                                           ├── parses each line → OTLP LogRecord
                                           └── gRPC → Aspire :4317
```

## Binaries

### `obmon-agent` (remote)

Deployed once to `~/.obmon/bin/obmon-agent` via SFTP. Rarely changes.

Responsibilities:
- Listen on a TCP port (current) or Unix socket (planned daemon mode)
- Tail `telemetry.jsonl` from a caller-specified path
- On each new connection: read `{"resume_line": N}`, skip N lines, stream remainder
- Follow new content as it is appended (poll with 50 ms sleep on EOF)

### `obmon` (local CLI)

Subcommands:

| Command | Description |
|---|---|
| `stream` | Connect to remote agent, stream JSONL, tee to local cache, forward to OTLP |
| `runs list` | List cached runs with status |
| `replay <id>` | Re-emit a cached run to an OTLP endpoint |
| `share <id>` | Send a cached run to a peer via Magic Wormhole *(planned)* |
| `receive <code>` | Receive a run from a peer via Magic Wormhole *(planned)* |
| `dashboard` | Ensure Aspire is running, print UI URL |

## Transport

### Current: ephemeral SSH exec + TCP port forward

```
obmon connects → ssh exec obmon-agent --file <path>
agent listens on random TCP port → prints {"port": N}
obmon dials ssh tunnel to 127.0.0.1:N
```

Each `obmon stream` invocation spawns a new agent process. On clean exit the
SSH session closure signals the agent. On unclean exit (client crash, network
drop) the agent lingers until it next tries to write to the closed tunnel.

### Planned: persistent daemon + Unix socket forward

```
obmon connects → ssh exec obmon-agent --daemon --file <path>   # idempotent
agent checks ~/.obmon/run/agent.pid:
  - PID alive + socket exists → exits 0 (daemon already running)
  - otherwise → writes PID file, binds ~/.obmon/run/agent.sock, tails file
obmon opens ssh local forward → agent.sock
```

One agent process per remote file, regardless of how many times `obmon stream`
reconnects. PID file staleness handled via `kill(pid, 0)` on startup.

Remote layout:
```
~/.obmon/
    bin/obmon-agent
    run/
        agent.pid
        agent.sock
```

`sshconn.Connect` interface is unchanged — still returns `io.ReadWriteCloser`.

See the [trade-off table](#agent-model-trade-offs) below.

## Resume Protocol

Before any JSONL is sent, the client writes one JSON line to the tunnel:

```
client → {"resume_line": N}
agent  → streams from line N+1 onward
```

`N = 0` means start from the beginning. `N` is the number of complete `\n`-
terminated lines already in the local cache file (`wc -l` equivalent).

## Local Cache

Root: `os.UserCacheDir()/obmon/runs/` (`~/.cache/obmon/runs/` on Linux).

```
<run-uuid>/
    telemetry.jsonl   # raw JSONL, O_APPEND on each reconnect
    meta.json         # id, host, remote_file, started_at, finished_at, lines
```

`finished_at` is null while streaming is in progress — crash-safe sentinel.
On reconnect, line count is derived from the file directly (`countLines`), not
from `meta.json`, so the count is always accurate even after an unclean exit.

## Key Dependencies

| Package | Role |
|---|---|
| `golang.org/x/crypto/ssh` | SSH client, port forwarding, exec |
| `github.com/pkg/sftp` | agent binary upload |
| `github.com/gliderlabs/ssh` | in-process SSH server for tests |
| `go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc` | OTLP gRPC log exporter |
| `github.com/google/uuid` | run ID generation |
| `github.com/psanford/wormhole-william` | peer-to-peer run sharing *(planned)* |

## Package Layout

```
cmd/obmon/main.go              # CLI: stream, runs, replay, share, receive
cmd/obmon-agent/main.go        # Agent entry point
internal/agent/server.go       # Serve(ctx, filePath, listener) — tail + stream
internal/sshconn/connect.go    # Connect(ctx, cfg) → io.ReadWriteCloser
internal/otlp/forward.go       # Forward(ctx, reader, addr) — JSONL → OTLP gRPC
internal/cache/cache.go        # Run cache: New, Resume, Get, List, Finish
```

## Agent Model Trade-offs

| | Ephemeral (current) | Daemon (planned) |
|---|---|---|
| Process count | 1 per stream session | 1 per file |
| Stale processes | possible on unclean exit | only if PID file stale |
| Agent restart on upgrade | automatic (new exec each time) | requires `obmon agent stop` |
| SSH sessions per stream | 1 (exec + TCP forward) | 1 (idempotent exec + socket forward) |
| Remote state | none | PID file + Unix socket |
