package main

// msginspect.go — Deep message inspection + .q command + AI tools
// ════════════════════════════════════════════════════════════
//
// Commands:
//   .q [reply]         → Dump 100% message structure as document
//   .q -snip [reply]   → Dump as code snippet (inline)
//
// AI Tools:
//   inspect_message     → Get full structure of quoted message
//   relay_message       → Relay a message by ID from CRM cache
//   get_message_raw     → Get raw proto bytes (base64) of message
//
// Deep parsing: recursively walks the entire waE2E.Message proto,
// including protocolMessage, secretEncryptedMessage, all nested
// fields, binary data (as base64), timestamps, etc.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// ─── .q command ──────────────────────────────────────────────────

// handleQCommand — .q [reply] [-snip] [-file]
// Dumps 100% of the replied message's structure as detailed JSON.
// ─── Deep message inspector ──────────────────────────────────────

// deepInspectMessage recursively walks a waE2E.Message proto and
// extracts ALL fields into a nested map[string]interface{}.
// Binary fields are base64-encoded. Nil fields are skipped.
// Nested messages (protocolMessage, secretEncryptedMessage, etc.)
// are recursively inspected.
// ─── AI tools ────────────────────────────────────────────────────

// aiInspectMessage — AI tool: inspect quoted message structure
func aiInspectMessage(client *whatsmeow.Client, msg *events.Message, args string) string {
	quoted := getQuotedMessage(msg)
	if quoted == nil || quoted.Message == nil {
		return "Error: reply pesan dulu untuk inspect"
	}

	inspect := deepInspectMessage(quoted.Message, quoted.Info)
	jsonBytes, err := json.MarshalIndent(inspect, "", "  ")
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	result := string(jsonBytes)
	if len(result) > 3000 {
		result = result[:3000] + "\n... (truncated, use .q for full dump)"
	}

	return "🔍 Message structure:\n" + result
}

