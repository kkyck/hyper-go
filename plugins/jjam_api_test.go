package main

// ─── jjam_api_test.go — the way the bot talks to the service ───────────
//
// The video of a render does not travel through the JSON of the answer: it
// comes on its own route, so the answer stays small even when the video is
// tens of megabytes. These tests check that the bot reads the answer of a
// render and then fetches the video, and that it notices an answer that is
// too big to read.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestJjamRequestTooBig checks that a too big answer is noticed.
//
// A JSON that is cut in half cannot be read at all, so the bot has to say
// that the answer was too big instead of reading half of it.
func TestJjamRequestTooBig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat("x", 64*1024)
		fmt.Fprint(w, `{"ok":true,"data":"`)
		for i := 0; i < (jjamMaxAnswer/len(chunk))+16; i++ {
			fmt.Fprint(w, chunk)
		}
		fmt.Fprint(w, `"}`)
	}))
	defer server.Close()
	t.Setenv("AMWEB_API", server.URL)

	_, err := jjamRequest(http.MethodGet, "/big", nil)
	if err == nil {
		t.Fatal("a too big answer was read as if it were complete")
	}
	if !strings.Contains(err.Error(), "terlalu besar") {
		t.Fatalf("the error does not say that the answer was too big: %v", err)
	}
}

// TestJjamRequestSmall checks that a normal answer is read.
func TestJjamRequestSmall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"state":"done","filename":"jjam.mp4"}`)
	}))
	defer server.Close()
	t.Setenv("AMWEB_API", server.URL)

	answer, err := jjamRequest(http.MethodGet, "/small", nil)
	if err != nil {
		t.Fatalf("the answer was not read: %v", err)
	}
	if jjamString(answer, "state") != "done" {
		t.Fatalf("the answer is %v, want the state done", answer)
	}
}

// TestJjamVideoRoute checks where the bot looks for the video.
func TestJjamVideoRoute(t *testing.T) {
	answer := map[string]any{"url": "/api/jam/video?userId=u1"}
	if got := jjamVideoRoute(answer, "u1"); got != "/api/jam/video?userId=u1" {
		t.Fatalf("jjamVideoRoute = %q, want the route of the answer", got)
	}
	empty := map[string]any{}
	want := "/api/jam/video?userId=u1"
	if got := jjamVideoRoute(empty, "u1"); got != want {
		t.Fatalf("jjamVideoRoute = %q, want %q", got, want)
	}
}

// TestJjamFetchVideo checks that the video of a render is fetched as bytes.
func TestJjamFetchVideo(t *testing.T) {
	payload := []byte("\x00\x01\x02the video\xff")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/jam/video" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("userId") != "u1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	t.Setenv("AMWEB_API", server.URL)

	file, size, err := jjamFetchVideo("/api/jam/video?userId=u1")
	if err != nil {
		t.Fatalf("the video was not fetched: %v", err)
	}
	defer func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}()
	if size != int64(len(payload)) {
		t.Fatalf("the video is %d bytes, want %d", size, len(payload))
	}
	got, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatalf("the file of the video is not readable: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("the video is %q, want %q", got, payload)
	}
}

// TestJjamFetchVideoMissing checks that a missing video is an error.
func TestJjamFetchVideoMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"error":"No video has been rendered yet."}`)
	}))
	defer server.Close()
	t.Setenv("AMWEB_API", server.URL)

	if _, _, err := jjamFetchVideo("/api/jam/video?userId=u1"); err == nil {
		t.Fatal("a missing video was not an error")
	}
}
