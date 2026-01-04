package rpc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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

func ReadAnyMessage(r *bufio.Reader) (JsonRPC, string, error) {
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
		return ReadAnyMessage(r)
	}
	var m JsonRPC
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return JsonRPC{}, "jsonl", err
	}
	return m, "jsonl", nil
}

func WriteMessage(w *bufio.Writer, msg JsonRPC, style string) error {
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

func WriteMessageLocked(mu *sync.Mutex, w *bufio.Writer, msg JsonRPC, style string) error {
	mu.Lock()
	defer mu.Unlock()
	return WriteMessage(w, msg, style)
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
