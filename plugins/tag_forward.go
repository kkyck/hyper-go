package main

// tag_forward.go — Auto-forward group image tags to owner + relay replies back.
// ════════════════════════════════════════════════════════════
//
// Flow:
//   1. Group member tags bot in an image caption.
//   2. Bot downloads image bytes from the group.
//   3. Bot uploads image ONCE + sends to ALL owners' private chats with a
//      context caption (sender name, group name, original caption).
//      Each owner gets a unique forwarded_msg_id.
//   4. Bot stores mapping: forwarded_msg_id (in owner's chat) → original
//      context (group_jid, original_msg_id, original_sender_jid, caption, ts).
//   5. Owner replies to the forwarded image in their private chat.
//   6. Bot detects the reply (ContextInfo.StanzaID matches a stored mapping).
//   7. If reply text doesn't start with "." (i.e. not a command), bot
//      forwards the reply text back to the original group as a WhatsApp
//      reply (quoted bubble showing original image+caption).
//
// Storage:
//   - kv_store key: "tag_forward:<forwarded_msg_id>"
//   - Value: JSONB with full context
//   - TTL: 7 days (orphaned entries auto-cleaned hourly)
//
// Edge cases handled:
//   - Skip if sender is bot itself (avoid loops)
//   - Skip if reply text starts with "." (let command dispatcher handle it)
//   - Skip if reply is in a group chat (only private-chat replies trigger relay)
//   - Multiple owners: forward to all, each gets own entry, reply from any
//   - Image download/upload failures: log + return (don't crash)
//   - Best-effort: never blocks main message pipeline (runs in background)

import (
	"context"
	"fmt"
	"time"

	whatsmeow "github.com/polymorfa/hypermeow"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	"google.golang.org/protobuf/proto"
)

// tagForwardKVKeyPrefix is the kv_store key prefix for forward mappings.
const tagForwardKVKeyPrefix = "tag_forward:"

// tagForwardTTL is how long a forward mapping is valid.
const tagForwardTTL = 7 * 24 * time.Hour

// tagForwardCleanupInterval is how often the cleanup goroutine runs.
const tagForwardCleanupInterval = 1 * time.Hour

// tagForwardCleanupStarted ensures cleanup goroutine starts only once.
var tagForwardCleanupStarted bool

// tagForwardEntry is the persisted mapping for one forwarded tag.
type tagForwardEntry struct {
	GroupJID           string `json:"group_jid"`
	OriginalMsgID      string `json:"original_msg_id"`
	OriginalSenderJID  string `json:"original_sender_jid"`
	OriginalSenderName string `json:"original_sender_name"`
	OriginalCaption    string `json:"original_caption"`
	OriginalMsgTS      int64  `json:"original_msg_ts"`
	ForwardedAt        int64  `json:"forwarded_at"`
	ForwardedMsgID     string `json:"forwarded_msg_id"`
	OwnerJID           string `json:"owner_jid"`
}

// tagForwardMaybeForward is the main hook called from main.go's background
// pipeline. Routes to one of two handlers based on the message shape.
//
// Scenario 1: group image + caption mentioning bot → forward to owners
// Scenario 2: owner private-chat reply to a forwarded tag → relay back to group
func tagForwardMaybeForward(client *whatsmeow.Client, msg *events.Message) {
	if msg == nil || msg.Message == nil {
		return
	}

	// Skip self-sent messages (avoid loops)
	if msg.Info.IsFromMe {
		return
	}

	// Scenario 2: owner replies in private chat to a forwarded tag message
	if !msg.Info.IsGroup && isOwnerMsg(msg) {
		tagForwardMaybeRelayReply(client, msg)
		return
	}

	// Scenario 1: group image with caption mentioning bot
	if msg.Info.IsGroup {
		tagForwardMaybeCaptureGroupImage(client, msg)
	}
}

