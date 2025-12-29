# Repository Guidelines

## Project Structure & Module Organization
- `main.go` contains the entire bridge implementation (stdio ↔ legacy SSE proxy).
- `go.mod` defines the Go module (`mcp-sse-bridge`) and Go version.
- `mcp-sse-bridge` is the built binary (if present) and can be regenerated.

## Build, Test, and Development Commands
- `go build ./...` builds the binary and validates module compilation.
- `go run ./...` runs the bridge locally for quick iteration.
- Example run:
  ```bash
  REMOTE_SSE_URL="https://host/mcp-servers/keyword-expand" \
  REMOTE_BEARER_TOKEN="..." \
  go run ./...
  ```
- Optional: `UPSTREAM_TIMEOUT=120s` overrides the upstream SSE response wait.

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
- The bridge is long-running; prefer logging to stderr and JSON-RPC on stdout.
