package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wsTestURL(serverURL string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http")
}

// newWSPair starts a one-connection WebSocket server and returns the adapter
// client plus the server end of the connection.
func newWSPair(t *testing.T) (*wsClient, *websocket.Conn) {
	t.Helper()

	upgrader := websocket.Upgrader{}
	serverConn := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
	}))
	t.Cleanup(srv.Close)

	client, err := dialWS(context.Background(), wsTestURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case server := <-serverConn:
		t.Cleanup(func() { _ = server.Close() })
		return client, server
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server connection")
		return nil, nil
	}
}

func TestWSClientTextRoundTrip(t *testing.T) {
	client, server := newWSPair(t)

	writeErr := make(chan error, 1)
	go func() { writeErr <- client.WriteText([]byte("hello")) }()

	messageType, payload, err := server.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage || string(payload) != "hello" {
		t.Fatalf("server received type=%d payload=%q", messageType, payload)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}

	if err := server.WriteMessage(websocket.TextMessage, []byte("world")); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := client.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Fatalf("client received %q", got)
	}
}

func TestWSClientCloseCode(t *testing.T) {
	client, server := newWSPair(t)

	_ = server.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(4009, "expired"),
		time.Now().Add(time.Second),
	)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := client.ReadText()

	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != 4009 {
		t.Fatalf("unexpected error: %v", err)
	}
}
