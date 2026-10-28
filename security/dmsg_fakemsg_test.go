package main

import (
	"testing"

	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// makeMsgWithMentions builds a fake *events.Message whose ExtendedTextMessage
// text contains visible mention tokens (with U+2068/U+2069 hidden chars) and
// whose ContextInfo.MentionedJID contains the corresponding JID list.
func makeMsgWithMentions(text string, mentions []string) *events.Message {
	ext := &waE2E.ExtendedTextMessage{
		Text: proto.String(text),
		ContextInfo: &waE2E.ContextInfo{
			MentionedJID: mentions,
		},
	}
	return &events.Message{
		Message: &waE2E.Message{
			ExtendedTextMessage: ext,
		},
	}
}

func TestParseDmsgFlags_NoFlags(t *testing.T) {
	msg := makeMsgWithMentions("hello world", nil)
	f, clean := parseDmsgFlags(msg, "hello world")
	if f.useSPR || f.hasViewer {
		t.Fatalf("expected no flags, got %+v", f)
	}
	if clean != "hello world" {
		t.Fatalf("clean=%q want %q", clean, "hello world")
	}
}

func TestParseDmsgFlags_LowercaseI(t *testing.T) {
	msg := makeMsgWithMentions("hello -i", nil)
	f, clean := parseDmsgFlags(msg, "hello -i")
	if !f.useSPR || f.hasViewer {
		t.Fatalf("expected useSPR=true, hasViewer=false, got %+v", f)
	}
	if clean != "hello" {
		t.Fatalf("clean=%q want %q", clean, "hello")
	}
}

func TestParseDmsgFlags_CapitalI_WithMention(t *testing.T) {
	// Visible text: ".dmsg -I @⁨kangwifi⁩"
	// Hidden chars: \u2068 and \u2069 wrap "kangwifi"
	visible := "-I @\u2068kangwifi\u2069"
	mentions := []string{"221217021784141@lid"}
	msg := makeMsgWithMentions(visible, mentions)

	f, clean := parseDmsgFlags(msg, visible)

	if !f.useSPR {
		t.Fatal("expected useSPR=true")
	}
	if !f.hasViewer {
		t.Fatal("expected hasViewer=true")
	}
	if f.viewer.IsEmpty() {
		t.Fatal("expected viewer to be resolved (non-empty JID)")
	}
	// Should resolve to the LID JID
	if f.viewer.User != "221217021784141" {
		t.Fatalf("viewer.User=%q want %q", f.viewer.User, "221217021784141")
	}
	if f.viewer.Server != types.HiddenUserServer {
		t.Fatalf("viewer.Server=%q want %q", f.viewer.Server, types.HiddenUserServer)
	}
	if clean != "" {
		t.Fatalf("clean=%q want empty (all tokens consumed)", clean)
	}
}

func TestParseDmsgFlags_CapitalI_MentionWithText(t *testing.T) {
	// Visible: ".say @⁨Ruby Hoshino⁩ hello -I @⁨kangwifi⁩"
	// First @ = Ruby Hoshino (mention idx 0)
	// Second @ = kangwifi (mention idx 1)
	visible := "@\u2068Ruby Hoshino\u2069 hello -I @\u2068kangwifi\u2069"
	mentions := []string{"99999999999@s.whatsapp.net", "221217021784141@lid"}
	msg := makeMsgWithMentions(visible, mentions)

	f, clean := parseDmsgFlags(msg, visible)

	if !f.hasViewer {
		t.Fatal("expected hasViewer=true")
	}
	// The viewer should be the SECOND mention (kangwifi's LID), not the first.
	if f.viewer.User != "221217021784141" {
		t.Fatalf("viewer.User=%q want %q (kangwifi's LID)", f.viewer.User, "221217021784141")
	}
	// The remaining text should still have "Ruby Hoshino hello"
	if clean == "" {
		t.Fatal("clean should not be empty (target mention + text remain)")
	}
	// Hidden chars should be stripped
	for _, bad := range []string{"\u2068", "\u2069"} {
		if containsStr(clean, bad) {
			t.Fatalf("clean still contains hidden char %q: %q", bad, clean)
		}
	}
	// Should still have "hello"
	if !containsStr(clean, "hello") {
		t.Fatalf("clean should contain 'hello', got %q", clean)
	}
}

func TestStripHiddenChars(t *testing.T) {
	in := "@\u2068kangwifi\u2069 hello\u200B world"
	out := stripHiddenChars(in)
	want := "@kangwifi hello world"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
