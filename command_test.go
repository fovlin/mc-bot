package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// httptestServer is a tiny wrapper so the fake QQ API can be described with a
// path -> response function.
type httptestServer struct {
	*httptest.Server
}

func newHTTPTestServer(t *testing.T, handle func(path string, body map[string]any) map[string]any) *httptestServer {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
		resp := handle(r.URL.Path, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return &httptestServer{Server: srv}
}

type fakeQQAPI struct {
	mu               sync.Mutex
	filesFail        bool
	duplicateFailure bool
	messageFailures  int
	uploadBodies     []map[string]any
	messageBodies    []map[string]any
	uploadPaths      []string
	messagePaths     []string
	unexpectedPaths  []string
}

func newFakeQQAPI(t *testing.T) (*fakeQQAPI, *httptestServer) {
	t.Helper()
	fake := &fakeQQAPI{}
	srv := newHTTPTestServer(t, func(path string, body map[string]any) map[string]any {
		switch {
		case path == "/app/getAppAccessToken":
			return map[string]any{"access_token": "t", "expires_in": "7200"}
		case strings.HasSuffix(path, "/files"):
			fake.mu.Lock()
			fake.uploadBodies = append(fake.uploadBodies, body)
			fake.uploadPaths = append(fake.uploadPaths, path)
			fail := fake.filesFail
			fake.mu.Unlock()
			if fail {
				return map[string]any{"err_code": 850031, "message": "too large"}
			}
			return map[string]any{"file_info": "FI", "ttl": 300}
		case strings.HasSuffix(path, "/messages"):
			fake.mu.Lock()
			fake.messageBodies = append(fake.messageBodies, body)
			fake.messagePaths = append(fake.messagePaths, path)
			duplicate := fake.duplicateFailure
			fail := !duplicate && fake.messageFailures > 0
			if fail {
				fake.messageFailures--
			}
			fake.mu.Unlock()
			if duplicate {
				return map[string]any{"err_code": 40054005, "message": "deduplicated"}
			}
			if fail {
				return map[string]any{"err_code": 40034005, "message": "expired"}
			}
			return map[string]any{"id": "mid"}
		default:
			fake.mu.Lock()
			fake.unexpectedPaths = append(fake.unexpectedPaths, path)
			fake.mu.Unlock()
			return map[string]any{"err_code": 404, "message": "not found"}
		}
	})
	t.Cleanup(srv.Close)
	return fake, srv
}

func (f *fakeQQAPI) messages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.messageBodies...)
}

func (f *fakeQQAPI) uploads() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.uploadBodies...)
}

func (f *fakeQQAPI) uploadTargets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.uploadPaths...)
}

func (f *fakeQQAPI) messageTargets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.messagePaths...)
}

func newTestBot(t *testing.T) (*bot, *fakeQQAPI) {
	t.Helper()
	useTempConfigDir(t)
	fake, srv := newFakeQQAPI(t)
	cfg := &config{AppID: "app", ClientSecret: "secret", Servers: map[string]string{}}
	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	return newBot(cfg, api, testRenderer(t)), fake
}

func TestAddAndDelServer(t *testing.T) {
	b, fake := newTestBot(t)
	ctx := context.Background()

	b.handleCommand(ctx, groupTarget("G1"), "M1", "/server add acovia mc.acovia.net")
	if got := b.cfg.Servers["acovia"]; got != "mc.acovia.net" {
		t.Fatalf("server address = %q", got)
	}
	msgs := fake.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"].(string), "Added server acovia") {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["msg_seq"].(float64) != 1 {
		t.Fatalf("msg_seq = %v", msgs[0]["msg_seq"])
	}

	loaded, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Servers["acovia"] != "mc.acovia.net" {
		t.Fatalf("saved config = %+v", loaded)
	}

	b.handleCommand(ctx, groupTarget("G1"), "M2", "/server add acovia other.example.com")
	msgs = fake.messages()
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), "already exists") {
		t.Fatalf("duplicate reply = %v", msgs[len(msgs)-1])
	}

	b.handleCommand(ctx, groupTarget("G1"), "M3", "/server del acovia")
	if _, ok := b.cfg.Servers["acovia"]; ok {
		t.Fatal("server not deleted")
	}
	if len(b.cfg.Servers) != 0 {
		t.Fatalf("servers = %v", b.cfg.Servers)
	}
	msgs = fake.messages()
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), "Deleted server acovia") {
		t.Fatalf("delete reply = %v", msgs[len(msgs)-1])
	}

	b.handleCommand(ctx, groupTarget("G1"), "M4", "/server del missing")
	msgs = fake.messages()
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), "not found") {
		t.Fatalf("missing reply = %v", msgs[len(msgs)-1])
	}
}

