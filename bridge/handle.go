package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"mcp-sse-bridge/rpc"
	"mcp-sse-bridge/sse"
)

func handleOne(
	ctx context.Context,
	httpClient *http.Client,
	sseMgr *sse.Manager,
	bearer string,
	logger *log.Logger,
	req rpc.JsonRPC,
	mu *sync.Mutex,
	outMu *sync.Mutex,
	pending map[string]chan rpc.JsonRPC,
	initTracker *upstreamInitTracker,
	forwardInit bool,
	oneShotTools bool,
	localToolsList bool,
	out *bufio.Writer,
	style string,
	remoteSSE string,
) error {
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}

	if req.Method == "initialize" {
		result := json.RawMessage([]byte(`{
			"protocolVersion":"2025-03-26",
			"capabilities": { "tools": {} },
			"serverInfo": { "name":"legacy-sse-bridge", "version":"0.1.1" }
		}`))
		resp := rpc.JsonRPC{JSONRPC: "2.0", ID: req.ID, Result: &result}
		if err := rpc.WriteMessageLocked(outMu, out, resp, style); err != nil {
			return err
		}
		if forwardInit {
			initTracker.start(ctx, httpClient, sseMgr.Endpoint, bearer, logger, req, mu, pending)
		}
		return nil
	}

	if req.Method == "initialized" {
		if forwardInit {
			initTracker.wait(defaultInitTimeout())
			go bestEffortPost(ctx, httpClient, sseMgr.Endpoint(), bearer, req, logger, "initialized")
		}
		return nil
	}

	if req.Method == "tools/list" && localToolsList {
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
		resp := rpc.JsonRPC{JSONRPC: "2.0", ID: req.ID, Result: &result}
		return rpc.WriteMessageLocked(outMu, out, resp, style)
	}

	if oneShotTools && req.Method == "tools/call" && req.ID != nil {
		var lastErr error
		for i := 0; i < oneShotAttempts(); i++ {
			if i > 0 {
				time.Sleep(reconnectPostDelay())
			}
			resp, err := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeoutFromEnv(), logger, true, initTracker, forwardInit)
			if err == nil {
				resp.ID = req.ID
				if resp.JSONRPC == "" {
					resp.JSONRPC = "2.0"
				}
				return rpc.WriteMessageLocked(outMu, out, resp, style)
			}
			lastErr = err
		}
		if lastErr != nil {
			logger.Printf("one-shot tools/call failed after %d attempts: %v", oneShotAttempts(), lastErr)
		}
	}

	if req.ID == nil {
		if forwardInit {
			initTracker.wait(defaultInitTimeout())
		}
		ep := sseMgr.Endpoint()
		if err := postJSONRPC(ctx, httpClient, ep, bearer, req); err != nil {
			logger.Printf("notify forward failed: %v", err)
		}
		return nil
	}

	if forwardInit && !initTracker.wait(defaultInitTimeout()) {
		logger.Printf("upstream initialize not ready after %s; continuing", defaultInitTimeout())
	}

	idKey := idToKey(req.ID)
	ch := make(chan rpc.JsonRPC, 1)

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

	sendOnce := func() (rpc.JsonRPC, error) {
		ep := sseMgr.Endpoint()
		logger.Printf(">> to upstream method=%s id=%v endpoint=%s", req.Method, req.ID, ep)

		if respDirect, ok, err := postJSONRPCWithResponse(ctx, httpClient, ep, bearer, req); err == nil {
			if ok {
				return respDirect, nil
			}
		} else if strings.Contains(err.Error(), "SessionId invalid") || strings.Contains(err.Error(), "Not Acceptable") {
			logger.Printf("retrying POST via base url due to endpoint error: %v", err)
			if respBase, ok2, err2 := postJSONRPCWithResponse(ctx, httpClient, remoteSSE, bearer, req); err2 == nil {
				if ok2 {
					return respBase, nil
				}
			} else {
				return rpc.JsonRPC{}, err2
			}
		} else {
			return rpc.JsonRPC{}, err
		}

		select {
		case resp := <-ch:
			return resp, nil
		case <-time.After(timeout):
			return rpc.JsonRPC{}, fmt.Errorf("timeout waiting upstream response")
		}
	}

	resp, err := sendOnce()
	if err == nil {
		logger.Printf("<< from upstream id=%v hasResult=%v hasError=%v", resp.ID, resp.Result != nil, resp.Error != nil)
		resp.ID = req.ID
		if resp.JSONRPC == "" {
			resp.JSONRPC = "2.0"
		}
		return rpc.WriteMessageLocked(outMu, out, resp, style)
	}

	errStr := err.Error()
	if strings.Contains(errStr, "SessionId invalid") || strings.Contains(errStr, "sessionid invalid") {
		if resp0, err0 := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeout, logger, true, initTracker, forwardInit); err0 == nil {
			logger.Printf("<< from upstream(one-shot) id=%v hasResult=%v hasError=%v", resp0.ID, resp0.Result != nil, resp0.Error != nil)
			resp0.ID = req.ID
			if resp0.JSONRPC == "" {
				resp0.JSONRPC = "2.0"
			}
			return rpc.WriteMessageLocked(outMu, out, resp0, style)
		}

		logger.Printf("upstream session invalid, reconnecting SSE then retrying once...")

		newEP, rerr := sseMgr.Reconnect(ctx, httpClient, remoteSSE, bearer, logger)
		if rerr != nil {
			respFail := rpc.JsonRPC{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &rpc.RPCError{
					Code:    -32001,
					Message: "upstream POST failed and reconnect failed: " + rerr.Error(),
				},
			}
			return rpc.WriteMessageLocked(outMu, out, respFail, style)
		}
		if !sseMgr.WaitForSwitch(2 * time.Second) {
			logger.Printf("SSE switch ack timed out; continuing")
		}
		logger.Printf("reconnected. new endpoint=%s", newEP)
		if forwardInit {
			initTracker.reinitialize(ctx, httpClient, sseMgr.Endpoint, bearer, logger, mu, pending)
		}
		if d := reconnectPostDelay(); d > 0 {
			time.Sleep(d)
		}

		resp2, err2 := sendOnce()
		if err2 == nil {
			logger.Printf("<< from upstream(after retry) id=%v hasResult=%v hasError=%v", resp2.ID, resp2.Result != nil, resp2.Error != nil)
			resp2.ID = req.ID
			if resp2.JSONRPC == "" {
				resp2.JSONRPC = "2.0"
			}
			return rpc.WriteMessageLocked(outMu, out, resp2, style)
		}

		if strings.Contains(err2.Error(), "SessionId invalid") || strings.Contains(err2.Error(), "sessionid invalid") {
			if resp3, err3 := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeout, logger, false, initTracker, forwardInit); err3 == nil {
				logger.Printf("<< from upstream(one-shot) id=%v hasResult=%v hasError=%v", resp3.ID, resp3.Result != nil, resp3.Error != nil)
				resp3.ID = req.ID
				if resp3.JSONRPC == "" {
					resp3.JSONRPC = "2.0"
				}
				return rpc.WriteMessageLocked(outMu, out, resp3, style)
			}
		}

		respFail := rpc.JsonRPC{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpc.RPCError{
				Code:    -32001,
				Message: "upstream failed after reconnect+retry: " + err2.Error(),
			},
		}
		return rpc.WriteMessageLocked(outMu, out, respFail, style)
	}

	if strings.Contains(errStr, "upstream SSE closed") {
		if resp, err := callWithFreshSession(ctx, httpClient, remoteSSE, bearer, req, timeout, logger, true, initTracker, forwardInit); err == nil {
			logger.Printf("<< from upstream(one-shot after SSE closed) id=%v hasResult=%v hasError=%v", resp.ID, resp.Result != nil, resp.Error != nil)
			resp.ID = req.ID
			if resp.JSONRPC == "" {
				resp.JSONRPC = "2.0"
			}
			return rpc.WriteMessageLocked(outMu, out, resp, style)
		}
	}

	respErr := rpc.JsonRPC{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error: &rpc.RPCError{
			Code:    -32001,
			Message: "upstream POST failed: " + errStr,
		},
	}
	return rpc.WriteMessageLocked(outMu, out, respErr, style)
}

