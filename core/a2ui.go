package main

// a2ui.go — A2UI Widget Builder + Interactive Message Sender
//
// Port dari lib/a2ui.js (Baileys) ke Go/whatsmeow.
// Menggunakan WhatsApp socket (relayMessage) dengan additionalNodes biz/interactive.
//
// A2UI adalah framework widget baru WhatsApp (im_a2ui) dengan bloksWidget
// yang berisi JSON spec components (Text, Image, Button, Card, Column, Row, dll).
//
// Commands:
//   .a2ui                  → bantuan + contoh
//   .a2ui demo             → kirim demo widget (text + image + button)
//   .a2ui text <teks>      → kirim widget text sederhana
//   .a2ui buttons          → kirim widget dengan tombol CTA
//
// Owner only.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/polymorfa/hypermeow"
	waBinary "github.com/polymorfa/hypermeow/binary"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── A2UI Builder ─────────────────────────────────────────────────

// A2UIBuilder builds an A2UI widget spec.
type A2UIBuilder struct {
	version      string
	catalogID    string
	components   []map[string]interface{}
	rootChildren []string
	counter      int
}

// NewA2UIBuilder creates a new A2UI builder with default catalog.
func NewA2UIBuilder() *A2UIBuilder {
	return &A2UIBuilder{
		version:   "v0.9",
		catalogID: getenvDefault("A2UI_CATALOG_URL", "https://a2ui.org/specification/v0_9/catalogs/basic/catalog.json"),
	}
}

func (b *A2UIBuilder) nextID(prefix string) string {
	b.counter++
	return fmt.Sprintf("%s_%s", prefix, fmt.Sprintf("%x", b.counter))
}

// Text adds a Text component.
func (b *A2UIBuilder) Text(text string, variant string) string {
	id := b.nextID("text")
	if variant == "" {
		variant = "body"
	}
	c := map[string]interface{}{
		"id":        id,
		"component": "Text",
		"text":      text,
		"variant":   variant,
	}
	b.components = append(b.components, c)
	return id
}

// Image adds an Image component.
func (b *A2UIBuilder) Image(url string, fit string) string {
	id := b.nextID("image")
	if fit == "" {
		fit = "cover"
	}
	c := map[string]interface{}{
		"id":        id,
		"component": "Image",
		"url":       url,
		"fit":       fit,
	}
	b.components = append(b.components, c)
	return id
}

// Button adds a Button component.
// childID must be the ID of a child component (e.g. from Text()).
func (b *A2UIBuilder) Button(childID string, variant string, action map[string]interface{}) string {
	id := b.nextID("button")
	if variant == "" {
		variant = "primary"
	}
	c := map[string]interface{}{
		"id":        id,
		"component": "Button",
		"child":     childID,
		"variant":   variant,
	}
	if action != nil {
		c["action"] = action
	}
	b.components = append(b.components, c)
	return id
}

// Card adds a Card component wrapping a child.
func (b *A2UIBuilder) Card(childID string) string {
	id := b.nextID("card")
	c := map[string]interface{}{
		"id":        id,
		"component": "Card",
		"child":     childID,
	}
	b.components = append(b.components, c)
	return id
}

// Column adds a Column component with children.
func (b *A2UIBuilder) Column(children []string) string {
	id := b.nextID("column")
	c := map[string]interface{}{
		"id":        id,
		"component": "Column",
		"children":  children,
	}
	b.components = append(b.components, c)
	return id
}

// Row adds a Row component with children.
func (b *A2UIBuilder) Row(children []string) string {
	id := b.nextID("row")
	c := map[string]interface{}{
		"id":        id,
		"component": "Row",
		"children":  children,
	}
	b.components = append(b.components, c)
	return id
}

// Divider adds a Divider component.
func (b *A2UIBuilder) Divider() string {
	id := b.nextID("divider")
	c := map[string]interface{}{
		"id":        id,
		"component": "Divider",
	}
	b.components = append(b.components, c)
	return id
}

// Root sets the root children (top-level component IDs).
func (b *A2UIBuilder) Root(children []string) *A2UIBuilder {
	b.rootChildren = children
	return b
}

