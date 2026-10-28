package main

// richcard.go — Rich Card Builder (WhatsApp "bot forwarded" rich response)
// ══════════════════════════════════════════════════════════════════
//
// Membangun pesan rich response WhatsApp yang dikirim sebagai
// `botForwardedMessage` — format yang dipakai Meta AI / WhatsApp untuk
// menampilkan kartu kaya (teks markdown, gambar, tabel, code, latex, map).
//
// Struktur pesan yang dihasilkan:
//
//	waE2E.Message
//	 ├── MessageContextInfo  (device list + attestation)
//	 └── BotForwardedMessage
//	      └── Message
//	           └── RichResponseMessage
//	                ├── MessageType
//	                ├── Submessages        (satu per section)
//	                ├── ContextInfo        (forwarded + quote + mention)
//	                └── UnifiedResponse    (JSON view-model)
//
// SECTION
// -------
// Section adalah satu unit tampilan. Builder menyimpan section + submessage
// secara berpasangan (lihat richCardNode). Section ditulis ke JSON sebagai:
//
//	{ "view_model": { "__typename": "GenAISingleLayoutViewModel",
//	                  "primitive": { ... } } }
//
// SUBMESSAGE
// ----------
// Submessage adalah pasangan "teks pendek" yang dipakai WhatsApp untuk
// merender preview / notifikasi. Kalau section tidak punya submessage,
// builder membuat otomatis lewat richCardAutoSubText.
//
// UNIFIED RESPONSE
// ----------------
// UnifiedResponse berisi JSON view-model lengkap yang akan di-render sisi
// klien. Builder menyusunnya otomatis dari daftar section, kecuali bila
// pemanggil menyediakannya sendiri lewat SetUnifiedResponseData().
//
// PEMAKAI
// -------
//	card := NewRichCard(client)
//	card.AddText("halo *dunia*")
//	msg := card.Build()

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/polymorfa/hypermeow"
	waAICommon "github.com/polymorfa/hypermeow/proto/waAICommon"
	waAICommonDeprecated "github.com/polymorfa/hypermeow/proto/waAICommonDeprecated"
	waAea "github.com/polymorfa/hypermeow/proto/waAea"
	waCommon "github.com/polymorfa/hypermeow/proto/waCommon"
	waE2E "github.com/polymorfa/hypermeow/proto/waE2E"
	"github.com/polymorfa/hypermeow/types"
)

// Alias netral untuk tipe proto dari dependency. Nama asli tipe-tipe ini
// mengikuti skema WhatsApp, bukan branding proyek ini.
type (
	richSubMessage        = waAICommonDeprecated.AIRichResponseSubMessage
	richImageURL          = waAICommonDeprecated.AIRichResponseImageURL
	richGridImageMetadata = waAICommonDeprecated.AIRichResponseGridImageMetadata
	richResponseMessage   = waE2E.AIRichResponseMessage
	richUnifiedResponse   = waAICommon.AIRichResponseUnifiedResponse
)

// Nilai enum dari dependency bertipe enum (bukan integer biasa), sehingga
// dideklarasikan sebagai var agar bisa dipakai langsung sebagai pointer.
var (
	richSubMessageTypeText    = waAICommonDeprecated.AIRichResponseSubMessageType_AI_RICH_RESPONSE_TEXT
	richSubMessageTypeGridImg = waAICommonDeprecated.AIRichResponseSubMessageType_AI_RICH_RESPONSE_GRID_IMAGE
	richResponseTypeStandard  = waAICommonDeprecated.AIRichResponseMessageType_AI_RICH_RESPONSE_TYPE_STANDARD
)

// ─── Tipe dasar ──────────────────────────────────────────────────

// richCardNode adalah satu unit konten: section + submessage opsional
// yang terikat bersama dengan id opsional.
type richCardNode struct {
	id         string
	section    *richCardSection
	submessage *richSubMessage
}

// richCardSection membungkus satu layout section (map JSON) sebagai key
// identitas (pointer) saat melacak item.
type richCardSection struct {
	id string
	vm map[string]any
}

// richCardMessageKey menyimpan remoteJid + id pesan terakhir yang dikirim.
type richCardMessageKey struct {
	jid types.JID
	id  types.MessageID
}

