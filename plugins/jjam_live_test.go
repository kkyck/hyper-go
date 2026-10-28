package main

// ─── jjam_live_test.go — the plugin against the running service ─────────
//
// The plugin does not render the videos itself: it talks to the AM Web
// service over HTTP. These tests check that the answers the plugin reads
// really look the way it expects.
//
// The tests are skipped when the service is not running, so the unit tests
// still work on a machine without it.

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// jjamLive runs a test against the running service, or skips it.
func jjamLive(t *testing.T) {
	t.Helper()
	addr := jjamAPI()
	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(addr + "/health")
	if err != nil {
		t.Skipf("the service at %s is not running: %v", addr, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Skipf("the service at %s answered %d", addr, res.StatusCode)
	}
}

// TestJjamLiveStatus checks the status of a session that does not exist.
func TestJjamLiveStatus(t *testing.T) {
	jjamLive(t)
	answer, err := jjamRequest(http.MethodGet, "/api/jam/status?userId=go-test-empty", nil)
	if err != nil {
		t.Fatalf("the service did not answer: %v", err)
	}
	if ok, _ := answer["ok"].(bool); !ok {
		t.Fatalf("the service did not take the request: %v", answer)
	}
	// The plugin asks the state to know what to do.
	if state := jjamState(answer); state == "" {
		t.Fatalf("the answer has no state: %v", answer)
	}
	// A session that does not exist has no missing media.
	if missing := jjamMissing(answer); len(missing) != 0 {
		t.Fatalf("an empty session reported %d missing slots", len(missing))
	}
}

// TestJjamLiveCommand checks the commands the plugin sends.
func TestJjamLiveCommand(t *testing.T) {
	jjamLive(t)
	// The help of the bot comes from the same core the plugin uses.
	answer, err := jjamRequest(http.MethodPost, "/api/jam/command", map[string]any{
		"userId":  "go-test-help",
		"command": "!help",
	})
	if err != nil {
		t.Fatalf("the service did not answer: %v", err)
	}
	text := jjamText(answer)
	if text == "" {
		t.Fatalf("the service did not send a message: %v", answer)
	}
	// The help mentions the sub commands of `!jjam`.
	for _, want := range []string{"!jjam render", "!jjam status", "!jjam cancel"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the help does not mention %q:\n%s", want, text)
		}
	}
}

// TestJjamLiveMedia checks that the service takes a file and puts it into
// a slot, and that it names the slot in the answer.
func TestJjamLiveMedia(t *testing.T) {
	jjamLive(t)
	// A tiny PNG: one pixel of a red square.
	png := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde, 0x00, 0x00, 0x00,
		0x0c, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00,
		0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d, 0xb0, 0x00, 0x00, 0x00,
		0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
	answer, err := jjamRequest(http.MethodPost, "/api/jam/media", map[string]any{
		"userId":   "go-test-media",
		"filename": "go-test.png",
		"mimeType": "image/png",
		"base64":   base64.StdEncoding.EncodeToString(png),
	})
	if err != nil {
		t.Fatalf("the service did not answer: %v", err)
	}
	// Without a template the service refuses the file, and it says why.
	text := jjamText(answer)
	if text == "" {
		t.Fatalf("the service did not send a message: %v", answer)
	}
	if !strings.Contains(text, "template") {
		t.Fatalf("the answer does not talk about the template:\n%s", text)
	}
}

// TestJjamLiveVideo checks that the video of a render can be fetched.
//
// The video comes on its own route, so it does not travel through the JSON
// of the answer. The test needs a session with a rendered video: the end to
// end script (`scripts/e2e-jam.mjs`) leaves one behind with the user
// "e2e".
func TestJjamLiveVideo(t *testing.T) {
	jjamLive(t)
	file, size, err := jjamFetchVideo("/api/jam/video?userId=e2e")
	if err != nil {
		t.Skipf("there is no rendered video on the service: %v", err)
	}
	defer func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}()
	if size <= 0 {
		t.Fatal("the video of the render is empty")
	}
	// The file has to be a real MP4, so it starts with the box of one.
	head := make([]byte, 12)
	if _, err := file.Read(head); err != nil {
		t.Fatalf("the video of the render is not readable: %v", err)
	}
	if !bytes.Contains(head, []byte("ftyp")) {
		t.Fatalf("the video of the render is not an MP4 file: %q", head)
	}
}

// TestMain keeps the output of the test short.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