func TestServerCommandValidation(t *testing.T) {
	b, fake := newTestBot(t)
	ctx := context.Background()

	cases := []struct {
		command string
		want    string
	}{
		{"/server", "Usage"},
		{"/server add", "Usage: /server add"},
		{"/server add onlyname", "Usage: /server add"},
		{"/server add a/b host", "Failed to add: name"},
		{"/server add good host:abc", "Failed to add: port"},
		{"/ls a b", "Usage: /ls"},
	}
	for _, tc := range cases {
		b.handleCommand(ctx, groupTarget("G1"), "M-"+tc.command, tc.command)
		msgs := fake.messages()
		last := msgs[len(msgs)-1]
		if !strings.Contains(last["content"].(string), tc.want) {
			t.Errorf("%q: reply = %q, want %q", tc.command, last["content"], tc.want)
		}
	}

	before := len(fake.messages())
	b.handleCommand(ctx, groupTarget("G1"), "M-unknown", "/unknown")
	if len(fake.messages()) != before {
		t.Fatal("unknown command should be ignored")
	}
}

func TestListSendsImage(t *testing.T) {
	b, fake := newTestBot(t)
	addr := startStatusServer(t, `{"version":{"name":"Test 1.20","protocol":765},"players":{"online":2,"max":10,"sample":[{"name":"alice"},{"name":"bob"}]},"description":"Hello"}`)
	b.cfg.Servers = map[string]string{"acovia": addr}

	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")

	uploads := fake.uploads()
	if len(uploads) != 1 {
		t.Fatalf("uploads = %d", len(uploads))
	}
	if paths := fake.uploadTargets(); len(paths) != 1 || paths[0] != "/v2/groups/G1/files" {
		t.Fatalf("upload paths = %v", paths)
	}
	if paths := fake.messageTargets(); len(paths) != 1 || paths[0] != "/v2/groups/G1/messages" {
		t.Fatalf("message paths = %v", paths)
	}
	if uploads[0]["file_type"].(float64) != 1 || uploads[0]["srv_send_msg"] != false {
		t.Fatalf("upload body = %v", uploads[0])
	}
	encoded, ok := uploads[0]["file_data"].(string)
	if !ok || encoded == "" {
		t.Fatalf("file_data missing: %v", uploads[0])
	}
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		t.Fatalf("file_data is not base64: %v", err)
	}

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["msg_type"].(float64) != 7 || msgs[0]["msg_id"] != "M1" || msgs[0]["msg_seq"].(float64) != 1 {
		t.Fatalf("media message = %v", msgs[0])
	}
	media, _ := msgs[0]["media"].(map[string]any)
	if media["file_info"] != "FI" {
		t.Fatalf("media = %v", msgs[0]["media"])
	}
}

func TestListNamedAndUnknown(t *testing.T) {
	b, fake := newTestBot(t)
	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":0,"max":1},"description":"x"}`)
	b.cfg.Servers = map[string]string{"acovia": addr, "other": addr}

	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls acovia")
	if len(fake.messages()) != 1 {
		t.Fatalf("messages = %v", fake.messages())
	}

	b.handleCommand(context.Background(), groupTarget("G1"), "M2", "/ls missing")
	msgs := fake.messages()
	last := msgs[len(msgs)-1]
	if !strings.Contains(last["content"].(string), "Server missing not found") {
		t.Fatalf("reply = %q", last["content"])
	}
	if !strings.Contains(last["content"].(string), "acovia") || !strings.Contains(last["content"].(string), "other") {
		t.Fatalf("reply should list servers: %q", last["content"])
	}
}

func TestListNoServers(t *testing.T) {
	b, fake := newTestBot(t)
	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")
	msgs := fake.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"].(string), "No servers configured") {
		t.Fatalf("messages = %v", msgs)
	}
}

