# 📐 Architecture Map — hyper-go

> **159 Go files** across five directories, one `package main`.
> This document maps every file to its role so you can navigate the codebase
> without grepping. Files are grouped by *logical layer*, not by filename.

---

## 🗂️ Logical Layers

```
┌────────────────────────────────────────────────────┐
│  Entry & Runtime                                   │
│  main.go · config.go · envload.go · logger.go      │
│  shutdown.go · profiling.go · eventbus.go          │
│  storage.go · endpoints.go · task.go · result.go   │
├────────────────────────────────────────────────────┤
│  WhatsApp I/O                                      │
│  session_sqlite.go · jidutil.go · webui.go         │
│  humanmsg.go · a2ui.go · a2ui_nodes.go             │
│  shortlink.go · own.go · say.go · own.go           │
├────────────────────────────────────────────────────┤
│  Plugin System                                     │
│  plugin.go · plugin_lifecycle.go · menu.go         │
├────────────────────────────────────────────────────┤
│  Storage                                           │
│  pg.go · pg_postgres.go · pg_sqlite.go             │
│  pg_group.go · filestore.go · cache.go             │
├────────────────────────────────────────────────────┤
│  Concurrency Primitives                            │
│  errgroup.go · pool.go · retry.go · ratelimit.go   │
│  backpressure.go · context_helper.go · task.go     │
├────────────────────────────────────────────────────┤
│  Core Features (built-in)                          │
│  selfmode.go · guard.go · afk.go · delcmd.go       │
│  groupcmd.go · extras.go · autopresence.go         │
│  maker_go.go · stickers.go · stickerwm.go          │
├────────────────────────────────────────────────────┤
│  Plugin Features (composable, mostly API-backed)   │
│  ai*.go (12 files) · spr*.go (8 files)             │
│  crm*.go (4 files) · stickers.go · jjam.go         │
│  lacak.go · media_dl.go · tiktok_engine.go         │
│  yt2.go · ytmp3.go · ytmusic_engine.go             │
│  kangwifi*.go · gameserver.go · call.go            │
├────────────────────────────────────────────────────┤
│  Security & Anti-Abuse                             │
│  antibot.go · antifdmsg.go · anti_spr.go           │
│  dmsg_fakemsg.go · dmsgva.go · pinva.go            │
│  auditlog.go · healthcheck.go · sysinfo.go         │
│  observer.go · metrics.go · responsemode.go        │
├────────────────────────────────────────────────────┤
│  OS / Runtime helpers                              │
│  rusage_unix.go · rusage_other.go · util.go        │
│  generic_helpers.go · rss.go                       │
└────────────────────────────────────────────────────┘
```

---

## 📋 File Index

### `core/` — entry, runtime, core features (50 files)

