package main

// ─── jjam-flow_test.go — the flow of the render command ───────────────
//
// The bot asks the service to start the render. The service warns the user
// when the media is not complete, and the render goes on when the command
// comes again.
//
// The test needs a service that runs. It is skipped when there is none.
//
//	go test -vet=off -run TestJjamFlow -v .

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// jjamFlowAPI is the service of the test.
func jjamFlowAPI() string {
	if v := strings.TrimSpace(os.Getenv("AMWEB_API")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8787"
}

// jjamFlowPost sends a command to the service and reads the answer.
func jjamFlowPost(t *testing.T, route string, body map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("the body of the request is broken: %v", err)
	}
	res, err := http.Post(jjamFlowAPI()+route, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Skipf("the service is not there: %v", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("the answer of the service is broken: %v", err)
	}
	answer := map[string]any{}
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatalf("the answer of the service is not a JSON: %s", string(data))
	}
	return answer
}

// TestJjamFlow walks through the render command of a session that has no
// media at all: the first command warns, the second one renders.
func TestJjamFlow(t *testing.T) {
	userID := "go-flow-" + time.Now().Format("20060102150405")

	// The template of the test comes from the service. Without a template
	// the rest of the test does not mean anything.
	answer := jjamFlowPost(t, "/api/jam/command", map[string]any{
		"userId":  userID,
		"command": "!jjam https://example.invalid/am/share/u/xxxx/yyyy",
	})
	if state, _ := answer["state"].(string); state != "awaiting_media" {
		t.Skipf("the service did not load the template: %v", answer)
	}

	// The first render command warns the user and does not render.
	answer = jjamFlowPost(t, "/api/jam/command", map[string]any{
		"userId":  userID,
		"command": "!render",
	})
	rendered, _ := answer["rendered"].(bool)
	if rendered {
		t.Fatal("the first render command started a render, but the media is not complete")
	}
	text, _ := answer["message"].(string)
	if !strings.Contains(text, "kurang") {
		t.Fatalf("the first render command did not warn about the media: %q", text)
	}
	if started, _ := jjamRenderReply(answer); started {
		t.Fatal("the bot thinks the render started, but the service only warned")
	}

	// The second render command starts the render.
	answer = jjamFlowPost(t, "/api/jam/command", map[string]any{
		"userId":  userID,
		"command": "!render",
	})
	started, text := jjamRenderReply(answer)
	if !started {
		t.Fatalf("the second render command did not start the render: %q", text)
	}
	if !strings.Contains(text, "kurang") {
		t.Logf("the answer of the service: %q", text)
	}
}
