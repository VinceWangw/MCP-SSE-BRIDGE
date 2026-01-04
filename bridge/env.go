package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

func defaultInitTimeout() time.Duration {
	timeout := 8 * time.Second
	if v := strings.TrimSpace(os.Getenv("UPSTREAM_INIT_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	return timeout
}

func timeoutFromEnv() time.Duration {
	timeout := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("UPSTREAM_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}
	return timeout
}

func oneShotAttempts() int {
	if v := strings.TrimSpace(os.Getenv("ONE_SHOT_MAX_RETRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 3
}

func idToKey(id any) string {
	switch v := id.(type) {
	case string:
		return v
	case float64:
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func envBool(name string, defaultValue bool) bool {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return defaultValue
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return defaultValue
	}
}

func reconnectPostDelay() time.Duration {
	ms := strings.TrimSpace(os.Getenv("RECONNECT_POST_DELAY_MS"))
	if ms == "" {
		return 300 * time.Millisecond
	}
	v, err := time.ParseDuration(ms + "ms")
	if err != nil {
		return 300 * time.Millisecond
	}
	return v
}
