package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type proxyObserver func(rpcMessage, string)

type proxyResponse struct {
	result json.RawMessage
	err    error
}

type proxyRequest struct {
	connectionID string
	response     chan proxyResponse
}

type proxyBridge struct {
	id            string
	downstream    *websocket.Conn
	upstream      *websocket.Conn
	clientName    string
	clientTitle   string
	clientVersion string
	downMu        sync.Mutex
	upMu          sync.Mutex
	closed        atomic.Bool
}

type tuiProxy struct {
	upstreamURL string
	listener    net.Listener
	server      *http.Server
	mu          sync.Mutex
	bridges     map[string]*proxyBridge
	primary     string
	pending     map[string]proxyRequest
	serverObs   []proxyObserver
	clientObs   []proxyObserver
	nextID      atomic.Uint64
	closed      bool
}

func newTUIProxy(upstreamURL string) *tuiProxy {
	return &tuiProxy{upstreamURL: upstreamURL, bridges: map[string]*proxyBridge{}, pending: map[string]proxyRequest{}}
}

func (p *tuiProxy) Start() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	p.listener = listener
	p.server = &http.Server{Handler: http.HandlerFunc(p.accept)}
	go func() { _ = p.server.Serve(listener) }()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func (p *tuiProxy) OnServerMessage(observer proxyObserver) {
	p.mu.Lock()
	p.serverObs = append(p.serverObs, observer)
	p.mu.Unlock()
}

func (p *tuiProxy) OnClientMessage(observer proxyObserver) {
	p.mu.Lock()
	p.clientObs = append(p.clientObs, observer)
	p.mu.Unlock()
}

func (p *tuiProxy) Request(ctx context.Context, method string, params map[string]any) (any, error) {
	bridge := p.primaryBridge()
	if bridge == nil {
		return nil, errors.New("the primary Codex TUI connection is unavailable")
	}
	id := fmt.Sprintf("codexdog-proxy-%d", p.nextID.Add(1))
	idJSON, _ := json.Marshal(id)
	key := string(idJSON)
	response := make(chan proxyResponse, 1)
	p.mu.Lock()
	p.pending[key] = proxyRequest{connectionID: bridge.id, response: response}
	p.mu.Unlock()
	data, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err == nil {
		err = bridge.writeUpstream(ctx, websocket.MessageText, data)
	}
	if err != nil {
		p.removePending(key)
		return nil, err
	}
	select {
	case result := <-response:
		if result.err != nil {
			return nil, result.err
		}
		return decodeResult(result.result)
	case <-ctx.Done():
		p.removePending(key)
		return nil, fmt.Errorf("TUI JSON-RPC request timed out: %s: %w", method, ctx.Err())
	}
}

func (p *tuiProxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	server := p.server
	bridges := make([]*proxyBridge, 0, len(p.bridges))
	for _, bridge := range p.bridges {
		bridges = append(bridges, bridge)
	}
	p.mu.Unlock()
	p.rejectPending(errors.New("TUI proxy closed"), "")
	for _, bridge := range bridges {
		bridge.close()
	}
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	}
	return nil
}

