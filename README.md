# obmon

Monitor your benchmarks (over ssh, or locally).

## What

`obmon` is a single binary that assumes two things:

- You are running `omnibenchmark >= 0.6` with telemetry support enabled.
- If they run on a remote server, you can ssh into that machine.

From there, it will get the _traces_ from your benchmark executor and help you
browsing those traces (including logs) from the comfort of your own
laptop. This means you don't need external infrastructure 24/7, but you need to
be able to run docker (or podman) on your own machine.

Need to collaborate with coworkers? No problem! `obmon` also allows you to
_share_ a trace of a particular run, so you can safely send the execution
traces to other devices. See [docs/collaboration.md](docs/collaboration.md).

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/omnibenchmark/obmon/main/install.sh | sh
```

See [INSTALL.md](INSTALL.md) for other installation methods.

## Quick start

### Local runs

```
ob run --telemetry benchmark.yaml
```

In a separate terminal:

```
obmon stream telemetry.jsonl
```


### Server runs

You run the benchmark on the server with `--telemetry`:

```
ob run --telemetry benchmark.yaml
```

And then, from your local machine, with a server configured in `~/.ssh/config`:

```sh
obmon stream myserver:/data/omnibenchmark/telemetry.jsonl
```

This will start the Aspire dashboard if not already running, deploy the agent to the remote host if needed, and stream telemetry to `http://localhost:18888`.

---

## Commands

### `obmon dashboard`

Starts the [Aspire](https://learn.microsoft.com/en-us/dotnet/aspire/fundamentals/dashboard/overview) OTel dashboard container if it is not already running, then exits.

```sh
obmon dashboard
```

| Flag | Default | Description |
|---|---|---|
| `--otlp` | `localhost:4317` | OTLP gRPC address to wait on before returning |

Dashboard UI is available at `http://localhost:18888` once running.

---

### `obmon stream`

Streams `telemetry.jsonl` to the local Aspire dashboard via OTLP gRPC. For remote targets, opens an SSH tunnel and deploys `obmon-agent` to the remote host if not present. For local targets, tails the file directly — no SSH, no agent. Starts Aspire automatically if not running.

```sh
obmon stream [user@]host:path [flags]   # remote
obmon stream <local-path> [flags]       # local
```

A target is treated as local when it is absolute (`/foo`), explicitly relative (`./foo`, `../foo`, `~/foo`), or contains no `:` separator. Otherwise it is parsed as scp-style `[user@]host:path`.

Examples:

```sh
# host alias from ~/.ssh/config
obmon stream myserver:/data/omnibenchmark/telemetry.jsonl

# explicit user and host
obmon stream alice@remote.example.com:/data/omnibenchmark/telemetry.jsonl

# with a non-default SSH key
obmon stream myserver:/data/omnibenchmark/telemetry.jsonl --identity ~/.ssh/id_ed25519

# local file (absolute or relative)
obmon stream /data/omnibenchmark/telemetry.jsonl
obmon stream ./telemetry.jsonl
```

`user`, hostname, port, and identity file are resolved from `~/.ssh/config` when not provided. Local runs are recorded in the cache under host `local`.

| Flag | Default | Description |
|---|---|---|
| `--identity` | `~/.ssh/config` | Path to SSH private key |
| `--aspire` | `localhost:4317` | Local Aspire OTLP gRPC endpoint |
| `--agent-path` | `~/.obmon/bin/obmon-agent` | Path to `obmon-agent` on remote |