// RichCardBuilder menyusun pesan rich response.
//
// Pakai NewRichCard() untuk membuat instance baru.
type RichCardBuilder struct {
	client     *whatsmeow.Client
	title      string
	footer     string
	responseID string

	// bypassDownload: bila true, Send() mengirim edit (protocolMessage
	// MESSAGE_EDIT) setelah pesan terkirim.
	bypassDownload bool

	// rawEnvelope mengikuti envelope minimal tanpa metadata tambahan.
	rawEnvelope   bool
	forwardBotJID string
	imagineType   *waAICommon.BotImagineMetadata_ImagineType

	// forwarded: bila false, contextInfo tidak memuat isForwarded /
	// forwardedAiBotMessageInfo / forwardOrigin.
	forwarded bool

	// mentions: JID yang di-tag di kartu (ContextInfo.MentionedJID).
	mentions []string

	// includesUnifiedResponse / includesSubmessages: kendali apakah
	// unifiedResponse.data / submessages ikut dipasang.
	includesUnifiedResponse bool
	includesSubmessages     bool

	subscriptionUpsell *waAICommon.AISubscriptionUpsellMetadata

	// botResponseIDOverride memasang botMetadata.botResponseID.
	botResponseIDOverride string

	// capabilities: flag kemampuan (BotCapabilityMetadata) yang dipasang
	// di botMetadata.
	capabilities []waAICommon.BotCapabilityMetadata_BotCapabilityType

	// useMessageSecret memasang messageContextInfo.messageSecret.
	useMessageSecret bool

	// noContextInfo membuang contextInfo dari richResponse.
	noContextInfo bool

	messageSecret []byte

	// ─── Model node ──────────────────────────────────────────────
	//   nodes   : urutan konten (section/submessage/id)
	//   idIndex : map id → node untuk lookup cepat
	nodes   []*richCardNode
	idIndex map[string]*richCardNode

	// pendingOpts: opsi (id/insertAt/replace) untuk panggilan Add* berikutnya.
	pendingOpts *richCardAddOptions

	// pending: antrean pekerjaan lazy yang dijalankan saat Build().
	pending []func()

	// dynamicIDs: refresh response id setiap Build().
	dynamicIDs bool
	// unsupportedTypeAlert: tampilkan alert untuk tipe yang belum didukung.
	unsupportedTypeAlert bool
	// verification: pasang verificationMetadata.
	verification bool
	// threadType menentukan tipe client info thread.
	threadType *waAICommon.AIThreadInfo_AIThreadClientInfo_AIThreadType

	sources []*waAICommon.BotSourcesMetadata_BotSourceItem

	// customUnifiedResponseData — JSON mentah yang menggantikan
	// unifiedResponse.data hasil build (lihat SetUnifiedResponseData).
	customUnifiedResponseData []byte

	quoteID          string
	quoteParticipant string
	quoteMessage     *waE2E.Message

	// notification: botMetadata diberi sessionTransparencyMetadata.
	notification bool

	// bloksVersioningID / pixelDensity controlling rendering Bloks UI.
	bloksVersioningID string
	pixelDensity      float64

	// lastMessageKey — key pesan terakhir yang dikirim via Send().
	lastMessageKey richCardMessageKey
}

// richCardAddOptions — opsi untuk pemanggilan Add* berikutnya.
type richCardAddOptions struct {
	id             string
	insertAt       string
	insertAtOffset int
	replace        string
}

// RichCardImageOptions — opsi untuk RichCardBuilder.AddImage.
type RichCardImageOptions struct {
	URLs       []string
	Status     string
	UpdateText string
	Width      int
	Height     int
}

// ─── Error hierarchy ─────────────────────────────────────────────

// RichCardError adalah base error dari seluruh kelas error builder.
type RichCardError struct {
	msg  string
	code string
}

func (e *RichCardError) Error() string { return e.msg }
func (e *RichCardError) Code() string  { return e.code }
func (e *RichCardError) Unwrap() error { return nil }

func newRichCardError(msg, code string) *RichCardError {
	if msg == "" {
		switch code {
		case "ITEM_NOT_FOUND":
			msg = "Item not found"
		case "DUPLICATE_ID":
			msg = "Duplicate id"
		case "INVALID_TARGET":
			msg = "Invalid target"
		case "CONTENT_VALIDATION":
			msg = "Content validation error"
		default:
			msg = "Rich card error"
		}
	}
	return &RichCardError{msg: msg, code: code}
}

// RichCardItemNotFoundError dilempar saat id yang dirujuk tidak terdaftar.
type RichCardItemNotFoundError struct {
	*RichCardError
	ID          string
	AvailableID []string
}

func (e *RichCardItemNotFoundError) Error() string {
	return fmt.Sprintf("Item not found: %s (available: %v)", e.ID, e.AvailableID)
}

// RichCardDuplicateIDError dilempar saat id yang sama dipakai dua kali.
type RichCardDuplicateIDError struct {
	*RichCardError
	ID string
}

func (e *RichCardDuplicateIDError) Error() string {
	return fmt.Sprintf("Duplicate id: %s", e.ID)
}

// RichCardInvalidTargetError dilempar saat target tidak bisa diresolve.
type RichCardInvalidTargetError struct {
	*RichCardError
}

func (e *RichCardInvalidTargetError) Error() string { return e.RichCardError.Error() }

// RichCardContentValidationError dilempar saat konten tidak valid.
type RichCardContentValidationError struct {
	*RichCardError
}

func (e *RichCardContentValidationError) Error() string { return e.RichCardError.Error() }

// ─── Helper ──────────────────────────────────────────────────────

// richCardStrSet membuat set dari daftar kata.
func richCardStrSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// richCardNewUUID menghasilkan UUID v4 acak.
func richCardNewUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// richCardRandomBytes menghasilkan n byte acak.
func richCardRandomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// richCardNumOr mem-parse float, memakai default bila tidak valid.
func richCardNumOr(v string, def float64) float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f == 0 {
		return def
	}
	return f
}

// richCardResolveMedia menormalkan URL media. Buffer lokal belum didukung
// dan diteruskan apa adanya.
func richCardResolveMedia(s string) string {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return s
}

// richCardNewLayout membuat satu section dari view-model primitive.
func richCardNewLayout(name string, data any, extra map[string]any) *richCardSection {
	vm := map[string]any{"__typename": "GenAI" + name + "LayoutViewModel"}
	switch data.(type) {
	case []map[string]any, []any:
		vm["primitives"] = data
	default:
		vm["primitive"] = data
	}
	out := map[string]any{}
	for k, v := range extra {
		out[k] = v
	}
	out["view_model"] = vm
	return &richCardSection{vm: out}
}