func TestListUploadFallback(t *testing.T) {
	b, fake := newTestBot(t)
	fake.mu.Lock()
	fake.filesFail = true
	fake.mu.Unlock()
	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":1,"max":2,"sample":[{"name":"a"}]},"description":"x"}`)
	b.cfg.Servers = map[string]string{"acovia": addr}

	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["msg_type"].(float64) != 0 || msgs[0]["msg_seq"].(float64) != 1 {
		t.Fatalf("fallback message = %v", msgs[0])
	}
	content := msgs[0]["content"].(string)
	if !strings.Contains(content, "image could not be sent") || !strings.Contains(content, "online 1/2") {
		t.Fatalf("fallback content = %q", content)
	}
}

func TestListSendFallbackUsesSeqTwo(t *testing.T) {
	b, fake := newTestBot(t)
	fake.mu.Lock()
	fake.messageFailures = 1 // the media message fails; the text fallback must use seq 2
	fake.mu.Unlock()
	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":1,"max":2,"sample":[{"name":"a"}]},"description":"x"}`)
	b.cfg.Servers = map[string]string{"acovia": addr}

	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")

	msgs := fake.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["msg_type"].(float64) != 7 || msgs[0]["msg_seq"].(float64) != 1 {
		t.Fatalf("first message = %v", msgs[0])
	}
	if msgs[1]["msg_type"].(float64) != 0 || msgs[1]["msg_seq"].(float64) != 2 {
		t.Fatalf("fallback message = %v", msgs[1])
	}
}

func TestHandleEventDedupesAndFilters(t *testing.T) {
	b, fake := newTestBot(t)
	ctx := context.Background()

	makeEvent := func(id, content string, botAuthor bool) event {
		d, _ := json.Marshal(map[string]any{
			"id":           id,
			"content":      content,
			"group_openid": "G1",
			"author":       map[string]any{"member_openid": "u", "bot": botAuthor},
		})
		return event{T: "GROUP_AT_MESSAGE_CREATE", D: d}
	}

	ev := makeEvent("M1", "/server add acovia mc.example.com", false)
	b.handleEvent(ctx, ev)
	b.handleEvent(ctx, ev) // duplicate delivery
	if got := len(fake.messages()); got != 1 {
		t.Fatalf("duplicate event produced %d replies", got)
	}

	b.handleEvent(ctx, makeEvent("M2", "/server add a b.example.com", true))
	if got := len(fake.messages()); got != 1 {
		t.Fatal("bot-authored message should be ignored")
	}

	ignored, _ := json.Marshal(map[string]any{"id": "M3", "content": "/ls", "group_openid": "G1"})
	b.handleEvent(ctx, event{T: "GUILD_MESSAGE_CREATE", D: ignored})
	if got := len(fake.messages()); got != 1 {
		t.Fatal("unhandled event should be ignored")
	}

	b.handleEvent(ctx, event{T: "GROUP_MESSAGE_CREATE", D: ev.D})
	// The wrapped event still has the same msg id, so it is deduped.
	if got := len(fake.messages()); got != 1 {
		t.Fatalf("messages = %d", got)
	}
}

