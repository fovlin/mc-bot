package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func readJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("request body %q: %v", data, err)
	}
	return m
}

func TestTokenCacheAndHeaders(t *testing.T) {
	tokenCalls := 0
	var authHeader string
	var msgs []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			tokenCalls++
			_, _ = w.Write([]byte(`{"access_token":"t1","expires_in":"7200"}`))
		case "/v2/groups/G1/messages":
			authHeader = r.Header.Get("Authorization")
			msgs = append(msgs, readJSONBody(t, r))
			_, _ = w.Write([]byte(`{"id":"m1"}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL

	if err := api.sendGroupText(context.Background(), "G1", "msg-1", 1, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := api.sendGroupText(context.Background(), "G1", "msg-1", 2, "again"); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls)
	}
	if authHeader != "QQBot t1" {
		t.Fatalf("authorization = %q", authHeader)
	}
	if len(msgs) != 2 {
		t.Fatalf("message calls = %d, want 2", len(msgs))
	}
	first := msgs[0]
	if first["msg_type"].(float64) != 0 || first["content"] != "hello" || first["msg_id"] != "msg-1" || first["msg_seq"].(float64) != 1 {
		t.Fatalf("first message body = %v", first)
	}
	if msgs[1]["msg_seq"].(float64) != 2 {
		t.Fatalf("second message body = %v", msgs[1])
	}
}

func TestTokenExpiresInNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"t1","expires_in":7200}`))
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	token, err := api.accessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "t1" {
		t.Fatalf("token = %q", token)
	}
}

func TestTokenError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":10016,"message":"invalid appid or secret"}`))
	}))
	defer srv.Close()

	api := newQQAPI("app", "bad")
	api.baseURL = srv.URL
	_, err := api.accessToken(context.Background())
	var apiErr *apiError
	if err == nil || !errors.As(err, &apiErr) || apiErr.Code != 10016 {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUnauthorizedRefreshesToken(t *testing.T) {
	tokenCalls := 0
	messageCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			tokenCalls++
			if tokenCalls == 1 {
				_, _ = w.Write([]byte(`{"access_token":"t1","expires_in":7200}`))
			} else {
				_, _ = w.Write([]byte(`{"access_token":"t2","expires_in":7200}`))
			}
		case "/v2/groups/G1/messages":
			messageCalls++
			if r.Header.Get("Authorization") == "QQBot t1" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"err_code":401,"message":"unauthorized"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"m1"}`))
		}
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	if err := api.sendGroupText(context.Background(), "G1", "m", 1, "x"); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 2 || messageCalls != 2 {
		t.Fatalf("token calls = %d, message calls = %d", tokenCalls, messageCalls)
	}
}

func TestAPIErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":7200}`))
			return
		}
		_, _ = w.Write([]byte(`{"err_code":40034005,"message":"expired","trace_id":"trace-1"}`))
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	err := api.sendGroupText(context.Background(), "G1", "m", 1, "x")
	var apiErr *apiError
	if err == nil || !errors.As(err, &apiErr) {
		t.Fatalf("expected apiError, got %v", err)
	}
	if apiErr.Code != 40034005 || apiErr.TraceID != "trace-1" {
		t.Fatalf("apiError = %+v", apiErr)
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":7200}`))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	err := api.sendGroupText(context.Background(), "G1", "m", 1, "x")
	var apiErr *apiError
	if err == nil || !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUploadAndSendMedia(t *testing.T) {
	png := []byte("fake-png-bytes")
	var upload map[string]any
	var media map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":7200}`))
		case "/v2/groups/G1/files":
			upload = readJSONBody(t, r)
			_, _ = w.Write([]byte(`{"file_info":"FI","ttl":300}`))
		case "/v2/groups/G1/messages":
			media = readJSONBody(t, r)
			_, _ = w.Write([]byte(`{"id":"m2"}`))
		}
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL

	fileInfo, err := api.uploadGroupImage(context.Background(), "G1", png)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo != "FI" {
		t.Fatalf("file_info = %q", fileInfo)
	}
	if upload["file_type"].(float64) != 1 || upload["srv_send_msg"] != false || upload["file_name"] != "servers.png" {
		t.Fatalf("upload body = %v", upload)
	}
	encoded, _ := upload["file_data"].(string)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !bytes.Equal(decoded, png) {
		t.Fatalf("file_data decode = %q, %v", decoded, err)
	}

	if err := api.sendGroupMedia(context.Background(), "G1", "msg-1", 1, fileInfo); err != nil {
		t.Fatal(err)
	}
	if media["msg_type"].(float64) != 7 || media["msg_id"] != "msg-1" || media["msg_seq"].(float64) != 1 {
		t.Fatalf("media body = %v", media)
	}
	mediaInfo, ok := media["media"].(map[string]any)
	if !ok || mediaInfo["file_info"] != "FI" {
		t.Fatalf("media = %v", media["media"])
	}
}

func TestGatewayURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/getAppAccessToken":
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":7200}`))
		case "/gateway/bot":
			_, _ = w.Write([]byte(`{"url":"wss://example.com/ws","shards":1}`))
		}
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	got, err := api.gatewayURL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://example.com/ws" {
		t.Fatalf("gateway = %q", got)
	}
}

func TestGatewayURLFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = w.Write([]byte(`{"access_token":"t","expires_in":7200}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 10)))
	}))
	defer srv.Close()

	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	got, err := api.gatewayURL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultGatewayURL {
		t.Fatalf("gateway = %q, want fallback", got)
	}
}
