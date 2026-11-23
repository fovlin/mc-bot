package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	uploadFailures   int
	duplicateFailure bool
	messageFailures  int
	uploadBodies     []map[string]any
	messageBodies    []map[string]any
	uploadPaths      []string
	messagePaths     []string
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
			fail := fake.uploadFailures > 0
			if fail {
				fake.uploadFailures--
			}
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
	t.Chdir(t.TempDir())
	fake, srv := newFakeQQAPI(t)
	api := newQQAPI("app", "secret")
	api.baseURL = srv.URL
	data, err := dataDir()
	if err != nil {
		t.Fatal(err)
	}
	return newBot(api, testRenderer(t), newSessionStore(data)), fake
}

func setSessionServers(t *testing.T, b *bot, target chatTarget, servers map[string]string) {
	t.Helper()
	sess, err := b.sessions.get(target)
	if err != nil {
		t.Fatal(err)
	}
	for name, addr := range servers {
		if err := sess.add(name, addr); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
}

func sessionServers(t *testing.T, b *bot, target chatTarget) map[string]string {
	t.Helper()
	sess, err := b.sessions.get(target)
	if err != nil {
		t.Fatal(err)
	}
	return sess.snapshot()
}

func TestAddAndDelServer(t *testing.T) {
	b, fake := newTestBot(t)
	ctx := context.Background()

	b.handleCommand(ctx, groupTarget("G1"), "M1", "/server add acovia mc.acovia.net")
	if got := sessionServers(t, b, groupTarget("G1"))["acovia"]; got != "mc.acovia.net" {
		t.Fatalf("server address = %q", got)
	}
	msgs := fake.messages()
	if len(msgs) != 1 || !strings.Contains(msgs[0]["content"].(string), "Added server acovia") {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["msg_seq"].(float64) != 1 {
		t.Fatalf("msg_seq = %v", msgs[0]["msg_seq"])
	}

	sessionFile := filepath.Join("data", "G1", "server.json")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatalf("read session config: %v", err)
	}
	if !strings.Contains(string(data), "mc.acovia.net") {
		t.Fatalf("session config = %s", data)
	}

	b.handleCommand(ctx, groupTarget("G1"), "M2", "/server add acovia other.example.com")
	msgs = fake.messages()
	if !strings.Contains(msgs[len(msgs)-1]["content"].(string), "already exists") {
		t.Fatalf("duplicate reply = %v", msgs[len(msgs)-1])
	}

	b.handleCommand(ctx, groupTarget("G1"), "M3", "/server del acovia")
	if servers := sessionServers(t, b, groupTarget("G1")); len(servers) != 0 {
		t.Fatalf("servers = %v", servers)
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
	setSessionServers(t, b, groupTarget("G1"), map[string]string{"acovia": addr})

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

func TestListSendsOneImagePerServer(t *testing.T) {
	b, fake := newTestBot(t)
	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":1,"max":2,"sample":[{"name":"a"}]},"description":"x"}`)
	setSessionServers(t, b, groupTarget("G1"), map[string]string{"alpha": addr, "beta": addr, "gamma": addr})

	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")

	msgs := fake.messages()
	uploads := fake.uploads()
	if len(msgs) != 3 || len(uploads) != 3 {
		t.Fatalf("messages = %d, uploads = %d, want 3 each", len(msgs), len(uploads))
	}
	seen := make(map[string]bool, len(uploads))
	for i, msg := range msgs {
		if msg["msg_type"].(float64) != 7 || msg["msg_id"] != "M1" || msg["msg_seq"].(float64) != float64(i+1) {
			t.Fatalf("message %d = %v", i, msg)
		}
	}
	for _, upload := range uploads {
		data, _ := upload["file_data"].(string)
		if data == "" || seen[data] {
			t.Fatalf("servers did not produce distinct images")
		}
		seen[data] = true
	}
}

func TestListPartialImageFallback(t *testing.T) {
	b, fake := newTestBot(t)
	fake.mu.Lock()
	fake.uploadFailures = 1 // alpha's image fails; beta's is still sent
	fake.mu.Unlock()
	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":1,"max":2,"sample":[{"name":"a"}]},"description":"x"}`)
	setSessionServers(t, b, groupTarget("G1"), map[string]string{"alpha": addr, "beta": addr})

	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")

	msgs := fake.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["msg_type"].(float64) != 7 || msgs[0]["msg_seq"].(float64) != 2 {
		t.Fatalf("beta image = %v", msgs[0])
	}
	if msgs[1]["msg_type"].(float64) != 0 || msgs[1]["msg_seq"].(float64) != 3 {
		t.Fatalf("alpha fallback = %v", msgs[1])
	}
	content := msgs[1]["content"].(string)
	if !strings.Contains(content, "image could not be sent") || !strings.Contains(content, "alpha") || strings.Contains(content, "beta") {
		t.Fatalf("fallback content = %q", content)
	}
}

func TestListNamedAndUnknown(t *testing.T) {
	b, fake := newTestBot(t)
	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":0,"max":1},"description":"x"}`)
	setSessionServers(t, b, groupTarget("G1"), map[string]string{"acovia": addr, "other": addr})

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

func TestListUploadFallbackUsesNextSeq(t *testing.T) {
	b, fake := newTestBot(t)
	fake.mu.Lock()
	fake.uploadFailures = 1
	fake.mu.Unlock()
	addr := startStatusServer(t, `{"version":{"name":"v","protocol":1},"players":{"online":1,"max":2,"sample":[{"name":"a"}]},"description":"x"}`)
	setSessionServers(t, b, groupTarget("G1"), map[string]string{"acovia": addr})

	b.handleCommand(context.Background(), groupTarget("G1"), "M1", "/ls")

	msgs := fake.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if msgs[0]["msg_type"].(float64) != 0 || msgs[0]["msg_seq"].(float64) != 2 {
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
	setSessionServers(t, b, groupTarget("G1"), map[string]string{"acovia": addr})

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
	setSessionServers(t, b, groupTarget("G1"), map[string]string{"acovia": addr})
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
	setSessionServers(t, b, userTarget("U1"), map[string]string{"acovia": addr})

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

func TestSessionsAreIsolated(t *testing.T) {
	b, fake := newTestBot(t)
	ctx := context.Background()

	b.handleCommand(ctx, groupTarget("G1"), "M1", "/server add group-server group.example.com")
	b.handleCommand(ctx, userTarget("U1"), "M2", "/server add private-server private.example.com")

	groupServers := sessionServers(t, b, groupTarget("G1"))
	userServers := sessionServers(t, b, userTarget("U1"))
	if groupServers["group-server"] != "group.example.com" {
		t.Fatalf("group servers = %v", groupServers)
	}
	if _, ok := groupServers["private-server"]; ok {
		t.Fatalf("private server leaked into group: %v", groupServers)
	}
	if userServers["private-server"] != "private.example.com" {
		t.Fatalf("user servers = %v", userServers)
	}
	if _, ok := userServers["group-server"]; ok {
		t.Fatalf("group server leaked into private: %v", userServers)
	}

	for _, path := range []string{
		filepath.Join("data", "G1", "server.json"),
		filepath.Join("data", "U1", "server.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if len(fake.messages()) != 2 {
		t.Fatalf("messages = %d", len(fake.messages()))
	}
}