| File | Lines | Layer | Role |
|------|------:|-------|------|
| `main.go` | 752 | Entry | Startup: storage → file stores → session → web UI → connect → event loop |
| `config.go` | 319 | Entry | `data/config.json` with env overrides (lacak / WS / Groq) |
| `envload.go` | 71 | Entry | Minimal `.env` reader (no external dependency) |
| `endpoints.go` | 117 | Entry | Third-party endpoint registry — every URL comes from the environment |
| `storage.go` | 73 | Entry | Backend selection (`postgres` / `sqlite` / `auto`) and paths |
| `logger.go` | 47 | Entry | Logging helpers |
| `shutdown.go` | 64 | Entry | Context-based graceful shutdown |
| `profiling.go` | 103 | Entry | pprof / runtime profiling endpoints |
| `eventbus.go` | 111 | Entry | Decoupled internal event bus |
| `session_sqlite.go` | 108 | WA I/O | whatsmeow session store over the selected backend |
| `jidutil.go` | 382 | WA I/O | JID parsing/formatting (`@s.whatsapp.net`, `@lid`, `@g.us`) |
| `webui.go` | 576 | WA I/O | Local web UI that renders the pairing QR |
| `humanmsg.go` | 185 | WA I/O | Human-readable message rendering |
| `a2ui.go` | 472 | WA I/O | Alternative UI: buttons, lists, native layouts |
| `a2ui_nodes.go` | 47 | WA I/O | A2UI node builders |
| `shortlink.go` | 151 | WA I/O | URL shortener (`SHORTLINK_BASE`) |
| `own.go` | 168 | WA I/O | Owner identity helpers (multi-owner) |
| `say.go` | 238 | WA I/O | `.say` — send a message as the bot |
| `plugin.go` | 237 | Plugins | `Plugin` struct, `RegisterPlugin`, registry |
| `plugin_lifecycle.go` | 399 | Plugins | Idle auto-kill, sweeper, per-category on/off |
| `menu.go` | 98 | Plugins | `.menu` — command catalogue by category |
| `filestore.go` | 240 | Storage | JSON file store (levels, guard, afk, kv) |
| `cache.go` | 118 | Storage | Generic TTL cache |
| `errgroup.go` | 45 | Concurrency | `errgroup` helper |
| `pool.go` | 42 | Concurrency | Bounded worker pool |
| `retry.go` | 61 | Concurrency | Retry with backoff |
| `ratelimit.go` | 137 | Concurrency | Per-user rate limiter |
| `backpressure.go` | 116 | Concurrency | Backpressure / load shedding |
| `context_helper.go` | 63 | Concurrency | Context utilities |
| `task.go` | 155 | Concurrency | Cancellable background tasks |
| `result.go` | 105 | Concurrency | Generic result type |
| `selfmode.go` | 811 | Core | Self mode, per-group trust, group ACL |
| `guard.go` | 441 | Core | Per-group guards: anti-link, anti-spam, welcome |
| `afk.go` | 144 | Core | AFK state and auto-reply |
| `delcmd.go` | 134 | Core | `.del` — delete messages |
| `groupcmd.go` | 138 | Core | Group metadata commands |
| `extras.go` | 89 | Core | Quote, facts, group info, hidetag |
| `autopresence.go` | 216 | Core | Auto-read / auto-typing presence |
| `maker_go.go` | 167 | Core | Maker helpers (text → image) |
| `stickers.go` | 1580 | Core | Sticker maker (image → WEBP, text → WEBP) |
| `stickerwm.go` | 366 | Core | Sticker watermark |
| `circuit.go` | 116 | Core | Circuit breaker for flaky AI backends |
| `antibot_mask.go` | 33 | Core | Message masking helpers |
| `undecryptable_track.go` | 81 | Core | Track undecryptable messages |
| `dl_kangwifi.go` | 354 | Core | Download worker client (aiodl-style) |
| `lrclib_go.go` | 71 | Core | Lyrics lookup client |
| `socks5_go.go` | 122 | Core | Optional SOCKS5 egress proxy |
| `tiktok_go.go` | 155 | Core | TikTok helpers |
| `yt_search_go.go` | 106 | Core | YouTube search + oEmbed |
| `meowcaller_init.go` | 17 | Core | meowcaller VoIP bootstrap |

### `plugins/` — feature plugins (87 files)

