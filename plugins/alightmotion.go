package main

// alightmotion.go — Alight Motion Premium activation (port dari snippet.js)
//
// Commands:
//   .am <email>                        → kirim link verifikasi ke email
//   .am <email> | <link_verifikasi>    → verifikasi + aktifkan premium
//
// Cara kerja:
//   1. Kirim email link verifikasi via Firebase Identity Toolkit
//   2. User buka email → copy link verifikasi
//   3. User kirim: .am email@example.com | <link>
//   4. Bot verify link → login → aktifkan premium via verifyPurchase

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/polymorfa/hypermeow"
	"github.com/polymorfa/hypermeow/types/events"
)

// amApiKey returns the API key used by the identity endpoints.
// It is deployment specific: set ALIGHT_API_KEY in .env.
func amApiKey() string { return strings.TrimSpace(os.Getenv("ALIGHT_API_KEY")) }

// amIdtURL returns the Google Identity Toolkit base URL.
func amIdtURL() string { return googleIdentityBaseURL() }

// amStkURL returns the Google Secure Token base URL.
func amStkURL() string { return googleSecureTokenBaseURL() }

// amVfyURL returns the purchase-verification endpoint.
func amVfyURL() string { return envBase("ALIGHT_VERIFY_URL") }

// amContinueURL returns the continue URL used during sign-in.
func amContinueURL() string {
	if base := alightBaseURL(); base != "" {
		return base
	}
	return "http://localhost"
}

var amHeaders1 = map[string]string{
	"Content-Type":      "application/json",
	"x-android-package": "com.alightcreative.motion",
	"x-android-cert":    "ECA6BF91B8715A6F810ED0BBFC65B6CD578F52A8",
	"User-Agent":        "dalvik/2.1.0 (linux; u; android 15; 23127pn0cc build/bp1a.250505.005)",
}

var amHeaders2 = map[string]string{
	"Content-Type":    "application/json; charset=utf-8",
	"User-Agent":      "okhttp/3.12.1",
	"Accept-Encoding": "gzip",
}

// ─── Session state ──────────────────────────────────────────────────

type AMSession struct {
	Email string
	ID    string
	Ref   string
	UID   string
	Pro   bool
}

var (
	amSessionMu sync.Mutex
	amSessions  = map[string]*AMSession{} // key = sender JID
)

func amGetSession(sender string) *AMSession {
	amSessionMu.Lock()
	defer amSessionMu.Unlock()
	return amSessions[sender]
}

func amSetSession(sender string, s *AMSession) {
	amSessionMu.Lock()
	defer amSessionMu.Unlock()
	amSessions[sender] = s
}

// ─── Helpers ────────────────────────────────────────────────────────

func amDip() string {
	// Generate random IP
	randInt := func(min, max int) int {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(max-min)))
		return int(n.Int64()) + min
	}
	return fmt.Sprintf("%d.%d.%d.%d", randInt(1, 255), randInt(0, 255), randInt(0, 255), randInt(1, 255))
}

func amSp(headers map[string]string) map[string]string {
	out := make(map[string]string)
	for k, v := range headers {
		out[k] = v
	}
	out["x-forwarded-for"] = amDip()
	out["x-real-ip"] = amDip()
	out["client-ip"] = amDip()
	out["x-client-ip"] = amDip()
	out["x-originating-ip"] = amDip()
	out["x-cluster-client-ip"] = amDip()
	return out
}

func amPost(ctx context.Context, urlStr string, body interface{}, headers map[string]string) ([]byte, int, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", urlStr, bytes.NewReader(jsonBody))
	for k, v := range amSp(headers) {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}

// ─── API functions ─────────────────────────────────────────────────

// amLink sends verification email link to the given email.
func amLink(email string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Step 1: createAuthUri
	_, _, _ = amPost(ctx, amIdtURL()+"/createAuthUri?key="+amApiKey(), map[string]interface{}{
		"identifier":  email,
		"continueUri": "http://localhost",
	}, amHeaders1)

	// Step 2: getOobConfirmationCode (send email link)
	_, code, err := amPost(ctx, amIdtURL()+"/getOobConfirmationCode?key="+amApiKey(), map[string]interface{}{
		"requestType":           6,
		"email":                 email,
		"androidInstallApp":     true,
		"canHandleCodeInApp":    true,
		"continueUrl":           amContinueURL(),
		"iosBundleId":           "com.alightcreative.motion",
		"androidPackageName":    "com.alightcreative.motion",
		"androidMinimumVersion": "585",
		"clientType":            "CLIENT_TYPE_ANDROID",
	}, amHeaders1)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("HTTP %d", code)
	}
	return nil
}

// amExtractCode extracts oobCode from a URL or raw string.
func amExtractCode(raw string) string {
	if raw == "" {
		return ""
	}
	s := strings.TrimSpace(raw)
	// Try URL parsing
	if u, err := url.Parse(s); err == nil {
		if c := u.Query().Get("oobCode"); c != "" {
			return c
		}
		// Try nested link param
		if nested := u.Query().Get("link"); nested != "" {
			if u2, err := url.Parse(nested); err == nil {
				if c := u2.Query().Get("oobCode"); c != "" {
					return c
				}
			}
		}
	}
	// Try regex match
	re := regexp.MustCompile(`oobCode=([a-zA-Z0-9_-]+)`)
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	// Try as raw token
	if len(s) >= 10 && !strings.Contains(s, "://") {
		re2 := regexp.MustCompile(`^[a-zA-Z0-9_-]{10,}$`)
		if re2.MatchString(s) {
			return s
		}
	}
	return ""
}