// tagForwardMaybeCaptureGroupImage checks if a group message is an image with
// caption mentioning the bot. If yes, downloads + uploads + sends to all owners
// WHO ARE PARTICIPANTS IN THE SAME GROUP (not all owners globally).
func tagForwardMaybeCaptureGroupImage(client *whatsmeow.Client, msg *events.Message) {
	if client == nil || client.Store == nil || client.Store.ID == nil {
		return
	}

	imgMsg := msg.Message.GetImageMessage()
	if imgMsg == nil {
		// Could also handle video with caption in the future; for now only image.
		return
	}

	caption := imgMsg.GetCaption()
	if caption == "" {
		return
	}

	// ─── Build bot's identity map (both PN + LID forms, device-stripped) ──
	// WA's MentionedJID uses the bare JID form (no device suffix), e.g.
	// "6281234567890@s.whatsapp.net" or "165292487516324@lid".
	// client.Store.ID might be "6281234567890:5@s.whatsapp.net" (with device).
	// We must compare against device-stripped forms of BOTH PN and LID.
	botIdentities := make(map[string]bool)
	botIdentities[client.Store.ID.ToNonAD().String()] = true
	fmt.Printf("[tag_forward] bot identity (PN): %s\n", client.Store.ID.ToNonAD().String())
	if di := client.DangerousInternals(); di != nil {
		if botLID := di.GetOwnLID(); !botLID.IsEmpty() {
			botLIDStr := botLID.ToNonAD().String()
			botIdentities[botLIDStr] = true
			fmt.Printf("[tag_forward] bot identity (LID): %s\n", botLIDStr)
		}
	}

	// Check if bot is mentioned in the caption's ContextInfo
	botMentioned := false
	var mentionedJIDs []string
	if ci := imgMsg.GetContextInfo(); ci != nil {
		mentionedJIDs = ci.GetMentionedJID()
		fmt.Printf("[tag_forward] msg %s: image caption=%q, mentionedJIDs=%v\n",
			msg.Info.ID, caption, mentionedJIDs)
		for _, jid := range mentionedJIDs {
			// Strip device suffix from mention JID for comparison
			parsed, perr := types.ParseJID(jid)
			normalized := jid
			if perr == nil && !parsed.IsEmpty() {
				normalized = parsed.ToNonAD().String()
			}
			if botIdentities[normalized] {
				botMentioned = true
				break
			}
		}
	}
	if !botMentioned {
		fmt.Printf("[tag_forward] msg %s: bot NOT mentioned (mentioned=%v, botIds=%v)\n",
			msg.Info.ID, mentionedJIDs, botIdentities)
		return
	}

	fmt.Printf("[tag_forward] msg %s: BOT MENTIONED in image caption, proceeding\n", msg.Info.ID)

	// Get group info ONCE (name + participants) — best-effort, 10s timeout
	groupName := msg.Info.Chat.String() // fallback
	var groupParticipants []types.GroupParticipant
	giCtx, giCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if gi, err := client.GetGroupInfo(giCtx, msg.Info.Chat); err == nil {
		if gi.Name != "" {
			groupName = gi.Name
		}
		groupParticipants = gi.Participants
		fmt.Printf("[tag_forward] msg %s: group %s has %d participants\n",
			msg.Info.ID, groupName, len(groupParticipants))
	} else {
		fmt.Printf("[tag_forward] msg %s: GetGroupInfo failed: %v (will try all owners)\n",
			msg.Info.ID, err)
	}
	giCancel()

	// Download image bytes
	dlCtx, dlCancel := context.WithTimeout(context.Background(), 60*time.Second)
	imageBytes, err := client.Download(dlCtx, imgMsg)
	dlCancel()
	if err != nil {
		fmt.Printf("[tag_forward] download image failed (msg %s): %v\n", msg.Info.ID, err)
		return
	}
	if len(imageBytes) == 0 {
		fmt.Printf("[tag_forward] image empty (msg %s)\n", msg.Info.ID)
		return
	}
	fmt.Printf("[tag_forward] msg %s: downloaded %d bytes\n", msg.Info.ID, len(imageBytes))

	mime := imgMsg.GetMimetype()
	if mime == "" {
		mime = "image/jpeg"
	}

	// Build forward caption with context
	senderName := msg.Info.PushName
	if senderName == "" {
		senderName = msg.Info.Sender.String()
	}
	forwardCaption := fmt.Sprintf("📌 *Tag dari grup*\n\n• Pengirim: %s\n• Grup: %s\n• Caption asli: %s\n\n_Reply pesan ini untuk membalas tag di grup._",
		senderName, groupName, caption)

	// Upload + send to owners who are in this group, capturing each forwarded_msg_id
	tagForwardCaptureAndSendToOwners(client, msg, imgMsg, imageBytes, mime, forwardCaption, senderName, groupParticipants)
}

