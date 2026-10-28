package main

// ─── jjam_test.go — the media of the JAM commands ─────────────────────
//
// The bot has to know what kind of media a message holds, because the
// service puts the file into the slot that wants it. WhatsApp does not
// always say what it sends, so the first bytes of the file decide when
// the message does not say it.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// TestJjamFileName checks the name of the file of a media message.
//
// The service reads the kind of the file from the name when the type of
// the message is not usable, so the extension has to follow the media.
func TestJjamFileName(t *testing.T) {
	cases := []struct {
		name string
		file string
		mime string
		want string
	}{
		{"a video", "", "video/mp4", "video.mp4"},
		{"a photo", "", "image/jpeg", "photo.jpg"},
		{"a sound", "", "audio/mpeg", "audio.mp3"},
		{"an unknown file", "", "application/x-thing", "file.bin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jjamFileName(nil, c.mime); got != c.want {
				t.Fatalf("jjamFileName(nil, %q) = %q, want %q", c.mime, got, c.want)
			}
		})
	}

	// A document keeps its name, but the extension follows the media.
	msg := &events.Message{
		Message: &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{
				FileName: proto.String("VID_001.dat"),
			},
		},
	}
	if got := jjamFileName(msg, "video/mp4"); got != "VID_001.mp4" {
		t.Fatalf("jjamFileName of a document = %q, want %q", got, "VID_001.mp4")
	}
	// A document without a name gets the name of its kind.
	nameless := &events.Message{
		Message: &waE2E.Message{
			DocumentMessage: &waE2E.DocumentMessage{},
		},
	}
	if got := jjamFileName(nameless, "video/mp4"); got != "video.mp4" {
		t.Fatalf("jjamFileName of a nameless document = %q, want %q", got, "video.mp4")
	}
}

func TestJjamSniffMime(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"jpeg", []byte{0xff, 0xd8, 0xff, 0xe0}, "image/jpeg"},
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, "image/png"},
		{"gif", []byte("GIF89a0000"), "image/gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"mp4", []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, "video/mp4"},
		{"mov", []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'q', 't', ' ', ' '}, "video/quicktime"},
		{"3gp", []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', '3', 'g', 'p', '4'}, "video/3gpp"},
		{"m4a", []byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '}, "audio/mp4"},
		{"mp3", []byte("ID3\x03\x00\x00\x00\x00\x00\x00"), "audio/mpeg"},
		{"ogg", []byte("OggS\x00\x02\x00\x00\x00\x00\x00"), "audio/ogg"},
		{"mkv", []byte{0x1a, 0x45, 0xdf, 0xa3, 0, 0, 0, 0}, "video/x-matroska"},
		{"unknown", []byte("hello there!!"), ""},
		{"short", []byte("hi"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jjamSniffMime(c.raw)
			if got != c.want {
				t.Fatalf("jjamSniffMime = %q, want %q", got, c.want)
			}
		})
	}
}

func TestJjamExt(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":       ".jpg",
		"image/png":        ".png",
		"video/mp4":        ".mp4",
		"video/3gpp":       ".3gp",
		"video/quicktime":  ".mov",
		"video/x-matroska": ".mkv",
		"audio/mpeg":       ".mp3",
		"audio/mp4":        ".m4a",
		"application/x":    "",
	}
	for mime, want := range cases {
		if got := jjamExt(mime); got != want {
			t.Fatalf("jjamExt(%q) = %q, want %q", mime, got, want)
		}
	}
}

