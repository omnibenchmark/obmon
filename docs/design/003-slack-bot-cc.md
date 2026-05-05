# 003 — Slack Bot, Multi-tenant Tokens, and Agent C&C

## Status

| Feature | State |
|---|---|
| Token issuance via Slack slash command | planned |
| Benchmark run start/end notifications | planned |
| Persistent bot registration (agent phones home) | planned |
| C&C channel (bot → agent commands) | planned |
| Agent-initiated wormhole transfer | planned |

---

## Goals

1. **Notifications** — receive a Slack DM when a benchmark run starts or ends on any registered remote host.
2. **Multi-tenancy** — one bot instance serves many users; each user's notifications are isolated.
3. **C&C** — the bot can issue commands to a running agent (e.g. "send me the logs").
4. **Agent-initiated wormhole** — on a bot command, the agent starts a Magic Wormhole transfer and the bot DMs the user the wormhole code, so they can do `obmon receive <code>` locally.

## Non-Goals

- Replacing the SSH streaming pipeline for normal `obmon stream` usage.
- Centralised log storage (logs stay local or peer-to-peer via wormhole).
- Real-time log streaming through the bot.
- Slack message threads, interactive buttons, or modal dialogs in v1.

---

## Architecture Overview

```
Slack workspace
  │  /obmon token                    user registers, gets token
  │  /obmon logs <run-id>            user requests wormhole transfer
  │  DM: "run started on host X"     bot notifies user
  │
  ▼
┌─────────────────────────────────────────────────────┐
│  obmon-bot  (new binary, runs anywhere reachable)   │
│                                                     │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────┐   │
│  │ Slack client │  │ Token store  │  │ C&C hub  │   │
│  │ (slash cmds) │  │ (SQLite)     │  │ (WS hub) │   │
│  └──────────────┘  └──────────────┘  └──────────┘   │
│         │                │                 │        │
│         └────────────────┴─────────────────┘        │
│                      HTTP API                       │
└─────────────────────┬───────────────────────────────┘
                      │  outbound WebSocket (agent dials bot)
                      │
          ┌───────────┴────────────┐
          │  obmon-agent (remote)  │
          │  ├── tails jsonl       │
          │  ├── detects events    │
          │  ├── C&C WS client     │
          │  └── wormhole sender   │
          └────────────────────────┘
```

The agent **always initiates** connections outbound (WebSocket to bot, wormhole relay). No inbound ports on the remote host are required beyond what SSH already uses.

---

## Components

### `obmon-bot` (new binary)

Runs as a long-lived service (systemd unit, Docker container, or fly.io app).

Responsibilities:
- Accept Slack slash commands via HTTP (Slack pushes POSTs to a public URL).
- Mint and store tokens, mapping them to Slack users.
- Accept WebSocket connections from agents; maintain a live registry.
- Receive event notifications from agents; route to the correct Slack user.
- Forward C&C commands (wormhole, status) to the appropriate agent WS.

Persistence: single SQLite file (`~/.local/share/obmon-bot/bot.db`).

### `obmon-agent` (enhanced)

New capabilities added to the daemon mode (see
[002](002-local-cache-replay-share.md) and [001](001-architecture.md)):

- On startup: read `~/.obmon/config.json` for `bot_url` and `token`.
- If configured: dial bot WebSocket, send registration, maintain connection with ping/pong.
- Event detector goroutine: watches parsed JSONL lines for run start/end signals, sends events over WS.
- Wormhole sender: on `wormhole` command from bot, call `wormhole.SendFile`, report code.

The JSONL streaming logic (`internal/agent/server.go`) is unchanged.

### `obmon` CLI (minor changes)

- `obmon agent configure --bot-url URL --token TOKEN` — writes `~/.obmon/config.json` on the remote host via SFTP (same mechanism as binary deploy).
- `obmon stream` — no changes required; bot notifications are independent of `obmon` being connected.

---

## Identity and Token Model

```
Slack user
  │  /obmon token
  ▼
bot mints opaque token: 32 random bytes → base64url (e.g. "obTok-kB3x...")
bot stores in SQLite:
  tokens(token TEXT PK, slack_user_id TEXT, slack_channel_id TEXT, created_at TIMESTAMP)
bot DMs user: "Your token: obTok-kB3x..."
```