// tagForwardCaptureAndSendToOwners uploads image once + sends to each owner
// WHO IS A PARTICIPANT IN THE SAME GROUP as the tag. Captures the response ID
// for each owner + stores a mapping per (owner, msg_id).
//
// Filtering logic:
//   - Build a set of participant JIDs from groupParticipants (all forms:
//     JID, PhoneNumber, LID — each device-stripped)
//   - For each owner in the global owner list, check if ANY of their
//     dual forms (PN + LID) is in the participant set
//   - If not in group, skip (with log)
//   - Also de-dup by dual forms (skip if any dual form already sent)
//
// Fallback: if groupParticipants is empty (e.g. GetGroupInfo failed),
// fall back to sending to ALL owners (previous behavior) — never
// silently drop the forward due to missing participant info.
func tagForwardCaptureAndSendToOwners(client *whatsmeow.Client, msg *events.Message,
	imgMsg *waE2E.ImageMessage, imageBytes []byte, mime, forwardCaption, senderName string,
	groupParticipants []types.GroupParticipant) {
	if client == nil {
		return
	}

	// Upload image once (same bytes → same hash → single upload)
	upCtx, upCancel := context.WithTimeout(context.Background(), 90*time.Second)
	up, err := client.Upload(upCtx, imageBytes, whatsmeow.MediaImage)
	upCancel()
	if err != nil {
		fmt.Printf("[tag_forward] upload failed: %v\n", err)
		return
	}

	owners := ownerJIDs()
	if len(owners) == 0 {
		return
	}

	// ─── Build participant set (all forms: JID + PhoneNumber + LID) ───
	// Each GroupParticipant has 3 JID fields: JID (primary), PhoneNumber (PN),
	// LID. We collect all device-stripped forms so any matching form
	// (PN form, LID form, or primary) counts as "in group".
	participantSet := make(map[string]bool)
	for _, p := range groupParticipants {
		if !p.JID.IsEmpty() {
			participantSet[p.JID.ToNonAD().String()] = true
		}
		if !p.PhoneNumber.IsEmpty() {
			participantSet[p.PhoneNumber.ToNonAD().String()] = true
		}
		if !p.LID.IsEmpty() {
			participantSet[p.LID.ToNonAD().String()] = true
		}
	}
	haveParticipants := len(participantSet) > 0
	if haveParticipants {
		fmt.Printf("[tag_forward] group has %d unique participant JID forms\n",
			len(participantSet))
	} else {
		fmt.Printf("[tag_forward] no participant info available, falling back to ALL owners\n")
	}

	// ─── Filter + de-duplicate owners ──────────────────────────────────
	// Two filters:
	//   1. Must be in group (skip if no dual form matches participant set)
	//   2. De-dup by account (skip if dual form already in sentAccounts)
	sentAccounts := make(map[string]bool)
	var filteredOwners []string
	skippedNotInGroup := 0
	skippedDuplicate := 0
	for _, ownerJIDStr := range owners {
		ownerJID, err := types.ParseJID(ownerJIDStr)
		if err != nil {
			continue
		}

		// Get all dual forms (PN + LID) for this owner
		duals := dualForms(client, ownerJID)

		// Filter 1: must be in group (skip if participant set is empty
		// to fall back to all owners — never silently drop)
		if haveParticipants {
			isInGroup := false
			for _, d := range duals {
				if participantSet[d.ToNonAD().String()] {
					isInGroup = true
					break
				}
			}
			// Also check raw ownerJID itself (not just duals)
			if !isInGroup {
				isInGroup = participantSet[ownerJID.ToNonAD().String()]
			}
			if !isInGroup {
				skippedNotInGroup++
				fmt.Printf("[tag_forward] skipping owner %s (NOT in group %s)\n",
					ownerJIDStr, msg.Info.Chat.String())
				continue
			}
		}

		// Filter 2: de-dup by account
		alreadySent := false
		for _, d := range duals {
			if sentAccounts[d.ToNonAD().String()] {
				alreadySent = true
				break
			}
		}
		if alreadySent {
			skippedDuplicate++
			fmt.Printf("[tag_forward] skipping duplicate owner %s (same account as previous)\n",
				ownerJIDStr)
			continue
		}
		// Mark all dual forms as sent
		for _, d := range duals {
			sentAccounts[d.ToNonAD().String()] = true
		}
		filteredOwners = append(filteredOwners, ownerJIDStr)
	}

	fmt.Printf("[tag_forward] filter summary: %d owners → %d in-group + deduped (%d not-in-group, %d duplicate)\n",
		len(owners), len(filteredOwners), skippedNotInGroup, skippedDuplicate)

	if len(filteredOwners) == 0 {
		fmt.Printf("[tag_forward] no owners are participants in group %s — skipping forward\n",
			msg.Info.Chat.String())
		return
	}

	// Send to each filtered owner (reuses the uploaded blob — no re-upload per owner)
	for _, ownerJIDStr := range filteredOwners {
		ownerJID, err := types.ParseJID(ownerJIDStr)
		if err != nil {
			continue
		}

		sendMsg := &waE2E.Message{
			ImageMessage: &waE2E.ImageMessage{
				URL:           proto.String(up.URL),
				DirectPath:    proto.String(up.DirectPath),
				MediaKey:      up.MediaKey,
				Mimetype:      proto.String(mime),
				FileEncSHA256: up.FileEncSHA256,
				FileSHA256:    up.FileSHA256,
				FileLength:    proto.Uint64(uint64(len(imageBytes))),
				Caption:       proto.String(forwardCaption),
			},
		}

		sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.SendMessage(sendCtx, ownerJID, sendMsg)
		sendCancel()
		if err != nil {
			fmt.Printf("[tag_forward] send to owner %s failed: %v\n", ownerJIDStr, err)
			continue
		}

		// Store mapping: forwarded_msg_id → original context
		entry := tagForwardEntry{
			GroupJID:           msg.Info.Chat.String(),
			OriginalMsgID:      msg.Info.ID,
			OriginalSenderJID:  msg.Info.Sender.String(),
			OriginalSenderName: senderName,
			OriginalCaption:    imgMsg.GetCaption(),
			OriginalMsgTS:      msg.Info.Timestamp.Unix(),
			ForwardedAt:        time.Now().Unix(),
			ForwardedMsgID:     resp.ID,
			OwnerJID:           ownerJIDStr,
		}
		tagForwardStoreEntry(entry)

		fmt.Printf("[tag_forward] forwarded tag from %s in group %s to owner %s (msg_id=%s)\n",
			senderName, msg.Info.Chat.String(), ownerJIDStr, resp.ID)
	}
}