// TestJjamMissing checks that the slots that still wait for a file are
// read from the answer of the service, with the number the user writes in
// the caption of the file to pick the slot.
func TestJjamMissing(t *testing.T) {
	answer := map[string]any{
		"state": "awaiting_media",
		"progress": map[string]any{
			"missing": []any{
				map[string]any{"name": "one.png", "kind": "photo", "number": float64(3)},
				map[string]any{"name": "two.mp4", "kind": "video", "number": float64(4)},
			},
		},
	}
	missing := jjamMissing(answer)
	if len(missing) != 2 {
		t.Fatalf("jjamMissing gave %d slots, want 2", len(missing))
	}
	if got := jjamSlotNumber(missing[0]); got != 3 {
		t.Fatalf("jjamSlotNumber = %d, want 3", got)
	}
	if got := jjamSlotName(missing[0]); got != "one.png" {
		t.Fatalf("jjamSlotName = %q, want %q", got, "one.png")
	}
	if got := jjamSlotKind(missing[1]); got != "video" {
		t.Fatalf("jjamSlotKind = %q, want %q", got, "video")
	}

	// An answer without slots has no missing media.
	if got := jjamMissing(map[string]any{}); len(got) != 0 {
		t.Fatalf("jjamMissing of an empty answer gave %d slots", len(got))
	}
	if got := jjamMissing(nil); len(got) != 0 {
		t.Fatalf("jjamMissing of nothing gave %d slots", len(got))
	}
	// A slot without a number has no number.
	if got := jjamSlotNumber(map[string]any{}); got != 0 {
		t.Fatalf("jjamSlotNumber of a slot without a number = %d, want 0", got)
	}
	// A slot without a name still says something.
	if got := jjamSlotName(map[string]any{}); got == "" {
		t.Fatal("jjamSlotName of a slot without a name is empty")
	}
	if got := jjamSlotKind(map[string]any{}); got != "media" {
		t.Fatalf("jjamSlotKind of a slot without a kind = %q, want %q", got, "media")
	}
}

// TestJjamRenderPercent checks how much of the render is done.
func TestJjamRenderPercent(t *testing.T) {
	rendering := map[string]any{
		"state":  "rendering",
		"render": map[string]any{"progress": 0.42},
	}
	if got := jjamRenderPercent(rendering); got != 42 {
		t.Fatalf("jjamRenderPercent = %d, want 42", got)
	}
	// The progress of a render that does not run means nothing.
	idle := map[string]any{
		"state":  "awaiting_media",
		"render": map[string]any{"progress": 0.42},
	}
	if got := jjamRenderPercent(idle); got != -1 {
		t.Fatalf("jjamRenderPercent of an idle session = %d, want -1", got)
	}
	if got := jjamRenderPercent(map[string]any{}); got != -1 {
		t.Fatalf("jjamRenderPercent of an empty answer = %d, want -1", got)
	}
}

// TestJjamRenderReply checks how the bot reads the answer of a render
// command.
//
// The service warns the user when the media is not complete. The render
// does not start then, and it goes on when the command comes again.
func TestJjamRenderReply(t *testing.T) {
	// The service warns: the render did not start.
	warned := map[string]any{
		"state":    "awaiting_media",
		"rendered": false,
		"message":  "⚠️ Media-nya masih kurang 1:\n1. one.png — photo",
	}
	started, text := jjamRenderReply(warned)
	if started {
		t.Fatal("the render started, but the service only warned")
	}
	if text == "" {
		t.Fatal("the warning of the service is empty")
	}

	// The command comes again: the render starts.
	rendering := map[string]any{
		"state":    "rendering",
		"rendered": true,
		"message":  "⚠️ Oke, dirender dengan 1 media yang kurang.",
	}
	started, text = jjamRenderReply(rendering)
	if !started {
		t.Fatal("the render did not start")
	}
	if text == "" {
		t.Fatal("the answer of the service is empty")
	}

	// An older service does not say whether the render started. The state
	// is then the answer.
	old := map[string]any{"state": "rendering", "message": "Render jalan ya."}
	started, _ = jjamRenderReply(old)
	if !started {
		t.Fatal("the render of an older service did not start")
	}
	old = map[string]any{"state": "awaiting_media", "message": "Masih kurang 1 media."}
	started, _ = jjamRenderReply(old)
	if started {
		t.Fatal("the render of an older service started, but the media is not complete")
	}
}

// TestJjamProgressDue checks when the user is told about the render.
//
// The chat has to stay quiet: a render that is quick says nothing, and a
// long render says it only once, in the middle.
func TestJjamProgressDue(t *testing.T) {
	cases := []struct {
		name    string
		percent int
		elapsed time.Duration
		want    bool
	}{
		{"a render that just began", 10, 10 * time.Second, false},
		{"a render that is slow", 10, 5 * time.Minute, false},
		{"a render that is quick", 90, 5 * time.Second, false},
		{"a render that is halfway", 50, 3 * time.Minute, true},
		{"a render that is nearly done", 96, 10 * time.Minute, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jjamProgressDue(c.percent, c.elapsed); got != c.want {
				t.Fatalf("jjamProgressDue(%d, %v) = %v, want %v", c.percent, c.elapsed, got, c.want)
			}
		})
	}
}

