package main

import (
	"os"

	"mcp-sse-bridge/bridge"
)

func main() {
	os.Exit(bridge.Run())
}