Tokens are **opaque** (not JWTs) — simpler to revoke, no shared secret needed between agent and CLI.

The token is the agent's identity with the bot. One token per user. The agent is configured with this token; all its events are routed to the owning Slack user.

**Revocation**: `/obmon revoke` deletes the token row; any agent using that token is rejected on its next WS message and closes the connection.

---

## Bot HTTP API

Internal to the system. Slack verification via `X-Slack-Signature`. Agent auth via `Authorization: Bearer <token>` on the WebSocket upgrade.

| Method | Path | Actor | Description |
|---|---|---|---|
| `POST` | `/slack/events` | Slack | Slash command handler |
| `GET` | `/ws/agent` | agent | WebSocket upgrade; `Authorization: Bearer <token>` |

The WebSocket endpoint is the only persistent connection. Everything else is request/response.

---

## C&C Channel (WebSocket)

The agent opens a single WebSocket to `wss://bot-host/ws/agent` on startup, authenticated by the token in the `Authorization` header.

All messages are newline-delimited JSON.

### Agent → Bot messages

```jsonc
// Registration (sent immediately after connection)
{"type": "hello", "hostname": "compute01.example.com", "agent_version": "0.3.1"}

// Event notification
{"type": "event", "event": "run_started", "run_id": "abc123", "file": "/data/telemetry.jsonl", "ts": "2026-02-27T10:00:00Z"}
{"type": "event", "event": "run_ended",   "run_id": "abc123", "duration_s": 142, "ts": "2026-02-27T10:02:22Z"}

// Wormhole code response
{"type": "wormhole_code", "cmd_id": "c1", "code": "7-crossword-clockwork"}

// Error response
{"type": "error", "cmd_id": "c1", "message": "wormhole send failed: ..."}

// Keepalive (standard WebSocket ping/pong; agent sends pings every 30 s)
```

### Bot → Agent messages

```jsonc
// Request wormhole send of a file
{"type": "cmd", "cmd": "wormhole_send", "cmd_id": "c1", "file": "/data/telemetry.jsonl"}

// Request agent status
{"type": "cmd", "cmd": "status", "cmd_id": "c2"}
```

The bot maintains an in-memory map of `token → websocket.Conn`. On Slack command, it looks up the live connection and writes the command. If the agent is not connected, the bot replies "agent not online" to the user.

---

## Event Detection

The agent needs to identify "run started" and "run ended" in the OTLP JSONL stream without knowing the exact benchmark schema up front.

### Detection strategy (two-level)

**Level 1 — File lifecycle (always active, zero config)**

| Signal | Event |
|---|---|
| File appears (was absent, now exists) | `run_started` |
| File stops growing for N seconds (default 60 s) | `run_ended` |

FIXME -- discuss end marker. the timeout heuristic is brittle.

This works regardless of OTLP content and requires no schema knowledge.

**Level 2 — OTLP span markers (opt-in, configured in `~/.obmon/config.json`)**

```jsonc
{
  "event_rules": [
    {"event": "run_started", "match": {"span_name": "benchmark.run"}},
    {"event": "run_ended",   "match": {"span_name": "benchmark.run", "span_has_end": true}}
  ]
}
```

The event detector goroutine reads from an internal copy of the line stream (the agent tees each line to both the TCP client and the detector channel). It JSON-peeks for `resourceSpans` and checks span names and end-time fields.

When both levels fire, the first one wins (deduplicated by run ID within a 5-minute window).

### Run ID

The run ID is derived from the OTLP trace ID of the first root span, if available, otherwise a UUID minted at detection time. Sent in every event message.

---

## Notification Flow

```
1. Agent detects "run_started"
2. Agent sends: {"type":"event","event":"run_started","run_id":"abc","file":"/data/telem.jsonl","ts":"..."}
3. Bot looks up Slack user for the token
4. Bot posts DM:
     > 🚀  Benchmark run started on compute01.example.com
     > Run ID: `abc`  •  File: `/data/telemetry.jsonl`
     > Started: 10:00 UTC

5. Agent detects "run_ended" (or file-idle timeout)
6. Bot posts DM:
     > ✅  Benchmark run ended on compute01.example.com
     > Run ID: `abc`  •  Duration: 2 m 22 s
     > To get the logs: /obmon logs abc
```

---

