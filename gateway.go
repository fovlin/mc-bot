package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

const intentsGroupAndC2C = 1 << 25

type event struct {
	ID string          `json:"id"`
	Op int             `json:"op"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
	D  json.RawMessage `json:"d"`
}

type gateway struct {
	api    *qqAPI
	handle func(context.Context, event)

	mu        sync.Mutex
	sessionID string
	seq       int64

	sem chan struct{}
}

func newGateway(api *qqAPI, handle func(context.Context, event)) *gateway {
	return &gateway{
		api:    api,
		handle: handle,
		sem:    make(chan struct{}, 8),
	}
}

// run keeps one gateway session alive, resuming when possible and otherwise
// identifying again. Fatal authentication errors are returned.
func (g *gateway) run(ctx context.Context) error {
	backoff := time.Second
	for {
		ready, err := g.connectOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if ready {
			backoff = time.Second
		}
		if err != nil {
			log.Printf("[gateway] connection lost: %v", err)
			var closeErr *wsCloseError
			if errors.As(err, &closeErr) && closeErr.Code == 4004 {
				// Invalid token: force a refresh before the next identify.
				g.api.invalidateToken()
			}
			if isFatalCredentialError(err) {
				return fmt.Errorf("credentials rejected: %w", err)
			}
			fatal, reset := classifyGatewayError(err)
			if fatal {
				return fmt.Errorf("gateway rejected the connection: %w", err)
			}
			if reset {
				g.resetSession()
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// classifyGatewayError decides whether a close code allows a resume, requires a
// fresh identify, or means the bot cannot connect at all.
func classifyGatewayError(err error) (fatal, reset bool) {
	var closeErr *wsCloseError
	if !errors.As(err, &closeErr) {
		return false, false
	}
	switch closeErr.Code {
	case 4013, 4014, 4914, 4915:
		return true, false
	case 4008, 4009:
		return false, false
	case 4006, 4007:
		return false, true
	default:
		return false, true
	}
}

// isFatalCredentialError reports whether the API rejected the app
// credentials. Retrying those errors forever would only hide the problem.
func isFatalCredentialError(err error) bool {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case 10004, 10007, 100016:
		return true
	}
	return false
}

// connectOnce runs a single gateway session until it fails or ctx ends.
func (g *gateway) connectOnce(ctx context.Context) (bool, error) {
	token, err := g.api.accessToken(ctx)
	if err != nil {
		return false, fmt.Errorf("get access token: %w", err)
	}
	gatewayURL, err := g.api.gatewayURL(ctx)
	if err != nil {
		return false, err
	}
	log.Printf("[gateway] connecting to %s", gatewayURL)
	conn, err := wsDial(ctx, gatewayURL)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	// Hello
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	raw, err := conn.readMessage()
	if err != nil {
		return false, fmt.Errorf("wait for Hello: %w", err)
	}
	var hello event
	if err := json.Unmarshal(raw, &hello); err != nil {
		return false, fmt.Errorf("parse Hello: %w", err)
	}
	if hello.Op != 10 {
		return false, fmt.Errorf("expected Hello (op10), got op%d", hello.Op)
	}
	var helloData struct {
		HeartbeatInterval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &helloData); err != nil {
		return false, fmt.Errorf("parse Hello payload: %w", err)
	}
	interval := time.Duration(helloData.HeartbeatInterval) * time.Millisecond
	if interval <= 0 {
		interval = 40 * time.Second
	}

	if err := g.sendAuth(ctx, conn, token); err != nil {
		return false, err
	}

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	ack := make(chan struct{}, 1)
	go g.heartbeat(hbCtx, conn, interval, ack)

	ready := false
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2*interval + 10*time.Second))
		raw, err := conn.readMessage()
		if err != nil {
			return ready, err
		}
		var ev event
		if err := json.Unmarshal(raw, &ev); err != nil {
			log.Printf("[gateway] cannot parse event: %v", err)
			continue
		}
		switch ev.Op {
		case 0:
			if ev.S > 0 {
				g.setSeq(ev.S)
			}
			switch ev.T {
			case "READY":
				var data struct {
					SessionID string `json:"session_id"`
					User      struct {
						ID       string `json:"id"`
						Username string `json:"username"`
					} `json:"user"`
				}
				_ = json.Unmarshal(ev.D, &data)
				g.setSessionID(data.SessionID)
				ready = true
				log.Printf("[gateway] ready, bot=%s id=%s session=%s", data.User.Username, data.User.ID, data.SessionID)
			case "RESUMED":
				ready = true
				log.Printf("[gateway] session resumed")
			default:
				g.dispatch(ctx, ev)
			}
		case 11:
			select {
			case ack <- struct{}{}:
			default:
			}
		case 7:
			return ready, errors.New("server requested reconnect")
		case 9:
			g.resetSession()
			return ready, errors.New("session invalidated; re-authenticating")
		case 10:
			// A second Hello should not happen; ignore it.
		}
	}
}

func (g *gateway) sendAuth(_ context.Context, conn *wsConn, token string) error {
	g.mu.Lock()
	sessionID, seq := g.sessionID, g.seq
	g.mu.Unlock()

	var op int
	var data any
	if sessionID != "" && seq > 0 {
		op = 6
		data = map[string]any{
			"token":      "QQBot " + token,
			"session_id": sessionID,
			"seq":        seq,
		}
	} else {
		op = 2
		data = map[string]any{
			"token":   "QQBot " + token,
			"intents": intentsGroupAndC2C,
			"shard":   []int{0, 1},
			"properties": map[string]string{
				"$os":      "linux",
				"$browser": "mcbot-go",
				"$device":  "mcbot-go",
			},
		}
	}

	payload, err := json.Marshal(map[string]any{"op": op, "d": data})
	if err != nil {
		return fmt.Errorf("marshal auth payload: %w", err)
	}
	if err := conn.writeFrame(wsOpText, payload); err != nil {
		return fmt.Errorf("write auth payload: %w", err)
	}
	return nil
}

func (g *gateway) heartbeat(ctx context.Context, conn *wsConn, interval time.Duration, ack <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastAck := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			select {
			case <-ack:
				lastAck = time.Now()
			default:
			}
			if time.Since(lastAck) > 2*interval {
				log.Printf("[gateway] heartbeat timed out; reconnecting")
				conn.Close()
				return
			}
			if err := conn.writeFrame(wsOpText, g.heartbeatPayload()); err != nil {
				conn.Close()
				return
			}
		}
	}
}

func (g *gateway) heartbeatPayload() []byte {
	g.mu.Lock()
	seq := g.seq
	g.mu.Unlock()
	if seq == 0 {
		return []byte(`{"op":1,"d":null}`)
	}
	payload, _ := json.Marshal(map[string]any{"op": 1, "d": seq})
	return payload
}

func (g *gateway) dispatch(ctx context.Context, ev event) {
	if g.handle == nil {
		return
	}
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}

	evCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	go func() {
		defer cancel()
		defer func() { <-g.sem }()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[gateway] panic while handling event: %v", r)
			}
		}()
		g.handle(evCtx, ev)
	}()
}

func (g *gateway) setSessionID(id string) {
	g.mu.Lock()
	g.sessionID = id
	g.mu.Unlock()
}

func (g *gateway) setSeq(seq int64) {
	g.mu.Lock()
	g.seq = seq
	g.mu.Unlock()
}

func (g *gateway) resetSession() {
	g.mu.Lock()
	g.sessionID = ""
	g.seq = 0
	g.mu.Unlock()
}