// tagForwardMaybeRelayReply checks if owner's private-chat reply is to a
// forwarded tag message. If yes, relays the reply back to the original
// group as a WhatsApp reply to the original tagged message. After
// successful relay, REVOKES (deletes for everyone) the original
// forwarded image from the owner's private chat (cleanup) so the
// private chat doesn't accumulate forwarded tag images.
//
// 2026-10-27 fix: removed the "." prefix skip. ALL replies are now
// forwarded to the group, regardless of whether they start with ".".
// (Previously, "." prefix replies were skipped to let the command
// dispatcher handle them, but user wants "." replies forwarded.)
func tagForwardMaybeRelayReply(client *whatsmeow.Client, msg *events.Message) {
	if client == nil {
		return
	}

	extMsg := msg.Message.GetExtendedTextMessage()
	if extMsg == nil {
		return
	}
	ci := extMsg.GetContextInfo()
	if ci == nil {
		return
	}
	stanzaID := ci.GetStanzaID()
	if stanzaID == "" {
		return
	}

	// Look up the stanzaID in our mapping
	entry, ok := tagForwardLookupEntry(stanzaID)
	if !ok {
		return // not a forwarded tag message
	}

	replyText := extMsg.GetText()
	fmt.Printf("[tag_forward] owner reply detected: stanzaID=%s, text=%q, mapped to group %s (orig msg %s)\n",
		stanzaID, replyText, entry.GroupJID, entry.OriginalMsgID)

	// Forward the reply back to the original group as a reply
	groupJID, err := types.ParseJID(entry.GroupJID)
	if err != nil {
		fmt.Printf("[tag_forward] invalid group jid %q: %v\n", entry.GroupJID, err)
		return
	}

	// Build reply message with ContextInfo quoting the original tagged image
	replyMsg := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(replyText),
			ContextInfo: &waE2E.ContextInfo{
				StanzaID:    proto.String(entry.OriginalMsgID),
				Participant: proto.String(entry.OriginalSenderJID),
				QuotedMessage: &waE2E.Message{
					ImageMessage: &waE2E.ImageMessage{
						Caption: proto.String(entry.OriginalCaption),
					},
				},
			},
		},
	}

	sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = client.SendMessage(sendCtx, groupJID, replyMsg)
	sendCancel()
	if err != nil {
		fmt.Printf("[tag_forward] relay reply to group %s failed: %v\n", entry.GroupJID, err)
		return
	}
	fmt.Printf("[tag_forward] relayed owner reply to group %s (original msg %s)\n",
		entry.GroupJID, entry.OriginalMsgID)

	// ─── Cleanup: REVOKE the original forwarded image from owner's private chat ──
	// After successful relay, delete the forwarded image so the private chat
	// doesn't accumulate forwarded tag images. The mapping entry is also
	// deleted from kv_store.
	ownerJID, err := types.ParseJID(entry.OwnerJID)
	if err == nil && entry.ForwardedMsgID != "" {
		revokeMsg := &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
				Key: &waCommon.MessageKey{
					FromMe:    proto.Bool(true),
					ID:        proto.String(entry.ForwardedMsgID),
					RemoteJID: proto.String(entry.OwnerJID),
				},
			},
		}
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, revErr := client.SendMessage(revokeCtx, ownerJID, revokeMsg)
		revokeCancel()
		if revErr != nil {
			fmt.Printf("[tag_forward] revoke forwarded msg %s failed: %v\n",
				entry.ForwardedMsgID, revErr)
		} else {
			fmt.Printf("[tag_forward] revoked forwarded msg %s from owner %s private chat\n",
				entry.ForwardedMsgID, entry.OwnerJID)
		}
	}

	// Delete the kv_store entry (no longer needed)
	tagForwardDeleteEntry(entry.ForwardedMsgID)
}

