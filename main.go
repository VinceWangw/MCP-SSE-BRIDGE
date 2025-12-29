package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type JsonRPC struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      any              `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  *json.RawMessage `json:"params,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func main() {
	remote := strings.TrimSpace(os.Getenv("REMOTE_SSE_URL"))
	if remote == "" {
		fmt.Fprintln(os.Stderr, "REMOTE_SSE_URL is required (e.g. https://.../mcp-servers/keyword-expand)")
		os.Exit(2)
	}
	bearer := strings.TrimSpace(os.Getenv("REMOTE_BEARER_TOKEN")) // optional

	logger := log.New(os.Stderr, "[bridge] ", log.LstdFlags|log.Lmicroseconds)

	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{
		Timeout: 0,
		Jar:     jar,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	baseURL, endpointURL, sseMsgCh, err := connectLegacySSE(ctx, httpClient, remote, bearer, logger)
	if err != nil {
		logger.Printf("failed to connect SSE: %v", err)
		os.Exit(1)
	}
	logger.Printf("connected. base=%s endpoint=%s", baseURL, endpointURL)

	var (
		mu      sync.Mutex
		pending = map[string]chan JsonRPC{} // idKey -> response channel
	)

	// SSE reader goroutine: route responses by id
	go func() {
		for msg := range sseMsgCh {
			if msg.ID == nil {
				continue
			}
			idKey := idToKey(msg.ID)

			mu.Lock()
			ch := pending[idKey]
			mu.Unlock()
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
		}

		// If SSE closes, unblock all waiters with an error
		mu.Lock()
		for k, ch := range pending {
			delete(pending, k)
			_ = writeMessage(bufio.NewWriter(os.Stdout), JsonRPC{
				JSONRPC: "2.0",
				ID:      k,
				Error: &RPCError{
					Code:    -32000,
					Message: "upstream SSE closed",
				},
			}, "jsonl")
			close(ch)
		}
		mu.Unlock()
	}()

	// STDIO loop
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// detect framing style from first message
	firstMsg, style, err := readAnyMessage(in)
	if err != nil {
		logger.Printf("failed to read first message: %v", err)
		os.Exit(1)
	}
	if err := handleOne(ctx, httpClient, endpointURL, bearer, logger, firstMsg, &mu, pending, out, style); err != nil {
		logger.Printf("handle error: %v", err)
	}

	for {
		msg, _, err := readAnyMessage(in)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			logger.Printf("read error: %v", err)
			return
		}
		if err := handleOne(ctx, httpClient, endpointURL, bearer, logger, msg, &mu, pending, out, style); err != nil {
			logger.Printf("handle error: %v", err)
		}
	}
}

func handleOne(
	ctx context.Context,
	httpClient *http.Client,
	endpointURL string,
	bearer string,
	logger *log.Logger,
	req JsonRPC,
	mu *sync.Mutex,
	pending map[string]chan JsonRPC,
	out *bufio.Writer,
	style string,
) error {
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}

	// 1) Codex 启动阶段：短路 initialize，避免等待上游 legacy 反应（否则会 boot 超时）
	if req.Method == "initialize" {
		// Minimal InitializeResult for MCP (enough for Codex to proceed)
		result := json.RawMessage([]byte(`{
			"protocolVersion":"2025-03-26",
			"capabilities": { "tools": {} },
			"serverInfo": { "name":"legacy-sse-bridge", "version":"0.1.0" }
		}`))
		resp := JsonRPC{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  &result,
		}
		if err := writeMessage(out, resp, style); err != nil {
			return err
		}
		go bestEffortPost(ctx, httpClient, endpointURL, bearer, req, logger, "initialize")
		return nil
	}

	// 2) 客户端可能会发 initialized 通知：吞掉即可
	if req.Method == "initialized" {
		go bestEffortPost(ctx, httpClient, endpointURL, bearer, req, logger, "initialized")
		return nil
	}

	// 3) tools/list：本地返回你这个 MCP 服务的工具清单（让 Codex 能看到工具）
	// MCP 标准方法名是 "tools/list"
	if req.Method == "tools/list" {
		// 你后台页面显示的工具：expand_search_keyword
		result := json.RawMessage([]byte(`{
			"tools": [
				{
					"name": "expand_search_keyword",
					"description": "ads agent 拓词，关键词同义扩展",
					"inputSchema": {
						"type": "object",
						"properties": {
							"keyword": { "type": "string", "description": "输入关键词" }
						},
						"required": ["keyword"]
					}
				}
			]
		}`))
		resp := JsonRPC{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  &result,
		}
		return writeMessage(out, resp, style)
	}

	// 4) Notification (no id) - best effort forward
	if req.ID == nil {
		if err := postJSONRPC(ctx, httpClient, endpointURL, bearer, req); err != nil {
			logger.Printf("notify forward failed: %v", err)
		}
		return nil
	}

	// 5) 有 id 的请求：正常走“转发上游 + 等 SSE 回包”的逻辑
	idKey := idToKey(req.ID)
	ch := make(chan JsonRPC, 1)

	mu.Lock()
	pending[idKey] = ch
	mu.Unlock()

	defer func() {
		mu.Lock()
		delete(pending, idKey)
		mu.Unlock()
		close(ch)
	}()

	logger.Printf(">> to upstream method=%s id=%v", req.Method, req.ID)

	// Send upstream
	if err := postJSONRPC(ctx, httpClient, endpointURL, bearer, req); err != nil {
		resp := JsonRPC{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &RPCError{
				Code:    -32001,
				Message: "upstream POST failed: " + err.Error(),
			},
		}
		return writeMessage(out, resp, style)
	}

	// Wait response from SSE
	timeout := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("UPSTREAM_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	select {
	case resp := <-ch:
		logger.Printf("<< from upstream id=%v hasResult=%v hasError=%v", resp.ID, resp.Result != nil, resp.Error != nil)

		resp.ID = req.ID // 保持 id 类型一致（Codex 有时要求）
		if resp.JSONRPC == "" {
			resp.JSONRPC = "2.0"
		}
		return writeMessage(out, resp, style)

	case <-time.After(timeout):
		resp := JsonRPC{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &RPCError{
				Code:    -32002,
				Message: "timeout waiting upstream response",
			},
		}
		return writeMessage(out, resp, style)
	}
}

