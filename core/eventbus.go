package main

// eventbus.go — Pub/sub event bus for decoupled event handling.
//
// ─── WHY EVENT BUS ─────────────────────────────────────────────────
//
// Currently, event handling is tightly coupled in handleEvent():
//   - Message received → check commands → check guard → check AFK → ...
//   - Each check modifies state or sends messages
//   - Hard to add new handlers without touching handleEvent()
//
// Event bus decouples:
//   - Publishers emit events (no knowledge of subscribers)
//   - Subscribers register interest in event types
//   - Multiple subscribers can react to same event
//   - Add/remove subscribers without touching publisher
//
// ─── USAGE ─────────────────────────────────────────────────────────
//
//   // Register subscriber (at startup)
//   eventBus.Subscribe("message", func(e Event) {
//       if e.Msg.Info.IsGroup { ... }
//   })
//
//   // Publish event (in handleEvent)
//   eventBus.Publish(Event{Type: "message", Msg: msg})
//
// ─── EVENT TYPES ──────────────────────────────────────────────────
//
//   "message"      — new message received
//   "connected"    — bot connected to WhatsApp
//   "disconnected" — bot disconnected
//   "join"         — user joined group
//   "leave"        — user left group
//   "promote"      — user promoted to admin
//   "demote"       — user demoted
//   "reaction"     — reaction received

import (
	"sync"

	"github.com/polymorfa/hypermeow/types/events"
)

// Event represents a bot event.
type Event struct {
	Type string          // event type ("message", "connected", etc.)
	Msg  *events.Message // WhatsApp message (if applicable)
	Data interface{}     // additional data (group JID, user JID, etc.)
}

// eventBusType is the global event bus.
type eventBusType struct {
	mu          sync.RWMutex
	subscribers map[string][]func(Event)
}

// eventBus is the global singleton.
var eventBus = &eventBusType{
	subscribers: make(map[string][]func(Event)),
}

// Subscribe registers a handler for a specific event type.
// Returns an unsubscribe function.
func (eb *eventBusType) Subscribe(eventType string, handler func(Event)) func() {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.subscribers[eventType] = append(eb.subscribers[eventType], handler)
	index := len(eb.subscribers[eventType]) - 1

	// Return unsubscribe function
	return func() {
		eb.mu.Lock()
		defer eb.mu.Unlock()
		if index < len(eb.subscribers[eventType]) {
			eb.subscribers[eventType][index] = nil // lazy removal
		}
	}
}

// Publish emits an event to all subscribers.
// Subscribers run in separate goroutines (non-blocking).
func (eb *eventBusType) Publish(e Event) {
	eb.mu.RLock()
	handlers := make([]func(Event), len(eb.subscribers[e.Type]))
	copy(handlers, eb.subscribers[e.Type])
	eb.mu.RUnlock()

	for _, handler := range handlers {
		if handler == nil {
			continue
		}
		go handler(e) // non-blocking
	}
}

// PublishSync emits an event to all subscribers synchronously (blocking).
// Use for events that need ordering (e.g., "connected" before "message").
func (eb *eventBusType) PublishSync(e Event) {
	eb.mu.RLock()
	handlers := make([]func(Event), len(eb.subscribers[e.Type]))
	copy(handlers, eb.subscribers[e.Type])
	eb.mu.RUnlock()

	for _, handler := range handlers {
		if handler == nil {
			continue
		}
		handler(e)
	}
}
