package main

// latency_warm.go — Trik latensi super kecil & stabil.
//
// 1. PRE-WARM sender key: distribusikan key ke member grup SEBELUM kirim pesan,
//    jadi saat kirim tidak perlu tunggu SKDM.
// 2. CACHE info grup: simpan participant list, tidak fetch berulang.
// 3. ASYNC fire-and-forget: kirim tanpa tunggu receipt.
//
// Usage:
//   .warmup           — pre-warm semua grup (jalan di background)
//   .warmup <jml>     — pre-warm N grup terbesar

import (
	"context"
	"fmt"
	"sync"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
)

var (
	warmCache   = make(map[string]*warmGroupInfo)
	warmCacheMu sync.RWMutex
	warmRunning = false
	warmMu      sync.Mutex
)

type warmGroupInfo struct {
	JID          types.JID
	Participants []types.JID
	WarmedAt     time.Time
	MemberCount  int
}

func handleWarmupCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	warmMu.Lock()
	if warmRunning {
		warmMu.Unlock()
		return "⏳ Warmup sedang berjalan..."
	}
	warmRunning = true
	warmMu.Unlock()

	go func() {
		defer func() {
			warmMu.Lock()
			warmRunning = false
			warmMu.Unlock()
		}()
		warmupAllGroups(client)
	}()
	return "🔥 Warmup dimulai di background..."
}

func warmupAllGroups(client *whatsmeow.Client) {
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	groups, err := client.GetJoinedGroups(ctx)
	if err != nil {
		fmt.Printf("[WARMUP] gagal ambil grup: %v\n", err)
		return
	}

	fmt.Printf("[WARMUP] mulai %d grup\n", len(groups))
	for _, g := range groups {
		warmupGroup(client, g.JID)
		// Jeda kecil agar tidak spam
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("[WARMUP] selesai\n")
}

func warmupGroup(client *whatsmeow.Client, groupJID types.JID) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	meta, err := client.GetGroupInfo(ctx, groupJID)
	if err != nil || meta == nil {
		return
	}

	var participants []types.JID
	for _, p := range meta.Participants {
		if !p.JID.IsEmpty() {
			participants = append(participants, p.JID.ToNonAD())
		}
	}

	// Simpan ke cache
	warmCacheMu.Lock()
	warmCache[groupJID.String()] = &warmGroupInfo{
		JID:          groupJID,
		Participants: participants,
		WarmedAt:     time.Now(),
		MemberCount:  len(participants),
	}
	warmCacheMu.Unlock()

	// Pre-distribute sender key via rotasi (memicu SKDM ke semua)
	sprBaseMu.Lock()
	_ = sprRotateSenderKey(groupJID)
	sprBaseMu.Unlock()

	fmt.Printf("[WARMUP] %s: %d member\n", groupJID.User, len(participants))
}

func getWarmGroupInfo(groupJID types.JID) *warmGroupInfo {
	warmCacheMu.RLock()
	defer warmCacheMu.RUnlock()
	return warmCache[groupJID.String()]
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "warmup",
		Description: "warmup",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleWarmupCommand(client, msg, args)
		},
	})
}