func connectLegacySSE(
	ctx context.Context,
	client *http.Client,
	remote string,
	bearer string,
	logger *log.Logger,
) (base string, endpoint string, msgCh <-chan JsonRPC, err error) {
	u, err := url.Parse(remote)
	if err != nil {
		return "", "", nil, err
	}
	base = fmt.Sprintf("%s://%s", u.Scheme, u.Host)

	req, err := http.NewRequestWithContext(ctx, "GET", remote, nil)
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return "", "", nil, fmt.Errorf("SSE status=%d body=%s", resp.StatusCode, string(b))
	}

	outCh := make(chan JsonRPC, 64)
	endpointCh := make(chan string, 1)

	go func() {
		defer close(outCh)
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)
		var eventName string
		var dataLines []string

		flush := func() {
			if len(dataLines) == 0 && eventName == "" {
				return
			}
			data := strings.Join(dataLines, "\n")

			if eventName == "endpoint" {
				ep := strings.TrimSpace(data)
				if strings.HasPrefix(ep, "/") {
					ep = base + ep
				}
				select {
				case endpointCh <- ep:
				default:
				}
				eventName = ""
				dataLines = nil
				return
			}

			if eventName == "message" || eventName == "" {
				var m JsonRPC
				if json.Unmarshal([]byte(data), &m) == nil {
					outCh <- m
				}
			}

			eventName = ""
			dataLines = nil
		}

		for {
			line, rerr := reader.ReadString('\n')
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					return
				}
				logger.Printf("SSE read error: %v", rerr)
				return
			}
			line = strings.TrimRight(line, "\r\n")

			if line == "" {
				flush()
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				continue
			}
		}
	}()

	select {
	case ep := <-endpointCh:
		if ep == "" {
			return base, "", nil, errors.New("empty endpoint")
		}
		return base, ep, outCh, nil
	case <-time.After(10 * time.Second):
		return base, "", nil, errors.New("timeout waiting SSE endpoint event")
	case <-ctx.Done():
		return base, "", nil, ctx.Err()
	}
}

func postJSONRPC(ctx context.Context, client *http.Client, endpoint string, bearer string, msg JsonRPC) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

func bestEffortPost(
	parent context.Context,
	client *http.Client,
	endpoint string,
	bearer string,
	msg JsonRPC,
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

func readAnyMessage(r *bufio.Reader) (JsonRPC, string, error) {
	peek, err := r.Peek(14)
	if err != nil && !errors.Is(err, io.EOF) {
		return JsonRPC{}, "", err
	}
	if strings.HasPrefix(string(peek), "Content-Length") {
		msg, err := readContentLength(r)
		return msg, "content-length", err
	}
	line, err := r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
			return JsonRPC{}, "", io.EOF
		}
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return readAnyMessage(r)
	}
	var m JsonRPC
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return JsonRPC{}, "jsonl", err
	}
	return m, "jsonl", nil
}

func readContentLength(r *bufio.Reader) (JsonRPC, error) {
	var contentLen int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return JsonRPC{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			fmt.Sscanf(line, "Content-Length: %d", &contentLen)
		}
	}
	if contentLen <= 0 {
		return JsonRPC{}, errors.New("invalid Content-Length")
	}
	buf := make([]byte, contentLen)
	_, err := io.ReadFull(r, buf)
	if err != nil {
		return JsonRPC{}, err
	}
	var m JsonRPC
	if err := json.Unmarshal(buf, &m); err != nil {
		return JsonRPC{}, err
	}
	return m, nil
}

func writeMessage(w *bufio.Writer, msg JsonRPC, style string) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if style == "content-length" {
		if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(b)); err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		return w.Flush()
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.Flush()
}
