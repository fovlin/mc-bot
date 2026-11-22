package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultMCPort = "25565"
	// maxStatusPacket bounds the status JSON so a broken server cannot make
	// the bot allocate an arbitrary amount of memory.
	maxStatusPacket = 2 << 20
	pingWorkers     = 8
	pingTimeout     = 5 * time.Second
	pingTotalLimit  = 20 * time.Second
)

type javaStatus struct {
	Version struct {
		Name     string `json:"name"`
		Protocol int    `json:"protocol"`
	} `json:"version"`
	Players struct {
		Online int `json:"online"`
		Max    int `json:"max"`
		Sample []struct {
			Name string `json:"name"`
		} `json:"sample"`
	} `json:"players"`
	Description json.RawMessage `json:"description"`
}

// serverStatus is the data needed to render one server card. It is also used
// when the server is offline: Err then explains why.
type serverStatus struct {
	Name          string
	Address       string
	Resolved      string
	Online        bool
	Err           string
	Version       string
	Protocol      int
	OnlinePlayers int
	MaxPlayers    int
	Sample        []string
	MOTD          string
	Latency       time.Duration
}

// lookupSRV is a variable so tests can avoid real DNS lookups.
var lookupSRV = net.LookupSRV

// resolveAddress turns a user supplied address into a dialable host:port.
// A bare host uses the SRV record _minecraft._tcp.<host> when available and
// falls back to port 25565. Addresses that already contain a port are used
// as-is.
func resolveAddress(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", errors.New("address is empty")
	}

	if host, port, err := net.SplitHostPort(addr); err == nil {
		if host == "" {
			return "", fmt.Errorf("address %q is missing a host", addr)
		}
		if !validPort(port) {
			return "", fmt.Errorf("invalid port: %q", port)
		}
		return net.JoinHostPort(host, port), nil
	}

	host := strings.TrimSuffix(addr, ".")
	if host == "" {
		return "", errors.New("address is empty")
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return net.JoinHostPort(ip.String(), defaultMCPort), nil
	}
	if strings.Contains(host, ":") {
		return "", fmt.Errorf("invalid address format: %q", addr)
	}

	_, records, err := lookupSRV("minecraft", "tcp", host)
	if err == nil && len(records) > 0 {
		target := strings.TrimSuffix(records[0].Target, ".")
		if target != "" && validPort(strconv.Itoa(int(records[0].Port))) {
			return net.JoinHostPort(target, strconv.Itoa(int(records[0].Port))), nil
		}
	}
	return net.JoinHostPort(host, defaultMCPort), nil
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n <= 65535
}

// pingJava performs a modern (1.7+) Minecraft status ping. It always returns a
// serverStatus: on failure Online is false and Err holds the reason, so one
// broken server never prevents the rest of the list from being rendered.
func pingJava(ctx context.Context, addr string, timeout time.Duration) *serverStatus {
	st := &serverStatus{Address: addr}

	resolved, err := resolveAddress(addr)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	st.Resolved = resolved

	host, portStr, err := net.SplitHostPort(resolved)
	if err != nil {
		st.Err = fmt.Sprintf("resolve address: %v", err)
		return st
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		st.Err = fmt.Sprintf("resolve port: %v", err)
		return st
	}

	start := time.Now()
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", resolved)
	if err != nil {
		st.Err = fmt.Sprintf("connection failed: %v", err)
		return st
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		st.Err = fmt.Sprintf("set deadline: %v", err)
		return st
	}

	payload := make([]byte, 0, 32+len(host))
	payload = appendVarInt(payload, 0)  // packet id: handshake
	payload = appendVarInt(payload, -1) // protocol version: -1 (status only)
	payload = appendVarInt(payload, len(host))
	payload = append(payload, host...)
	payload = append(payload, byte(port>>8), byte(port))
	payload = appendVarInt(payload, 1) // next state: status

	packet := appendVarInt(nil, len(payload))
	packet = append(packet, payload...)
	packet = append(packet, 0x01, 0x00) // status request

	if _, err := conn.Write(packet); err != nil {
		st.Err = fmt.Sprintf("write handshake: %v", err)
		return st
	}

	length, err := readVarInt(conn)
	if err != nil {
		st.Err = fmt.Sprintf("read status length: %v", err)
		return st
	}
	if length <= 0 || length > maxStatusPacket {
		st.Err = fmt.Sprintf("invalid status packet length: %d", length)
		return st
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		st.Err = fmt.Sprintf("read status packet: %v", err)
		return st
	}

	r := bytes.NewReader(body)
	packetID, err := readVarInt(r)
	if err != nil {
		st.Err = fmt.Sprintf("read status packet id: %v", err)
		return st
	}
	if packetID != 0 {
		st.Err = fmt.Sprintf("invalid status packet id: %d", packetID)
		return st
	}

	strLen, err := readVarInt(r)
	if err != nil || strLen < 0 || strLen > r.Len() {
		st.Err = "invalid status JSON length"
		return st
	}
	jsonData := make([]byte, strLen)
	if _, err := io.ReadFull(r, jsonData); err != nil {
		st.Err = fmt.Sprintf("read status JSON: %v", err)
		return st
	}

	var status javaStatus
	if err := json.Unmarshal(jsonData, &status); err != nil {
		st.Err = fmt.Sprintf("parse status JSON: %v", err)
		return st
	}

	st.Online = true
	st.Latency = time.Since(start)
	st.Protocol = status.Version.Protocol
	st.Version = strings.TrimSpace(status.Version.Name)
	if st.Version == "" {
		st.Version = "unknown"
	}
	st.OnlinePlayers = max(0, status.Players.Online)
	st.MaxPlayers = max(0, status.Players.Max)
	st.Sample = make([]string, 0, len(status.Players.Sample))
	for _, player := range status.Players.Sample {
		name := strings.TrimSpace(player.Name)
		if name != "" {
			st.Sample = append(st.Sample, name)
		}
	}
	st.MOTD = flattenDescription(status.Description)
	return st
}

