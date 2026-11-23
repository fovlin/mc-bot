package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type incomingMessage struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	GroupOpenID string `json:"group_openid"`
	Timestamp   string `json:"timestamp"`
	Author      struct {
		MemberOpenID string `json:"member_openid"`
		UserOpenID   string `json:"user_openid"`
		Bot          bool   `json:"bot"`
	} `json:"author"`
}

type bot struct {
	api      *qqAPI
	renderer *renderer
	sessions *sessionStore

	mu        sync.Mutex
	seen      map[string]struct{}
	seenOrder []string
}

func newBot(api *qqAPI, renderer *renderer, sessions *sessionStore) *bot {
	return &bot{
		api:      api,
		renderer: renderer,
		sessions: sessions,
		seen:     make(map[string]struct{}),
	}
}

func (b *bot) handleEvent(ctx context.Context, ev event) {
	switch ev.T {
	case "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE", "C2C_MESSAGE_CREATE":
	default:
		log.Printf("[cmd] ignoring event %q", ev.T)
		return
	}

	var msg incomingMessage
	if err := json.Unmarshal(ev.D, &msg); err != nil {
		log.Printf("[cmd] parse message: %v", err)
		return
	}

	var target chatTarget
	var member string
	switch ev.T {
	case "C2C_MESSAGE_CREATE":
		target = userTarget(msg.Author.UserOpenID)
		member = msg.Author.UserOpenID
	default:
		target = groupTarget(msg.GroupOpenID)
		member = msg.Author.MemberOpenID
	}
	if msg.ID == "" || !target.valid() || msg.Author.Bot {
		return
	}

	content := normalizeCommandText(strings.TrimSpace(msg.Content))
	if content == "" {
		return
	}
	if b.seenMessage(msg.ID) {
		return
	}
	log.Printf("[cmd] event=%s target=%s msg=%s member=%s time=%s content=%q",
		ev.T, target, msg.ID, member, msg.Timestamp, content)
	b.handleCommand(ctx, target, msg.ID, content)
}

func (b *bot) handleCommand(ctx context.Context, target chatTarget, msgID, content string) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return
	}

	switch fields[0] {
	case "/server":
		b.handleServer(ctx, target, msgID, fields[1:])
	case "/ls":
		if len(fields) > 2 {
			b.replyText(ctx, target, msgID, 1, "Usage: /ls [name]")
			return
		}
		name := ""
		if len(fields) == 2 {
			name = fields[1]
		}
		b.handleList(ctx, target, msgID, name)
	}
}

func (b *bot) handleServer(ctx context.Context, target chatTarget, msgID string, args []string) {
	const usage = "Usage:\n/server add <name> <address>\n/server del <name>"
	if len(args) == 0 {
		b.replyText(ctx, target, msgID, 1, usage)
		return
	}

	switch args[0] {
	case "add":
		if len(args) != 3 {
			b.replyText(ctx, target, msgID, 1, "Usage: /server add <name> <address>")
			return
		}
		reply, err := b.addServer(target, args[1], args[2])
		if err != nil {
			log.Printf("[cmd] add server: %v", err)
			b.replyText(ctx, target, msgID, 1, "Failed to add: "+err.Error())
			return
		}
		b.replyText(ctx, target, msgID, 1, reply)
	case "del":
		if len(args) != 2 {
			b.replyText(ctx, target, msgID, 1, "Usage: /server del <name>")
			return
		}
		reply, err := b.delServer(target, args[1])
		if err != nil {
			log.Printf("[cmd] delete server: %v", err)
			b.replyText(ctx, target, msgID, 1, "Failed to delete: "+err.Error())
			return
		}
		b.replyText(ctx, target, msgID, 1, reply)
	default:
		b.replyText(ctx, target, msgID, 1, usage)
	}
}

func (b *bot) handleList(ctx context.Context, target chatTarget, msgID, name string) {
	sess, err := b.sessions.get(target)
	if err != nil {
		log.Printf("[cmd] load session %s: %v", target, err)
		b.replyText(ctx, target, msgID, 1, "Failed to load session config: "+err.Error())
		return
	}
	servers := sess.snapshot()
	if len(servers) == 0 {
		b.replyText(ctx, target, msgID, 1, "No servers configured. Add one with /server add <name> <address>.")
		return
	}

	var names []string
	if name != "" {
		if _, ok := servers[name]; !ok {
			b.replyText(ctx, target, msgID, 1,
				fmt.Sprintf("Server %s not found. Available: %s", name, strings.Join(sortedServers(servers), ", ")))
			return
		}
		names = []string{name}
	} else {
		names = sortedServers(servers)
	}

	statuses := pingAll(ctx, names, servers)
	b.sendStatusImages(ctx, target, msgID, statuses)
}

