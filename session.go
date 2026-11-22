package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// session holds the isolated server list for one group or private chat.
type session struct {
	path    string
	mu      sync.Mutex
	servers map[string]string
}

func newSession(path string) *session {
	return &session{
		path:    path,
		servers: make(map[string]string),
	}
}

func (s *session) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read session config %s: %w", s.path, err)
	}
	var servers map[string]string
	if err := json.Unmarshal(data, &servers); err != nil {
		return fmt.Errorf("parse session config %s: %w", s.path, err)
	}
	if servers == nil {
		servers = make(map[string]string)
	}
	s.mu.Lock()
	s.servers = servers
	s.mu.Unlock()
	return nil
}

func (s *session) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneServers(s.servers)
}

func (s *session) add(name, addr string) error {
	return s.update(func(servers map[string]string) error {
		if _, ok := servers[name]; ok {
			return fmt.Errorf("server %s already exists; delete it first with /server del %s", name, name)
		}
		servers[name] = addr
		return nil
	})
}

func (s *session) delete(name string) error {
	return s.update(func(servers map[string]string) error {
		if _, ok := servers[name]; !ok {
			return fmt.Errorf("server %s not found", name)
		}
		delete(servers, name)
		return nil
	})
}

// update applies mutate to a copy, persists it, and only then swaps the
// in-memory state. A failed write leaves the session unchanged.
func (s *session) update(mutate func(map[string]string) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneServers(s.servers)
	if err := mutate(next); err != nil {
		return err
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session config: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return err
	}
	s.servers = next
	return nil
}

// sessionStore lazily loads and caches one session per openid. The data layout
// is data/<session-id>/server.json.
type sessionStore struct {
	root string
	mu   sync.Mutex
	byID map[string]*session
}

func newSessionStore(root string) *sessionStore {
	return &sessionStore{
		root: root,
		byID: make(map[string]*session),
	}
}

func (st *sessionStore) get(target chatTarget) (*session, error) {
	if !validSessionID(target.openID) {
		return nil, fmt.Errorf("invalid session id %q", target.openID)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if sess, ok := st.byID[target.openID]; ok {
		return sess, nil
	}

	sess := newSession(filepath.Join(st.root, target.openID, "server.json"))
	if err := sess.load(); err != nil {
		return nil, err
	}
	st.byID[target.openID] = sess
	return sess, nil
}

func validSessionID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return !strings.ContainsAny(id, `/\`)
}

func cloneServers(servers map[string]string) map[string]string {
	next := make(map[string]string, len(servers))
	for name, addr := range servers {
		next[name] = addr
	}
	return next
}
