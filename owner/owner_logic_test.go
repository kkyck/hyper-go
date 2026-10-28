package main

import (
	"os"
	"testing"
)

func TestNormalizeOwnerEntry(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"62812xxx", "62812xxx@s.whatsapp.net"},
		{"62812xxx@s.whatsapp.net", "62812xxx@s.whatsapp.net"},
		{"12345@lid", "12345@lid"},
		{"12345@LID", "12345@lid"},
		{"  62812  ", "62812@s.whatsapp.net"},
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeOwnerEntry(c.in)
		if got != c.want {
			t.Errorf("normalizeOwnerEntry(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeOwnerLID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"12345", "12345@lid"},
		{"12345@lid", "12345@lid"},
		{"12345@s.whatsapp.net", "12345@lid"}, // strips server, forces lid
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeOwnerLID(c.in)
		if got != c.want {
			t.Errorf("normalizeOwnerLID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOwnerJIDsFromEnv(t *testing.T) {
	os.Setenv("OWNER_JID", "628111,628222@s.whatsapp.net")
	os.Setenv("OWNER_LID", "999888,777666@lid")
	defer func() {
		os.Unsetenv("OWNER_JID")
		os.Unsetenv("OWNER_LID")
	}()

	list := ownerJIDs()
	want := map[string]bool{
		"628111@s.whatsapp.net": true,
		"628222@s.whatsapp.net": true,
		"999888@lid":            true,
		"777666@lid":            true,
	}
	if len(list) != 4 {
		t.Fatalf("ownerJIDs len=%d list=%v, want 4", len(list), list)
	}
	for _, j := range list {
		if !want[j] {
			t.Errorf("unexpected owner entry %q", j)
		}
	}
}

func TestIsOwnerPNAndLID(t *testing.T) {
	os.Setenv("OWNER_JID", "628123456789")
	os.Setenv("OWNER_LID", "111222333444")
	defer func() {
		os.Unsetenv("OWNER_JID")
		os.Unsetenv("OWNER_LID")
	}()

	if !isOwner("628123456789@s.whatsapp.net") {
		t.Error("should match OWNER_JID phone")
	}
	if !isOwner("628123456789:5@s.whatsapp.net") {
		t.Error("should match OWNER_JID with device suffix")
	}
	if !isOwner("111222333444@lid") {
		t.Error("should match OWNER_LID")
	}
	if isOwner("999999999@s.whatsapp.net") {
		t.Error("should NOT match random phone")
	}
	if isOwner("999999999@lid") {
		t.Error("should NOT match random LID")
	}
}

func TestGroupPublicDefault(t *testing.T) {
	// fresh map
	groupPermsMu.Lock()
	groupPerms = make(map[string]*GroupPerm)
	groupPermsMu.Unlock()

	if isGroupPublic("120363@g.us") {
		t.Error("default group should be locked (not public)")
	}
	setGroupPublic("120363@g.us", true)
	if !isGroupPublic("120363@g.us") {
		t.Error("after setGroupPublic true, should be public")
	}
	setGroupPublic("120363@g.us", false)
	if isGroupPublic("120363@g.us") {
		t.Error("after setGroupPublic false, should be locked")
	}
}

func TestTrustAddRemove(t *testing.T) {
	groupPermsMu.Lock()
	groupPerms = make(map[string]*GroupPerm)
	groupPermsMu.Unlock()

	g := "120363@g.us"
	if !addTrust(g, "628111@s.whatsapp.net") {
		t.Fatal("addTrust should succeed")
	}
	if addTrust(g, "628111") {
		t.Error("duplicate trust should return false")
	}
	if !isTrustedInGroup(g, "628111@s.whatsapp.net") {
		t.Error("should be trusted")
	}
	if !removeTrust(g, "628111") {
		t.Error("removeTrust should succeed")
	}
	if isTrustedInGroup(g, "628111@s.whatsapp.net") {
		t.Error("should no longer be trusted")
	}
}

func TestStripDevice(t *testing.T) {
	got := stripDevice("62812:5@s.whatsapp.net")
	want := "62812@s.whatsapp.net"
	if got != want {
		t.Errorf("stripDevice = %q, want %q", got, want)
	}
}

func TestIsOwnerNoFalseLIDMatch(t *testing.T) {
	os.Setenv("OWNER_JID", "628123456789")
	os.Unsetenv("OWNER_LID")
	defer os.Unsetenv("OWNER_JID")

	// LID with digits that happen to not be the phone should not match as phone
	if isOwner("999888777666@lid") {
		t.Error("random LID must not match OWNER_JID-only config")
	}
	// Phone match still works
	if !isOwner("628123456789@s.whatsapp.net") {
		t.Error("PN should still match")
	}
}

func TestSelfModeDefaultOn(t *testing.T) {
	selfModeMu.Lock()
	selfModeState = SelfModeState{SelfMode: true, UpdatedAt: 0}
	selfModeMu.Unlock()
	if !isSelfMode() {
		t.Error("default self mode should be ON")
	}
	setSelfMode(false)
	if isSelfMode() {
		t.Error("after setSelfMode(false) should be OFF")
	}
	setSelfMode(true)
	if !isSelfMode() {
		t.Error("after setSelfMode(true) should be ON")
	}
}
