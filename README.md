# obmon

Admin tools for nicer benchmarking.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/omnibenchmark/obmon/main/install.sh | sh
```

See [INSTALL.md](INSTALL.md) for other installation methods.

## Quick start

With a host configured in `~/.ssh/config`:

```sh
obmon stream myserver:/data/omnibenchmark/telemetry.jsonl
```

This will start the Aspire dashboard if not already running, deploy the agent to the remote host if needed, and stream telemetry to `http://localhost:18888`.

---

## Commands

### `obmon dashboard`

Starts the [Aspire](https://learn.microsoft.com/en-us/dotnet/aspire/fundamentals/dashboard/overview) OTel dashboard container if it is not already running, then exits.

```sh
./obmon dashboard
```

| Flag | Default | Description |
|---|---|---|
| `--otlp` | `localhost:4317` | OTLP gRPC address to wait on before returning |

Dashboard UI is available at `http://localhost:18888` once running.

---

### `obmon stream`

Streams `telemetry.jsonl` from a remote host to the local Aspire dashboard via OTLP gRPC over an SSH tunnel. Starts Aspire automatically if not running. Deploys `obmon-agent` to the remote host if not present.

```sh
./obmon stream [user@]host:path [flags]
```

Examples:

```sh
# host alias from ~/.ssh/config
./obmon stream myserver:/data/omnibenchmark/telemetry.jsonl

# explicit user and host
./obmon stream alice@remote.example.com:/data/omnibenchmark/telemetry.jsonl

# with a non-default SSH key
./obmon stream myserver:/data/omnibenchmark/telemetry.jsonl --identity ~/.ssh/id_ed25519
```

`user`, hostname, port, and identity file are resolved from `~/.ssh/config` when not provided.

| Flag | Default | Description |
|---|---|---|
| `--identity` | `~/.ssh/config` | Path to SSH private key |
| `--aspire` | `localhost:4317` | Local Aspire OTLP gRPC endpoint |
| `--agent-path` | `~/.obmon/bin/obmon-agent` | Path to `obmon-agent` on remote |
