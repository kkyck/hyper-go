package main

// ─── jjam.go — JAM (Alight Motion) video editor on WhatsApp ──────────
//
// The bot does not render the videos itself: it forwards the messages to
// the AM Web service, which holds the importer, the engine and the
// renderer of the editor. The service is the very same code the web
// editor uses, so a video the bot sends looks like one the user exports
// from the timeline.
//
//   .jjam <link>      → load the template of a share link
//   .jjamstatus       → the progress of the session
//   .jjamrender       → render the video and send it
//   .jjamcancel       → forget the session
//
// The photos, videos and sounds the user sends afterwards fill the slots
// of the template. A number in the caption picks the slot.
//
// The address of the service comes from the environment variable
// AMWEB_API; the default is the local service of the box.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// jjamAPI is the address of the AM Web service.
func jjamAPI() string {
	if v := strings.TrimSpace(os.Getenv("AMWEB_API")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8787"
}

// jjamClient is the HTTP client of the service.
//
// The render of a long template takes a quarter of an hour, so the client
// waits longer than the usual timeouts of a web request.
var jjamClient = &http.Client{Timeout: 60 * time.Minute}

// jjamMaxMedia is the biggest file the bot forwards (100 MB).
const jjamMaxMedia = 100 << 20

// jjamMaxAnswer is the biggest answer of the service the bot reads.
//
// The answers of the service are small: the video of a render comes on its
// own route, so it never travels through the JSON.
const jjamMaxAnswer = 32 << 20

// jjamUserID is the key of the session of a user. Every user has his own
// session, also inside a group.
func jjamUserID(msg *events.Message) string {
	if msg == nil {
		return "unknown"
	}
	chat := msg.Info.Chat.String()
	sender := msg.Info.Sender.String()
	return chat + "|" + sender
}

/* ------------------------------------------------------------------ *
 * The service
 * ------------------------------------------------------------------ */

// jjamRequest sends one request to the service and decodes the answer.
func jjamRequest(method, path string, body map[string]any) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, jjamAPI()+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := jjamClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// One byte more than the limit, so a too big answer is noticed instead
	// of being cut in half: a cut JSON cannot be read at all.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, jjamMaxAnswer+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > jjamMaxAnswer {
		return nil, fmt.Errorf("jawaban layanan terlalu besar (lebih dari %d byte)", jjamMaxAnswer)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("jawaban layanan tidak valid: %w", err)
		}
	}
	return out, nil
}

// jjamText is the message of the service.
func jjamText(answer map[string]any) string {
	if answer == nil {
		return ""
	}
	if s, ok := answer["message"].(string); ok {
		return s
	}
	if s, ok := answer["error"].(string); ok {
		return s
	}
	return ""
}

// jjamState is the state of the session.
func jjamState(answer map[string]any) string {
	if answer == nil {
		return ""
	}
	if s, ok := answer["state"].(string); ok {
		return s
	}
	return ""
}

/* ------------------------------------------------------------------ *
 * The commands
 * ------------------------------------------------------------------ */

// handleJjam loads the template of a share link.
//
// The command also takes the name of one of the other commands, so
// `.jjam render` is the same as `.jjamrender`.
func handleJjam(client *whatsmeow.Client, msg *events.Message, args string) string {
	link := strings.TrimSpace(args)
	switch strings.ToLower(link) {
	case "render":
		return handleJjamRender(client, msg, args)
	case "status":
		return handleJjamStatus(client, msg, args)
	case "cancel":
		return handleJjamCancel(client, msg, args)
	case "help", "?":
		return jjamHelp()
	}
	if link == "" {
		return jjamHelp()
	}
	answer, err := jjamRequest(http.MethodPost, "/api/jam/start", map[string]any{
		"userId": jjamUserID(msg),
		"link":   link,
	})
	if err != nil {
		return fmt.Sprintf("❌ Gagal menghubungi layanan JAM: %v", err)
	}
	if ok, _ := answer["ok"].(bool); !ok {
		msg := jjamText(answer)
		if msg == "" {
			msg = "Permintaan ditolak."
		}
		return "❌ " + msg
	}
	jjamCacheSet(jjamUserID(msg), "awaiting_media")
	return jjamText(answer)
}