// ─── Storage helpers ───────────────────────────────────────────

// tagForwardStoreEntry stores a forwarded tag mapping in PG kv_store.
func tagForwardStoreEntry(entry tagForwardEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := tagForwardKVKeyPrefix + entry.ForwardedMsgID
	_ = kvStoreSet(ctx, key, &entry)
}

// tagForwardLookupEntry retrieves a forwarded tag mapping by forwarded_msg_id.
// Returns false if not found OR if entry is older than TTL.
func tagForwardLookupEntry(forwardedMsgID string) (tagForwardEntry, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := tagForwardKVKeyPrefix + forwardedMsgID
	var entry tagForwardEntry
	if !kvStoreGet(ctx, key, &entry) {
		return tagForwardEntry{}, false
	}
	if time.Since(time.Unix(entry.ForwardedAt, 0)) > tagForwardTTL {
		return tagForwardEntry{}, false
	}
	return entry, true
}

// tagForwardDeleteEntry removes a forwarded tag mapping from kv_store.
// Called after the relay is complete (forwarded image was revoked, so
// the entry is no longer needed).
func tagForwardDeleteEntry(forwardedMsgID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := tagForwardKVKeyPrefix + forwardedMsgID
	_ = kvStoreDelete(ctx, key)
}

// ─── Cleanup goroutine ─────────────────────────────────────────

// tagForwardStartCleanup starts a background goroutine that periodically
// deletes expired tag_forward entries. Idempotent.
func tagForwardStartCleanup() {
	if tagForwardCleanupStarted {
		return
	}
	tagForwardCleanupStarted = true
	go func() {
		ticker := time.NewTicker(tagForwardCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			tagForwardCleanupExpired()
		}
	}()
}

// tagForwardCleanupExpired deletes kv_store entries with tag_forward: prefix
// that are older than tagForwardTTL.
func tagForwardCleanupExpired() {
	db, err := pgGet()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := time.Now().Add(-tagForwardTTL).Unix()
	// kv_store schema: (key TEXT PRIMARY KEY, value JSONB, updated_at BIGINT)
	// JSONB value has a "forwarded_at" field. Delete rows where forwarded_at
	// < cutoff AND key starts with "tag_forward:".
	_, _ = pgExec(ctx, db,
		`DELETE FROM kv_store
                 WHERE key LIKE ?
                   AND (value->>'forwarded_at')::BIGINT < ?`,
		tagForwardKVKeyPrefix+"%", cutoff)
}

func init() {
	tagForwardStartCleanup()
}
