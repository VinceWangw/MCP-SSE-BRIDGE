#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${REMOTE_SSE_URL:-}" ]]; then
  echo "REMOTE_SSE_URL is required" >&2
  exit 2
fi

if ! command -v go >/dev/null 2>&1; then
  echo "go is required in PATH" >&2
  exit 2
fi

runner=(go run ./...)
if [[ -n "${BRIDGE_BIN:-}" ]]; then
  runner=("${BRIDGE_BIN}")
fi

timeout_cmd=()
if command -v timeout >/dev/null 2>&1; then
  timeout_cmd=(timeout 20s)
fi

init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}'
list='{"jsonrpc":"2.0","id":2,"method":"tools/list"}'

printf '%s\n%s\n' "$init" "$list" | \
  REMOTE_SSE_URL="$REMOTE_SSE_URL" \
  REMOTE_BEARER_TOKEN="${REMOTE_BEARER_TOKEN:-}" \
  "${timeout_cmd[@]}" "${runner[@]}"