func callWithFreshSession(
	parent context.Context,
	client *http.Client,
	remoteSSE string,
	bearer string,
	req rpc.JsonRPC,
	timeout time.Duration,
	logger *log.Logger,
	delay bool,
	initTracker *upstreamInitTracker,
	forwardInit bool,
) (rpc.JsonRPC, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	_, endpoint, msgCh, err := sse.ConnectLegacySSE(ctx, client, remoteSSE, bearer, logger)
	if err != nil {
		return rpc.JsonRPC{}, err
	}

	if delay {
		time.Sleep(reconnectPostDelay())
	}

	respCh := make(chan rpc.JsonRPC, 1)
	var initCh chan rpc.JsonRPC
	var initKey string
	idKey := idToKey(req.ID)
	go func() {
		for msg := range msgCh {
			if msg.ID == nil {
				continue
			}
			key := idToKey(msg.ID)
			if initCh != nil && key == initKey {
				initCh <- msg
				continue
			}
			if key == idKey {
				respCh <- msg
				return
			}
		}
	}()

	if forwardInit && req.Method == "tools/list" && initTracker != nil {
		if initReq, ok := initTracker.initRequest(); ok {
			initCh = make(chan rpc.JsonRPC, 1)
			initKey = idToKey(initReq.ID)
			if _, okResp, err := postJSONRPCWithResponse(ctx, client, endpoint, bearer, initReq); err == nil {
				if okResp {
					goto initDone
				}
			} else if strings.Contains(err.Error(), "SessionId invalid") || strings.Contains(err.Error(), "Not Acceptable") {
				if _, okResp2, err2 := postJSONRPCWithResponse(ctx, client, remoteSSE, bearer, initReq); err2 == nil {
					if okResp2 {
						goto initDone
					}
				} else {
					return rpc.JsonRPC{}, err2
				}
			} else {
				return rpc.JsonRPC{}, err
			}
			select {
			case <-initCh:
			case <-time.After(defaultInitTimeout()):
				return rpc.JsonRPC{}, fmt.Errorf("timeout waiting upstream initialize")
			}
		initDone:
		}
	}

	if respDirect, okResp, err := postJSONRPCWithResponse(ctx, client, endpoint, bearer, req); err == nil {
		if okResp {
			return respDirect, nil
		}
	} else if strings.Contains(err.Error(), "SessionId invalid") || strings.Contains(err.Error(), "Not Acceptable") {
		if respBase, okResp2, err2 := postJSONRPCWithResponse(ctx, client, remoteSSE, bearer, req); err2 == nil {
			if okResp2 {
				return respBase, nil
			}
		} else {
			return rpc.JsonRPC{}, err2
		}
	} else {
		return rpc.JsonRPC{}, err
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-time.After(timeout):
		return rpc.JsonRPC{}, fmt.Errorf("timeout waiting upstream response")
	}
}
