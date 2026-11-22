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

type groupMessage struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	GroupOpenID string `json:"group_openid"`
	Author      struct {
		MemberOpenID   string `json:"member_openid"`
		MemberNickname string `json:"member_nickname"`
		Bot            bool   `json:"bot"`
	} `json:"author"`
}

type bot struct {
	api      *qqAPI
	renderer *renderer

	mu        sync.Mutex
	cfg       *config
	seen      map[string]struct{}
	seenOrder []string
}

func newBot(cfg *config, api *qqAPI, renderer *renderer) *bot {
	return &bot{
		cfg:      cfg,
		api:      api,
		renderer: renderer,
		seen:     make(map[string]struct{}),
	}
}

func (b *bot) handleEvent(ctx context.Context, ev event) {
	switch ev.T {
	case "GROUP_AT_MESSAGE_CREATE", "GROUP_MESSAGE_CREATE":
	default:
		return
	}

	var msg groupMessage
	if err := json.Unmarshal(ev.D, &msg); err != nil {
		log.Printf("[cmd] parse group message: %v", err)
		return
	}
	if msg.ID == "" || msg.GroupOpenID == "" || msg.Author.Bot {
		return
	}
	if b.seenMessage(msg.ID) {
		return
	}

	content := strings.TrimSpace(msg.Content)
	if content == "" {
		return
	}
	b.handleCommand(ctx, msg.GroupOpenID, msg.ID, content)
}

func (b *bot) handleCommand(ctx context.Context, groupOpenID, msgID, content string) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return
	}
	switch fields[0] {
	case "/server":
		b.handleServer(ctx, groupOpenID, msgID, fields[1:])
	case "/ls":
		if len(fields) > 2 {
			b.replyText(ctx, groupOpenID, msgID, 1, "Usage: /ls [name]")
			return
		}
		name := ""
		if len(fields) == 2 {
			name = fields[1]
		}
		b.handleList(ctx, groupOpenID, msgID, name)
	}
}

func (b *bot) handleServer(ctx context.Context, groupOpenID, msgID string, args []string) {
	const usage = "Usage:\n/server add <name> <address>\n/server del <name>"
	if len(args) == 0 {
		b.replyText(ctx, groupOpenID, msgID, 1, usage)
		return
	}

	switch args[0] {
	case "add":
		if len(args) != 3 {
			b.replyText(ctx, groupOpenID, msgID, 1, "Usage: /server add <name> <address>")
			return
		}
		reply, err := b.addServer(args[1], args[2])
		if err != nil {
			log.Printf("[cmd] add server: %v", err)
			b.replyText(ctx, groupOpenID, msgID, 1, "Failed to add: "+err.Error())
			return
		}
		b.replyText(ctx, groupOpenID, msgID, 1, reply)
	case "del":
		if len(args) != 2 {
			b.replyText(ctx, groupOpenID, msgID, 1, "Usage: /server del <name>")
			return
		}
		reply, err := b.delServer(args[1])
		if err != nil {
			log.Printf("[cmd] delete server: %v", err)
			b.replyText(ctx, groupOpenID, msgID, 1, "Failed to delete: "+err.Error())
			return
		}
		b.replyText(ctx, groupOpenID, msgID, 1, reply)
	default:
		b.replyText(ctx, groupOpenID, msgID, 1, usage)
	}
}

func (b *bot) handleList(ctx context.Context, groupOpenID, msgID, name string) {
	servers := b.servers()
	if len(servers) == 0 {
		b.replyText(ctx, groupOpenID, msgID, 1, "No servers configured. Add one with /server add <name> <address>.")
		return
	}

	var names []string
	if name != "" {
		if _, ok := servers[name]; !ok {
			b.replyText(ctx, groupOpenID, msgID, 1,
				fmt.Sprintf("Server %s not found. Available: %s", name, strings.Join(sortedServers(servers), ", ")))
			return
		}
		names = []string{name}
	} else {
		names = sortedServers(servers)
	}

	statuses := pingAll(ctx, names, servers)
	png, err := b.renderer.render(statuses, time.Now())
	if err != nil {
		log.Printf("[cmd] render status image: %v", err)
		b.sendFallback(ctx, groupOpenID, msgID, statuses, 1)
		return
	}

	fileInfo, err := b.api.uploadGroupImage(ctx, groupOpenID, png)
	if err != nil {
		log.Printf("[cmd] upload status image: %v", err)
		b.sendFallback(ctx, groupOpenID, msgID, statuses, 1)
		return
	}

	if err := b.api.sendGroupMedia(ctx, groupOpenID, msgID, 1, fileInfo); err != nil {
		log.Printf("[cmd] send status image: %v", err)
		b.sendFallback(ctx, groupOpenID, msgID, statuses, 2)
	}
}

func (b *bot) sendFallback(ctx context.Context, groupOpenID, msgID string, statuses []serverStatus, seq int) {
	b.replyText(ctx, groupOpenID, msgID, seq, formatStatusText(statuses))
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

func (b *bot) replyText(ctx context.Context, groupOpenID, msgID string, seq int, text string) {
	if err := b.api.sendGroupText(ctx, groupOpenID, msgID, seq, text); err != nil {
		log.Printf("[cmd] send text: %v", err)
	}
}

func (b *bot) addServer(name, addr string) (string, error) {
	if err := validateServerName(name); err != nil {
		return "", err
	}
	if err := validateServerAddress(addr); err != nil {
		return "", err
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.cfg.Servers[name]; ok {
		return "", fmt.Errorf("server %s already exists; delete it first with /server del %s", name, name)
	}
	next := cloneServers(b.cfg.Servers)
	next[name] = addr
	updated := *b.cfg
	updated.Servers = next
	if err := updated.save(); err != nil {
		return "", fmt.Errorf("save config: %w", err)
	}
	b.cfg = &updated
	return fmt.Sprintf("Added server %s: %s", name, addr), nil
}

func (b *bot) delServer(name string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.cfg.Servers[name]; !ok {
		return "", fmt.Errorf("server %s not found", name)
	}
	next := cloneServers(b.cfg.Servers)
	delete(next, name)
	updated := *b.cfg
	updated.Servers = next
	if err := updated.save(); err != nil {
		return "", fmt.Errorf("save config: %w", err)
	}
	b.cfg = &updated
	return fmt.Sprintf("Deleted server %s.", name), nil
}

func (b *bot) servers() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneServers(b.cfg.Servers)
}

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

func cloneServers(servers map[string]string) map[string]string {
	next := make(map[string]string, len(servers))
	for name, addr := range servers {
		next[name] = addr
	}
	return next
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
