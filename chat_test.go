package main

import "testing"

func TestChatTargets(t *testing.T) {
	tests := []struct {
		name   string
		target chatTarget
		valid  bool
		str    string
		path   string
	}{
		{"group", groupTarget("G1"), true, "group:G1", "/v2/groups/G1/messages"},
		{"user", userTarget("U1"), true, "user:U1", "/v2/users/U1/messages"},
		{"empty group", groupTarget(""), false, "group:", "/v2/groups//messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.target.valid(); got != tt.valid {
				t.Fatalf("valid = %v, want %v", got, tt.valid)
			}
			if got := tt.target.String(); got != tt.str {
				t.Fatalf("String = %q, want %q", got, tt.str)
			}
			if got := tt.target.path("/messages"); got != tt.path {
				t.Fatalf("path = %q, want %q", got, tt.path)
			}
		})
	}
}
