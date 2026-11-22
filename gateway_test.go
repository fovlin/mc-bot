package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestClassifyGatewayError(t *testing.T) {
	tests := []struct {
		code  int
		fatal bool
		reset bool
	}{
		{4006, false, true},
		{4007, false, true},
		{4008, false, false},
		{4009, false, false},
		{4013, true, false},
		{4014, true, false},
		{4914, true, false},
		{4915, true, false},
		{4999, false, true},
	}
	for _, tt := range tests {
		fatal, reset := classifyGatewayError(&wsCloseError{Code: tt.code})
		if fatal != tt.fatal || reset != tt.reset {
			t.Errorf("code %d: fatal=%v reset=%v, want %v/%v", tt.code, fatal, reset, tt.fatal, tt.reset)
		}
	}
	if fatal, reset := classifyGatewayError(errors.New("network")); fatal || reset {
		t.Errorf("plain error: fatal=%v reset=%v", fatal, reset)
	}
}

func TestFatalCredentialError(t *testing.T) {
	for _, code := range []int{10004, 10007, 100016} {
		if !isFatalCredentialError(&apiError{Code: code}) {
			t.Errorf("code %d should be fatal", code)
		}
	}
	if isFatalCredentialError(&apiError{Code: 100001}) {
		t.Error("rate limit should not be fatal")
	}
	if isFatalCredentialError(errors.New("network")) {
		t.Error("plain error should not be fatal")
	}
	wrapped := fmt.Errorf("get token: %w", &apiError{Code: 100016})
	if !isFatalCredentialError(wrapped) {
		t.Error("wrapped credential error should be fatal")
	}
}

func TestHeartbeatPayload(t *testing.T) {
	g := newGateway(nil, nil)
	if got := string(g.heartbeatPayload()); got != `{"op":1,"d":null}` {
		t.Fatalf("empty seq payload = %s", got)
	}
	g.setSeq(42)
	var pkt struct {
		Op int   `json:"op"`
		D  int64 `json:"d"`
	}
	if err := json.Unmarshal(g.heartbeatPayload(), &pkt); err != nil {
		t.Fatal(err)
	}
	if pkt.Op != 1 || pkt.D != 42 {
		t.Fatalf("payload = %+v", pkt)
	}
}

func TestSendAuthIdentify(t *testing.T) {
	client, server := newPipeClient(t)
	g := newGateway(nil, nil)

	frameCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		_, payload, _, err := readMaskedFrame(server)
		if err != nil {
			errCh <- err
			return
		}
		frameCh <- payload
	}()

	if err := g.sendAuth(t.Context(), client, "tok"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		t.Fatal(err)
	case payload := <-frameCh:
		var pkt struct {
			Op int `json:"op"`
			D  struct {
				Token   string `json:"token"`
				Intents int64  `json:"intents"`
				Shard   []int  `json:"shard"`
			} `json:"d"`
		}
		if err := json.Unmarshal(payload, &pkt); err != nil {
			t.Fatal(err)
		}
		if pkt.Op != 2 {
			t.Fatalf("op = %d", pkt.Op)
		}
		if pkt.D.Token != "QQBot tok" {
			t.Fatalf("token = %q", pkt.D.Token)
		}
		if pkt.D.Intents != intentsGroupAndC2C {
			t.Fatalf("intents = %d", pkt.D.Intents)
		}
		if len(pkt.D.Shard) != 2 || pkt.D.Shard[0] != 0 || pkt.D.Shard[1] != 1 {
			t.Fatalf("shard = %v", pkt.D.Shard)
		}
	}
}

func TestSendAuthResume(t *testing.T) {
	client, server := newPipeClient(t)
	g := newGateway(nil, nil)
	g.setSessionID("session-1")
	g.setSeq(7)

	frameCh := make(chan []byte, 1)
	go func() {
		_, payload, _, err := readMaskedFrame(server)
		if err != nil {
			return
		}
		frameCh <- payload
	}()

	if err := g.sendAuth(t.Context(), client, "tok"); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-frameCh:
		var pkt struct {
			Op int `json:"op"`
			D  struct {
				Token     string `json:"token"`
				SessionID string `json:"session_id"`
				Seq       int64  `json:"seq"`
			} `json:"d"`
		}
		if err := json.Unmarshal(payload, &pkt); err != nil {
			t.Fatal(err)
		}
		if pkt.Op != 6 || pkt.D.SessionID != "session-1" || pkt.D.Seq != 7 || pkt.D.Token != "QQBot tok" {
			t.Fatalf("resume payload = %+v", pkt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auth frame")
	}
}

func TestSendAuthIdentifyWhenSeqZero(t *testing.T) {
	client, server := newPipeClient(t)
	g := newGateway(nil, nil)
	g.setSessionID("session-1") // no seq yet: identify, not resume

	frameCh := make(chan byte, 1)
	go func() {
		_, payload, _, err := readMaskedFrame(server)
		if err != nil {
			return
		}
		var pkt struct {
			Op int `json:"op"`
		}
		_ = json.Unmarshal(payload, &pkt)
		frameCh <- byte(pkt.Op)
	}()

	if err := g.sendAuth(t.Context(), client, "tok"); err != nil {
		t.Fatal(err)
	}
	select {
	case op := <-frameCh:
		if op != 2 {
			t.Fatalf("op = %d, want identify (2)", op)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auth frame")
	}
}

func TestResetSession(t *testing.T) {
	g := newGateway(nil, nil)
	g.setSessionID("s")
	g.setSeq(9)
	g.resetSession()
	if g.sessionID != "" || g.seq != 0 {
		t.Fatalf("session=%q seq=%d", g.sessionID, g.seq)
	}
}

func TestDispatch(t *testing.T) {
	got := make(chan string, 1)
	g := newGateway(nil, func(_ context.Context, ev event) {
		got <- ev.T
	})
	g.dispatch(context.Background(), event{T: "GROUP_AT_MESSAGE_CREATE"})
	select {
	case name := <-got:
		if name != "GROUP_AT_MESSAGE_CREATE" {
			t.Fatalf("event = %q", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called")
	}
}

func TestDispatchNilHandler(t *testing.T) {
	g := newGateway(nil, nil)
	g.dispatch(context.Background(), event{T: "X"})
}

func TestDispatchRecoversPanic(t *testing.T) {
	g := newGateway(nil, func(context.Context, event) { panic("boom") })
	g.dispatch(context.Background(), event{T: "X"})
	// The package must keep running; just make sure nothing escapes.
	time.Sleep(50 * time.Millisecond)
	if len(g.sem) != 0 {
		t.Fatalf("semaphore not released: %d", len(g.sem))
	}
}