// Build constructs the bloksWidget data (JSON string).
// Returns: uuid, data (JSON string), type.
func (b *A2UIBuilder) Build() (uuid, data, widgetType string) {
	uuidBytes := make([]byte, 16)
	_, _ = rand.Read(uuidBytes)
	// Format as UUID v4
	uuid = fmt.Sprintf("%x-%x-%x-%x-%x",
		uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:16])

	if len(b.rootChildren) == 0 {
		// Default root = first component
		if len(b.components) > 0 {
			b.rootChildren = []string{b.components[0]["id"].(string)}
		}
	}

	root := map[string]interface{}{
		"id":        "root",
		"component": "Column",
		"children":  b.rootChildren,
	}
	allComponents := append([]map[string]interface{}{root}, b.components...)

	spec := map[string]interface{}{
		"version": b.version,
		"createSurface": map[string]interface{}{
			"surfaceId":  fmt.Sprintf("starcore-widget=%s", uuid),
			"catalogId":  b.catalogID,
			"components": allComponents,
		},
	}

	jsonBytes, _ := json.Marshal(spec)
	data = string(jsonBytes)
	widgetType = "im_a2ui"
	return
}

// ─── Send A2UI Widget via WhatsApp socket ────────────────────────

// a2uiButton represents a CTA button for nativeFlowMessage.
type a2uiButton struct {
	Name   string                 `json:"name"`
	Params map[string]interface{} `json:"params"`
}

// sendA2UIWidget sends an A2UI widget via relayMessage with additionalNodes.
// This matches the Baileys sendA2UIWidget pattern (biz/interactive/native_flow nodes).
func sendA2UIWidget(client *whatsmeow.Client, chat types.JID, builder *A2UIBuilder, bodyText, footerText string, buttons []a2uiButton, quoted *events.Message) error {
	// Build bloksWidget
	uuid, data, widgetType := builder.Build()

	// Build nativeFlowMessage buttons
	var nativeButtons []*waE2E.InteractiveMessage_NativeFlowMessage_NativeFlowButton
	for _, btn := range buttons {
		paramsJSON := "{}"
		if btn.Params != nil {
			if b, err := json.Marshal(btn.Params); err == nil {
				paramsJSON = string(b)
			}
		}
		name := btn.Name
		if name == "" {
			name = "cta_url"
		}
		nativeButtons = append(nativeButtons, &waE2E.InteractiveMessage_NativeFlowMessage_NativeFlowButton{
			Name:             proto.String(name),
			ButtonParamsJSON: proto.String(paramsJSON),
		})
	}

	paramsJSON := proto.String("{}")
	if len(nativeButtons) == 0 {
		paramsJSON = proto.String("")
	}
	version := proto.Int32(1)

	nativeFlow := &waE2E.InteractiveMessage_NativeFlowMessage{
		Buttons:           nativeButtons,
		MessageParamsJSON: paramsJSON,
		MessageVersion:    version,
	}

	// Build interactiveMessage
	interactive := &waE2E.InteractiveMessage{
		InteractiveMessage: &waE2E.InteractiveMessage_NativeFlowMessage_{
			NativeFlowMessage: nativeFlow,
		},
		BloksWidget: &waE2E.InteractiveMessage_BloksWidget{
			Uuid: proto.String(uuid),
			Data: proto.String(data),
			Type: proto.String(widgetType),
		},
	}
	if bodyText != "" {
		interactive.Body = &waE2E.InteractiveMessage_Body{
			Text: proto.String(bodyText),
		}
	}
	if footerText != "" {
		interactive.Footer = &waE2E.InteractiveMessage_Footer{
			Text: proto.String(footerText),
		}
		interactive.Header = &waE2E.InteractiveMessage_Header{
			HasMediaAttachment: proto.Bool(false),
		}
	}

	// Build full message with messageContextInfo
	secretBytes := make([]byte, 32)
	_, _ = rand.Read(secretBytes)

	msg := &waE2E.Message{
		MessageContextInfo: &waE2E.MessageContextInfo{
			MessageSecret: secretBytes,
		},
		InteractiveMessage: interactive,
	}

	// Build additionalNodes (biz/interactive/native_flow) — same as Baileys a2ui.js
	ts := fmt.Sprintf("%d", time.Now().Unix())
	decisionID := strings.ReplaceAll(generateUUID(), "-", "")

	additionalNodes := waBinary.Node{
		Tag: "biz",
		Attrs: waBinary.Attrs{
			"actual_actors":   "2",
			"host_storage":    "2",
			"privacy_mode_ts": ts,
		},
		Content: []waBinary.Node{
			{
				Tag: "interactive",
				Attrs: waBinary.Attrs{
					"type": "native_flow",
					"v":    "1",
				},
				Content: []waBinary.Node{
					{
						Tag: "native_flow",
						Attrs: waBinary.Attrs{
							"v":    "9",
							"name": "mixed",
						},
					},
				},
			},
			{
				Tag: "quality_control",
				Attrs: waBinary.Attrs{
					"decision_id": decisionID,
					"source_type": "third_party",
				},
				Content: []waBinary.Node{
					{
						Tag: "decision_source",
						Attrs: waBinary.Attrs{
							"value": "df",
						},
					},
				},
			},
		},
	}

	// Send via whatsmeow with additionalNodes
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use SendMessage with SendRequestExtra for additionalNodes
	additionalNodesSlice := []waBinary.Node{additionalNodes}
	_, err := client.SendMessage(ctx, chat, msg, whatsmeow.SendRequestExtra{
		AdditionalNodes: &additionalNodesSlice,
	})
	return err
}