func (p *tuiProxy) accept(writer http.ResponseWriter, request *http.Request) {
	downstream, err := websocket.Accept(writer, request, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	downstream.SetReadLimit(64 * 1024 * 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	upstream, _, err := websocket.Dial(ctx, p.upstreamURL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	cancel()
	if err != nil {
		_ = downstream.Close(websocket.StatusBadGateway, "upstream unavailable")
		return
	}
	upstream.SetReadLimit(64 * 1024 * 1024)
	bridge := &proxyBridge{id: randomID(), downstream: downstream, upstream: upstream}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		bridge.close()
		return
	}
	p.bridges[bridge.id] = bridge
	// The first connection is the TUI spawned by the supervisor. It remains
	// primary for the proxy lifetime, so a dashboard or queue client cannot
	// become the recovery/control path by sending a later message.
	if p.primary == "" {
		p.primary = bridge.id
	}
	p.mu.Unlock()

	done := make(chan struct{}, 2)
	go func() { p.forwardDownstream(bridge); done <- struct{}{} }()
	go func() { p.forwardUpstream(bridge); done <- struct{}{} }()
	<-done
	bridge.close()
	p.removeBridge(bridge.id)
}

func (p *tuiProxy) forwardDownstream(bridge *proxyBridge) {
	for {
		typeName, data, err := bridge.downstream.Read(context.Background())
		if err != nil {
			return
		}
		message, parsed := parseRPC(data)
		if parsed && message.Method == "initialize" {
			p.recordClientInfo(bridge.id, message.Params)
		}
		p.mu.Lock()
		observers := append([]proxyObserver(nil), p.clientObs...)
		p.mu.Unlock()
		if parsed {
			for _, observer := range observers {
				observer(message, bridge.id)
			}
		}
		if err := bridge.writeUpstream(context.Background(), typeName, data); err != nil {
			return
		}
	}
}

func (p *tuiProxy) forwardUpstream(bridge *proxyBridge) {
	for {
		typeName, data, err := bridge.upstream.Read(context.Background())
		if err != nil {
			return
		}
		message, parsed := parseRPC(data)
		if parsed && len(message.ID) > 0 {
			key := rpcIDKey(message.ID)
			p.mu.Lock()
			pending, exists := p.pending[key]
			if exists {
				delete(p.pending, key)
			}
			p.mu.Unlock()
			if exists {
				if len(message.Error) > 0 && string(message.Error) != "null" {
					pending.response <- proxyResponse{err: fmt.Errorf("JSON-RPC error: %s", string(message.Error))}
				} else {
					pending.response <- proxyResponse{result: message.Result}
				}
				continue
			}
		}
		p.mu.Lock()
		observers := append([]proxyObserver(nil), p.serverObs...)
		p.mu.Unlock()
		p.observe(data, bridge.id, observers)
		if err := bridge.writeDownstream(context.Background(), typeName, data); err != nil {
			return
		}
	}
}

func (p *tuiProxy) observe(data []byte, connectionID string, observers []proxyObserver) {
	message, ok := parseRPC(data)
	if !ok {
		return
	}
	for _, observer := range observers {
		observer(message, connectionID)
	}
}

func (p *tuiProxy) primaryBridge() *proxyBridge {
	p.mu.Lock()
	defer p.mu.Unlock()
	if bridge := p.bridges[p.primary]; bridge != nil && !bridge.closed.Load() {
		return bridge
	}
	return nil
}

func (p *tuiProxy) IsPrimary(connectionID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return connectionID != "" && p.primary == connectionID && p.bridges[connectionID] != nil
}

func (p *tuiProxy) PrimaryClientInfo() (string, string) {
	info := p.PrimaryClientIdentity()
	return info.Name, info.Version
}

func (p *tuiProxy) PrimaryClientIdentity() appServerClientInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if bridge := p.bridges[p.primary]; bridge != nil {
		return appServerClientInfo{Name: bridge.clientName, Title: bridge.clientTitle, Version: bridge.clientVersion}
	}
	return appServerClientInfo{}
}

func (p *tuiProxy) recordClientInfo(connectionID string, params map[string]any) {
	info, ok := asObject(params["clientInfo"])
	if !ok {
		return
	}
	name, _ := readString(info["name"])
	title, _ := readString(info["title"])
	clientVersion, _ := readString(info["version"])
	p.mu.Lock()
	if bridge := p.bridges[connectionID]; bridge != nil {
		bridge.clientName = name
		bridge.clientTitle = title
		bridge.clientVersion = clientVersion
	}
	p.mu.Unlock()
}

func (p *tuiProxy) removeBridge(connectionID string) {
	p.mu.Lock()
	delete(p.bridges, connectionID)
	if p.primary == connectionID {
		p.primary = ""
	}
	p.mu.Unlock()
	p.rejectPending(errors.New("Codex TUI connection closed"), connectionID)
}

func (p *tuiProxy) removePending(key string) {
	p.mu.Lock()
	delete(p.pending, key)
	p.mu.Unlock()
}

func (p *tuiProxy) rejectPending(err error, connectionID string) {
	p.mu.Lock()
	pending := []proxyRequest{}
	for id, request := range p.pending {
		if connectionID == "" || request.connectionID == connectionID {
			pending = append(pending, request)
			delete(p.pending, id)
		}
	}
	p.mu.Unlock()
	for _, request := range pending {
		request.response <- proxyResponse{err: err}
	}
}

func (b *proxyBridge) writeUpstream(ctx context.Context, typeName websocket.MessageType, data []byte) error {
	b.upMu.Lock()
	defer b.upMu.Unlock()
	return b.upstream.Write(ctx, typeName, data)
}

func (b *proxyBridge) writeDownstream(ctx context.Context, typeName websocket.MessageType, data []byte) error {
	b.downMu.Lock()
	defer b.downMu.Unlock()
	return b.downstream.Write(ctx, typeName, data)
}

func (b *proxyBridge) close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}
	_ = b.downstream.CloseNow()
	_ = b.upstream.CloseNow()
}

func randomID() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}