// richCardMarshalJSON marshal map ke JSON.
func richCardMarshalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// ─── Constructor & setter ────────────────────────────────────────

// NewRichCard membuat builder rich card baru untuk client tertentu.
func NewRichCard(client *whatsmeow.Client) *RichCardBuilder {
	return &RichCardBuilder{
		client:                  client,
		responseID:              richCardNewUUID(),
		messageSecret:           richCardRandomBytes(32),
		bypassDownload:          true,
		forwarded:               true,
		dynamicIDs:              true,
		verification:            false,
		unsupportedTypeAlert:    true,
		includesUnifiedResponse: true,
		includesSubmessages:     true,
		nodes:                   make([]*richCardNode, 0),
		idIndex:                 make(map[string]*richCardNode),
	}
}

// SetTitle mengatur judul kartu.
func (b *RichCardBuilder) SetTitle(t string) *RichCardBuilder { b.title = t; return b }

// SetFooter mengatur footer kartu.
func (b *RichCardBuilder) SetFooter(t string) *RichCardBuilder { b.footer = t; return b }

// SetResponseID memaksa response_id — untuk streaming semua edit harus
// memakai response_id yang sama dengan pesan awal.
func (b *RichCardBuilder) SetResponseID(id string) *RichCardBuilder {
	if id != "" {
		b.responseID = id
	}
	return b
}

// SetByPassDownload menyalakan/mematikan follow-up edit setelah kirim.
func (b *RichCardBuilder) SetByPassDownload(v bool) *RichCardBuilder {
	b.bypassDownload = v
	return b
}

// SetForwarded menentukan apakah contextInfo ditandai sebagai forwarded.
func (b *RichCardBuilder) SetForwarded(v bool) *RichCardBuilder {
	b.forwarded = v
	return b
}

// SetIncludesUnifiedResponse menentukan apakah unifiedResponse ikut dipasang.
func (b *RichCardBuilder) SetIncludesUnifiedResponse(v bool) *RichCardBuilder {
	b.includesUnifiedResponse = v
	return b
}

// SetIncludesSubmessages menentukan apakah submessages ikut dipasang.
func (b *RichCardBuilder) SetIncludesSubmessages(v bool) *RichCardBuilder {
	b.includesSubmessages = v
	return b
}

// SetRawEnvelope memakai envelope minimal tanpa metadata tambahan.
func (b *RichCardBuilder) SetRawEnvelope(v bool) *RichCardBuilder {
	b.rawEnvelope = v
	return b
}

// SetForwardBotJID mengganti JID bot pada contextInfo forwarded.
func (b *RichCardBuilder) SetForwardBotJID(jid string) *RichCardBuilder {
	b.forwardBotJID = jid
	return b
}

// SetImagineType menentukan tipe imagine pada kartu gambar.
func (b *RichCardBuilder) SetImagineType(t waAICommon.BotImagineMetadata_ImagineType) *RichCardBuilder {
	b.imagineType = &t
	return b
}

// SetSubscriptionUpsell memasang metadata upsell langganan.
func (b *RichCardBuilder) SetSubscriptionUpsell(t waAICommon.AISubscriptionRequestType) *RichCardBuilder {
	b.subscriptionUpsell = &waAICommon.AISubscriptionUpsellMetadata{RequestType: &t}
	return b
}

// SetBotResponseID memasang botMetadata.botResponseID.
func (b *RichCardBuilder) SetBotResponseID(id string) *RichCardBuilder {
	b.botResponseIDOverride = id
	return b
}

// SetMessageSecret menentukan apakah messageSecret dipasang.
func (b *RichCardBuilder) SetMessageSecret(v bool) *RichCardBuilder {
	b.useMessageSecret = v
	return b
}

// SetNoContextInfo membuang contextInfo dari richResponse.
func (b *RichCardBuilder) SetNoContextInfo(v bool) *RichCardBuilder {
	b.noContextInfo = v
	return b
}

// SetUnifiedResponseData menyuplai JSON mentah unifiedResponse, menggantikan
// hasil build otomatis.
func (b *RichCardBuilder) SetUnifiedResponseData(rawJSON []byte) *RichCardBuilder {
	b.customUnifiedResponseData = rawJSON
	return b
}

// SetDynamicID menentukan apakah response id di-refresh tiap Build().
func (b *RichCardBuilder) SetDynamicIDs(v bool) *RichCardBuilder {
	b.dynamicIDs = v
	return b
}

// SetThreadType menentukan tipe thread client info.
func (b *RichCardBuilder) SetThreadType(t waAICommon.AIThreadInfo_AIThreadClientInfo_AIThreadType) *RichCardBuilder {
	b.threadType = &t
	return b
}

// SetVerificationMetadata menentukan apakah verificationMetadata dipasang.
func (b *RichCardBuilder) SetVerificationMetadata(v bool) *RichCardBuilder {
	b.verification = v
	return b
}

// SetUnsupportedTypeAlert menentukan apakah alert tipe tak didukung muncul.
func (b *RichCardBuilder) SetUnsupportedTypeAlert(v bool) *RichCardBuilder {
	b.unsupportedTypeAlert = v
	return b
}