func TestValidateServerName(t *testing.T) {
	valid := []string{"acovia", "服务器", "a-b_c.d", "x"}
	for _, name := range valid {
		if err := validateServerName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	invalid := []string{"", "a b", "a/b", "a\\b", strings.Repeat("x", 33)}
	for _, name := range invalid {
		if err := validateServerName(name); err == nil {
			t.Errorf("%q: expected error", name)
		}
	}
}

func TestValidateServerAddress(t *testing.T) {
	valid := []string{"mc.example.com", "mc.example.com:25566", "1.2.3.4", "1.2.3.4:25565", "[2001:db8::1]:25565"}
	for _, addr := range valid {
		if err := validateServerAddress(addr); err != nil {
			t.Errorf("%q: %v", addr, err)
		}
	}
	invalid := []string{"", "a b", "host:abc", "host:0", "host:70000", ":25565", "host:"}
	for _, addr := range invalid {
		if err := validateServerAddress(addr); err == nil {
			t.Errorf("%q: expected error", addr)
		}
	}
}

func TestFormatStatusText(t *testing.T) {
	text := formatStatusText([]serverStatus{
		{Name: "a", Address: "a.example.com", Online: true, OnlinePlayers: 1, MaxPlayers: 2, Version: "v", Latency: 5},
		{Name: "b", Address: "b.example.com", Err: "timeout"},
	})
	if !strings.Contains(text, "online 1/2") || !strings.Contains(text, "offline, timeout") {
		t.Fatalf("text = %q", text)
	}
}

func TestNormalizeCommandText(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"/ls", "/ls"},
		{"@Arcovia /ls", "/ls"},
		{"@Arcovia /server add a b", "/server add a b"},
		{"@Arcovia", "@Arcovia"},
		{"/server add a b", "/server add a b"},
	}
	for _, tt := range tests {
		if got := normalizeCommandText(tt.in); got != tt.want {
			t.Errorf("normalizeCommandText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHandleFullModePlainCommand(t *testing.T) {
	b, fake := newTestBot(t)
	d, _ := json.Marshal(map[string]any{
		"id":           "M1",
		"content":      "/server add fullmc mc.example.com",
		"group_openid": "G1",
		"timestamp":    "2026-11-22T18:00:00+08:00",
		"author":       map[string]any{"member_openid": "U1"},
	})
	b.handleEvent(context.Background(), event{T: "GROUP_MESSAGE_CREATE", D: d})
	if got := len(fake.messages()); got != 1 {
		t.Fatalf("full-mode command produced %d replies, want 1", got)
	}
}

func TestListDuplicateMediaSkipsFallback(t *testing.T) {
	b, fake := newTestBot(t)
	fake.mu.Lock()
	fake.duplicateFailure = true
	fake.mu.Unlock()

	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":1,"max":2,"sample":[{"name":"a"}]},"description":"x"}`)
	b.cfg.Servers = map[string]string{"acovia": addr}
	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want only the failed image attempt", msgs)
	}
	if msgs[0]["msg_type"].(float64) != 7 || msgs[0]["msg_seq"].(float64) != 1 {
		t.Fatalf("message = %v, want image attempt", msgs[0])
	}
}

func TestPrivateChatList(t *testing.T) {
	b, fake := newTestBot(t)
	addr := startStatusServer(t, `{"version":{"name":"Paper 26.3","protocol":777},"players":{"online":0,"max":16},"description":"Welcome"}`)
	b.cfg.Servers = map[string]string{"acovia": addr}

	d, _ := json.Marshal(map[string]any{
		"id":      "M1",
		"content": "/ls",
		"author":  map[string]any{"user_openid": "U1"},
	})
	b.handleEvent(context.Background(), event{T: "C2C_MESSAGE_CREATE", D: d})

	uploads := fake.uploads()
	if len(uploads) != 1 {
		t.Fatalf("uploads = %d", len(uploads))
	}
	if paths := fake.uploadTargets(); len(paths) != 1 || paths[0] != "/v2/users/U1/files" {
		t.Fatalf("upload paths = %v", paths)
	}
	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if paths := fake.messageTargets(); len(paths) != 1 || paths[0] != "/v2/users/U1/messages" {
		t.Fatalf("message paths = %v", paths)
	}
	if msgs[0]["msg_type"].(float64) != 7 || msgs[0]["msg_seq"].(float64) != 1 {
		t.Fatalf("message = %v", msgs[0])
	}
}

func TestPrivateChatRejectsServerManagement(t *testing.T) {
	b, fake := newTestBot(t)
	d, _ := json.Marshal(map[string]any{
		"id":      "M1",
		"content": "/server add acovia mc.example.com",
		"author":  map[string]any{"user_openid": "U1"},
	})
	b.handleEvent(context.Background(), event{T: "C2C_MESSAGE_CREATE", D: d})

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if !strings.Contains(msgs[0]["content"].(string), "only available in group chats") {
		t.Fatalf("reply = %v", msgs[0]["content"])
	}
	if paths := fake.messageTargets(); len(paths) != 1 || paths[0] != "/v2/users/U1/messages" {
		t.Fatalf("message paths = %v", paths)
	}
	if len(b.cfg.Servers) != 0 {
		t.Fatalf("private /server changed config: %v", b.cfg.Servers)
	}
}
