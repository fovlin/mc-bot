package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xa

	maxWSMessage = 1 << 20
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// wsCloseError reports a WebSocket close frame received from the server.
type wsCloseError struct {
	Code   int
	Reason string
}

func (e *wsCloseError) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("WebSocket closed: %d", e.Code)
	}
	return fmt.Sprintf("WebSocket closed: %d %s", e.Code, e.Reason)
}

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// wsDial opens a WebSocket connection (ws or wss) and performs the opening
// handshake. No extensions are negotiated.
func wsDial(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse WebSocket URL %q: %w", rawURL, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported WebSocket scheme: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("WebSocket URL is missing a host: %q", rawURL)
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}

	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("connect WebSocket: %w", err)
	}
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}
	if u.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("WebSocket TLS handshake: %w", err)
		}
		conn = tlsConn
	}

	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		conn.Close()
		return nil, fmt.Errorf("generate WebSocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])

	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	request := fmt.Sprintf(
		"GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: mcbot-go\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, key,
	)
	if _, err := io.WriteString(conn, request); err != nil {
		conn.Close()
		return nil, fmt.Errorf("write WebSocket handshake: %w", err)
	}

	br := bufio.NewReader(conn)
	tp := textproto.NewReader(br)
	statusLine, err := tp.ReadLine()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read WebSocket handshake response: %w", err)
	}
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 || parts[1] != "101" {
		conn.Close()
		return nil, fmt.Errorf("WebSocket handshake failed: %s", statusLine)
	}
	headers, err := tp.ReadMIMEHeader()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read WebSocket response headers: %w", err)
	}
	want := wsAcceptKey(key)
	got := headers.Get("Sec-WebSocket-Accept")
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		conn.Close()
		return nil, errors.New("WebSocket handshake failed: Sec-WebSocket-Accept mismatch")
	}

	_ = conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: br}, nil
}

func wsAcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *wsConn) Close() error {
	_ = c.writeFrame(wsOpClose, []byte{0x03, 0xe8}) // 1000 normal closure
	return c.conn.Close()
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	var header [14]byte
	header[0] = 0x80 | op
	n := 2
	switch length := len(payload); {
	case length < 126:
		header[1] = 0x80 | byte(length)
	case length <= 65535:
		header[1] = 0x80 | 126
		binary.BigEndian.PutUint16(header[2:], uint16(length))
		n = 4
	default:
		header[1] = 0x80 | 127
		binary.BigEndian.PutUint64(header[2:], uint64(length))
		n = 10
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("generate WebSocket mask: %w", err)
	}
	copy(header[n:], mask[:])
	n += 4

	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i&3]
	}

	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(header[:n]); err != nil {
		return err
	}
	if len(masked) > 0 {
		if _, err := c.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("WebSocket frame uses an unnegotiated extension")
	}
	fin = head[0]&0x80 != 0
	op = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := int(head[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		size := binary.BigEndian.Uint64(ext[:])
		if size > maxWSMessage {
			return false, 0, nil, fmt.Errorf("WebSocket frame too large: %d", size)
		}
		length = int(size)
	}
	if length > maxWSMessage {
		return false, 0, nil, fmt.Errorf("WebSocket frame too large: %d", length)
	}
	if op&0x8 != 0 && (!fin || length > 125) {
		return false, 0, nil, errors.New("invalid WebSocket control frame")
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, op, payload, nil
}

// readMessage returns one complete text or binary message, answering ping
// frames and handling close frames along the way.
func (c *wsConn) readMessage() ([]byte, error) {
	var message []byte
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case wsOpPing:
			if err := c.writeFrame(wsOpPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			code := 1005
			reason := ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload[:2]))
				reason = string(payload[2:])
			}
			_ = c.writeFrame(wsOpClose, payload)
			return nil, &wsCloseError{Code: code, Reason: reason}
		case wsOpText, wsOpBinary:
			message = append(message, payload...)
		case wsOpContinuation:
			if message == nil {
				return nil, errors.New("unexpected WebSocket continuation frame")
			}
			message = append(message, payload...)
		default:
			return nil, fmt.Errorf("unknown WebSocket opcode: %d", op)
		}
		if len(message) > maxWSMessage {
			return nil, errors.New("WebSocket message too large")
		}
		if fin && (op == wsOpText || op == wsOpBinary || op == wsOpContinuation) {
			return message, nil
		}
	}
}