// SetCapabilities memasang daftar capability yang diizinkan dirender klien.
func (b *RichCardBuilder) SetCapabilities(caps ...waAICommon.BotCapabilityMetadata_BotCapabilityType) *RichCardBuilder {
	b.capabilities = append(b.capabilities, caps...)
	return b
}

// SetBotRenderingConfig mengatur metadata rendering Bloks UI.
func (b *RichCardBuilder) SetBotRenderingConfig(bloksVersioningID string, pixelDensity float64) *RichCardBuilder {
	b.bloksVersioningID = bloksVersioningID
	b.pixelDensity = pixelDensity
	return b
}

// SetNotification menentukan apakah sessionTransparencyMetadata dipasang.
func (b *RichCardBuilder) SetNotification(v bool) *RichCardBuilder {
	b.notification = v
	return b
}

// SetQuoted memasang pesan yang dikutip.
func (b *RichCardBuilder) SetQuoted(id, participant string, msg *waE2E.Message) *RichCardBuilder {
	b.quoteID = id
	b.quoteParticipant = participant
	b.quoteMessage = msg
	return b
}

// SetMentions memasang JID yang di-tag di kartu.
func (b *RichCardBuilder) SetMentions(jids ...string) *RichCardBuilder {
	b.mentions = jids
	return b
}

// refreshDynamicIDs membuat response id baru bila mode dinamis aktif.
func (b *RichCardBuilder) refreshDynamicIDs() {
	if !b.dynamicIDs {
		return
	}
	b.responseID = richCardNewUUID()
	// botResponseId selalu UUID independen dari response_id: klien memakai
	//-nya untuk mengidentifikasi sesi generate per chat.
	b.botResponseIDOverride = richCardNewUUID()
}

// ─── Node management ─────────────────────────────────────────────

// sections mengembalikan daftar section yang sudah terisi.
func (b *RichCardBuilder) sections() []*richCardSection {
	var out []*richCardSection
	for _, n := range b.nodes {
		if n.section != nil {
			out = append(out, n.section)
		}
	}
	return out
}

// submessages mengembalikan daftar submessage yang sudah terisi.
func (b *RichCardBuilder) submessages() []*richSubMessage {
	var out []*richSubMessage
	for _, n := range b.nodes {
		if n.submessage != nil {
			out = append(out, n.submessage)
		}
	}
	return out
}

// GetIDs mengembalikan daftar id node yang terdaftar.
func (b *RichCardBuilder) GetIDs() []string {
	out := make([]string, 0, len(b.idIndex))
	for id := range b.idIndex {
		out = append(out, id)
	}
	return out
}

func (b *RichCardBuilder) makeNode(id string, section *richCardSection, submessage *richSubMessage) *richCardNode {
	return &richCardNode{
		id:         id,
		section:    section,
		submessage: submessage,
	}
}

func (b *RichCardBuilder) registerID(node *richCardNode, id string) {
	if id == "" {
		return
	}
	node.id = id
	b.idIndex[id] = node
}

func (b *RichCardBuilder) unregisterID(node *richCardNode) {
	if node.id != "" && b.idIndex[node.id] == node {
		delete(b.idIndex, node.id)
	}
	node.id = ""
}

func (b *RichCardBuilder) getNode(id string) *richCardNode {
	n, ok := b.idIndex[id]
	if !ok {
		avail := b.GetIDs()
		if len(avail) == 0 {
			avail = nil
		}
		panic(&RichCardItemNotFoundError{RichCardError: newRichCardError("", "ITEM_NOT_FOUND"), ID: id, AvailableID: avail})
	}
	return n
}

func (b *RichCardBuilder) resolveTarget(target any) (id string, offset int) {
	switch t := target.(type) {
	case []any:
		if len(t) < 1 || len(t) > 2 {
			panic(&RichCardContentValidationError{RichCardError: newRichCardError("Target must be id or [id, offset]", "CONTENT_VALIDATION")})
		}
		s, ok := t[0].(string)
		if !ok || s == "" {
			panic(&RichCardContentValidationError{RichCardError: newRichCardError("Target id must be a non-empty string", "CONTENT_VALIDATION")})
		}
		id = s
		if len(t) == 2 {
			if off, ok := t[1].(int); ok {
				offset = off
			}
		}
		return id, offset
	case string:
		if t == "" {
			panic(&RichCardContentValidationError{RichCardError: newRichCardError("Target must be a non-empty id or [id, offset]", "CONTENT_VALIDATION")})
		}
		return t, 0
	default:
		panic(&RichCardContentValidationError{RichCardError: newRichCardError("Target must be a non-empty id or [id, offset]", "CONTENT_VALIDATION")})
	}
}

func (b *RichCardBuilder) resolveNodeIndex(target any) int {
	id, offset := b.resolveTarget(target)
	node := b.getNode(id)
	base := -1
	for i, n := range b.nodes {
		if n == node {
			base = i
			break
		}
	}
	if base == -1 {
		panic(&RichCardInvalidTargetError{RichCardError: newRichCardError(fmt.Sprintf("Item id %q is registered but not present in the node list (internal desync)", id), "INVALID_TARGET")})
	}
	idx := base + offset
	if idx < 0 || idx >= len(b.nodes) {
		panic(&RichCardInvalidTargetError{RichCardError: newRichCardError(fmt.Sprintf("Target %q with offset %d resolves to index %d, out of range (0-%d)", id, offset, idx, len(b.nodes)-1), "INVALID_TARGET")})
	}
	return idx
}

