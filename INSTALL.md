# Installing obmon

## Pre-built binaries (Linux and macOS, amd64/arm64)

```sh
curl -fsSL https://raw.githubusercontent.com/omnibenchmark/obmon/main/install.sh | sh
```

Builds are published under the [`nightly`](https://github.com/omnibenchmark/obmon/releases/tag/nightly) tag.

## Build from source

Requires Go 1.24+.

```sh
go build -o obmon ./cmd/obmon
go build -o obmon-agent ./cmd/obmon-agent
```
