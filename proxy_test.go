package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestTUIProxyForwardsAndInjects(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			message, ok := parseRPC(data)
			if !ok {
				continue
			}
			// Preserve the original JSON-RPC id type.
			response := []byte(fmt.Sprintf(`{"id":%s,"result":{"ok":true}}`, message.ID))
			if err := conn.Write(context.Background(), websocket.MessageText, response); err != nil {
				return
			}
			notification, _ := json.Marshal(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
			if err := conn.Write(context.Background(), websocket.MessageText, notification); err != nil {
				return
			}
		}
	})}
	go server.Serve(listener)
	defer server.Close()

	proxy := newTUIProxy(fmt.Sprintf("ws://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port))
	port, err := proxy.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	observed := make(chan rpcMessage, 4)
	proxy.OnServerMessage(func(message rpcMessage, _ string) { observed <- message })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d", port), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	request, _ := json.Marshal(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{}})
	if err := client.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	seenTurn := false
	for !seenTurn {
		select {
		case message := <-observed:
			seenTurn = message.Method == "turn/started"
		case <-ctx.Done():
			t.Fatal("proxy did not observe the upstream notification")
		}
	}
	result, err := proxy.Request(ctx, "turn/start", map[string]any{"threadId": "thread-1"})
	if err != nil {
		t.Fatal(err)
	}
	object, _ := asObject(result)
	if object["ok"] != true {
		t.Fatalf("unexpected injected response: %#v", result)
	}
}