// sendStatusImages answers with one image per server, so each card is a
// separate message and its height only depends on that server. Servers whose
// image could not be sent are summarized in one trailing text reply.
func (b *bot) sendStatusImages(ctx context.Context, target chatTarget, msgID string, statuses []serverStatus) {
	now := time.Now()
	var failed []serverStatus
	for i, status := range statuses {
		seq := i + 1
		err := b.sendStatusImage(ctx, target, msgID, seq, status, now)
		switch {
		case err == nil:
			log.Printf("[cmd] image reply sent msg=%s seq=%d target=%s server=%s", msgID, seq, target, status.Name)
		case isDuplicateMessageError(err):
			// This msg_seq was already answered, for example by another bot
			// instance; the image exists, so a text fallback would be noise.
			log.Printf("[cmd] image already sent msg=%s seq=%d; skipping fallback", msgID, seq)
		default:
			log.Printf("[cmd] send image for %s: %v", status.Name, err)
			failed = append(failed, status)
		}
	}
	if len(failed) == 0 {
		return
	}
	b.replyText(ctx, target, msgID, len(statuses)+1, formatStatusText(failed))
}

func (b *bot) sendStatusImage(ctx context.Context, target chatTarget, msgID string, seq int, status serverStatus, now time.Time) error {
	png, err := b.renderer.render(status, now)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	fileInfo, err := b.api.uploadImage(ctx, target, png)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return b.api.sendMedia(ctx, target, msgID, seq, fileInfo)
}

func formatStatusText(statuses []serverStatus) string {
	var sb strings.Builder
	sb.WriteString("Status (image could not be sent):\n")
	for _, status := range statuses {
		if status.Online {
			fmt.Fprintf(&sb, "- %s (%s): online %d/%d, %s, %d ms\n",
				status.Name, status.Address, status.OnlinePlayers, status.MaxPlayers,
				status.Version, status.Latency.Milliseconds())
		} else {
			fmt.Fprintf(&sb, "- %s (%s): offline, %s\n", status.Name, status.Address, status.Err)
		}
		if sb.Len() > 1800 {
			sb.WriteString("…\n")
			break
		}
	}
	return sb.String()
}

func (b *bot) replyText(ctx context.Context, target chatTarget, msgID string, seq int, text string) {
	if err := b.api.sendText(ctx, target, msgID, seq, text); err != nil {
		log.Printf("[cmd] send text: %v", err)
		return
	}
	log.Printf("[cmd] text reply sent msg=%s seq=%d target=%s", msgID, seq, target)
}

func (b *bot) addServer(target chatTarget, name, addr string) (string, error) {
	if err := validateServerName(name); err != nil {
		return "", err
	}
	if err := validateServerAddress(addr); err != nil {
		return "", err
	}
	sess, err := b.sessions.get(target)
	if err != nil {
		return "", err
	}
	if err := sess.add(name, addr); err != nil {
		return "", err
	}
	return fmt.Sprintf("Added server %s: %s", name, addr), nil
}

func (b *bot) delServer(target chatTarget, name string) (string, error) {
	sess, err := b.sessions.get(target)
	if err != nil {
		return "", err
	}
	if err := sess.delete(name); err != nil {
		return "", err
	}
	return fmt.Sprintf("Deleted server %s.", name), nil
}

// normalizeCommandText removes a leading @mention so full-mode group events
// (which may keep it) match the content of an @ event.
func normalizeCommandText(content string) string {
	if !strings.HasPrefix(content, "@") {
		return content
	}
	fields := strings.Fields(content)
	if len(fields) < 2 {
		return content
	}
	return strings.TrimSpace(strings.TrimPrefix(content, fields[0]))
}

// seenMessage implements the protocol requirement that the same msg_id may be
// delivered more than once.
func (b *bot) seenMessage(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.seen[id]; ok {
		return true
	}
	b.seen[id] = struct{}{}
	b.seenOrder = append(b.seenOrder, id)
	if len(b.seenOrder) > 4096 {
		oldest := b.seenOrder[0]
		b.seenOrder = b.seenOrder[1:]
		delete(b.seen, oldest)
	}
	return false
}

func sortedServers(servers map[string]string) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func validateServerName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if utf8.RuneCountInString(name) > 32 {
		return errors.New("name must be at most 32 characters")
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '\\' {
			return errors.New("name must not contain whitespace, control characters, or slashes")
		}
	}
	return nil
}

func validateServerAddress(addr string) error {
	if addr == "" {
		return errors.New("address must not be empty")
	}
	if len(addr) > 255 {
		return errors.New("address must be at most 255 bytes")
	}
	for _, r := range addr {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("address must not contain whitespace or control characters")
		}
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if host == "" {
			return errors.New("address is missing a host")
		}
		if !validPort(port) {
			return errors.New("port must be a number between 1 and 65535")
		}
		return nil
	}
	if strings.Contains(addr, ":") && net.ParseIP(strings.Trim(addr, "[]")) == nil {
		return errors.New("invalid address; example: mc.example.com or mc.example.com:25565")
	}
	return nil
}
