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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

	sseCtx, sseCancel := context.WithCancel(ctx)
	baseURL, endpointURL, sseMsgCh, err := connectLegacySSE(sseCtx, httpClient, remote, bearer, logger)
	if err != nil {
		logger.Printf("failed to connect SSE: %v", err)
		os.Exit(1)
	}
	logger.Printf("connected. base=%s endpoint=%s", baseURL, endpointURL)

	var (
		mu      sync.Mutex
		outMu   sync.Mutex
		pending = map[string]chan JsonRPC{} // idKey -> response channel
	)
	initTracker := newUpstreamInitTracker()
	forwardInit := envBool("FORWARD_INITIALIZE", false)
	oneShotTools := envBool("ONE_SHOT_TOOLS", true)

	// STDIO loop
	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// SSE reader goroutine: route responses by id
	sseMgr := newSSEManager(endpointURL)
	sseMgr.set(endpointURL, sseMsgCh, sseCancel)
	go func() {
		activeCh := sseMsgCh
		for {
			select {
			case <-sseMgr.switchCh:
				activeCh = sseMgr.msgCh()
				select {
				case sseMgr.switchAck <- struct{}{}:
				default:
				}
				continue
			case msg, ok := <-activeCh:
				if !ok {
					current := sseMgr.msgCh()
					if current != nil && current != activeCh {
						activeCh = current
						continue
					}
					// If SSE closes, unblock all waiters with an error
					mu.Lock()
					for k, ch := range pending {
						delete(pending, k)
						resp := JsonRPC{
							JSONRPC: "2.0",
							ID:      k,
							Error: &RPCError{
								Code:    -32000,
								Message: "upstream SSE closed",
							},
						}
						select {
						case ch <- resp:
						default:
						}
					}
					mu.Unlock()

					// Reconnect SSE for future requests.
					backoff := 500 * time.Millisecond
					for {
						if ctx.Err() != nil {
							return
						}
						newEP, rerr := sseMgr.reconnect(ctx, httpClient, remote, bearer, logger)
						if rerr == nil {
							if !sseMgr.waitForSwitch(2 * time.Second) {
								logger.Printf("SSE switch ack timed out; continuing")
							}
							logger.Printf("SSE reconnected. new endpoint=%s", newEP)
							if forwardInit {
								initTracker.reinitialize(ctx, httpClient, sseMgr.endpoint, bearer, logger, &mu, pending)
							}
							activeCh = sseMgr.msgCh()
							break
						}
						logger.Printf("SSE reconnect failed: %v", rerr)
						time.Sleep(backoff)
						if backoff < 5*time.Second {
							backoff *= 2
						}
					}
					continue
				}
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
		}
	}()

	// detect framing style from first message
	firstMsg, style, err := readAnyMessage(in)
	if err != nil {
		logger.Printf("failed to read first message: %v", err)
		os.Exit(1)
	}
	if err := handleOne(ctx, httpClient, sseMgr, bearer, logger, firstMsg, &mu, &outMu, pending, initTracker, forwardInit, oneShotTools, out, style, remote); err != nil {
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
		if err := handleOne(ctx, httpClient, sseMgr, bearer, logger, msg, &mu, &outMu, pending, initTracker, forwardInit, oneShotTools, out, style, remote); err != nil {
			logger.Printf("handle error: %v", err)
		}
	}
}

