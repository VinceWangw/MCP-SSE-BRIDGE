package sse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-sse-bridge/rpc"
)

func ConnectLegacySSE(
	ctx context.Context,
	client *http.Client,
	remote string,
	bearer string,
	logger *log.Logger,
) (base string, endpoint string, msgCh <-chan rpc.JsonRPC, err error) {
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

	outCh := make(chan rpc.JsonRPC, 64)
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
				var m rpc.JsonRPC
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

type Manager struct {
	mu          sync.Mutex
	endpointVal atomic.Value
	msgChVal    atomic.Value
	switchCh    chan struct{}
	switchAck   chan struct{}
	cancel      context.CancelFunc
}

func NewManager(endpoint string) *Manager {
	m := &Manager{
		switchCh:  make(chan struct{}, 1),
		switchAck: make(chan struct{}, 1),
	}
	m.endpointVal.Store(endpoint)
	return m
}

func (m *Manager) Endpoint() string {
	v := m.endpointVal.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func (m *Manager) MsgCh() <-chan rpc.JsonRPC {
	v := m.msgChVal.Load()
	if v == nil {
		return nil
	}
	return v.(<-chan rpc.JsonRPC)
}

func (m *Manager) SwitchCh() <-chan struct{} {
	return m.switchCh
}

func (m *Manager) AckSwitch() {
	select {
	case m.switchAck <- struct{}{}:
	default:
	}
}

func (m *Manager) Set(endpoint string, msgCh <-chan rpc.JsonRPC, cancel context.CancelFunc) {
	m.endpointVal.Store(endpoint)
	m.msgChVal.Store(msgCh)
	m.cancel = cancel
	select {
	case m.switchCh <- struct{}{}:
	default:
	}
}

func (m *Manager) WaitForSwitch(timeout time.Duration) bool {
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

func (m *Manager) Reconnect(
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
	_, endpoint, msgCh, err := ConnectLegacySSE(ctx, client, remote, bearer, logger)
	if err != nil {
		cancel()
		return "", err
	}
	m.Set(endpoint, msgCh, cancel)
	return endpoint, nil
}
