# Local Cache, Replay, and Share

## Status

| Feature | State |
|---|---|
| Local cache tee (`io.TeeReader` → `telemetry.jsonl`) | **done** |
| Resume protocol (`{"resume_line": N}` handshake) | **done** |
| `obmon runs list` | **done** |
| `obmon replay <id>` | **done** |
| Persistent agent daemon (Unix socket) | **next spike** |
| `obmon share` / `obmon receive` (Magic Wormhole) | **next spike** |
| `obmon replay --from-line N` | deferred |
| `obmon replay --speed 2.0` | deferred |

---

## Next Spike: Persistent Agent Daemon

### Problem

Every `obmon stream` invocation — including auto-resume reconnects — spawns a
new agent process via `ssh exec`. On unclean client exit the old agent lingers.
Repeated reconnects can leave multiple agents tailing the same file.

### Design

Make agent startup idempotent using a PID file and Unix domain socket at
`~/.obmon/run/` on the remote host. Full design in
[001-architecture.md](001-architecture.md#planned-persistent-daemon--unix-socket-forward).

### Changes required

**`cmd/obmon-agent/main.go`**
- Add `--daemon` flag
- On startup: check `~/.obmon/run/agent.pid` via `kill(pid, 0)`
  - PID alive + socket exists → exit 0
  - Otherwise → write PID file, bind `agent.sock`, tail file
- On exit: remove PID file and socket

**`internal/sshconn/connect.go`**
- Change `ensureAgent` to pass `--daemon` flag
- Change port-forward logic to Unix socket forward (`ssh.Dial("unix", socketPath)`)
- Remove `{"port": N}` stdout read (socket path is fixed: `~/.obmon/run/agent.sock`)

**`internal/agent/server.go`** — no changes (protocol unchanged)

**`cmd/obmon/main.go`** — no changes (interface unchanged)

### Tests

- `TestDaemon_IdempotentStart` — start daemon twice, verify only one process
- `TestDaemon_Stalepid` — write stale PID file, verify fresh start
- Existing `TestConnect_StreamsViaSSH` — update to use socket forward instead of TCP

---

## Next Spike: Share / Receive (Magic Wormhole)

### Design

Transfer a cached run directory to a peer using
[wormhole-william](https://github.com/psanford/wormhole-william) — the pure-Go
Magic Wormhole implementation. No server to operate; data is E2E encrypted;
rendezvous via a public relay.

Interoperates with the standard Python `magic-wormhole` CLI on the receiver
side.

### `obmon share <run-id>`

```
obmon share abc123
→ opens ~/.cache/obmon/runs/abc123/telemetry.jsonl
→ initiates wormhole file transfer
→ prints: wormhole code: 7-crossword-clockwork
→ blocks until receiver connects and transfer completes
```

Implementation sketch:

```go
import "github.com/psanford/wormhole-william/wormhole"

c := wormhole.Client{}
code, status, err := c.SendFile(ctx, "telemetry.jsonl", file)
fmt.Printf("wormhole code: %s\n", code)
s := <-status
if s.Error != nil { ... }
```

### `obmon receive <code>`

```
obmon receive 7-crossword-clockwork
→ downloads telemetry.jsonl into ~/.cache/obmon/runs/<new-uuid>/
→ writes meta.json with source noted
→ ready for: obmon replay <new-uuid>
```

### Changes required

- Add `github.com/psanford/wormhole-william` to `go.mod`
- Add `runShare(args)` and `runReceive(args)` to `cmd/obmon/main.go`
- `internal/cache` — no changes (share/receive use existing `New` + `Writer`)

### Non-goals

- Streaming share (share while run is in progress)
- Encryption of local cache (OS filesystem permissions sufficient)
- Cloud/centralised run repository