// pingAll pings the named servers concurrently, preserving the order of names.
func pingAll(ctx context.Context, names []string, servers map[string]string) []serverStatus {
	if len(names) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, pingTotalLimit)
	defer cancel()

	results := make([]serverStatus, len(names))
	sem := make(chan struct{}, pingWorkers)
	var wg sync.WaitGroup

	for i, name := range names {
		addr := servers[name]
		wg.Add(1)
		go func(i int, name, addr string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = serverStatus{Name: name, Address: addr, Err: ctx.Err().Error()}
				return
			}
			results[i] = *pingJava(ctx, addr, pingTimeout)
			results[i].Name = name
		}(i, name, addr)
	}
	wg.Wait()
	return results
}

// appendVarInt appends a Minecraft protocol VarInt.
func appendVarInt(dst []byte, value int) []byte {
	v := uint32(value)
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if v == 0 {
			return dst
		}
	}
}

// readVarInt reads a Minecraft protocol VarInt (at most 5 bytes).
func readVarInt(r io.Reader) (int, error) {
	var value uint32
	for i := 0; i < 5; i++ {
		var buf [1]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, err
		}
		value |= uint32(buf[0]&0x7f) << (7 * i)
		if buf[0]&0x80 == 0 {
			return int(value), nil
		}
	}
	return 0, errors.New("VarInt too long")
}

// flattenDescription turns a Minecraft chat component into plain text. Server
// descriptions are either a string or an object with text/translate/extra.
func flattenDescription(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	var b strings.Builder
	writeDescription(&b, value)
	return strings.TrimSpace(stripFormatting(b.String()))
}

func writeDescription(b *strings.Builder, value any) {
	switch v := value.(type) {
	case string:
		b.WriteString(v)
	case []any:
		for _, item := range v {
			writeDescription(b, item)
		}
	case map[string]any:
		text, hasText := v["text"].(string)
		if hasText {
			b.WriteString(text)
		}
		if translate, ok := v["translate"].(string); ok && !hasText {
			b.WriteString(translate)
		}
		if with, ok := v["with"].([]any); ok {
			for _, item := range with {
				writeDescription(b, item)
			}
		}
		if extra, ok := v["extra"].([]any); ok {
			for _, item := range extra {
				writeDescription(b, item)
			}
		}
	}
}

// stripFormatting removes legacy § formatting codes and normalizes line
// endings/tabs.
func stripFormatting(s string) string {
	var b strings.Builder
	skip := false
	for _, r := range s {
		if r == '§' {
			skip = true
			continue
		}
		if skip {
			skip = false
			continue
		}
		switch r {
		case '\r':
			continue
		case '\t':
			r = ' '
		}
		b.WriteRune(r)
	}
	return b.String()
}