// generateUUID returns a random UUID v4 string.
func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ─── Command handler ─────────────────────────────────────────────

func handleA2UICommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}

	input := strings.TrimSpace(args)
	lower := strings.ToLower(input)

	if input == "" || lower == "help" || lower == "bantuan" {
		return a2uiHelp()
	}

	// .a2ui demo
	if lower == "demo" {
		builder := NewA2UIBuilder()
		// Build a demo widget: Column with [Card(Text+Button), Divider, Text]
		titleID := builder.Text("🎵 Now Playing", "title")
		subID := builder.Text("Lofi Hip Hop — Chill Beats", "body")
		colID := builder.Column([]string{titleID, subID})
		btnID := builder.Button(titleID, "primary", map[string]interface{}{
			"type": "cta_url",
			"url":  "https://example.com/play",
		})
		cardID := builder.Card(btnID)
		divID := builder.Divider()
		footID := builder.Text("Powered by A2UI Widget", "caption")
		builder.Root([]string{colID, cardID, divID, footID})

		buttons := []a2uiButton{
			{Name: "cta_url", Params: map[string]interface{}{"url": "https://example.com"}},
		}

		err := sendA2UIWidget(client, msg.Info.Chat, builder, "A2UI Demo Widget", "Hiura Bot", buttons, msg)
		if err != nil {
			return fmt.Sprintf("❌ Gagal kirim widget: %v", err)
		}
		return ""
	}

	// .a2ui text <teks>
	if strings.HasPrefix(lower, "text ") {
		text := strings.TrimSpace(input[5:])
		if text == "" {
			return "❓ Masukkan teks. Contoh: .a2ui text Halo dunia"
		}
		builder := NewA2UIBuilder()
		textID := builder.Text(text, "body")
		builder.Root([]string{textID})

		err := sendA2UIWidget(client, msg.Info.Chat, builder, text, "", nil, msg)
		if err != nil {
			return fmt.Sprintf("❌ Gagal kirim widget: %v", err)
		}
		return ""
	}

	// .a2ui buttons
	if lower == "buttons" {
		builder := NewA2UIBuilder()
		titleID := builder.Text("Pilih Menu", "title")
		btn1TextID := builder.Text("🎵 Music", "body")
		btn2TextID := builder.Text("📺 Video", "body")
		btn1ID := builder.Button(btn1TextID, "primary", map[string]interface{}{
			"type": "cta_url",
			"url":  "https://example.com/music",
		})
		btn2ID := builder.Button(btn2TextID, "secondary", map[string]interface{}{
			"type": "cta_url",
			"url":  "https://example.com/video",
		})
		colID := builder.Column([]string{titleID, btn1ID, btn2ID})
		builder.Root([]string{colID})

		buttons := []a2uiButton{
			{Name: "cta_url", Params: map[string]interface{}{"url": "https://example.com"}},
			{Name: "cta_url", Params: map[string]interface{}{"url": "https://example.com/about"}},
		}

		err := sendA2UIWidget(client, msg.Info.Chat, builder, "Menu Interaktif", "Hiura Bot", buttons, msg)
		if err != nil {
			return fmt.Sprintf("❌ Gagal kirim widget: %v", err)
		}
		return ""
	}

	return a2uiHelp()
}

func a2uiHelp() string {
	return `🧩 *A2UI Widget* — Interactive WhatsApp Widget

Fitur WebSocket: kirim widget interaktif via relayMessage dengan additionalNodes biz/interactive/native_flow.

*Commands:*
• .a2ui demo → kirim demo widget (text + button + card)
• .a2ui text <teks> → widget text sederhana
• .a2ui buttons → widget dengan tombol CTA

*Components didukung:*
• Text (variant: title, body, caption)
• Image (url, fit: cover)
• Button (child, variant: primary/secondary, action)
• Card (child)
• Column (children[])
• Row (children[])
• Divider

Powered by A2UI spec v0.9`
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "a2ui",
		Aliases:     []string{"widget"},
		Description: "a2ui",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleA2UICommand(client, msg, args)
			// ─── Owner-only powerful ───────────────────────────────
		},
	})
}