func (b *RichCardBuilder) validateSections(section any) []*richCardSection {
	switch s := section.(type) {
	case *richCardSection:
		if s == nil {
			panic(&RichCardContentValidationError{RichCardError: newRichCardError("Sections must be plain objects", "CONTENT_VALIDATION")})
		}
		return []*richCardSection{s}
	case []*richCardSection:
		if len(s) == 0 {
			panic(&RichCardContentValidationError{RichCardError: newRichCardError("At least one section is required", "CONTENT_VALIDATION")})
		}
		for _, it := range s {
			if it == nil {
				panic(&RichCardContentValidationError{RichCardError: newRichCardError("Sections must be plain objects", "CONTENT_VALIDATION")})
			}
		}
		return s
	case nil:
		return nil
	default:
		panic(&RichCardContentValidationError{RichCardError: newRichCardError("Sections must be plain objects", "CONTENT_VALIDATION")})
	}
}

func (b *RichCardBuilder) validateSubmessages(submessage any) []*richSubMessage {
	switch s := submessage.(type) {
	case nil:
		return nil
	case *richSubMessage:
		if s == nil {
			return nil
		}
		return []*richSubMessage{s}
	case []*richSubMessage:
		return s
	default:
		panic(&RichCardContentValidationError{RichCardError: newRichCardError("Submessages must be plain objects", "CONTENT_VALIDATION")})
	}
}

func (b *RichCardBuilder) pairSubmessages(sections []*richCardSection, submessages []*richSubMessage) []*richSubMessage {
	n := len(sections)
	m := len(submessages)
	if m == 0 {
		return make([]*richSubMessage, n)
	}
	if m == 1 {
		paired := make([]*richSubMessage, n)
		paired[0] = submessages[0]
		return paired
	}
	if m == n {
		return submessages
	}
	panic(&RichCardContentValidationError{RichCardError: newRichCardError(
		"Section and submessage count must match",
		"CONTENT_VALIDATION",
	)})
}

// richCardAutoSubText membuat submessage teks otomatis dari isi section —
// dipakai saat section tidak menyertakan submessage sendiri.
func richCardAutoSubText(section *richCardSection) *richSubMessage {
	text := ""
	if p, ok := section.vm["view_model"].(map[string]any); ok {
		if pr, ok := p["primitive"].(map[string]any); ok {
			for _, k := range []string{"prompt_text", "title", "text", "message", "header", "placeholder", "name", "content", "body", "update_text", "subtitle", "expr"} {
				if s, ok := pr[k].(string); ok && s != "" {
					text = s
					break
				}
			}
		} else if list, ok := p["primitives"].([]any); ok {
			if len(list) > 0 {
				if pm, ok := list[0].(map[string]any); ok {
					for _, k := range []string{"title", "text", "prompt_text", "message", "name"} {
						if s, ok := pm[k].(string); ok && s != "" {
							text = s
							break
						}
					}
				}
			}
			if text == "" {
				text = fmt.Sprintf("%d item", len(list))
			}
		}
	}
	if text == "" {
		if vm, ok := section.vm["view_model"].(map[string]any); ok {
			if tn, ok := vm["__typename"].(string); ok && tn != "" {
				text = strings.TrimPrefix(tn, "GenAI")
			}
		}
	}
	mt := richSubMessageTypeText
	return &richSubMessage{MessageType: &mt, MessageText: proto.String(text)}
}

// addContent menambahkan section (+ submessage) ke daftar node.
func (b *RichCardBuilder) addContent(section any, submessage any, opts richCardAddOptions) *RichCardBuilder {
	// Konsumsi opsi yang di-set lewat Opts() bila ada.
	if b.pendingOpts != nil {
		p := *b.pendingOpts
		b.pendingOpts = nil
		if opts.id == "" {
			opts.id = p.id
		}
		if opts.insertAt == "" {
			opts.insertAt = p.insertAt
			opts.insertAtOffset = p.insertAtOffset
		}
		if opts.replace == "" {
			opts.replace = p.replace
		}
	}
	hasReplace := opts.replace != ""
	hasInsertAt := opts.insertAt != ""
	if hasReplace && hasInsertAt {
		panic(&RichCardContentValidationError{RichCardError: newRichCardError("replace and insertAt cannot be used together", "CONTENT_VALIDATION")})
	}

	var sections []*richCardSection
	if section != nil {
		sections = b.validateSections(section)
	}
	if len(sections) == 0 {
		panic(&RichCardContentValidationError{RichCardError: newRichCardError("At least one section is required", "CONTENT_VALIDATION")})
	}

	subs := b.validateSubmessages(submessage)
	if len(subs) == 0 {
		// Klien hanya menampilkan section yang punya submessage (umumnya tipe
		// TEXT). Tanpa ini section "headless" tidak dirender sama sekali.
		subs = make([]*richSubMessage, len(sections))
		for i := range sections {
			subs[i] = richCardAutoSubText(sections[i])
		}
	}
	paired := b.pairSubmessages(sections, subs)

	if opts.id != "" && len(sections) != 1 {
		panic(&RichCardContentValidationError{RichCardError: newRichCardError("One id can only be assigned to one node", "CONTENT_VALIDATION")})
	}
	if opts.id != "" {
		if _, exists := b.idIndex[opts.id]; exists && !(hasReplace) {
			panic(&RichCardDuplicateIDError{RichCardError: newRichCardError("", "DUPLICATE_ID"), ID: opts.id})
		}
	}

	newNodes := make([]*richCardNode, len(sections))
	for i, s := range sections {
		id := ""
		if i == 0 {
			id = opts.id
		}
		newNodes[i] = b.makeNode(id, s, paired[i])
	}

	if hasReplace {
		if len(newNodes) != 1 {
			panic(&RichCardContentValidationError{RichCardError: newRichCardError("replace only supports adding exactly one node", "CONTENT_VALIDATION")})
		}
		idx := b.resolveNodeIndex(opts.replace)
		oldNode := b.nodes[idx]
		newNode := newNodes[0]
		if newNode.id == "" && oldNode.id != "" {
			newNode.id = oldNode.id
		}
		b.unregisterID(oldNode)
		b.nodes[idx] = newNode
		if newNode.id != "" {
			b.idIndex[newNode.id] = newNode
		}
		return b
	}

	if hasInsertAt {
		idx := b.resolveNodeIndex(opts.insertAt)
		insertIdx := idx + 1
		if opts.insertAtOffset < 0 {
			insertIdx = idx + opts.insertAtOffset
		}
		if insertIdx < 0 {
			insertIdx = 0
		}
		if insertIdx > len(b.nodes) {
			insertIdx = len(b.nodes)
		}
		rest := make([]*richCardNode, 0, len(b.nodes)-insertIdx)
		rest = append(rest, b.nodes[insertIdx:]...)
		b.nodes = append(b.nodes[:insertIdx], append(newNodes, rest...)...)
		for _, n := range newNodes {
			if n.id != "" {
				b.idIndex[n.id] = n
			}
		}
		return b
	}

	b.nodes = append(b.nodes, newNodes...)
	for _, n := range newNodes {
		if n.id != "" {
			b.idIndex[n.id] = n
		}
	}
	return b
}

