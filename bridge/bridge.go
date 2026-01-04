package bridge

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync"
	"time"

	"mcp-sse-bridge/rpc"
	"mcp-sse-bridge/sse"
)

func Run() int {
	remote := strings.TrimSpace(os.Getenv("REMOTE_SSE_URL"))
	if remote == "" {
		fmt.Fprintln(os.Stderr, "REMOTE_SSE_URL is required (e.g. https://host/mcp-servers/example)")
		return 2
	}
	bearer := strings.TrimSpace(os.Getenv("REMOTE_BEARER_TOKEN"))

	logger := log.New(os.Stderr, "[bridge] ", log.LstdFlags|log.Lmicroseconds)

	jar, _ := cookiejar.New(nil)
	httpClient := &http.Client{
		Timeout: 0,
		Jar:     jar,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sseCtx, sseCancel := context.WithCancel(ctx)
	baseURL, endpointURL, sseMsgCh, err := sse.ConnectLegacySSE(sseCtx, httpClient, remote, bearer, logger)
	if err != nil {
		logger.Printf("failed to connect SSE: %v", err)
		return 1
	}
	logger.Printf("connected. base=%s endpoint=%s", baseURL, endpointURL)

	var (
		mu      sync.Mutex
		outMu   sync.Mutex
		pending = map[string]chan rpc.JsonRPC{}
	)
	initTracker := newUpstreamInitTracker()
	forwardInit := envBool("FORWARD_INITIALIZE", false)
	oneShotTools := envBool("ONE_SHOT_TOOLS", true)
	localToolsList := envBool("LOCAL_TOOLS_LIST", false)

	in := bufio.NewReader(os.Stdin)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	sseMgr := sse.NewManager(endpointURL)
	sseMgr.Set(endpointURL, sseMsgCh, sseCancel)
	go func() {
		activeCh := sseMsgCh
		for {
			select {
			case <-sseMgr.SwitchCh():
				activeCh = sseMgr.MsgCh()
				sseMgr.AckSwitch()
				continue
			case msg, ok := <-activeCh:
				if !ok {
					current := sseMgr.MsgCh()
					if current != nil && current != activeCh {
						activeCh = current
						continue
					}
					mu.Lock()
					for k, ch := range pending {
						delete(pending, k)
						resp := rpc.JsonRPC{
							JSONRPC: "2.0",
							ID:      k,
							Error: &rpc.RPCError{
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

					backoff := 500 * time.Millisecond
					for {
						if ctx.Err() != nil {
							return
						}
						newEP, rerr := sseMgr.Reconnect(ctx, httpClient, remote, bearer, logger)
						if rerr == nil {
							if !sseMgr.WaitForSwitch(2 * time.Second) {
								logger.Printf("SSE switch ack timed out; continuing")
							}
							logger.Printf("SSE reconnected. new endpoint=%s", newEP)
							if forwardInit {
								initTracker.reinitialize(ctx, httpClient, sseMgr.Endpoint, bearer, logger, &mu, pending)
							}
							activeCh = sseMgr.MsgCh()
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

	firstMsg, style, err := rpc.ReadAnyMessage(in)
	if err != nil {
		logger.Printf("failed to read first message: %v", err)
		return 1
	}
	if err := handleOne(ctx, httpClient, sseMgr, bearer, logger, firstMsg, &mu, &outMu, pending, initTracker, forwardInit, oneShotTools, localToolsList, out, style, remote); err != nil {
		logger.Printf("handle error: %v", err)
	}

	for {
		msg, _, err := rpc.ReadAnyMessage(in)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0
			}
			logger.Printf("read error: %v", err)
			return 1
		}
		if err := handleOne(ctx, httpClient, sseMgr, bearer, logger, msg, &mu, &outMu, pending, initTracker, forwardInit, oneShotTools, localToolsList, out, style, remote); err != nil {
			logger.Printf("handle error: %v", err)
		}
	}
}