| File | Lines | Role |
|------|------:|------|
| `ai_agentic.go` | 1961 | The main AI brain: prompt building, tool loop, fallbacks |
| `ai_agent.go` | 802 | AI agent session state and command handling |
| `ai_call.go` | 869 | Voice: speech-to-text (Groq) + call flow |
| `ai_agentic_extra.go` | 615 | Extra AI backends (OpenAI-compatible, GEMINI fallback) |
| `ai_feedback.go` | 527 | AI answer feedback loop |
| `ai_roles.go` | 454 | Role/persona presets |
| `ai_pp_tools.go` | 461 | AI tool plugins (image gen, search, …) |
| `ai_fast.go` | 265 | Low-latency AI path |
| `ai_actions.go` | 165 | AI-triggered actions |
| `ai_run_go.go` | 318 | AI runner helpers |
| `autoai.go` | 82 | `.autoai` — auto AI per chat |
| `call.go` | 1597 | WhatsApp VoIP calling (meowcaller) |
| `playcall.go` | 161 | Play audio into a call |
| `spr.go` | 1772 | SPR (Secret Private Relay) engine |
| `spr_metrics.go` | 1262 | Sender-key metrics, percentiles (dialect-aware SQL) |
| `spr2.go` | 358 | SPR variant 2 |
| `spr_cloak.go` | 341 | SPR cloaking |
| `spr_senderkey.go` | 148 | Sender-key bookkeeping |
| `selfspr.go` | 436 | Self-directed SPR |
| `crm.go` | 1254 | CRM: capture messages for replay |
| `crm_envelope.go` | 819 | CRM envelope encoding |
| `crm_toggle.go` | 113 | CRM per-group toggles |
| `stickers.go` | 1512 | Sticker commands (send, search, pack) |
| `sticker_engine.go` | 806 | Sticker rendering engine |
| `sticker_cmd.go` | 196 | Sticker command routing |
| `asticker.go` | 143 | Animated sticker handling |
| `lottie.go` | 118 | Lottie sticker reactions |
| `lacak.go` | 1592 | Lacak: OSINT-style message tracker |
| `media_dl.go` | 601 | Media download orchestration |
| `download_fix.go` | 155 | Download fallbacks (tikwm-style, cobalt) |
| `tiktok_engine.go` | 1177 | TikTok download engine |
| `tiktok_cmd.go` | 151 | TikTok commands |
| `tt2.go` | 91 | TikTok variant 2 |
| `yt2.go` | 980 | YouTube download engine |
| `ytmp3.go` | 476 | MP3 conversion helpers |
| `ytmp3_cmd.go` | 201 | `.ytmp3` command |
| `ytmusic_engine.go` | 793 | YouTube Music engine |
| `jjam.go` | 1099 | Alight Motion template rendering flow |
| `gameserver.go` | 917 | Game / live streaming WebSocket server |
| `playmusic.go` | 379 | Music search + playback |
| `kangwifi.go` | 350 | Media API client |
| `kangwifi_dl.go` | 117 | Shared downloader handler |
| `kangrouter.go` | 199 | Model routing for AI |
| `richcard.go` | 1279 | Rich-card message builder (AI rich responses) |
| `compress.go` | 494 | Video compression |
| `kompres.go` | 331 | `.kompres` — compress a replied video |
| `upscale.go` | 246 | Upscale video/photo |
| `tag_forward.go` | 565 | Tag forwarding |
| `qresend.go` | 313 | Resend a quoted message |
| `sbpr.go` | 779 | Status broadcast family |
| `ads.go` | 190 | Album + ad-reply greeting message |
| `afterread.go` | 58 | Send text that disappears after being read |
| `textone.go` | 56 | View-once text |
| `gemini_free.go` | 476 | Gemini web client (cookie + bl bootstrap) |
| `alightmotion.go` | 360 | Alight Motion activation flow |
| `autodel.go` | 319 | Per-group auto-delete mode (disappearing messages) |
| `latency_exp.go` | 149 | Latency experiments: keepalive tuning, TCP_NODELAY, RTT monitoring |
| `latency_warm.go` | 132 | Latency warm-up: pre-distribute sender keys, cache group info |
| `stealth_primary.go` | 402 | `.stealth primary` — deliver only to the primary device |
| `fun.go` | 602 | Fun commands (8ball, dice, fortune, …) |
| `combo.go` | 702 | Command bundles |
| `gender.go` | 424 | Gender-guess mini game |
| `chatstat.go` | 218 | Per-chat statistics |
| `go_install.go` | 274 | `go install` helper command |
| `connection_mode.go` | 171 | Web/mobile connection mode switch |
| `proxyrelay.go` | 151 | Optional egress proxy relay |
| `shared_http.go` | 20 | Shared HTTP client |
| `migrated_manual.go` | 147 | Manually migrated commands |
| `hermes_cmd.go` | 66 | Hermes command |
| `tts_cmd.go` | 41 | `.tts` command |
| `tts_providers.go` | 182 | TTS providers (Edge neural, Google fallback) |
| `ruby_db.go` | 197 | AI conversation memory |
| `ruby_cycle.go` | 144 | Memory cycle (summarise / prune) |
| `compat_owner.go` | 1025 | Owner-command compatibility surface |
| `compat_inspect.go` | 303 | Message inspection compatibility surface |
| `pg.go` | 306 | Storage facade: `pgGet` / `pgExec` / `pgQuery` / `pgQueryRow` |
| `pg_postgres.go` | 190 | PostgreSQL schema (DDL) |
| `pg_sqlite.go` | 418 | SQLite schema + SQL transpiler |
| `pg_group.go` | 511 | Per-group settings storage |

#### Test files

| File | Lines | Covers |
|------|------:|--------|
| `storage_test.go` | 491 | SQL transpiler, schema creation, round-trips on both backends |
| `spr_test.go` | 572 | SPR logic |
| `spr_crm_test.go` | 253 | SPR ↔ CRM interaction |
| `jjam_test.go` | 407 | JAM parsing and crypto |
| `jjam_api_test.go` | 128 | JAM API client |
| `jjam_crypto_test.go` | 155 | JAM crypto helpers |
| `jjam_flow_test.go` | 98 | JAM render flow (skipped without a service) |
| `jjam_live_test.go` | 145 | JAM live render (opt-in) |

