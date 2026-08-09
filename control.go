package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type controlServer struct {
	Port   int
	Token  string
	server *http.Server
}

func startControlServer(state func() supervisorState, stop func()) (*controlServer, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	mux := http.NewServeMux()
	authorized := func(request *http.Request) bool {
		supplied := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if len(supplied) != len(token) {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) == 1
	}
	mux.HandleFunc("/status", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(state())
	})
	mux.HandleFunc("/stop", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusAccepted)
		go stop()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	return &controlServer{Port: listener.Addr().(*net.TCPAddr).Port, Token: token, server: server}, nil
}

func (c *controlServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.server.Shutdown(ctx)
}

func queryControl(state supervisorState) bool {
	return controlRequest(state, http.MethodGet, "/status", time.Second)
}

func requestStop(state supervisorState) bool {
	return controlRequest(state, http.MethodPost, "/stop", 2*time.Second)
}

func controlRequest(state supervisorState, method, path string, timeout time.Duration) bool {
	if state.ControlPort == 0 || state.ControlToken == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", state.ControlPort, path), nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode >= 200 && response.StatusCode <= 299
}