func handleOne(
	ctx context.Context,
	httpClient *http.Client,
	sseMgr *sseManager,
	bearer string,
	logger *log.Logger,
	req JsonRPC,
	mu *sync.Mutex,
	outMu *sync.Mutex,
	pending map[string]chan JsonRPC,
	initTracker *upstreamInitTracker,
	forwardInit bool,
	oneShotTools bool,
	out *bufio.Writer,
	style string,
	// 为了重连：需要 remote SSE url
	remoteSSE string,
) error {
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}

	// 1) short-circuit initialize
	if req.Method == "initialize" {
		result := json.RawMessage([]byte(`{
			"protocolVersion":"2025-03-26",
			"capabilities": { "tools": {} },
			"serverInfo": { "name":"legacy-sse-bridge", "version":"0.1.1" }
		}`))
		resp := JsonRPC{JSONRPC: "2.0", ID: req.ID, Result: &result}
		if err := writeMessageLocked(outMu, out, resp, style); err != nil {
			return err
		}
		if forwardInit {
			initTracker.start(ctx, httpClient, sseMgr.endpoint, bearer, logger, req, mu, pending)
		}
		return nil
	}

	// 2) swallow initialized
	if req.Method == "initialized" {
		if forwardInit {
			initTracker.wait(defaultInitTimeout())
			go bestEffortPost(ctx, httpClient, sseMgr.endpoint(), bearer, req, logger, "initialized")
		}
		return nil
	}

	// 3) tools/list local
	if req.Method == "tools/list" {
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
		resp := JsonRPC{JSONRPC: "2.0", ID: req.ID, Result: &result}
		return writeMessageLocked(outMu, out, resp, style)
	}

	if oneShotTools && req.Method == "tools/call" && req.ID != nil {
		var lastErr error
		for i := 0; i < oneShotAttempts(); i++ {
			if i > 0 {
				time.Sleep(reconnectPostDelay())
			}
			if resp, err := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeoutFromEnv(), logger, true); err == nil {
				resp.ID = req.ID
				if resp.JSONRPC == "" {
					resp.JSONRPC = "2.0"
				}
				return writeMessageLocked(outMu, out, resp, style)
			} else {
				lastErr = err
			}
		}
		if lastErr != nil {
			logger.Printf("one-shot tools/call failed after %d attempts: %v", oneShotAttempts(), lastErr)
		}
	}

	// Notification (no id)
	if req.ID == nil {
		if forwardInit {
			initTracker.wait(defaultInitTimeout())
		}
		ep := sseMgr.endpoint()
		if err := postJSONRPC(ctx, httpClient, ep, bearer, req); err != nil {
			logger.Printf("notify forward failed: %v", err)
		}
		return nil
	}

	if forwardInit && !initTracker.wait(defaultInitTimeout()) {
		logger.Printf("upstream initialize not ready after %s; continuing", defaultInitTimeout())
	}

	// Request with id
	idKey := idToKey(req.ID)
	ch := make(chan JsonRPC, 1)

	mu.Lock()
	pending[idKey] = ch
	mu.Unlock()

	defer func() {
		mu.Lock()
		delete(pending, idKey)
		mu.Unlock()
	}()

	timeout := 120 * time.Second
	if v := strings.TrimSpace(os.Getenv("UPSTREAM_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	// helper: send+wait once
	sendOnce := func() (JsonRPC, error) {
		ep := sseMgr.endpoint()
		logger.Printf(">> to upstream method=%s id=%v endpoint=%s", req.Method, req.ID, ep)

		if err := postJSONRPC(ctx, httpClient, ep, bearer, req); err != nil {
			return JsonRPC{}, err
		}

		select {
		case resp := <-ch:
			return resp, nil
		case <-time.After(timeout):
			return JsonRPC{}, fmt.Errorf("timeout waiting upstream response")
		}
	}

	// try #1
	resp, err := sendOnce()
	if err == nil {
		logger.Printf("<< from upstream id=%v hasResult=%v hasError=%v", resp.ID, resp.Result != nil, resp.Error != nil)
		resp.ID = req.ID
		if resp.JSONRPC == "" {
			resp.JSONRPC = "2.0"
		}
		return writeMessageLocked(outMu, out, resp, style)
	}

	// If error indicates SessionId invalid, reconnect SSE and retry once
	errStr := err.Error()
	if strings.Contains(errStr, "SessionId invalid") || strings.Contains(errStr, "sessionid invalid") {
		if resp0, err0 := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeout, logger, true); err0 == nil {
			logger.Printf("<< from upstream(one-shot) id=%v hasResult=%v hasError=%v", resp0.ID, resp0.Result != nil, resp0.Error != nil)
			resp0.ID = req.ID
			if resp0.JSONRPC == "" {
				resp0.JSONRPC = "2.0"
			}
			return writeMessageLocked(outMu, out, resp0, style)
		}

		logger.Printf("upstream session invalid, reconnecting SSE then retrying once...")

		// reconnect SSE to get a new endpoint (new sessionId)
		newEP, rerr := sseMgr.reconnect(ctx, httpClient, remoteSSE, bearer, logger)
		if rerr != nil {
			// can't recover
			resp := JsonRPC{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &RPCError{
					Code:    -32001,
					Message: "upstream POST failed and reconnect failed: " + rerr.Error(),
				},
			}
			return writeMessageLocked(outMu, out, resp, style)
		}
		if !sseMgr.waitForSwitch(2 * time.Second) {
			logger.Printf("SSE switch ack timed out; continuing")
		}
		logger.Printf("reconnected. new endpoint=%s", newEP)
		if forwardInit {
			initTracker.reinitialize(ctx, httpClient, sseMgr.endpoint, bearer, logger, mu, pending)
		}
		if d := reconnectPostDelay(); d > 0 {
			time.Sleep(d)
		}

		// retry #2
		resp2, err2 := sendOnce()
		if err2 == nil {
			logger.Printf("<< from upstream(after retry) id=%v hasResult=%v hasError=%v", resp2.ID, resp2.Result != nil, resp2.Error != nil)
			resp2.ID = req.ID
			if resp2.JSONRPC == "" {
				resp2.JSONRPC = "2.0"
			}
			return writeMessageLocked(outMu, out, resp2, style)
		}

		if strings.Contains(err2.Error(), "SessionId invalid") || strings.Contains(err2.Error(), "sessionid invalid") {
			if resp3, err3 := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeout, logger, false); err3 == nil {
				logger.Printf("<< from upstream(one-shot) id=%v hasResult=%v hasError=%v", resp3.ID, resp3.Result != nil, resp3.Error != nil)
				resp3.ID = req.ID
				if resp3.JSONRPC == "" {
					resp3.JSONRPC = "2.0"
				}
				return writeMessageLocked(outMu, out, resp3, style)
			}
		}

		// still failing after retry
		respFail := JsonRPC{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &RPCError{
				Code:    -32001,
				Message: "upstream failed after reconnect+retry: " + err2.Error(),
			},
		}
		return writeMessageLocked(outMu, out, respFail, style)
	}

	if strings.Contains(errStr, "upstream SSE closed") {
		if resp, err := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeout, logger, true); err == nil {
			logger.Printf("<< from upstream(one-shot after SSE closed) id=%v hasResult=%v hasError=%v", resp.ID, resp.Result != nil, resp.Error != nil)
			resp.ID = req.ID
			if resp.JSONRPC == "" {
				resp.JSONRPC = "2.0"
			}
			return writeMessageLocked(outMu, out, resp, style)
		}
	}

	// generic error
	respErr := JsonRPC{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: &RPCError{
			Code:    -32001,
			Message: "upstream POST failed: " + errStr,
		},
	}
	return writeMessageLocked(outMu, out, respErr, style)
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

