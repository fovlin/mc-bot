package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxWSMessage = 1 << 20

// wsClient is a thin gorilla/websocket adapter. It keeps the outbound writer
// serialized and restricts the gateway to text messages.
type wsClient struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func dialWS(ctx context.Context, rawURL string) (*wsClient, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	conn, resp, err := dialer.DialContext(ctx, rawURL, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("connect WebSocket: %w", err)
	}
	conn.SetReadLimit(maxWSMessage)
	return &wsClient{conn: conn}, nil
}

func (c *wsClient) ReadText() ([]byte, error) {
	messageType, payload, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	if messageType != websocket.TextMessage {
		return nil, fmt.Errorf("unexpected WebSocket message type %d", messageType)
	}
	return payload, nil
}

func (c *wsClient) WriteText(payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

func (c *wsClient) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *wsClient) Close() error {
	c.mu.Lock()
	_ = c.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(time.Second),
	)
	c.mu.Unlock()
	return c.conn.Close()
}
