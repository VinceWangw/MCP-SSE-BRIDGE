package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"mcp-sse-bridge/rpc"
)

func postJSONRPCWithResponse(ctx context.Context, client *http.Client, endpoint string, bearer string, msg rpc.JsonRPC) (rpc.JsonRPC, bool, error) {
	b, err := json.Marshal(msg)
	if err != nil {
		return rpc.JsonRPC{}, false, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return rpc.JsonRPC{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return rpc.JsonRPC{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := readLimited(resp.Body, 2<<20)
		return rpc.JsonRPC{}, false, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	contentType := resp.Header.Get("Content-Type")
	body, readErr := readLimited(resp.Body, 2<<20)
	if readErr != nil {
		return rpc.JsonRPC{}, false, readErr
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return rpc.JsonRPC{}, false, nil
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return rpc.JsonRPC{}, false, nil
	}
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:")) {
		return rpc.JsonRPC{}, false, nil
	}
	var out rpc.JsonRPC
	if err := json.Unmarshal(body, &out); err != nil {
		return rpc.JsonRPC{}, false, fmt.Errorf("invalid JSON response: %v", err)
	}
	return out, true, nil
}

func postJSONRPC(ctx context.Context, client *http.Client, endpoint string, bearer string, msg rpc.JsonRPC) error {
	_, _, err := postJSONRPCWithResponse(ctx, client, endpoint, bearer, msg)
	return err
}

func bestEffortPost(
	parent context.Context,
	client *http.Client,
	endpoint string,
	bearer string,
	msg rpc.JsonRPC,
	logger *log.Logger,
	label string,
) {
	if endpoint == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	if err := postJSONRPC(ctx, client, endpoint, bearer, msg); err != nil {
		logger.Printf("best-effort %s forward failed: %v", label, err)
	}
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		return io.ReadAll(r)
	}
	lr := &io.LimitedReader{R: r, N: limit + 1}
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return b[:limit], fmt.Errorf("response too large (limit %d bytes)", limit)
	}
	return b, nil
}