// ─── Konten ──────────────────────────────────────────────────────

// AddText menambahkan seksi teks markdown. Link/cite/latex di dalam teks
// diekstrak menjadi inline entity.
func (b *RichCardBuilder) AddText(text string) *RichCardBuilder {
	extracted, entities := richCardExtractInlineEntities(text)
	// Submessage memakai teks ASLI, section memakai hasil ekstraksi
	// (dengan placeholder inline entity).
	mt := richSubMessageTypeText
	sub := &richSubMessage{
		MessageType: &mt,
		MessageText: proto.String(text),
	}
	prim := map[string]any{"text": extracted, "__typename": "GenAIMarkdownTextUXPrimitive"}
	if len(entities) > 0 {
		prim["inline_entities"] = entities
	}
	return b.addContent(richCardNewLayout("Single", prim, nil), sub, richCardAddOptions{})
}

// AddTextRaw menambahkan teks markdown mentah tanpa parsing entity apa pun.
func (b *RichCardBuilder) AddTextRaw(text string) *RichCardBuilder {
	if text == "" {
		return b
	}
	mt := richSubMessageTypeText
	sub := &richSubMessage{
		MessageType: &mt,
		MessageText: proto.String(text),
	}
	prim := map[string]any{"text": text, "__typename": "GenAIMarkdownTextUXPrimitive"}
	return b.addContent(richCardNewLayout("Single", prim, nil), sub, richCardAddOptions{})
}

// AddImage menambahkan seksi gambar dari URL.
func (b *RichCardBuilder) AddImage(urls ...string) *RichCardBuilder {
	return b.AddImageOpts(RichCardImageOptions{URLs: urls})
}

// AddImageOpts menambahkan seksi gambar dengan opsi lengkap.
func (b *RichCardBuilder) AddImageOpts(o RichCardImageOptions) *RichCardBuilder {
	var list []*richImageURL
	for _, u := range o.URLs {
		resolved := richCardResolveMedia(u)
		list = append(list, &waAICommonDeprecated.AIRichResponseImageURL{
			ImagePreviewURL: proto.String(resolved),
			ImageHighResURL: proto.String(resolved),
			SourceURL:       proto.String(resolved),
		})
	}
	if len(list) == 0 {
		return b
	}
	mt := richSubMessageTypeGridImg
	grid := &waAICommonDeprecated.AIRichResponseGridImageMetadata{
		GridImageURL: &waAICommonDeprecated.AIRichResponseImageURL{ImagePreviewURL: list[0].ImagePreviewURL},
		ImageURLs:    list,
	}
	sub := &waAICommonDeprecated.AIRichResponseSubMessage{
		MessageType:       &mt,
		GridImageMetadata: grid,
	}
	status := o.Status
	if status == "" {
		status = "READY"
	}
	var secs []*richCardSection
	for _, img := range list {
		media := map[string]any{
			"url":       img.GetImagePreviewURL(),
			"mime_type": "image/png",
		}
		if o.Width > 0 {
			media["width"] = o.Width
		}
		if o.Height > 0 {
			media["height"] = o.Height
		}
		st := map[string]any{"status": status}
		if o.UpdateText != "" {
			st["update_text"] = o.UpdateText
		}
		secs = append(secs, richCardNewLayout("Single", map[string]any{
			"media":        media,
			"imagine_type": "IMAGE",
			"status":       st,
			"__typename":   "GenAIImaginePrimitive",
		}, nil))
	}
	return b.addContent(secs, sub, richCardAddOptions{})
}