// jjamHelp is the help of the JAM commands.
func jjamHelp() string {
	return "🎬 *JAM* — kirim link template Alight Motion:\n" +
		"`.jjam <link>`\n\n" +
		"Contoh: `.jjam <link share template>`\n\n" +
		"Perintah lain:\n" +
		"`.jjam render` — render videonya (sama dengan `.jjamrender`)\n" +
		"`.jjam status` — lihat progres (sama dengan `.jjamstatus`)\n" +
		"`.jjam cancel` — batalin sesi (sama dengan `.jjamcancel`)\n\n" +
		"Setelah template-nya kebaca, kirim fotonya atau videonya satu per satu ya.\n" +
		"Kalau mau slot tertentu, tulis nomornya di caption."
}

// handleJjamStatus tells the user where the session stands.
func handleJjamStatus(client *whatsmeow.Client, msg *events.Message, args string) string {
	answer, err := jjamRequest(http.MethodGet, "/api/jam/status?userId="+url.QueryEscape(jjamUserID(msg)), nil)
	if err != nil {
		return fmt.Sprintf("❌ Gagal menghubungi layanan JAM: %v", err)
	}
	state := jjamState(answer)
	if state == "" || state == "idle" {
		return "🎬 Belum ada sesi. Kirim `.jjam <link>` dulu ya."
	}
	// How far the render has come.
	if percent := jjamRenderPercent(answer); percent >= 0 {
		return fmt.Sprintf("⏳ Render jalan: %d%%", percent)
	}
	// Which media are still missing.
	missing := jjamMissing(answer)
	if len(missing) == 0 {
		return "🎬 Semua media sudah lengkap. Kirim `.jjam render` ya."
	}
	lines := []string{fmt.Sprintf("🎬 Masih kurang %d media:", len(missing))}
	for _, m := range missing {
		lines = append(lines, fmt.Sprintf("%d. %s — %s", jjamSlotNumber(m), jjamSlotName(m), jjamSlotKind(m)))
	}
	return strings.Join(lines, "\n")
}

// handleJjamCancel forgets the session.
func handleJjamCancel(client *whatsmeow.Client, msg *events.Message, args string) string {
	answer, err := jjamRequest(http.MethodPost, "/api/jam/command", map[string]any{
		"userId":  jjamUserID(msg),
		"command": "!cancel",
	})
	if err != nil {
		return fmt.Sprintf("❌ Gagal menghubungi layanan JAM: %v", err)
	}
	jjamCacheSet(jjamUserID(msg), "idle")
	text := jjamText(answer)
	if text == "" {
		text = "Sesi dibatalkan."
	}
	return "🗑️ " + text
}

// handleJjamRender renders the video. The rendering takes a while, so it
// runs in the background and the video is sent when it is ready.
//
// The media does not have to be complete. The service warns the user once
// about the media that is missing, and the render goes on when the command
// comes again.
func handleJjamRender(client *whatsmeow.Client, msg *events.Message, args string) string {
	userID := jjamUserID(msg)
	answer, err := jjamRequest(http.MethodGet, "/api/jam/status?userId="+url.QueryEscape(userID), nil)
	if err != nil {
		return fmt.Sprintf("❌ Gagal menghubungi layanan JAM: %v", err)
	}
	if state := jjamState(answer); state == "idle" || state == "" {
		return "Belum ada template yang dimuat. Kirim `.jjam <link>` dulu ya."
	}
	// The service starts the render and says what it did with it. When the
	// media is not complete, it warns the user instead of rendering.
	answer, err = jjamRequest(http.MethodPost, "/api/jam/command", map[string]any{
		"userId":  userID,
		"command": "!render",
	})
	if err != nil {
		return fmt.Sprintf("❌ Gagal menghubungi layanan JAM: %v", err)
	}
	started, text := jjamRenderReply(answer)
	if !started {
		// The render did not start: the service warns about the media that
		// is missing. The render goes on when the command comes again.
		if text == "" {
			text = "Render belum jalan."
		}
		return text
	}
	chat := msg.Info.Chat
	jjamStartRender(client, chat, userID)
	if text == "" {
		text = "⏳ Render jalan ya, tunggu sebentar…\nVideo bakal dikirim otomatis setelah selesai."
	}
	return text
}

