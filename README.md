# poke-go

A zero-dependency Go port of the [`poke`](https://poke.com) CLI. Single binary, no Node.js or npm required.

## What it does

| Command | Description |
|---------|-------------|
| `poke login` | Open browser to authenticate with poke.com, save token to `~/.config/poke/credentials.json` |
| `poke logout` | Remove stored credentials |
| `poke tunnel <url> [--name <name>]` | Expose a local server through a poke.com tunnel |

Credentials are stored at `~/.config/poke/credentials.json` — same path as the npm package, so `poke-go` and the npm `poke` CLI are interchangeable.

## Install

### Download a release

Grab the binary for your platform from the [Releases](https://github.com/kacperkwapisz/poke-go/releases) page and put it on your `$PATH`.

### Build from source

Requires Go 1.22+.

```bash
git clone https://github.com/kacperkwapisz/poke-go
cd poke-go
go build -o poke ./cmd/poke
```

Install to `$GOPATH/bin`:

```bash
go install ./cmd/poke
```

## Usage

```bash
# Authenticate
poke login

# Expose a local MCP server
poke tunnel http://localhost:3000/mcp --name my-server

# Remove credentials
poke logout
```

## With poke-mail

`poke-go` is a drop-in replacement for the npm `poke` CLI used in [poke-mail](https://github.com/kacperkwapisz/poke-mail)'s `start.sh`.

To use it there instead of the npm package:

```bash
# Build or install poke-go, then run start.sh as normal
./start.sh
```

## Cross-compile

```bash
# Linux (amd64)
GOOS=linux GOARCH=amd64 go build -o dist/poke-linux-amd64 ./cmd/poke

# macOS (arm64 — Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o dist/poke-darwin-arm64 ./cmd/poke

# Windows
GOOS=windows GOARCH=amd64 go build -o dist/poke-windows-amd64.exe ./cmd/poke
```

## Design

- **Zero external dependencies** — stdlib only (`net/http`, `crypto/tls`, `encoding/json`, etc.)
- **Minimal WebSocket client** — RFC 6455-compliant, hand-written in `internal/ws`
- **Automatic reconnect** — tunnel reconnects with exponential backoff on disconnect
- **Clean output** — only prints what matters; errors go to stderr
- **Credentials at `~/.config/poke/credentials.json`** — compatible with the npm CLI
