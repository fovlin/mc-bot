package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func serverWriteFrame(t *testing.T, w io.Writer, fin bool, op byte, payload []byte) error {
	t.Helper()
	header := make([]byte, 0, 14)
	b0 := op
	if fin {
		b0 |= 0x80
	}
	header = append(header, b0)
	switch length := len(payload); {
	case length < 126:
		header = append(header, byte(length))
	case length <= 65535:
		header = append(header, 126, byte(length>>8), byte(length))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(length))
		header = append(header, ext[:]...)
	}
	header = append(header, payload...)
	_, err := w.Write(header)
	return err
}

// readMaskedFrame reads a client-sent frame and verifies that it was masked.
func readMaskedFrame(r io.Reader) (op byte, payload []byte, masked bool, err error) {
	var head [2]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return 0, nil, false, err
	}
	op = head[0] & 0x0f
	masked = head[1]&0x80 != 0
	length := int(head[1] & 0x7f)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, false, err
		}
		length = int(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, false, err
		}
		length = int(binary.BigEndian.Uint64(ext[:]))
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, false, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return op, payload, masked, nil
}

func newPipeClient(t *testing.T) (*wsConn, net.Conn) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	client := &wsConn{conn: clientSide, br: bufio.NewReader(clientSide)}
	t.Cleanup(func() {
		clientSide.Close()
		serverSide.Close()
	})
	return client, serverSide
}

func TestWSAcceptKey(t *testing.T) {
	// Example from RFC 6455 section 1.3.
	if got, want := wsAcceptKey("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("accept key = %q, want %q", got, want)
	}
}

func TestWSClientWritesMaskedFrame(t *testing.T) {
	client, server := newPipeClient(t)

	done := make(chan error, 1)
	go func() {
		op, payload, masked, err := readMaskedFrame(server)
		if err != nil {
			done <- err
			return
		}
		if op != wsOpText || string(payload) != "hello" {
			done <- fmt.Errorf("op=%d payload=%q", op, payload)
			return
		}
		if !masked {
			done <- errors.New("client frame was not masked")
			return
		}
		done <- nil
	}()

	if err := client.writeFrame(wsOpText, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWSReadText(t *testing.T) {
	client, server := newPipeClient(t)
	go func() { _ = serverWriteFrame(t, server, true, wsOpText, []byte("hello")) }()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := client.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "hello" {
		t.Fatalf("message = %q", msg)
	}
}

func TestWSReadFragment(t *testing.T) {
	client, server := newPipeClient(t)
	go func() {
		_ = serverWriteFrame(t, server, false, wsOpText, []byte("hel"))
		_ = serverWriteFrame(t, server, true, wsOpContinuation, []byte("lo"))
	}()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := client.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "hello" {
		t.Fatalf("message = %q", msg)
	}
}

func TestWSReadPingPong(t *testing.T) {
	client, server := newPipeClient(t)

	done := make(chan error, 1)
	go func() {
		if err := serverWriteFrame(t, server, true, wsOpPing, []byte("p")); err != nil {
			done <- err
			return
		}
		op, payload, _, err := readMaskedFrame(server)
		if err != nil {
			done <- err
			return
		}
		if op != wsOpPong || string(payload) != "p" {
			done <- fmt.Errorf("expected pong, got op=%d payload=%q", op, payload)
			return
		}
		done <- serverWriteFrame(t, server, true, wsOpText, []byte("ok"))
	}()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := client.readMessage()
	if err != nil {
		t.Fatal(err)
	}
	if string(msg) != "ok" {
		t.Fatalf("message = %q", msg)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWSReadClose(t *testing.T) {
	client, server := newPipeClient(t)
	closePayload := []byte{0x03, 0xe8, 'b', 'y', 'e'}

	done := make(chan error, 1)
	go func() {
		if err := serverWriteFrame(t, server, true, wsOpClose, closePayload); err != nil {
			done <- err
			return
		}
		op, payload, _, err := readMaskedFrame(server)
		if err != nil {
			done <- err
			return
		}
		if op != wsOpClose || len(payload) < 2 || binary.BigEndian.Uint16(payload) != 1000 {
			done <- fmt.Errorf("expected close echo, got op=%d payload=%v", op, payload)
			return
		}
		done <- nil
	}()

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := client.readMessage()
	var closeErr *wsCloseError
	if !errors.As(err, &closeErr) || closeErr.Code != 1000 || closeErr.Reason != "bye" {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWSLargeFrames(t *testing.T) {
	for _, size := range []int{125, 126, 300, 65535, 70000} {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			client, server := newPipeClient(t)
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i)
			}
			go func() { _ = serverWriteFrame(t, server, true, wsOpBinary, payload) }()

			_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
			msg, err := client.readMessage()
			if err != nil {
				t.Fatal(err)
			}
			if len(msg) != size {
				t.Fatalf("message length = %d, want %d", len(msg), size)
			}
		})
	}
}

func TestWSDialHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		tp := textproto.NewReader(br)
		requestLine, err := tp.ReadLine()
		if err != nil {
			done <- err
			return
		}
		if !strings.HasPrefix(requestLine, "GET /ws ") {
			done <- fmt.Errorf("request line = %q", requestLine)
			return
		}
		headers, err := tp.ReadMIMEHeader()
		if err != nil {
			done <- err
			return
		}
		key := headers.Get("Sec-WebSocket-Key")
		if key == "" {
			done <- errors.New("missing Sec-WebSocket-Key")
			return
		}
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAcceptKey(key))
		// Read the close frame sent by client.Close.
		_, _, _, _ = readMaskedFrame(conn)
		done <- nil
	}()

	client, err := wsDial(t.Context(), "ws://"+ln.Addr().String()+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWSDialRejectsBadAccept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		tp := textproto.NewReader(br)
		_, _ = tp.ReadLine()
		_, _ = tp.ReadMIMEHeader()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nSec-WebSocket-Accept: bogus\r\n\r\n")
	}()

	if _, err := wsDial(t.Context(), "ws://"+ln.Addr().String()+"/"); err == nil {
		t.Fatal("expected accept key error")
	}
}

func TestWSDialRejectsNon101(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		tp := textproto.NewReader(br)
		_, _ = tp.ReadLine()
		_, _ = tp.ReadMIMEHeader()
		_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
	}()

	if _, err := wsDial(t.Context(), "ws://"+ln.Addr().String()+"/"); err == nil || !strings.Contains(err.Error(), "handshake failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}
