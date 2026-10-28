package main

// endpoints.go — Third-party service endpoints (all environment driven).
// ════════════════════════════════════════════════════════════
//
// The bot talks to a handful of external services (download helpers, lyric
// lookup, geo-IP, text completion, …). None of those endpoints are baked
// into the source: every one of them is read from an environment variable.
//
// An empty value means "integration not configured". Plugins that need one
// check for it and return a clear, actionable error instead of silently
// contacting somebody else's server.
//
//	DL_BASE_URL                download worker (aiodl-style API)
//	KANGWIFI_API_BASE          media / metadata API
//	POLLINATIONS_BASE          OpenAI-compatible text completion
//	TIKWM_API_BASE             TikTok / Instagram / Facebook downloader
//	COBALT_API_BASE            cobalt media downloader
//	VIDEODOWNLOADERTOOLS_BASE  fallback media downloader
//	YT_LOADER_BASE             YouTube Music download worker
//	SPOTIFY_OEMBED_BASE        Spotify oEmbed metadata
//	IP_API_BASE                geo-IP lookup
//	LRCLIB_BASE                lyrics lookup
//	SHORTLINK_BASE             URL shortener
//	ADS_SOURCE_URL             remote ad/notice source
//	GROQ_API_BASE              Groq (OpenAI-compatible) API
//	GOOGLE_IDENTITY_BASE       Google Identity Toolkit
//	GOOGLE_SECURETOKEN_BASE    Google Secure Token
//	GOOGLE_TTS_BASE            Google Translate text-to-speech
//	GEMINI_BASE                Gemini web client
//	ALIGHT_BASE                Alight Motion API

import (
	"os"
	"strings"
)

// envBase returns a trimmed endpoint base URL from the environment.
// Trailing slashes are removed so callers can append "/path" safely.
func envBase(name string) string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv(name)), "/")
}

// envRequired returns the endpoint or an error explaining which variable is
// missing.
func envRequired(name string) (string, error) {
	base := envBase(name)
	if base == "" {
		return "", &envMissingError{Name: name}
	}
	return base, nil
}

// envMissingError reports an endpoint that was not configured.
type envMissingError struct {
	Name string
}

func (e *envMissingError) Error() string {
	return e.Name + " is not configured — set it in .env to enable this feature"
}

// ─── Endpoint helpers ───────────────────────────────────────────

// dlBaseURL returns the configured download worker base URL.
func dlBaseURL() string { return envBase("DL_BASE_URL") }

// kangwifiBaseURL returns the configured media API base URL.
func kangwifiBaseURL() string { return envBase("KANGWIFI_API_BASE") }

// pollinationsBaseURL returns the configured text-completion base URL.
func pollinationsBaseURL() string { return envBase("POLLINATIONS_BASE") }

// tikwmBaseURL returns the configured media downloader base URL.
func tikwmBaseURL() string { return envBase("TIKWM_API_BASE") }

// cobaltBaseURL returns the configured cobalt downloader base URL.
func cobaltBaseURL() string { return envBase("COBALT_API_BASE") }

// videoDownloaderToolsBaseURL returns the configured fallback downloader URL.
func videoDownloaderToolsBaseURL() string { return envBase("VIDEODOWNLOADERTOOLS_BASE") }

// ytLoaderBaseURL returns the configured YouTube Music download worker URL.
func ytLoaderBaseURL() string { return envBase("YT_LOADER_BASE") }

// spotifyEmbedBaseURL returns the configured Spotify oEmbed base URL.
func spotifyEmbedBaseURL() string { return envBase("SPOTIFY_OEMBED_BASE") }

// ipAPIBaseURL returns the configured geo-IP lookup base URL.
func ipAPIBaseURL() string { return envBase("IP_API_BASE") }

// lrclibBaseURL returns the configured lyrics lookup base URL.
func lrclibBaseURL() string { return envBase("LRCLIB_BASE") }

// shortlinkBaseURL returns the configured URL shortener base URL.
func shortlinkBaseURL() string { return envBase("SHORTLINK_BASE") }

// adsSourceURL returns the configured remote notice source.
func adsSourceURL() string { return envBase("ADS_SOURCE_URL") }

// groqAPIBaseURL returns the configured Groq API base URL.
func groqAPIBaseURL() string { return envBase("GROQ_API_BASE") }

// googleIdentityBaseURL returns the configured Google Identity Toolkit URL.
func googleIdentityBaseURL() string { return envBase("GOOGLE_IDENTITY_BASE") }

// googleSecureTokenBaseURL returns the configured Google Secure Token URL.
func googleSecureTokenBaseURL() string { return envBase("GOOGLE_SECURETOKEN_BASE") }

// googleTTSBaseURL returns the configured Google TTS base URL.
func googleTTSBaseURL() string { return envBase("GOOGLE_TTS_BASE") }

// geminiBaseURL returns the configured Gemini web base URL.
func geminiBaseURL() string { return envBase("GEMINI_BASE") }

// alightBaseURL returns the configured Alight Motion base URL.
func alightBaseURL() string { return envBase("ALIGHT_BASE") }
