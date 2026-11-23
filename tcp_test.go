package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func readPacket(r io.Reader) ([]byte, error) {
	length, err := readVarInt(r)
	if err != nil {
		return nil, err
	}
	if length < 0 || length > maxStatusPacket {
		return nil, errors.New("bad packet length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func statusResponse(statusJSON string) []byte {
	body := appendVarInt(nil, 0)
	body = appendVarInt(body, len(statusJSON))
	body = append(body, statusJSON...)
	out := appendVarInt(nil, len(body))
	return append(out, body...)
}

func startStatusServer(t *testing.T, statusJSON string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if _, err := readPacket(conn); err != nil {
					return
				}
				if _, err := readPacket(conn); err != nil {
					return
				}
				_, _ = conn.Write(statusResponse(statusJSON))
			}(conn)
		}
	}()
	return ln.Addr().String()
}

func TestPingJava(t *testing.T) {
	status := `{
		"version": {"name": "Test 1.20.4", "protocol": 765},
		"players": {"online": 2, "max": 10, "sample": [{"name": "alice"}, {"name": " bob "}, {"name": ""}]},
		"description": {"text": "Hello ", "extra": [{"text": "World"}]}
	}`
	addr := startStatusServer(t, status)

	st := pingJava(context.Background(), addr, 2*time.Second)
	if !st.Online {
		t.Fatalf("expected online, got error %q", st.Err)
	}
	if st.Version != "Test 1.20.4" {
		t.Fatalf("version = %q", st.Version)
	}
	if st.OnlinePlayers != 2 || st.MaxPlayers != 10 {
		t.Fatalf("players = %d/%d", st.OnlinePlayers, st.MaxPlayers)
	}
	if len(st.Sample) != 2 || st.Sample[0] != "alice" || st.Sample[1] != "bob" {
		t.Fatalf("sample = %v", st.Sample)
	}
	if st.MOTD != "Hello World" {
		t.Fatalf("motd = %q", st.MOTD)
	}
	if st.Latency <= 0 {
		t.Fatalf("latency = %v", st.Latency)
	}
	if st.Resolved != addr {
		t.Fatalf("resolved = %q, want %q", st.Resolved, addr)
	}
}

func TestPingJavaTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-release
	}()

	start := time.Now()
	st := pingJava(context.Background(), ln.Addr().String(), 200*time.Millisecond)
	if st.Online {
		t.Fatal("expected offline")
	}
	if st.Err == "" {
		t.Fatal("expected an error message")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestPingJavaOversize(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = readPacket(conn)
		_, _ = readPacket(conn)
		_, _ = conn.Write(appendVarInt(nil, maxStatusPacket+1))
	}()

	st := pingJava(context.Background(), ln.Addr().String(), 2*time.Second)
	if st.Online || !strings.Contains(st.Err, "invalid status packet length") {
		t.Fatalf("expected length error, got online=%v err=%q", st.Online, st.Err)
	}
}

func TestPingJavaTruncated(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = readPacket(conn)
		_, _ = readPacket(conn)
		_, _ = conn.Write([]byte{0x40, 0x01})
	}()

	st := pingJava(context.Background(), ln.Addr().String(), 2*time.Second)
	if st.Online || st.Err == "" {
		t.Fatalf("expected offline with error, got online=%v err=%q", st.Online, st.Err)
	}
}

func TestPingJavaBadJSON(t *testing.T) {
	addr := startStatusServer(t, `{not json`)
	st := pingJava(context.Background(), addr, 2*time.Second)
	if st.Online || !strings.Contains(st.Err, "parse status JSON") {
		t.Fatalf("expected json error, got online=%v err=%q", st.Online, st.Err)
	}
}

func TestResolveAddress(t *testing.T) {
	old := lookupSRV
	lookupSRV = func(service, proto, name string) (string, []*net.SRV, error) {
		if name == "mc.example.com" {
			return "", []*net.SRV{{Target: "srv.example.com.", Port: 25566}}, nil
		}
		return "", nil, &net.DNSError{Err: "not found", Name: name, IsNotFound: true}
	}
	t.Cleanup(func() { lookupSRV = old })

	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		{"srv hit", "mc.example.com", "srv.example.com:25566", false},
		{"srv hit trailing dot", "mc.example.com.", "srv.example.com:25566", false},
		{"srv miss", "plain.example.com", "plain.example.com:25565", false},
		{"ipv4", "1.2.3.4", "1.2.3.4:25565", false},
		{"host port", "1.2.3.4:1234", "1.2.3.4:1234", false},
		{"ipv6", "2001:db8::1", "[2001:db8::1]:25565", false},
		{"ipv6 port", "[2001:db8::1]:25565", "[2001:db8::1]:25565", false},
		{"bad port", "host:abc", "", true},
		{"zero port", "host:0", "", true},
		{"empty", "   ", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAddress(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFlattenDescription(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"hello"`, "hello"},
		{"extra", `{"text":"Hello ","extra":[{"text":"World"}]}`, "Hello World"},
		{"formatting", `{"text":"§aGreen §rText"}`, "Green Text"},
		{"translate", `{"translate":"multiplayer.status.old"}`, "multiplayer.status.old"},
		{"with", `{"translate":"a %s","with":["b"]}`, "a %sb"},
		{"newlines", `{"text":"line1\nline2"}`, "line1\nline2"},
		{"array", `[{"text":"a"},{"text":"b"}]`, "ab"},
		{"null", `null`, ""},
		{"empty", ``, ""},
		{"bad", `{`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flattenDescription(json.RawMessage(tt.raw)); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVarIntRoundTrip(t *testing.T) {
	for _, value := range []int{0, 1, 127, 128, 255, 300, 16383, 1 << 20} {
		encoded := appendVarInt(nil, value)
		got, err := readVarInt(strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatalf("value %d: %v", value, err)
		}
		if got != value {
			t.Fatalf("value %d: got %d", value, got)
		}
	}
	if got := len(appendVarInt(nil, -1)); got != 5 {
		t.Fatalf("-1 encoded to %d bytes, want 5", got)
	}
	if _, err := readVarInt(strings.NewReader("\xff\xff\xff\xff\xff\x01")); err == nil {
		t.Fatal("expected error for overlong VarInt")
	}
}

func TestPingAllOrder(t *testing.T) {
	status := `{"version":{"name":"v","protocol":1},"players":{"online":0,"max":1},"description":"x"}`
	addr := startStatusServer(t, status)
	servers := map[string]string{"b": addr, "a": addr}
	got := pingAll(context.Background(), []string{"b", "a"}, servers)
	if len(got) != 2 || got[0].Name != "b" || got[1].Name != "a" {
		t.Fatalf("order not preserved: %+v", got)
	}
	for _, st := range got {
		if !st.Online {
			t.Fatalf("server %s offline: %s", st.Name, st.Err)
		}
	}
}