// amAuth verifies the email link and returns tokens.
func amAuth(email, rawCode string) (idToken, refreshToken, localID string, isNew bool, err error) {
	oobCode := amExtractCode(rawCode)
	if oobCode == "" {
		return "", "", "", false, fmt.Errorf("kode verifikasi tidak ditemukan di link")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// emailLinkSignin
	respBody, code, err := amPost(ctx, amIdtURL()+"/emailLinkSignin?key="+amApiKey(), map[string]interface{}{
		"email":      email,
		"oobCode":    oobCode,
		"clientType": "CLIENT_TYPE_ANDROID",
	}, amHeaders1)
	if err != nil {
		return "", "", "", false, err
	}
	if code != 200 {
		return "", "", "", false, fmt.Errorf("login gagal: HTTP %d: %s", code, string(respBody))
	}

	var loginResp struct {
		IDToken      string `json:"idToken"`
		RefreshToken string `json:"refreshToken"`
		LocalID      string `json:"localId"`
		IsNewUser    bool   `json:"isNewUser"`
	}
	if err := json.Unmarshal(respBody, &loginResp); err != nil {
		return "", "", "", false, fmt.Errorf("parse login response: %w", err)
	}
	if loginResp.IDToken == "" {
		return "", "", "", false, fmt.Errorf("idToken kosong")
	}
	return loginResp.IDToken, loginResp.RefreshToken, loginResp.LocalID, loginResp.IsNewUser, nil
}

// amPro activates premium via verifyPurchase.
func amPro(idToken string) (orderID string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Generate random order ID
	orderBytes := make([]byte, 6)
	_, _ = rand.Read(orderBytes)
	orderID = "neo-" + fmt.Sprintf("%x", orderBytes)

	body := map[string]interface{}{
		"data": map[string]interface{}{
			"productId": "am.full.sub.annual.19q4",
			"token":     "mmgaobamlahbbeccfplmbkbb.AO-J1OzqG0or_GJJIx-ms8GrTm-jaglCRfhQSRPUZKpl2YspYS-oN7_94uv8RC5vQbvd_Ios2pPDStZ2n7F0hLE3FiOU7HS3R6Fquulv5xLXFECSv4ctElw",
			"skuType":   "subs",
			"orderId":   orderID,
		},
	}

	headers := make(map[string]string)
	for k, v := range amHeaders2 {
		headers[k] = v
	}
	headers["authorization"] = "Bearer " + idToken
	headers["firebase-instance-id-token"] = "cSDnCyp3T-uwp07z3tL86T:APA91bFkmvvsHw5nnqa1SBFci-99DRsKClLiETdRrVcJjS5yBx1v_FbCb1d8WhBuea_zmwnYBktyTIzcRhN4b6uNOUur9wPc0gKXmJDoZic0LhNq5V2s0xI"

	respBody, code, err := amPost(ctx, amVfyURL(), body, headers)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("verifyPurchase gagal: HTTP %d: %s", code, string(respBody))
	}
	return orderID, nil
}

// ─── Command handler ──────────────────────────────────────────────

func handleAMCommand(client *whatsmeow.Client, msg *events.Message, args string) string {
	if !isOwnerMsg(msg) {
		return "❌ Hanya owner"
	}
	if args == "" {
		return `🎬 *Alight Motion Premium*

❓ *Cara pakai:*
• Kirim link verifikasi: .am email@example.com
• Verifikasi + premium (setelah dapat link dari email): .am email@example.com | <link_verifikasi>`
	}

	sender := msg.Info.Sender.String()

	// Check if it contains | (email | link)
	if strings.Contains(args, "|") {
		parts := strings.SplitN(args, "|", 2)
		email := strings.TrimSpace(parts[0])
		linkURL := strings.TrimSpace(parts[1])

		if email == "" || linkURL == "" {
			return "❌ Format salah\n❓ *Cara pakai:* .am email@example.com | <link_verifikasi>"
		}

		// Auth
		idToken, refreshToken, uid, isNew, err := amAuth(email, linkURL)
		if err != nil {
			return fmt.Sprintf("❌ Gagal verifikasi: %v", err)
		}

		// Pro
		orderID, err := amPro(idToken)
		if err != nil {
			return fmt.Sprintf("✅ *Login Berhasil*\n\n• Email: %s\n• Akun baru: %v\n\n❌ Gagal premium: %v", email, isNew, err)
		}

		// Save session
		amSetSession(sender, &AMSession{
			Email: email,
			ID:    idToken,
			Ref:   refreshToken,
			UID:   uid,
			Pro:   true,
		})

		return fmt.Sprintf("✅ *Premium Berhasil*\n\n• Email: %s\n• Order: %s\n• Akun baru: %v\n• Status: Active\n\n🎉 Akun Alight Motion telah dipremium!", email, orderID, isNew)
	}

	// No | → just email → send verification link
	email := strings.TrimSpace(args)
	if !strings.Contains(email, "@") {
		return "❌ Format tidak valid\n❓ *Cara pakai:*\n• .am email@example.com\n• .am email@example.com | <link>"
	}

	// Send link
	err := amLink(email)
	if err != nil {
		return fmt.Sprintf("❌ Gagal mengirim link: %v", err)
	}

	// Save email in session
	amSetSession(sender, &AMSession{Email: email})

	return fmt.Sprintf("✅ *Link Verifikasi Terkirim*\n\n• Email: %s\n• Cek inbox atau folder spam\n• Copy link verifikasi, lalu gunakan: `.am %s | <link>`", email, email)
}

func init() {
	RegisterPlugin(Plugin{
		Command:     "am",
		Aliases:     []string{"amprem"},
		Description: "am",
		Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
			return handleAMCommand(client, msg, args)
			// ─── Say (fake message from target user) ───────────────
		},
	})
}
