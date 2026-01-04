package bridge

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"mcp-sse-bridge/rpc"
)

type upstreamInitTracker struct {
	started atomic.Bool
	ready   chan struct{}
	readyOK atomic.Bool
	lastReq atomic.Value
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
	req rpc.JsonRPC,
	mu *sync.Mutex,
	pending map[string]chan rpc.JsonRPC,
) {
	initReq := req
	initReq.ID = fmt.Sprintf("bridge-init-%d", time.Now().UnixNano())
	t.lastReq.Store(initReq)

	if t.started.Swap(true) {
		return
	}
	go func() {
		if !t.sendAndWait(ctx, client, endpointFn, bearer, logger, initReq, mu, pending) {
			logger.Printf("upstream initialize did not complete before timeout")
			return
		}
		t.closeReady(true)
	}()
}

func (t *upstreamInitTracker) reinitialize(
	ctx context.Context,
	client *http.Client,
	endpointFn func() string,
	bearer string,
	logger *log.Logger,
	mu *sync.Mutex,
	pending map[string]chan rpc.JsonRPC,
) {
	if !t.started.Load() {
		return
	}
	v := t.lastReq.Load()
	initReq, ok := v.(rpc.JsonRPC)
	if !ok {
		return
	}
	if !t.sendAndWait(ctx, client, endpointFn, bearer, logger, initReq, mu, pending) {
		logger.Printf("upstream re-initialize did not complete before timeout")
		return
	}
	t.closeReady(true)
}

func (t *upstreamInitTracker) wait(timeout time.Duration) bool {
	if !t.started.Load() {
		return true
	}
	select {
	case <-t.ready:
		return t.readyOK.Load()
	case <-time.After(timeout):
		return false
	}
}

func (t *upstreamInitTracker) closeReady(ok bool) {
	if ok {
		t.readyOK.Store(true)
	}
	select {
	case <-t.ready:
		return
	default:
		close(t.ready)
	}
}

func (t *upstreamInitTracker) initRequest() (rpc.JsonRPC, bool) {
	v := t.lastReq.Load()
	initReq, ok := v.(rpc.JsonRPC)
	if !ok {
		return rpc.JsonRPC{}, false
	}
	initReq.ID = fmt.Sprintf("bridge-init-%d", time.Now().UnixNano())
	return initReq, true
}

func (t *upstreamInitTracker) sendAndWait(
	ctx context.Context,
	client *http.Client,
	endpointFn func() string,
	bearer string,
	logger *log.Logger,
	req rpc.JsonRPC,
	mu *sync.Mutex,
	pending map[string]chan rpc.JsonRPC,
) bool {
	ep := endpointFn()
	idKey := idToKey(req.ID)
	ch := make(chan rpc.JsonRPC, 1)

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
