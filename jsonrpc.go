package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type rpcResponse struct {
	result json.RawMessage
	err    error
}

type notificationHandler func(rpcMessage)

type jsonRPCClient struct {
	url            string
	requestTimeout time.Duration
	conn           *websocket.Conn
	writeMu        sync.Mutex
	mu             sync.Mutex
	pending        map[string]chan rpcResponse
	handlers       map[uint64]notificationHandler
	nextID         atomic.Uint64
	nextHandlerID  atomic.Uint64
	closed         bool
}

func newJSONRPCClient(url string, requestTimeout time.Duration) *jsonRPCClient {
	return &jsonRPCClient{url: url, requestTimeout: requestTimeout, pending: map[string]chan rpcResponse{}, handlers: map[uint64]notificationHandler{}}
}

func (c *jsonRPCClient) Connect(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(connectCtx, c.url, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return fmt.Errorf("connect to %s: %w", c.url, err)
	}
	conn.SetReadLimit(64 * 1024 * 1024)
	c.mu.Lock()
	c.conn = conn
	c.closed = false
	c.mu.Unlock()
	go c.readLoop()
	return nil
}

func (c *jsonRPCClient) Initialize(ctx context.Context) error {
	_, err := c.Request(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "codexdog", "title": "Codexdog", "version": version},
		"capabilities": map[string]any{"experimentalApi": false},
	})
	if err != nil {
		return err
	}
	return c.Notify(ctx, "initialized", map[string]any{})
}

func (c *jsonRPCClient) Request(ctx context.Context, method string, params map[string]any) (any, error) {
	requestCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && c.requestTimeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, c.requestTimeout)
	}
	defer cancel()
	id := fmt.Sprintf("codexdog-%d", c.nextID.Add(1))
	keyBytes, _ := json.Marshal(id)
	key := string(keyBytes)
	response := make(chan rpcResponse, 1)
	c.mu.Lock()
	if c.closed || c.conn == nil {
		c.mu.Unlock()
		return nil, errors.New("JSON-RPC client is not connected")
	}
	c.pending[key] = response
	c.mu.Unlock()

	if err := c.writeJSON(requestCtx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.removePending(key)
		return nil, err
	}
	select {
	case result := <-response:
		if result.err != nil {
			return nil, result.err
		}
		return decodeResult(result.result)
	case <-requestCtx.Done():
		c.removePending(key)
		return nil, fmt.Errorf("JSON-RPC request timed out: %s: %w", method, requestCtx.Err())
	}
}

func (c *jsonRPCClient) Notify(ctx context.Context, method string, params map[string]any) error {
	return c.writeJSON(ctx, map[string]any{"method": method, "params": params})
}

func (c *jsonRPCClient) AddNotificationHandler(handler notificationHandler) func() {
	id := c.nextHandlerID.Add(1)
	c.mu.Lock()
	c.handlers[id] = handler
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.handlers, id)
		c.mu.Unlock()
	}
}

func (c *jsonRPCClient) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "codexdog closed")
	}
	c.rejectPending(errors.New("JSON-RPC client closed"))
}

func (c *jsonRPCClient) writeJSON(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	closed := c.closed
	c.mu.Unlock()
	if conn == nil || closed {
		return errors.New("JSON-RPC client is not connected")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.Write(ctx, websocket.MessageText, data)
}

func (c *jsonRPCClient) readLoop() {
	for {
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn == nil {
			return
		}
		_, data, err := conn.Read(context.Background())
		if err != nil {
			c.rejectPending(fmt.Errorf("Codex app-server connection closed: %w", err))
			return
		}
		message, ok := parseRPC(data)
		if !ok {
			continue
		}
		key := rpcIDKey(message.ID)
		if key != "" {
			c.mu.Lock()
			pending := c.pending[key]
			delete(c.pending, key)
			c.mu.Unlock()
			if pending != nil {
				if len(message.Error) > 0 && string(message.Error) != "null" {
					pending <- rpcResponse{err: fmt.Errorf("JSON-RPC error: %s", string(message.Error))}
				} else {
					pending <- rpcResponse{result: message.Result}
				}
				continue
			}
		}
		if message.Method != "" {
			c.mu.Lock()
			handlers := make([]notificationHandler, 0, len(c.handlers))
			for _, handler := range c.handlers {
				handlers = append(handlers, handler)
			}
			c.mu.Unlock()
			for _, handler := range handlers {
				handler(message)
			}
		}
	}
}

func (c *jsonRPCClient) removePending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *jsonRPCClient) rejectPending(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = map[string]chan rpcResponse{}
	c.mu.Unlock()
	for _, channel := range pending {
		channel <- rpcResponse{err: err}
	}
}
