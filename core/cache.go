package main

// cache.go — Cache layer for WhatsApp API calls (group info + owner JIDs)
// Non-breaking: cache miss falls back to original API call.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types"
)

type cachedGroupInfo struct {
	info      *types.GroupInfo
	fetchedAt time.Time
}

var (
	groupInfoCache    = map[string]*cachedGroupInfo{}
	groupInfoCacheMu  sync.RWMutex
	groupInfoCacheTTL = 5 * time.Minute

	ownerJIDCache   = map[string]types.JID{}
	ownerJIDCacheMu sync.RWMutex
)

func getCachedGroupInfo(client *whatsmeow.Client, groupJID types.JID) (*types.GroupInfo, error) {
	key := groupJID.String()

	groupInfoCacheMu.RLock()
	if cached, ok := groupInfoCache[key]; ok {
		if time.Since(cached.fetchedAt) < groupInfoCacheTTL {
			groupInfoCacheMu.RUnlock()
			recordCacheHit()
			return cached.info, nil
		}
	}
	groupInfoCacheMu.RUnlock()

	recordCacheMiss()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := client.GetGroupInfo(ctx, groupJID)
	if err != nil {
		return nil, err
	}

	groupInfoCacheMu.Lock()
	groupInfoCache[key] = &cachedGroupInfo{info: info, fetchedAt: time.Now()}
	groupInfoCacheMu.Unlock()

	return info, nil
}

func invalidateGroupInfoCache(groupJID string) {
	groupInfoCacheMu.Lock()
	delete(groupInfoCache, groupJID)
	groupInfoCacheMu.Unlock()
}

func cacheOwnerJID(user string, jid types.JID) {
	ownerJIDCacheMu.Lock()
	defer ownerJIDCacheMu.Unlock()
	if existing, ok := ownerJIDCache[user]; ok {
		if isPN(existing) && !isPN(jid) {
			return
		}
	}
	ownerJIDCache[user] = jid
}

func getCachedOwnerJID(user string) types.JID {
	ownerJIDCacheMu.RLock()
	defer ownerJIDCacheMu.RUnlock()
	if j, ok := ownerJIDCache[user]; ok {
		return j
	}
	return types.EmptyJID
}

func precomputeWarmup(client *whatsmeow.Client) {
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("[CACHE] warmup panic: %v\n", r)
			}
		}()
		fmt.Println("[CACHE] Starting warmup...")
		owners := ownerJIDs()
		for _, ownerStr := range owners {
			if ownerStr == "" {
				continue
			}
			if j, err := types.ParseJID(ownerStr); err == nil && !j.IsEmpty() {
				cacheOwnerJID(j.User, j)
			}
		}
		fmt.Printf("[CACHE] Cached %d owner JIDs\n", len(owners))
		fmt.Println("[CACHE] Warmup complete")
	}()
}

func cacheStats() string {
	groupInfoCacheMu.RLock()
	groupCount := len(groupInfoCache)
	groupInfoCacheMu.RUnlock()
	ownerJIDCacheMu.RLock()
	ownerCount := len(ownerJIDCache)
	ownerJIDCacheMu.RUnlock()
	return fmt.Sprintf("📦 *Cache Stats*\n• Group info: %d (TTL 5m)\n• Owner JIDs: %d",
		groupCount, ownerCount)
}
