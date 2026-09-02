package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestInitializeWithClientInfoPreservesCodexIdentity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	request := make(chan rpcMessage, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, httpRequest *http.Request) {
		connection, acceptErr := websocket.Accept(writer, httpRequest, nil)
		if acceptErr != nil {
			return
		}
		defer connection.CloseNow()
		_, data, readErr := connection.Read(context.Background())
		if readErr != nil {
			return
		}
		message, ok := parseRPC(data)
		if !ok {
			return
		}
		request <- message
		response, _ := json.Marshal(map[string]any{"id": message.ID, "result": map[string]any{}})
		_ = connection.Write(context.Background(), websocket.MessageText, response)
		_, _, _ = connection.Read(context.Background())
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	client := newJSONRPCClient("ws://"+listener.Addr().String(), time.Second)
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	identity := appServerClientInfo{Name: "codex-tui", Title: "Codex CLI", Version: "0.150.1"}
	if err := client.InitializeWithClientInfo(context.Background(), identity, false); err != nil {
		t.Fatal(err)
	}

	select {
	case message := <-request:
		info, ok := asObject(message.Params["clientInfo"])
		if !ok {
			t.Fatalf("clientInfo = %#v", message.Params["clientInfo"])
		}
		if name, _ := readString(info["name"]); name != identity.Name {
			t.Fatalf("client name = %q", name)
		}
		if title, _ := readString(info["title"]); title != identity.Title {
			t.Fatalf("client title = %q", title)
		}
		if clientVersion, _ := readString(info["version"]); clientVersion != identity.Version {
			t.Fatalf("client version = %q", clientVersion)
		}
	case <-time.After(time.Second):
		t.Fatal("initialize request was not observed")
	}
}