// TestJjamRenderElapsed checks how long the render has run.
//
// The service counts the time of the render, so the bot does not have to
// count it on its own.
func TestJjamRenderElapsed(t *testing.T) {
	answer := map[string]any{
		"state":  "rendering",
		"render": map[string]any{"progress": 0.5, "elapsed": float64(90)},
	}
	if got := jjamRenderElapsed(answer); got != 90*time.Second {
		t.Fatalf("jjamRenderElapsed = %v, want 90s", got)
	}
	// An answer without a render says nothing.
	if got := jjamRenderElapsed(map[string]any{}); got != 0 {
		t.Fatalf("jjamRenderElapsed of an empty answer = %v, want 0", got)
	}
	// A render that just began has no time yet.
	fresh := map[string]any{"render": map[string]any{"elapsed": float64(0)}}
	if got := jjamRenderElapsed(fresh); got != 0 {
		t.Fatalf("jjamRenderElapsed of a fresh render = %v, want 0", got)
	}
}

// TestJjamRenderOver checks when the bot stops watching a render.
//
// A render that is done or that failed does not run anymore, so the bot
// does not keep asking about it.
func TestJjamRenderOver(t *testing.T) {
	// A render that runs is not over.
	if jjamRenderOver(map[string]any{"state": "rendering"}) {
		t.Fatal("a render that runs is over")
	}
	// A render that is done is over.
	if !jjamRenderOver(map[string]any{"state": "done"}) {
		t.Fatal("a render that is done is not over")
	}
	// A session that was never used is over.
	if !jjamRenderOver(map[string]any{"state": "idle"}) {
		t.Fatal("a session that was never used is not over")
	}
	// A render that failed goes back to the media, with the error.
	failed := map[string]any{"state": "awaiting_media", "error": "Render gagal"}
	if !jjamRenderOver(failed) {
		t.Fatal("a render that failed is not over")
	}
	// A session that waits for media is not over: a render can start.
	waiting := map[string]any{"state": "awaiting_media", "error": nil}
	if jjamRenderOver(waiting) {
		t.Fatal("a session that waits for media is over")
	}
}

// TestJjamWatchProgressOnce checks that the watcher tells the user once.
//
// The chat has to stay quiet: the watcher sends a single message and then
// stops, no matter how long the render runs.
func TestJjamWatchProgressOnce(t *testing.T) {
	// The service answers that the render is half way.
	var polls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&polls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"state":"rendering","render":{"progress":0.6}}`)
	}))
	defer server.Close()
	t.Setenv("AMWEB_API", server.URL)

	// The timings are shortened, so the test does not take minutes.
	oldDelay, oldPoll := jjamProgressDelay, jjamProgressPoll
	jjamProgressDelay, jjamProgressPoll = 0, 10*time.Millisecond
	defer func() {
		jjamProgressDelay, jjamProgressPoll = oldDelay, oldPoll
	}()

	// The client is nil, so no message goes out. The watcher still has to
	// stop after the one message.
	progress := &jjamProgress{}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		jjamWatchProgress(nil, types.JID{}, "u1", done, progress)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		close(done)
		t.Fatal("the watcher did not stop after the one message")
	}
	close(done)

	// The watcher asked the service a few times, but it stopped right
	// after the message.
	if n := atomic.LoadInt32(&polls); n == 0 {
		t.Fatal("the watcher did not ask the service about the render")
	}
	if n := atomic.LoadInt32(&polls); n > 10 {
		t.Fatalf("the watcher asked the service %d times, want only a few", n)
	}
}

// TestJjamWatchProgressQuiet checks that a quick render says nothing.
//
// A render that is over before the watcher looks at it does not get a
// message: the video alone is enough.
func TestJjamWatchProgressQuiet(t *testing.T) {
	var polls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&polls, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"state":"done","render":{"progress":1}}`)
	}))
	defer server.Close()
	t.Setenv("AMWEB_API", server.URL)

	oldDelay, oldPoll := jjamProgressDelay, jjamProgressPoll
	jjamProgressDelay, jjamProgressPoll = 0, 10*time.Millisecond
	defer func() {
		jjamProgressDelay, jjamProgressPoll = oldDelay, oldPoll
	}()

	progress := &jjamProgress{}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		jjamWatchProgress(nil, types.JID{}, "u1", done, progress)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		close(done)
		t.Fatal("the watcher did not stop")
	}
	close(done)

	// A render that is done has no percentage, so nothing was said.
	progress.mu.Lock()
	ids := len(progress.ids)
	progress.mu.Unlock()
	if ids != 0 {
		t.Fatalf("the watcher sent %d messages for a render that was over", ids)
	}
}
