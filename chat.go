package main

import "net/url"

type chatKind int

const (
	chatGroup chatKind = iota
	chatUser
)

// chatTarget is either a group or a private (C2C) conversation. Keeping the
// target explicit lets the command layer stay identical for both.
type chatTarget struct {
	kind   chatKind
	openID string
}

func groupTarget(openID string) chatTarget { return chatTarget{kind: chatGroup, openID: openID} }
func userTarget(openID string) chatTarget  { return chatTarget{kind: chatUser, openID: openID} }

func (t chatTarget) valid() bool { return t.openID != "" }

func (t chatTarget) String() string {
	if t.kind == chatUser {
		return "user:" + t.openID
	}
	return "group:" + t.openID
}

func (t chatTarget) path(suffix string) string {
	prefix := "/v2/groups/"
	if t.kind == chatUser {
		prefix = "/v2/users/"
	}
	return prefix + url.PathEscape(t.openID) + suffix
}