type sseManager struct {
	mu          sync.Mutex
	endpointVal atomic.Value
	msgChVal    atomic.Value
	switchCh    chan struct{}
	switchAck   chan struct{}
	cancel      context.CancelFunc
}

func newSSEManager(endpoint string) *sseManager {
	m := &sseManager{
		switchCh:  make(chan struct{}, 1),
		switchAck: make(chan struct{}, 1),
	}
	m.endpointVal.Store(endpoint)
	return m
}

func (m *sseManager) endpoint() string {
	v := m.endpointVal.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func (m *sseManager) msgCh() <-chan JsonRPC {
	v := m.msgChVal.Load()
	if v == nil {
		return nil
	}
	return v.(<-chan JsonRPC)
}

func (m *sseManager) set(endpoint string, msgCh <-chan JsonRPC, cancel context.CancelFunc) {
	m.endpointVal.Store(endpoint)
	m.msgChVal.Store(msgCh)
	m.cancel = cancel
	select {
	case m.switchCh <- struct{}{}:
	default:
	}
}

func (m *sseManager) waitForSwitch(timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	select {
	case <-m.switchAck:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (m *sseManager) reconnect(
	parent context.Context,
	client *http.Client,
	remote string,
	bearer string,
	logger *log.Logger,
) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	ctx, cancel := context.WithCancel(parent)
	_, endpoint, msgCh, err := connectLegacySSE(ctx, client, remote, bearer, logger)
	if err != nil {
		cancel()
		return "", err
	}
	m.set(endpoint, msgCh, cancel)
	return endpoint, nil
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

type upstreamInitTracker struct {
	started atomic.Bool
	ready   chan struct{}
	lastReq atomic.Value // JsonRPC
}

func newUpstreamInitTracker() *upstreamInitTracker {
	return &upstreamInitTracker{ready: make(chan struct{})}
}

func (t *upstreamInitTracker) start(
	ctx context.Context,
	client *http.Client,
	endpointFn func() string,
	bearer string,
	logger *log.Logger,
	req JsonRPC,
	mu *sync.Mutex,
	pending map[string]chan JsonRPC,
) {
	initReq := req
	initReq.ID = fmt.Sprintf("bridge-init-%d", time.Now().UnixNano())
	t.lastReq.Store(initReq)

	if t.started.Swap(true) {
		return
	}
	go func() {
		defer close(t.ready)
		if !t.sendAndWait(ctx, client, endpointFn, bearer, logger, initReq, mu, pending) {
			logger.Printf("upstream initialize did not complete before timeout")
		}
	}()
}

func (t *upstreamInitTracker) reinitialize(
	ctx context.Context,
	client *http.Client,
	endpointFn func() string,
	bearer string,
	logger *log.Logger,
	mu *sync.Mutex,
	pending map[string]chan JsonRPC,
) {
	if !t.started.Load() {
		return
	}
	v := t.lastReq.Load()
	initReq, ok := v.(JsonRPC)
	if !ok {
		return
	}
	if !t.sendAndWait(ctx, client, endpointFn, bearer, logger, initReq, mu, pending) {
		logger.Printf("upstream re-initialize did not complete before timeout")
	}
}

func (t *upstreamInitTracker) wait(timeout time.Duration) bool {
	if !t.started.Load() {
		return true
	}
	select {
	case <-t.ready:
		return true
	case <-time.After(timeout):
		return false
	}
}

func (t *upstreamInitTracker) sendAndWait(
	ctx context.Context,
	client *http.Client,
	endpointFn func() string,
	bearer string,
	logger *log.Logger,
	req JsonRPC,
	mu *sync.Mutex,
	pending map[string]chan JsonRPC,
) bool {
	ep := endpointFn()
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

	if err := postJSONRPC(ctx, client, ep, bearer, req); err != nil {
		logger.Printf("upstream initialize forward failed: %v", err)
		return false
	}

	select {
	case <-ch:
		return true
	case <-time.After(defaultInitTimeout()):
		return false
	}
}

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

func writeMessageLocked(mu *sync.Mutex, w *bufio.Writer, msg JsonRPC, style string) error {
	mu.Lock()
	defer mu.Unlock()
	return writeMessage(w, msg, style)
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

func callWithFreshSession(
	parent context.Context,
	client *http.Client,
	remoteSSE string,
	bearer string,
	req JsonRPC,
	timeout time.Duration,
	logger *log.Logger,
	delay bool,
) (JsonRPC, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	_, endpoint, msgCh, err := connectLegacySSE(ctx, client, remoteSSE, bearer, logger)
	if err != nil {
		return JsonRPC{}, err
	}

	if delay {
		time.Sleep(reconnectPostDelay())
	}

	respCh := make(chan JsonRPC, 1)
	idKey := idToKey(req.ID)
	go func() {
		for msg := range msgCh {
			if msg.ID == nil {
				continue
			}
			if idToKey(msg.ID) == idKey {
				respCh <- msg
				return
			}
		}
	}()

	if err := postJSONRPC(ctx, client, endpoint, bearer, req); err != nil {
		return JsonRPC{}, err
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-time.After(timeout):
		return JsonRPC{}, fmt.Errorf("timeout waiting upstream response")
	}
}