// ─── Inline entity extraction ────────────────────────────────────

// richCardInlineEntityOptions mengatur perilaku ekstraksi inline entity.
type richCardInlineEntityOptions struct {
	Extract   bool
	Hyperlink bool
	Citation  bool
	Latex     bool
}

// richCardExtractInlineEntities mengubah markdown link/cite/latex menjadi
// placeholder + daftar entity.
func richCardExtractInlineEntities(text string) (string, []map[string]any) {
	return richCardExtractInlineEntitiesOpts(text, richCardInlineEntityOptions{Extract: true, Hyperlink: true, Citation: true, Latex: true})
}

// richCardExtractInlineEntitiesOpts adalah varian dengan opsi eksplisit.
func richCardExtractInlineEntitiesOpts(text string, o richCardInlineEntityOptions) (string, []map[string]any) {
	if !o.Extract || text == "" || !strings.Contains(text, "[") {
		return text, nil
	}
	var entities []map[string]any
	var result strings.Builder
	last := 0
	citationIndex := 1
	hyperlinkIndex := 0
	latexIndex := 0
	var stack []int

	for i := 0; i < len(text); i++ {
		c := text[i]
		prev := byte(0)
		if i > 0 {
			prev = text[i-1]
		}
		if c == '[' && prev != '\\' {
			stack = append(stack, i)
			continue
		}
		if c != ']' || i+1 >= len(text) || (text[i+1] != '(' && text[i+1] != '<') {
			continue
		}
		if len(stack) == 0 {
			continue
		}
		start := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		open := text[i+1]
		closeChar := byte(')')
		isLatex := false
		if open == '<' {
			closeChar = '>'
			isLatex = true
		}

		end := i + 2
		depth := 1
		for end < len(text) && depth > 0 {
			if text[end] == open && (end == 0 || text[end-1] != '\\') {
				depth++
			} else if text[end] == closeChar && (end == 0 || text[end-1] != '\\') {
				depth--
			}
			end++
		}
		if depth != 0 {
			continue
		}

		raw := strings.TrimSpace(text[start+1 : i])
		url := strings.TrimSpace(text[i+2 : end-1])

		var key, tag string
		var entity map[string]any

		if isLatex {
			parts := strings.Split(raw, "|")
			txt := ""
			if len(parts) > 0 {
				txt = parts[0]
			}
			var width, height, fontHeight, padding string
			if len(parts) > 1 {
				width = parts[1]
			}
			if len(parts) > 2 {
				height = parts[2]
			}
			if len(parts) > 3 {
				fontHeight = parts[3]
			}
			if len(parts) > 4 {
				padding = parts[4]
			}
			if !o.Latex {
				continue
			}
			key = fmt.Sprintf("LATEX_ENTITY_%d", latexIndex)
			latexIndex++
			display := txt
			if display == "" {
				display = "image"
			}
			tag = "{{" + key + "}}" + display + "{{/" + key + "}}"
			entity = map[string]any{
				"key": key,
				"metadata": map[string]any{
					"latex_expression": txt,
					"latex_image": map[string]any{
						"url":    url,
						"width":  richCardNumOr(width, 100),
						"height": richCardNumOr(height, 100),
					},
					"font_height": richCardNumOr(fontHeight, 83.333333333333),
					"padding":     richCardNumOr(padding, 15),
					"__typename":  "GenAILatexItem",
				},
			}
		} else if raw != "" {
			if !o.Hyperlink {
				continue
			}
			trusted := !strings.HasPrefix(url, "!")
			if !trusted {
				url = url[1:]
			}
			key = fmt.Sprintf("HYPERLINK_ENTITY_%d", hyperlinkIndex)
			hyperlinkIndex++
			tag = "{{" + key + "}}" + url + "{{/" + key + "}}"
			entity = map[string]any{
				"key": key,
				"metadata": map[string]any{
					"display_name": raw,
					"is_trusted":   trusted,
					"url":          url,
					"__typename":   "GenAIInlineLinkItem",
				},
			}
		} else {
			key = fmt.Sprintf("CITATION_ENTITY_%d", citationIndex-1)
			tag = "{{" + key + "}}" + url + "{{/" + key + "}}"
			entity = map[string]any{
				"key": key,
				"metadata": map[string]any{
					"reference_id":           citationIndex,
					"reference_url":          url,
					"reference_title":        url,
					"reference_display_name": url,
					"sources":                []any{},
					"__typename":             "GenAISearchCitationItem",
				},
			}
			citationIndex++
		}

		result.WriteString(text[last:start])
		result.WriteString(tag)
		last = end
		entities = append(entities, entity)
		i = end - 1
	}
	result.WriteString(text[last:])
	return result.String(), entities
}

// ─── Build & send ────────────────────────────────────────────────

// forwardContext menyusun ContextInfo untuk rich response.
func (b *RichCardBuilder) forwardContext() *waE2E.ContextInfo {
	ctxInfo := &waE2E.ContextInfo{}
	if b.forwarded {
		ctxInfo.ForwardingScore = proto.Uint32(1)
		ctxInfo.IsForwarded = proto.Bool(true)
		ctxInfo.ForwardedAiBotMessageInfo = &waAICommon.ForwardedAIBotMessageInfo{
			BotJID: proto.String(func() string {
				if b.forwardBotJID != "" {
					return b.forwardBotJID
				}
				return types.NewMetaAIJID.String()
			}()),
		}
		ctxInfo.ForwardOrigin = waE2E.ContextInfo_META_AI.Enum()
	}
	if b.quoteID != "" {
		ctxInfo.StanzaID = proto.String(b.quoteID)
		ctxInfo.Participant = proto.String(b.quoteParticipant)
		ctxInfo.QuotedMessage = b.quoteMessage
		ctxInfo.QuotedType = waE2E.ContextInfo_EXPLICIT.Enum()
	}
	if len(b.mentions) > 0 {
		ctxInfo.MentionedJID = b.mentions
	}
	return ctxInfo
}

