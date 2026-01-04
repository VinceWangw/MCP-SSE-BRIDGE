# mcp-sse-bridge

A small Go bridge that proxies MCP JSON-RPC over stdio to a legacy SSE backend. It keeps
Codex-style clients responsive by short-circuiting `initialize`, forwarding traffic to
the upstream SSE endpoint, and reconnecting on session invalidation.

## Features
- Stdio JSON-RPC (jsonl or `Content-Length`) to legacy SSE translation.
- Auto-reconnect when the upstream session expires.
- Best-effort upstream `initialize` forwarding with response tracking.

## Requirements
- Go 1.25+ (see `go.mod`).
- A reachable SSE backend URL.

## Build
```bash
go build -o mcp-sse-bridge .
```

## Run
```bash
REMOTE_SSE_URL="https://host/mcp-servers/example" \
REMOTE_BEARER_TOKEN="..." \
./mcp-sse-bridge
```

## Configuration
Required:
- `REMOTE_SSE_URL`: SSE endpoint that emits `endpoint` and `message` events.

Optional:
- `REMOTE_BEARER_TOKEN`: Bearer token for upstream auth.
- `UPSTREAM_TIMEOUT`: JSON-RPC response wait (default: 120s).
- `UPSTREAM_INIT_TIMEOUT`: Wait for upstream `initialize` (default: 8s).
- `FORWARD_INITIALIZE`: Whether to forward client `initialize` upstream (default: false).
- `RECONNECT_POST_DELAY_MS`: Delay before retry after reconnect (default: 300).
- `ONE_SHOT_TOOLS`: Run each `tools/call` with a fresh SSE session (default: true).
- `ONE_SHOT_MAX_RETRIES`: Max attempts for one-shot calls (default: 3).
- `LOCAL_TOOLS_LIST`: Serve a local hardcoded `tools/list` response (default: false).

## Notes
- The bridge logs to stderr and emits JSON-RPC to stdout.
- Reconnects switch to a new SSE stream and re-send upstream initialize.
- With `LOCAL_TOOLS_LIST=false`, `tools/list` is forwarded to upstream.

## Repository Layout
- `main.go`: all bridge logic.
- `go.mod`: module definition.
