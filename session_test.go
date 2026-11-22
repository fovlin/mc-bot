package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStoreIsolationAndPersistence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	store := newSessionStore(root)

	group, err := store.get(groupTarget("G1"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.get(userTarget("U1"))
	if err != nil {
		t.Fatal(err)
	}

	if err := group.add("group-server", "group.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := user.add("private-server", "private.example.com"); err != nil {
		t.Fatal(err)
	}

	if got := group.snapshot(); got["group-server"] != "group.example.com" || len(got) != 1 {
		t.Fatalf("group servers = %v", got)
	}
	if got := user.snapshot(); got["private-server"] != "private.example.com" || len(got) != 1 {
		t.Fatalf("user servers = %v", got)
	}

	for _, path := range []string{
		filepath.Join(root, "G1", "server.json"),
		filepath.Join(root, "U1", "server.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}

	reloaded := newSessionStore(root)
	groupAgain, err := reloaded.get(groupTarget("G1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := groupAgain.snapshot(); got["group-server"] != "group.example.com" || len(got) != 1 {
		t.Fatalf("reloaded group servers = %v", got)
	}

	if err := group.delete("group-server"); err != nil {
		t.Fatal(err)
	}
	if got := group.snapshot(); len(got) != 0 {
		t.Fatalf("servers after delete = %v", got)
	}
}

func TestSessionInvalidID(t *testing.T) {
	store := newSessionStore(t.TempDir())
	for _, id := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := store.get(userTarget(id)); err == nil {
			t.Errorf("id %q should be rejected", id)
		}
	}
}

func TestSessionCorruptConfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	dir := filepath.Join(root, "G1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(root)
	if _, err := store.get(groupTarget("G1")); err == nil {
		t.Fatal("expected corrupt session config error")
	}
}
