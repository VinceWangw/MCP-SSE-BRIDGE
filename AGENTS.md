# Repository Guidelines

## Project Structure & Module Organization
- `main.go` contains the entire bridge implementation (stdio ↔ legacy SSE proxy).
- `go.mod` defines the Go module (`mcp-sse-bridge`) and Go version.
- `mcp-sse-bridge` is the built binary (if present) and can be regenerated.

## Build, Test, and Development Commands
- `go build ./...` builds the binary and validates module compilation.
- After any code change, run `go build -o mcp-sse-bridge .` to refresh the local binary.
- Note: sandboxed runs may need permission to access the Go build cache outside the repo.
- `go run ./...` runs the bridge locally for quick iteration.
- Example run:
  ```bash
  REMOTE_SSE_URL="https://host/mcp-servers/example" \
  REMOTE_BEARER_TOKEN="..." \
  go run ./...
  ```
- Optional: `UPSTREAM_TIMEOUT=120s` overrides the upstream SSE response wait.
- Optional: `FORWARD_INITIALIZE=true` forwards client `initialize` upstream (default false).
- Optional: `RECONNECT_POST_DELAY_MS=300` delays the retry after reconnect.
- Optional: `ONE_SHOT_TOOLS=true` runs each `tools/call` with a fresh SSE session.
- Optional: `ONE_SHOT_MAX_RETRIES=3` controls one-shot retry attempts.
- Optional: `LOCAL_TOOLS_LIST=true` serves a local hardcoded `tools/list` response (default false).

## Coding Style & Naming Conventions
- Use standard Go formatting (`gofmt`); indentation is tabs by default.
- Names follow Go conventions: `CamelCase` for exported, `camelCase` for local.
- Keep JSON-RPC shapes and HTTP/SSE wiring in `main.go`; add small helpers near use.

## Testing Guidelines
- No tests are present in this repository.
- If you add tests, use Go’s standard tooling (`go test ./...`) and keep names like
  `TestHandleOne` in `_test.go` files.

## Commit & Pull Request Guidelines
- This directory is not a Git repository, so no commit history or conventions exist.
- If you initialize Git, use concise, imperative commit messages (e.g., “Fix SSE timeout”)
  and include a short PR description summarizing behavior changes and how seen.

## Configuration & Runtime Notes
- Required env: `REMOTE_SSE_URL`.
- Optional env: `REMOTE_BEARER_TOKEN`, `UPSTREAM_TIMEOUT`.
- Optional env: `LOCAL_TOOLS_LIST` to force a local `tools/list` response.
- The bridge is long-running; prefer logging to stderr and JSON-RPC on stdout.