// jjamRenderReply says whether the render started, and what the service
// answered.
//
// The service warns the user when the media is not complete. Only then the
// render does not start, and the render goes on when the command comes
// again.
func jjamRenderReply(answer map[string]any) (bool, string) {
	text := jjamText(answer)
	// The service says whether the command started a render. The state is
	// only a fallback, for an older service.
	if rendered, ok := answer["rendered"].(bool); ok {
		return rendered || jjamState(answer) == "rendering", text
	}
	return jjamState(answer) == "rendering", text
}

// jjamMissing reads the slots that still wait for a file.
func jjamMissing(answer map[string]any) []map[string]any {
	if answer == nil {
		return nil
	}
	progress, ok := answer["progress"].(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := progress["missing"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if slot, ok := item.(map[string]any); ok {
			out = append(out, slot)
		}
	}
	return out
}

// jjamSlotName is the name of a slot of the answer.
func jjamSlotName(slot map[string]any) string {
	name, _ := slot["name"].(string)
	if name == "" {
		return "(tanpa nama)"
	}
	return name
}

// jjamSlotNumber is the number of a slot of the answer.
//
// The number is the place of the slot among the slots the user has to
// supply, so it is the number the user writes in the caption of a file to
// pick the slot it belongs to.
func jjamSlotNumber(slot map[string]any) int {
	number, ok := slot["number"].(float64)
	if !ok || number < 1 {
		return 0
	}
	return int(number)
}

// jjamSlotKind is the kind of a slot of the answer.
func jjamSlotKind(slot map[string]any) string {
	kind, _ := slot["kind"].(string)
	if kind == "" {
		return "media"
	}
	return kind
}

// jjamRenderAndSend renders the video and sends it to the chat.
//
// The render of a long template takes minutes. The chat stays quiet: at
// most one message tells the user that the render is going on, and that
// message is deleted when the video arrives. The chat keeps the video and
// nothing else.
func jjamRenderAndSend(client *whatsmeow.Client, chat types.JID, userID string) {
	// The progress of the render is told while it runs.
	done := make(chan struct{})
	progress := &jjamProgress{client: client, chat: chat}
	go jjamWatchProgress(client, chat, userID, done, progress)

	answer, err := jjamRequest(http.MethodPost, "/api/jam/render", map[string]any{
		"userId": userID,
		// The render already runs, so this only waits for it. A second
		// render would be a second video of the same template.
		"waitOnly": true,
	})
	close(done)
	if err != nil {
		progress.remove()
		jjamSendText(client, chat, fmt.Sprintf("❌ Render gagal: %v", err))
		return
	}
	if ok, _ := answer["ok"].(bool); !ok {
		progress.remove()
		text := jjamText(answer)
		if text == "" {
			text = "Render gagal."
		}
		jjamSendText(client, chat, "❌ "+text)
		return
	}
	// The video comes on its own route, so it does not have to travel
	// through the JSON of the answer.
	route := jjamVideoRoute(answer, userID)
	file, size, err := jjamFetchVideo(route)
	if err != nil {
		progress.remove()
		jjamSendText(client, chat, fmt.Sprintf("❌ Video hasil render gagal diambil: %v", err))
		return
	}
	defer func() {
		name := file.Name()
		_ = file.Close()
		_ = os.Remove(name)
	}()
	mime := jjamString(answer, "mimeType")
	if mime == "" {
		mime = "video/mp4"
	}
	name := jjamString(answer, "filename")
	if name == "" {
		name = "jjam.mp4"
	}
	if err := jjamSendVideoFile(client, chat, file, size, mime, "🎬 Video JAM kamu", name); err != nil {
		progress.remove()
		jjamSendText(client, chat, fmt.Sprintf("❌ Gagal mengirim video: %v", err))
		return
	}
	// The video is in the chat, so the message about the progress is not
	// needed anymore.
	progress.remove()
}

// jjamVideoRoute is the route of the video of a render.
//
// The answer of a render says where the video is. When it does not, the
// route of the video of the session is used.
func jjamVideoRoute(answer map[string]any, userID string) string {
	if route := jjamString(answer, "url"); route != "" {
		return route
	}
	return "/api/jam/video?userId=" + url.QueryEscape(userID)
}

// jjamFetchVideo downloads the video of a render into a temporary file.
//
// The video of a long template is tens of megabytes, so it is written on
// the disk instead of the memory of the bot. The file is returned open and
// positioned at its first byte.
func jjamFetchVideo(route string) (*os.File, int64, error) {
	req, err := http.NewRequest(http.MethodGet, jjamAPI()+route, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := jjamClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, 0, fmt.Errorf("layanan menjawab %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	file, err := os.CreateTemp("", "jjam-video-*.mp4")
	if err != nil {
		return nil, 0, err
	}
	size, err := io.Copy(file, resp.Body)
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, 0, err
	}
	if size == 0 {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, 0, fmt.Errorf("video hasil render kosong")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, 0, err
	}
	return file, size, nil
}

// jjamWatchProgress tells the user how far the render has come.
//
// The chat stays quiet: the message comes once, in the middle of the
// render, and only when the render takes a while. The id of the message
// comes back, so the message can be deleted when the video is sent.
func jjamWatchProgress(client *whatsmeow.Client, chat types.JID, userID string, done <-chan struct{}, progress *jjamProgress) {
	started := time.Now()
	for {
		select {
		case <-done:
			return
		case <-time.After(jjamProgressPoll):
		}
		answer, err := jjamRequest(http.MethodGet, "/api/jam/status?userId="+url.QueryEscape(userID), nil)
		if err != nil {
			continue
		}
		// The render is over. There is nothing left to tell.
		if jjamRenderOver(answer) {
			return
		}
		percent := jjamRenderPercent(answer)
		// The time the render has run comes from the service, so the delay
		// is counted from the start of the render and not from the start
		// of the watcher.
		elapsed := time.Since(started)
		if running := jjamRenderElapsed(answer); running > 0 {
			elapsed = running
		}
		if !jjamProgressDue(percent, elapsed) {
			continue
		}
		// The render may be over now. Nothing is said after the video.
		select {
		case <-done:
			return
		default:
		}
		// One message is enough. The rest of the render is quiet.
		progress.add(jjamSendText(client, chat,
			fmt.Sprintf("⏳ Render %d%% selesai, videonya menyusul ya…", percent)))
		return
	}
}

// jjamRenderOver says whether the render of an answer is over.
//
// A render that is done or that failed does not run anymore, so the bot
// does not keep asking about it.
func jjamRenderOver(answer map[string]any) bool {
	switch jjamState(answer) {
	case "done", "idle", "error":
		return true
	case "awaiting_media":
		// The session waits for media again. That is the end of a render
		// that failed, but it is also the state before a render starts.
		return jjamString(answer, "error") != ""
	}
	return false
}

// jjamProgressStep is how much of the render has to be done before the
// user is told about it.
const jjamProgressStep = 50

// jjamProgressDelay is how long the render has to run before the user gets
// a message about it. A render that is quick does not need one.
var jjamProgressDelay = 2 * time.Minute

// jjamProgressPoll is the time between two checks of the progress.
var jjamProgressPoll = 20 * time.Second

// jjamProgressDue says whether the user has to be told about the render.
//
// The message comes once, in the middle of the render, and only when the
// render takes a while. A short render does not get a message at all.
func jjamProgressDue(percent int, elapsed time.Duration) bool {
	if percent < jjamProgressStep {
		return false
	}
	return elapsed >= jjamProgressDelay
}

// jjamProgress keeps the messages of the progress, so they can be deleted
// when the video is sent.
type jjamProgress struct {
	mu     sync.Mutex
	ids    []types.MessageID
	closed bool
	client *whatsmeow.Client
	chat   types.JID
}

// add remembers a message of the progress.
//
// A message that comes in after the progress was closed is deleted at
// once, so it does not stay in the chat.
func (p *jjamProgress) add(id types.MessageID) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.revoke(id)
		return
	}
	p.ids = append(p.ids, id)
	p.mu.Unlock()
}