### `owner/` — owner-only commands (4 files)

| File | Lines | Role |
|------|------:|------|
| `admincmd.go` | 643 | tagall, kick, promote, demote, broadcast, `.shell`, `.getfile` |
| `evalcmd.go` | 503 | `.eval` / `.evaljs` expression engines |
| `compat_media.go` | 143 | Owner media helpers |
| `owner_logic_test.go` | 174 | Owner command logic tests |

### `security/` — anti-abuse, audit, hardening (13 files)

| File | Lines | Role |
|------|------:|------|
| `antibot.go` | 1857 | Anti-bot / anti-spam engine |
| `dmsg_fakemsg.go` | 611 | Fake-message generation for verification |
| `anti_spr.go` | 491 | Anti-leak protection for relayed messages |
| `responsemode.go` | 420 | Response-mode enforcement |
| `antifdmsg.go` | 320 | Forwarded-message protection |
| `dmsgva.go` | 220 | Message validation |
| `sysinfo.go` | 491 | System information |
| `healthcheck.go` | 158 | Health checks |
| `auditlog.go` | 135 | Append-only audit log |
| `pinva.go` | 99 | Pin validation |
| `observer.go` | 92 | Passive event observation |
| `metrics.go` | 48 | Metric collection |
| `dmsg_fakemsg_test.go` | 130 | Fake-message tests |

### `util/` — OS helpers (5 files)

| File | Lines | Role |
|------|------:|------|
| `rss.go` | 245 | Runtime stats (`.stats`, `.proses`, `.rss`) |
| `generic_helpers.go` | 77 | Small generic helpers |
| `util.go` | 55 | Miscellaneous utilities |
| `rusage_unix.go` | 21 | `getrusage` on Unix |
| `rusage_other.go` | 8 | Stub for other platforms |

---

## 🗄️ Storage architecture

One storage API, two backends.

```
      plugin code
           │
           ▼
   pgGet / pgExec / pgQuery / pgQueryRow      ← dialect-neutral, `?` placeholders
           │
    ┌──────┴───────┐
    ▼              ▼
 PostgreSQL      SQLite
 (lib/pq)    (modernc.org/sqlite, pure Go)
    │              │
    ▼              ▼
 whatsmeow_*    whatsmeow_*
 + bot tables   + bot tables
```

- **`plugins/pg.go`** — the facade. It owns the connection pool, translates
  `?` → `$1, $2, …` for PostgreSQL, and exposes the four helpers every plugin
  uses.
- **`plugins/pg_postgres.go`** — the PostgreSQL DDL.
- **`plugins/pg_sqlite.go`** — the SQLite DDL plus a small SQL transpiler that
  rewrites `NOW()`, `NOW() - INTERVAL '…'`, `EXTRACT(EPOCH FROM …)` and
  `::type` casts into SQLite equivalents.
- **`core/storage.go`** — picks the backend from `STORAGE` and resolves paths.
- **`core/session_sqlite.go`** — the whatsmeow session store, on the same
  database as the bot tables.

`pgInitSessionSchema()` runs *after* the session store is created and adds the
two extra columns the bot reads (`whatsmeow_device.platform`,
`whatsmeow_sender_keys.created_at`).

## 🔌 Plugin lifecycle

1. `init()` in a plugin file calls `RegisterPlugin(Plugin{…})`.
2. The plugin lands in the registry (command + aliases → handler).
3. `.menu` renders it under its `Category`.
4. The sweeper auto-kills plugins that have been idle past `IdleKill`
   (default 10 minutes), calling `OnKill` so the plugin can release
   resources.

Plugins sit behind the **gate**: self mode, group lock and trust checks all
run first. Admin tools set `PreGate: true` so they still answer when a group
is locked.

## 🔐 Security model

- **Self mode** — on by default; only the owner gets a reply in private, and
  groups stay silent until explicitly unlocked.
- **Trust list** — per-group, per-user unlock.
- **Guard** — per-group anti-link, anti-spam and welcome rules.
- **Rate limiting** — per user, per command family.
- **`.shell`** — allowlisted by default; `SHELL_ALLOW_ALL=1` opens it up.
- **`.getfile`** — path-traversal guarded, optionally extended by
  `OWNER_EXTRA_FILE_ROOT`.
- **Audit log** — append-only JSONL of privileged actions.