// aiRelayMessage — AI tool: relay a message by ID from CRM cache
func aiRelayMessage(client *whatsmeow.Client, msg *events.Message, args string) string {
	if args == "" {
		return "Error: message ID kosong"
	}

	chat := msg.Info.Chat.String()
	loaded := fioraCRMLoadMessage(args, chat)
	if loaded == nil {
		return "Error: pesan tidak ditemukan di CRM cache"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.SendMessage(ctx, msg.Info.Chat, loaded)
	if err != nil {
		return fmt.Sprintf("Error: relay gagal: %v", err)
	}
	return "✅ Pesan berhasil di-relay"
}

// aiGetMessageRaw — AI tool: get raw proto bytes (base64) of quoted message
func aiGetMessageRaw(client *whatsmeow.Client, msg *events.Message, args string) string {
	quoted := getQuotedMessage(msg)
	if quoted == nil || quoted.Message == nil {
		return "Error: reply pesan dulu"
	}

	data, err := proto.Marshal(quoted.Message)
	if err != nil {
		return fmt.Sprintf("Error: marshal: %v", err)
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	msgType := fioraCRMGetMessageType(quoted.Message)

	result := fmt.Sprintf("📦 Type: %s\nSize: %d bytes\nBase64: %s",
		msgType, len(data), b64[:min(200, len(b64))]+ternary(len(b64) > 200, "...", ""))

	return result
}

// ─── Unify .listmsg + .crm ──────────────────────────────────────

// handleUnifiedListMsg — .listmsg [group_jid] [user_phone]
// Now searches BOTH message_archive.json AND crmdb.db
// ─── Helpers ────────────────────────────────────────────────────

var _ = whatsmeow.MediaAudio
var _ = waE2E.Message{}

func deepInspectMessage(msg *waE2E.Message, info types.MessageInfo) map[string]interface{} {
	result := map[string]interface{}{}

	// Message info
	result["_info"] = map[string]interface{}{
		"id":         info.ID,
		"type":       info.Type,
		"push_name":  info.PushName,
		"timestamp":  info.Timestamp.Format(time.RFC3339),
		"category":   info.Category,
		"media_type": info.MediaType,
		"is_from_me": info.IsFromMe,
		"is_group":   info.IsGroup,
	}

	if msg == nil {
		result["_error"] = "message is nil"
		return result
	}

	// Walk all message types using reflection
	msgVal := reflect.ValueOf(msg).Elem()
	msgType := msgVal.Type()

	for i := 0; i < msgType.NumField(); i++ {
		field := msgType.Field(i)
		fieldVal := msgVal.Field(i)

		// Skip unexported fields
		if !field.IsExported() {
			continue
		}

		// Skip nil pointer fields
		if fieldVal.Kind() == reflect.Ptr && fieldVal.IsNil() {
			continue
		}

		// Skip nil slice/map fields
		if (fieldVal.Kind() == reflect.Slice || fieldVal.Kind() == reflect.Map) && fieldVal.IsNil() {
			continue
		}

		fieldName := field.Name
		jsonName := field.Tag.Get("json")
		if jsonName != "" {
			fieldName = strings.Split(jsonName, ",")[0]
		}

		result[fieldName] = deepInspectValue(fieldVal.Interface(), 0, 10)
	}

	return result
}

// deepInspectValue recursively inspects any value.
// Max depth prevents infinite recursion.

func deepInspectValue(v interface{}, depth, maxDepth int) interface{} {
	if depth > maxDepth {
		return "[max depth reached]"
	}

	if v == nil {
		return nil
	}

	val := reflect.ValueOf(v)

	// Handle pointers
	if val.Kind() == reflect.Ptr {
		if val.IsNil() {
			return nil
		}
		return deepInspectValue(val.Elem().Interface(), depth, maxDepth)
	}

	// Handle byte slices → base64
	if val.Kind() == reflect.Slice && val.Type().Elem().Kind() == reflect.Uint8 {
		bytes := val.Bytes()
		if len(bytes) == 0 {
			return nil
		}
		// If small, show as string too
		b64 := base64.StdEncoding.EncodeToString(bytes)
		return map[string]interface{}{
			"_type":  "bytes",
			"length": len(bytes),
			"base64": b64[:min(200, len(b64))] + ternary(len(b64) > 200, "...", ""),
			"hex":    fmt.Sprintf("%x", bytes[:min(32, len(bytes))]) + ternary(len(bytes) > 32, "...", ""),
		}
	}

	// Handle strings
	if val.Kind() == reflect.String {
		s := val.String()
		if s == "" {
			return nil
		}
		return s
	}

	// Handle basic types
	switch val.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return v
	}

	// Handle slices/arrays
	if val.Kind() == reflect.Slice || val.Kind() == reflect.Array {
		if val.Len() == 0 {
			return nil
		}
		result := make([]interface{}, val.Len())
		for i := 0; i < val.Len(); i++ {
			result[i] = deepInspectValue(val.Index(i).Interface(), depth+1, maxDepth)
		}
		return result
	}

	// Handle maps
	if val.Kind() == reflect.Map {
		if val.Len() == 0 {
			return nil
		}
		result := map[string]interface{}{}
		for _, key := range val.MapKeys() {
			result[fmt.Sprintf("%v", key.Interface())] = deepInspectValue(val.MapIndex(key).Interface(), depth+1, maxDepth)
		}
		return result
	}

	// Handle structs (including proto messages) via reflection
	if val.Kind() == reflect.Struct {
		result := map[string]interface{}{}
		typ := val.Type()
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			fieldVal := val.Field(i)
			if fieldVal.Kind() == reflect.Ptr && fieldVal.IsNil() {
				continue
			}
			if (fieldVal.Kind() == reflect.Slice || fieldVal.Kind() == reflect.Map) && fieldVal.IsNil() {
				continue
			}

			fieldName := field.Name
			jsonName := field.Tag.Get("json")
			if jsonName != "" {
				fieldName = strings.Split(jsonName, ",")[0]
			}

			result[fieldName] = deepInspectValue(fieldVal.Interface(), depth+1, maxDepth)
		}
		if len(result) == 0 {
			return nil
		}
		return result
	}

	// Fallback: string representation
	return fmt.Sprintf("%v", v)
}

// ─── AI tools ────────────────────────────────────────────────────

// aiInspectMessage — AI tool: inspect quoted message structure

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// keep imports referenced
var _ = whatsmeow.MediaAudio
var _ = waE2E.Message{}
