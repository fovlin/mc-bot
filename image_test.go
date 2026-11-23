package main

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
)

func testRenderer(t *testing.T) *renderer {
	t.Helper()
	f, err := opentype.Parse(gomono.TTF)
	if err != nil {
		t.Fatal(err)
	}
	r, err := newRendererFromFont(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func onlineStatus(name string, players int) serverStatus {
	st := serverStatus{
		Name:          name,
		Address:       name + ".example.com",
		Resolved:      "1.2.3.4:25565",
		Online:        true,
		Version:       "1.20.4",
		OnlinePlayers: players,
		MaxPlayers:    100,
		MOTD:          "Hello World",
		Latency:       42 * time.Millisecond,
	}
	for i := 0; i < players; i++ {
		st.Sample = append(st.Sample, name+"-player-"+strings.Repeat("x", i%5))
	}
	return st
}

func renderAndDecode(t *testing.T, r *renderer, st serverStatus) [2]int {
	t.Helper()
	data, err := r.render(st, time.Date(2026, 11, 22, 18, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != imgWidth {
		t.Fatalf("width = %d, want %d", b.Dx(), imgWidth)
	}
	if b.Dy() <= 0 {
		t.Fatalf("height = %d", b.Dy())
	}
	return [2]int{b.Dx(), b.Dy()}
}

func TestRenderBasic(t *testing.T) {
	r := testRenderer(t)
	onlineSize := renderAndDecode(t, r, onlineStatus("acovia", 5))
	offline := serverStatus{Name: "offline", Address: "offline.example.com", Err: "connection timed out"}
	offlineSize := renderAndDecode(t, r, offline)
	if onlineSize[1] < 200 || offlineSize[1] < 200 {
		t.Fatalf("heights = %v, %v", onlineSize, offlineSize)
	}
}

func TestRenderHeightGrowsWithPlayers(t *testing.T) {
	r := testRenderer(t)
	short := renderAndDecode(t, r, onlineStatus("a", 4))
	long := renderAndDecode(t, r, onlineStatus("a", 45))
	if long[1] <= short[1] {
		t.Fatalf("height did not grow: %d -> %d", short[1], long[1])
	}
}

func TestRenderLongMOTD(t *testing.T) {
	r := testRenderer(t)
	st := onlineStatus("long", 3)
	st.MOTD = strings.Repeat("long server description ", 500)
	renderAndDecode(t, r, st)
	if lines := len(r.layoutCard(st).motd); lines > maxMOTDLines {
		t.Fatalf("motd lines = %d, want <= %d", lines, maxMOTDLines)
	}
}

func TestRenderOfflineLongError(t *testing.T) {
	r := testRenderer(t)
	st := serverStatus{Name: "broken", Address: "broken.example.com", Err: strings.Repeat("error ", 200)}
	renderAndDecode(t, r, st)
}

func TestRenderPlayerListCapped(t *testing.T) {
	r := testRenderer(t)
	c := r.layoutCard(onlineStatus("many", 200))
	if got, max := len(c.playerNames), maxPlayerRows*playerColumns; got > max {
		t.Fatalf("player names = %d, want <= %d", got, max)
	}
}

func TestRenderNoSample(t *testing.T) {
	r := testRenderer(t)
	st := onlineStatus("empty", 0)
	st.OnlinePlayers = 7
	st.Sample = nil
	renderAndDecode(t, r, st)
}

func TestRenderChineseWithSystemFont(t *testing.T) {
	r, path, err := newRenderer("")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("font = %s", path)
	st := onlineStatus("中文服务器", 3)
	st.MOTD = "欢迎来到 §a中文 §r服务器\n第二行"
	renderAndDecode(t, r, st)
}

func TestDisplayAddress(t *testing.T) {
	tests := []struct {
		addr, resolved, want string
	}{
		{"mc.example.com", "mc.example.com:25565", "mc.example.com"},
		{"mc.example.com:25565", "mc.example.com:25565", "mc.example.com:25565"},
		{"mc.example.com", "srv.example.com:25566", "mc.example.com (srv.example.com:25566)"},
		{"2001:db8::1", "[2001:db8::1]:25565", "2001:db8::1"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := displayAddress(tt.addr, tt.resolved); got != tt.want {
			t.Errorf("displayAddress(%q, %q) = %q, want %q", tt.addr, tt.resolved, got, tt.want)
		}
	}
}

func TestNewRendererMissingFont(t *testing.T) {
	if _, _, err := newRenderer("/nonexistent/font.ttf"); err == nil {
		t.Fatal("expected error")
	}
}

func TestTruncate(t *testing.T) {
	r := testRenderer(t)
	maxWidth := 80
	got := truncate(r.body, strings.Repeat("abcdef", 100), maxWidth)
	if measure(r.body, got) > maxWidth {
		t.Fatalf("truncated width = %d, want <= %d", measure(r.body, got), maxWidth)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated text = %q", got)
	}
}

func TestWrapText(t *testing.T) {
	r := testRenderer(t)
	maxWidth := 120
	lines := wrapText(r.body, strings.Repeat("word ", 30), maxWidth)
	if len(lines) < 2 {
		t.Fatalf("expected multiple lines, got %d", len(lines))
	}
	for _, line := range lines {
		if measure(r.body, line) > maxWidth {
			t.Fatalf("line %q width %d > %d", line, measure(r.body, line), maxWidth)
		}
	}

	cjk := wrapText(r.body, strings.Repeat("中文测试", 20), maxWidth)
	if len(cjk) < 2 {
		t.Fatalf("expected CJK wrapping, got %d lines", len(cjk))
	}
}

func TestTextVerticalBounds(t *testing.T) {
	r := testRenderer(t)
	top, bottom, ok := textVerticalBounds(r.title, "acovia")
	if !ok || top >= 0 || bottom < 0 {
		t.Fatalf("bounds = %d..%d ok=%v", top, bottom, ok)
	}
	if _, _, ok := textVerticalBounds(r.title, ""); ok {
		t.Fatal("empty text should have no glyph bounds")
	}
}