// wrapRich membungkus rich response menjadi pesan botForwardedMessage.
func (b *RichCardBuilder) wrapRich(rich *richResponseMessage) *waE2E.Message {
	messageContextInfo := &waE2E.MessageContextInfo{
		DeviceListMetadata:        &waE2E.DeviceListMetadata{},
		DeviceListMetadataVersion: proto.Int32(2),
	}
	if b.useMessageSecret {
		messageContextInfo.MessageSecret = b.messageSecret
	}
	messageContextInfo.AccountEncryptionAttestation = &waAea.NonE2EEAttestation{
		AccountType: waAea.NonE2EEAttestation_NON_E2EE.Enum(),
	}

	return &waE2E.Message{
		MessageContextInfo: messageContextInfo,
		BotForwardedMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{RichResponseMessage: rich},
		},
	}
}

// buildRich menyusun payload rich response (submessages + unifiedResponse).
func (b *RichCardBuilder) buildRich() *richResponseMessage {
	msgType := richResponseTypeStandard
	rich := &waE2E.AIRichResponseMessage{
		MessageType: &msgType,
	}
	if b.includesSubmessages {
		rich.Submessages = b.submessages()
	}
	if !b.noContextInfo {
		rich.ContextInfo = b.forwardContext()
	}

	if b.includesUnifiedResponse {
		var urJSON []byte
		if len(b.customUnifiedResponseData) > 0 {
			urJSON = b.customUnifiedResponseData
		} else {
			sections := make([]map[string]any, 0, len(b.nodes)+1)
			for _, s := range b.sections() {
				sections = append(sections, s.vm)
			}
			if b.footer != "" {
				sections = append(sections, richCardNewLayout("Single", map[string]any{
					"text":       b.footer,
					"__typename": "GenAIMetadataTextPrimitive",
				}, nil).vm)
			}

			ur := map[string]any{"response_id": b.responseID, "sections": sections}
			var err error
			urJSON, err = richCardMarshalJSON(ur)
			if err != nil {
				urJSON = []byte(`{"response_id":"","sections":[]}`)
			}
		}
		rich.UnifiedResponse = &richUnifiedResponse{Data: urJSON}
	}

	return rich
}

// Build merakit pesan rich response final.
func (b *RichCardBuilder) Build() *waE2E.Message {
	b.refreshDynamicIDs()
	b.runPending()
	return b.wrapRich(b.buildRich())
}

// BuildEdit menyusun pesan edit (MESSAGE_EDIT) untuk pesan rich response
// yang sudah terkirim.
func (b *RichCardBuilder) BuildEdit(chat types.JID, id types.MessageID, msg *waE2E.Message) *waE2E.Message {
	if msg == nil {
		msg = b.Build()
	}
	return &waE2E.Message{
		BotForwardedMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{
				ProtocolMessage: &waE2E.ProtocolMessage{
					Key: &waCommon.MessageKey{
						RemoteJID: proto.String(chat.String()),
						FromMe:    proto.Bool(true),
						ID:        proto.String(id),
					},
					Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
					EditedMessage: msg,
				},
			},
		},
	}
}

// LastMessageKey mengembalikan key pesan terakhir yang dikirim via Send().
func (b *RichCardBuilder) LastMessageKey() (types.JID, types.MessageID) {
	return b.lastMessageKey.jid, b.lastMessageKey.id
}

// SendEdit mengirim edit dari pesan rich response sebelumnya.
func (b *RichCardBuilder) SendEdit(to types.JID, id types.MessageID, msg *waE2E.Message) error {
	if to.IsEmpty() {
		to = b.lastMessageKey.jid
	}
	if id == "" {
		id = b.lastMessageKey.id
	}
	if to.IsEmpty() {
		return fmt.Errorf("JID is required")
	}
	if id == "" {
		return fmt.Errorf("Message id is required")
	}
	_, err := b.client.SendMessage(context.Background(), to, b.BuildEdit(to, id, msg))
	return err
}

// Send membangun, mengirim, lalu (bila bypassDownload aktif) mengirim edit
// lanjutan dengan konten yang sama.
func (b *RichCardBuilder) Send(to types.JID) (types.MessageID, error) {
	msg := b.Build()
	resp, err := b.client.SendMessage(context.Background(), to, msg)
	if err != nil {
		return "", err
	}
	b.lastMessageKey = richCardMessageKey{jid: to, id: resp.ID}
	if b.bypassDownload && b.includesUnifiedResponse {
		_ = b.SendEdit(to, resp.ID, msg)
	}
	return resp.ID, nil
}

// runPending menjalankan antrean pekerjaan lazy (jika ada).
func (b *RichCardBuilder) runPending() {
	if len(b.pending) == 0 {
		return
	}
	todo := b.pending
	b.pending = nil
	for _, fn := range todo {
		fn()
	}
}