## Agent-Initiated Wormhole Transfer

Leverages `github.com/psanford/wormhole-william` (already in the dependency list; see [002](002-local-cache-replay-share.md)).

```
User: /obmon logs abc123

Bot:
  1. Looks up token for user.
  2. Finds live WS connection for that token.
  3. Sends: {"type":"cmd","cmd":"wormhole_send","cmd_id":"c1","file":"/data/telem.jsonl"}

Agent:
  4. Opens /data/telemetry.jsonl (or run-specific cached path).
  5. c := wormhole.Client{}
     code, status, err := c.SendFile(ctx, "telemetry.jsonl", f)
  6. Sends: {"type":"wormhole_code","cmd_id":"c1","code":"7-crossword-clockwork"}

Bot:
  7. DMs user:
       > Your logs are ready.
       > Run: `obmon receive 7-crossword-clockwork`
       > (Code expires in ~10 minutes.)

User:
  8. obmon receive 7-crossword-clockwork
     → downloads to ~/.cache/obmon/runs/<new-uuid>/telemetry.jsonl
     → ready for: obmon replay <new-uuid>
```

No files pass through the bot server. The wormhole rendezvous is end-to-end encrypted via the public relay (relay.magic-wormhole.io). The bot is only a signalling layer.

---

## Configuration

### Bot (`~/.config/obmon-bot/config.toml`)

```toml
[slack]
bot_token   = "xoxb-..."          # Bot OAuth token
signing_secret = "..."            # For request verification

[server]
listen = ":8080"
public_url = "https://bot.example.com"   # Used in Slack app config

[db]
path = "~/.local/share/obmon-bot/bot.db"
```

### Agent (`~/.obmon/config.json` on remote host)

```jsonc
{
  "bot_url": "wss://bot.example.com/ws/agent",
  "token": "obTok-kB3x...",
  "event_idle_timeout_s": 60,
  "event_rules": []
}
```

Written by `obmon agent configure` via SFTP.

### Slash commands to register in Slack app

| Command | Description |
|---|---|
| `/obmon token` | Mint a new token (revokes previous) |
| `/obmon revoke` | Revoke current token |
| `/obmon status` | Show agent connection status |
| `/obmon logs [run-id]` | Request wormhole transfer of a run |

---

## Implementation Phases

### Phase 1 — Notifications (no C&C)

Minimal viable: agent POSTs events over plain HTTPS, no persistent WS.

- `obmon-bot`: SQLite token store + `/slack/events` handler + `/api/v1/event` endpoint.
- `obmon-agent`: add `--bot-url` flag + goroutine that POSTs on file-lifecycle events.
- `obmon agent configure` subcommand (SFTP write of config).
- Slack slash command: `/obmon token`.

No wormhole. No C&C. The agent just fires-and-forgets HTTP POSTs.

### Phase 2 — Persistent daemon + C&C channel

Builds on Phase 1 and the daemon work from [002](002-local-cache-replay-share.md).

- Agent connects to bot via WebSocket on daemon startup.
- Bot maintains WS registry.
- `/obmon status` slash command works.
- OTLP-level event rules (Level 2 detection).

### Phase 3 — Wormhole

- Agent wormhole sender (`wormhole.Client.SendFile`).
- `/obmon logs` slash command.
- Bot DMs wormhole code.

---

## Open Questions

1. **Bot hosting** — Where does `obmon-bot` run? Needs a public HTTPS URL for Slack. Fly.io free tier, a VPS, or a spare machine are all fine. Does not need to be on the same host as the agents.

2. **Idle timeout tuning** — 60 s file-idle = "run ended" will false-positive on slow benchmarks with long pauses. Should this be per-user configurable via Slack, or only in `config.json`?

3. **Multiple agents per user** — Can one token be used on multiple remote hosts (e.g. different compute nodes)? If yes, the bot registry maps `token → []conn` and `/obmon status` lists all of them. `/obmon logs` would need a hostname qualifier.

4. **Wormhole relay** — The default public relay (`relay.magic-wormhole.io`) should be fine for logs. If data sensitivity requires it, a private relay (`wormhole-william` supports custom relay URL) can be configured.

5. **Bot binary distribution** — `obmon-bot` would be a separate release artifact. Is it published alongside `obmon` and `obmon-agent` in GitHub Releases?