// remove deletes the messages of the progress.
//
// The chat keeps the video and nothing else, so it does not fill up with
// the messages of the render.
func (p *jjamProgress) remove() {
	if p == nil || p.client == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	ids := p.ids
	p.ids = nil
	p.mu.Unlock()
	for _, id := range ids {
		p.revoke(id)
	}
}

// revoke deletes one message of the progress.
func (p *jjamProgress) revoke(id types.MessageID) {
	if p.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = p.client.RevokeMessage(ctx, p.chat, id)
}

// jjamRenderPercent reads the percentage of the render of an answer.
//
// It returns -1 when the render is not running.
func jjamRenderPercent(answer map[string]any) int {
	// The progress only means something while the render runs.
	if state := jjamState(answer); state != "rendering" {
		return -1
	}
	render, ok := answer["render"].(map[string]any)
	if !ok {
		return -1
	}
	progress, ok := render["progress"].(float64)
	if !ok {
		return -1
	}
	percent := int(progress*100 + 0.5)
	if percent < 0 {
		return 0
	}
	if percent > 100 {
		return 100
	}
	return percent
}

// jjamRenderElapsed reads how long the render of an answer has run.
//
// The service counts the time of the render itself, so the bot does not
// have to count it. It returns 0 when the service does not say.
func jjamRenderElapsed(answer map[string]any) time.Duration {
	render, ok := answer["render"].(map[string]any)
	if !ok {
		return 0
	}
	seconds, ok := render["elapsed"].(float64)
	if !ok || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// jjamString reads a string of the answer.
func jjamString(answer map[string]any, key string) string {
	if answer == nil {
		return ""
	}
	if s, ok := answer[key].(string); ok {
		return s
	}
	return ""
}

/* ------------------------------------------------------------------ *
 * The media
 * ------------------------------------------------------------------ */

// jjamMedia picks the media of a message and its name.
//
// The kind of the media is read from the message itself, so a video that
// comes as a document is still a video.
func jjamMedia(msg *events.Message) (whatsmeow.DownloadableMessage, string, string, bool) {
	if msg == nil || msg.Message == nil {
		return nil, "", "", false
	}
	if img := msg.Message.GetImageMessage(); img != nil {
		return img, img.GetCaption(), "image/jpeg", true
	}
	if vid := msg.Message.GetVideoMessage(); vid != nil {
		mime := strings.TrimSpace(vid.GetMimetype())
		if mime == "" {
			mime = "video/mp4"
		}
		return vid, vid.GetCaption(), mime, true
	}
	if aud := msg.Message.GetAudioMessage(); aud != nil {
		mime := strings.TrimSpace(aud.GetMimetype())
		if mime == "" {
			mime = "audio/mpeg"
		}
		return aud, "", mime, true
	}
	if doc := msg.Message.GetDocumentMessage(); doc != nil {
		mime := strings.TrimSpace(doc.GetMimetype())
		if mime == "" || mime == "application/octet-stream" {
			// The type is looked at when the file is downloaded.
			mime = ""
		}
		return doc, doc.GetCaption(), mime, true
	}
	return nil, "", "", false
}

// jjamSniffMime looks at the first bytes of a file and tells what it is.
//
// WhatsApp does not always say what kind of file it sends, and a video
// that comes as a document often has the type "application/octet-stream".
func jjamSniffMime(raw []byte) string {
	if len(raw) < 4 {
		return ""
	}
	switch {
	case bytes.HasPrefix(raw, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(raw, []byte{0x89, 'P', 'N', 'G'}):
		return "image/png"
	case bytes.HasPrefix(raw, []byte("GIF8")):
		return "image/gif"
	case len(raw) >= 12 && bytes.HasPrefix(raw, []byte("RIFF")) && string(raw[8:12]) == "WEBP":
		return "image/webp"
	case bytes.HasPrefix(raw, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return "video/x-matroska"
	case len(raw) >= 12 && string(raw[4:8]) == "ftyp":
		switch string(raw[8:12]) {
		case "qt  ":
			return "video/quicktime"
		case "M4A ":
			return "audio/mp4"
		case "3gp4", "3gp5", "3g2a":
			return "video/3gpp"
		}
		return "video/mp4"
	case bytes.HasPrefix(raw, []byte("ID3")),
		bytes.HasPrefix(raw, []byte{0xff, 0xfb}),
		bytes.HasPrefix(raw, []byte{0xff, 0xf3}),
		bytes.HasPrefix(raw, []byte{0xff, 0xf2}):
		return "audio/mpeg"
	case bytes.HasPrefix(raw, []byte("OggS")):
		return "audio/ogg"
	}
	return ""
}

// jjamExt is the file name of a media type.
//
// The extension matters, because the service reads the kind of the file
// from it.
func jjamExt(mime string) string {
	switch {
	case mime == "image/jpeg":
		return ".jpg"
	case mime == "image/png":
		return ".png"
	case mime == "image/webp":
		return ".webp"
	case mime == "image/gif":
		return ".gif"
	case mime == "video/mp4":
		return ".mp4"
	case mime == "video/3gpp":
		return ".3gp"
	case mime == "video/quicktime":
		return ".mov"
	case mime == "video/x-matroska":
		return ".mkv"
	case mime == "audio/mpeg":
		return ".mp3"
	case mime == "audio/mp4":
		return ".m4a"
	case mime == "audio/aac":
		return ".aac"
	case mime == "audio/ogg":
		return ".ogg"
	}
	return ""
}

// jjamFileName is the name of the file of a media message.
//
// The name of a document is kept, but its extension follows the kind of
// the media: a video that comes as a document named "VID_001.dat" is
// still a video, and the service reads the kind of the file from the
// name when the type of the message is not usable.
func jjamFileName(msg *events.Message, mime string) string {
	ext := jjamExt(mime)
	base := ""
	if msg != nil && msg.Message != nil {
		if doc := msg.Message.GetDocumentMessage(); doc != nil {
			base = doc.GetFileName()
		}
	}
	if base == "" {
		switch {
		case strings.HasPrefix(mime, "image/"):
			return "photo" + ext
		case strings.HasPrefix(mime, "video/"):
			return "video" + ext
		case strings.HasPrefix(mime, "audio/"):
			return "audio" + ext
		}
		return "file.bin"
	}
	if ext == "" {
		return base
	}
	// The extension of the name is replaced by the one of the media.
	if dot := strings.LastIndex(base, "."); dot > 0 {
		base = base[:dot]
	}
	return base + ext
}

// jjamHandleMedia forwards the media of a message to the service, when
// the user has a session that waits for media.
//
// It returns true when the message was the media of a session.
func jjamHandleMedia(client *whatsmeow.Client, msg *events.Message, text string) bool {
	if msg == nil || msg.Message == nil {
		return false
	}
	// A command is not media.
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, ".") || strings.HasPrefix(trimmed, "!") || strings.HasPrefix(trimmed, "$") {
		return false
	}
	downloadable, caption, mime, ok := jjamMedia(msg)
	if !ok {
		return false
	}
	userID := jjamUserID(msg)
	if !jjamCacheIsAwaiting(userID) {
		// The cache may be empty; ask the service once.
		answer, err := jjamRequest(http.MethodGet, "/api/jam/status?userId="+url.QueryEscape(userID), nil)
		if err != nil {
			return false
		}
		if jjamState(answer) != "awaiting_media" {
			jjamCacheSet(userID, jjamState(answer))
			return false
		}
		jjamCacheSet(userID, "awaiting_media")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw, err := client.Download(ctx, downloadable)
	if err != nil {
		jjamSendText(client, msg.Info.Chat, fmt.Sprintf("❌ Gagal mengunduh media: %v", err))
		return true
	}
	if len(raw) == 0 {
		return true
	}
	if len(raw) > jjamMaxMedia {
		jjamSendText(client, msg.Info.Chat, "❌ File-nya kebesaran (maksimal 100 MB).")
		return true
	}
	// WhatsApp does not always say what kind of file it is, so the first
	// bytes of the file decide when the message does not say it.
	if mime == "" {
		mime = jjamSniffMime(raw)
	}
	if mime == "" {
		jjamSendText(client, msg.Info.Chat, "❌ File-nya bukan foto, video atau audio yang bisa dipake.")
		return true
	}
	name := jjamFileName(msg, mime)
	answer, err := jjamRequest(http.MethodPost, "/api/jam/media", map[string]any{
		"userId":   userID,
		"filename": name,
		"mimeType": mime,
		"caption":  caption,
		"base64":   base64.StdEncoding.EncodeToString(raw),
	})
	if err != nil {
		jjamSendText(client, msg.Info.Chat, fmt.Sprintf("❌ Gagal mengirim media ke layanan JAM: %v", err))
		return true
	}
	text2 := jjamText(answer)
	if text2 == "" {
		text2 = "✅ Media diterima."
	}
	jjamSendText(client, msg.Info.Chat, text2)
	// The media of the template is complete: the service starts the render
	// and the video is sent when it is ready.
	if jjamState(answer) == "rendering" {
		jjamStartRender(client, msg.Info.Chat, userID)
	}
	return true
}

// jjamStartRender waits for the render of a user and sends the video.
//
// Only one watcher runs per user, so the video is not sent twice.
func jjamStartRender(client *whatsmeow.Client, chat types.JID, userID string) {
	jjamRenderMu.Lock()
	if jjamRenderers[userID] {
		jjamRenderMu.Unlock()
		return
	}
	jjamRenderers[userID] = true
	jjamRenderMu.Unlock()

	go func() {
		defer func() {
			jjamRenderMu.Lock()
			delete(jjamRenderers, userID)
			jjamRenderMu.Unlock()
		}()
		jjamRenderAndSend(client, chat, userID)
	}()
}

/* ------------------------------------------------------------------ *
 * The sending
 * ------------------------------------------------------------------ */

// jjamSendText sends a text from a goroutine.
//
// The id of the sent message comes back, so the message can be deleted
// later. An empty id means the message did not go out.
func jjamSendText(client *whatsmeow.Client, chat types.JID, text string) types.MessageID {
	if client == nil || text == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.SendMessage(ctx, chat, &waE2E.Message{
		Conversation: proto.String(text),
	})
	if err != nil {
		return ""
	}
	return resp.ID
}

// jjamSendVideoFile uploads a video from a file and sends it to the chat.
//
// The file is read while it is uploaded, so the bot does not have to hold
// the whole video in the memory.
func jjamSendVideoFile(client *whatsmeow.Client, chat types.JID, file *os.File, size int64, mime, caption, name string) error {
	err := jjamUploadMedia(client, chat, file, size, mime, caption, name, false)
	if err == nil {
		return nil
	}
	// WhatsApp does not take every video as a video. A file is bigger than
	// a video, so the video goes through that way.
	if err2 := jjamUploadMedia(client, chat, file, size, mime, caption, name, true); err2 != nil {
		return fmt.Errorf("%v (sebagai file juga gagal: %v)", err, err2)
	}
	return nil
}

// jjamUploadMedia uploads the file and sends it to the chat.
//
// The kind of the message follows the media: a video is sent as a video, a
// document as a file. The file is encrypted into a second file, so the
// video can be sent again when the first try fails.
func jjamUploadMedia(client *whatsmeow.Client, chat types.JID, file *os.File, size int64, mime, caption, name string, asDocument bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	appInfo := whatsmeow.MediaVideo
	if asDocument {
		appInfo = whatsmeow.MediaDocument
	}
	// The upload reads the file from its first byte.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	encrypted, err := os.CreateTemp("", "jjam-encrypted-*")
	if err != nil {
		return err
	}
	defer func() {
		temp := encrypted.Name()
		_ = encrypted.Close()
		_ = os.Remove(temp)
	}()

	up, err := client.UploadReader(ctx, file, encrypted, appInfo)
	if err != nil {
		return err
	}
	length := uint64(size)
	if up.FileLength > 0 {
		length = up.FileLength
	}
	out := &waE2E.Message{}
	if asDocument {
		out.DocumentMessage = &waE2E.DocumentMessage{
			URL:           proto.String(up.URL),
			FileSHA256:    up.FileSHA256,
			FileEncSHA256: up.FileEncSHA256,
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			DirectPath:    proto.String(up.DirectPath),
			FileLength:    proto.Uint64(length),
			FileName:      proto.String(name),
			Caption:       proto.String(caption),
		}
	} else {
		out.VideoMessage = &waE2E.VideoMessage{
			URL:           proto.String(up.URL),
			FileSHA256:    up.FileSHA256,
			FileEncSHA256: up.FileEncSHA256,
			MediaKey:      up.MediaKey,
			Mimetype:      proto.String(mime),
			DirectPath:    proto.String(up.DirectPath),
			FileLength:    proto.Uint64(length),
			Caption:       proto.String(caption),
		}
	}
	_, err = client.SendMessage(ctx, chat, out)
	return err
}

/* ------------------------------------------------------------------ *
 * The cache of the state
 * ------------------------------------------------------------------ */

type jjamSession struct {
	state string
	at    time.Time
}

var (
	jjamCacheMu sync.Mutex
	jjamCache   = map[string]jjamSession{}
)

// The renders that are watched, so the video of a user is sent only once.
var (
	jjamRenderMu  sync.Mutex
	jjamRenderers = map[string]bool{}
)

// jjamCacheSet remembers the state of a session.
func jjamCacheSet(userID, state string) {
	jjamCacheMu.Lock()
	defer jjamCacheMu.Unlock()
	jjamCache[userID] = jjamSession{state: state, at: time.Now()}
}

// jjamCacheIsAwaiting tells whether the session waits for media. The
// answer is used for a minute, so a normal conversation is not slowed
// down by a request to the service.
func jjamCacheIsAwaiting(userID string) bool {
	jjamCacheMu.Lock()
	defer jjamCacheMu.Unlock()
	s, ok := jjamCache[userID]
	if !ok {
		return false
	}
	if time.Since(s.at) > time.Minute {
		delete(jjamCache, userID)
		return false
	}
	return s.state == "awaiting_media"
}

/* ------------------------------------------------------------------ *
 * The registration
 * ------------------------------------------------------------------ */

func init() {
	RegisterPlugin(Plugin{
		Command:     "jjam",
		Aliases:     []string{"jjamstart"},
		Description: "Buat video dari template Alight Motion (butuh link share)",
		Category:    "Video",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleJjam(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "jjamstatus",
		Description: "Status sesi JAM (sama dengan .jjam status)",
		Category:    "Video",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleJjamStatus(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "jjamrender",
		Description: "Render video JAM (sama dengan .jjam render)",
		Category:    "Video",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleJjamRender(client, msg, args)
		},
	})
	RegisterPlugin(Plugin{
		Command:     "jjamcancel",
		Description: "Batalkan sesi JAM (sama dengan .jjam cancel)",
		Category:    "Video",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleJjamCancel(client, msg, args)
		},
	})
}
