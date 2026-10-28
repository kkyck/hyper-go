# hyper-go

> A reusable **base WhatsApp bot** written in Go — 155 commands, dual storage
> (PostgreSQL **or** SQLite), no credentials and no third-party endpoints
> baked into the source.

`hyper-go` is a clean foundation you can fork and build your own bot on. It
gives you the boring, hard parts — WhatsApp session handling, a plugin
system, per-group permissions, storage that works on both a Raspberry Pi and
a VPS, security hardening, AI, media handling — and leaves the branding,
the business logic and the credentials to you.

Everything personal is gone. There is no owner phone number, no API key, no
hardcoded service URL. You supply them through `.env`.

---

## ✨ Highlights

| | |
|---|---|
| **155 commands** | AI, media, downloaders, stickers, group management, owner tools, utilities |
| **Dual storage** | PostgreSQL *or* SQLite — one file, zero config, or a real database |
| **Plugin system** | Drop a file in `plugins/`, call `RegisterPlugin(...)`, done |
| **Self mode** | Locked down by default: only you can talk to it, per-group trust |
| **Per-group ACL** | Guard (anti-link, anti-spam, welcome), ban, mute, read-only |
| **QR in the browser** | A tiny local web UI shows the pairing QR |
| **One-file database** | SQLite via `modernc.org/sqlite` — no database server, no ORM |
| **Security built in** | Rate limits, audit log, path-traversal guard, allowlisted `.shell` |
| **Graceful shutdown** | Context-based drain, clean disconnect on SIGINT/SIGTERM |

---

## 🚀 Quick start

```bash
git clone https://github.com/kkyck/hyper-go.git
cd hyper-go

cp .env.example .env
$EDITOR .env          # set OWNER_JID (required) and anything else you want

make build            # -> ./bot
./bot
```

On the first run the bot prints a QR code and — if `WEB_PORT` is set — also
serves it at <http://localhost:3000>. Scan it with **WhatsApp → Settings →
Linked devices → Link a device**.

After pairing, send `.menu` in a chat to see everything the bot can do.

> **Build note.** Go cannot build one `package main` spread across several
> directories, so the sources live in `core/`, `plugins/`, `owner/`,
> `security/` and `util/` for navigability. `make build` flattens them into a
> temporary `.build/` directory, compiles there and moves the binary back.
> **Do not run `go build .` at the repository root** — use `make`.

### Requirements

- Go 1.26+
- A C compiler (`gcc` / `clang`) — the sticker encoder bundles libwebp and
  compiles it from source, so CGO is enabled by default.
- No database server, no Node, no Python.

---

## ⚙️ Configuration

Everything is environment-driven. The bot loads `.env` from the working
directory on startup (override the path with `ENV_FILE`).

```bash
# .env
OWNER_JID=6281234567890
OWNER_NAME=Your Name
STORAGE=sqlite
```

### Storage

| `STORAGE` | Backend | Needs |
|---|---|---|
| `auto` *(default)* | PostgreSQL if `POSTGRES_DSN` is set, otherwise SQLite | — |
| `sqlite` | SQLite file at `SQLITE_DB` (default `./data/bot.db`) | — |
| `postgres` | PostgreSQL | `POSTGRES_DSN` |

Both the WhatsApp **session store** and the bot's **own tables** live in the
same database, so sender-key metrics, device state and audit data stay
consistent on either backend.

The SQL is written once in a dialect-neutral way (`?` placeholders) and
translated per backend. PostgreSQL-specific constructs are handled by a small
transpiler for SQLite (`NOW()`, `INTERVAL`, `::type` casts, percentile
functions), so every plugin works on both.

```bash
# PostgreSQL
STORAGE=postgres
POSTGRES_DSN=postgres://user:password@localhost:5432/botdb?sslmode=disable

# SQLite — the whole state is one portable file
STORAGE=sqlite
SQLITE_DB=./data/bot.db
```

### Third-party endpoints

**No external service is hardcoded.** Each integration reads its base URL
from the environment, and an empty value simply keeps that feature disabled —
the command then answers with a clear "not configured" message instead of
failing in a confusing way.

| Variable | Used for |
|---|---|
| `DL_BASE_URL` | Download worker (aiodl-style API) |
| `KANGWIFI_API_BASE` | Media / metadata API |
| `TIKWM_API_BASE` | TikTok / Instagram / Facebook downloader |
| `COBALT_API_BASE` | cobalt media downloader |
| `VIDEODOWNLOADERTOOLS_BASE` | Fallback media downloader |
| `YT_LOADER_BASE` | YouTube Music download worker |
| `SPOTIFY_OEMBED_BASE` | Spotify oEmbed metadata |
| `AMWEB_API` | Alight Motion template renderer |
| `POLLINATIONS_BASE` | OpenAI-compatible text completion |
| `GEMINI_BASE` | Gemini web client (no API key) |
| `GROQ_API_BASE` / `GROQ_API_KEY` | Groq (AI + speech-to-text) |
| `IP_API_BASE` | Geo-IP lookup |
| `LRCLIB_BASE` | Lyrics lookup |
| `SHORTLINK_BASE` | URL shortener |
| `ADS_SOURCE_URL` | Remote notice / ad source |
| `GOOGLE_TTS_BASE` | Google Translate text-to-speech |
| `GOOGLE_IDENTITY_BASE`, `GOOGLE_SECURETOKEN_BASE` | Google Identity Toolkit |
| `ALIGHT_API_KEY`, `ALIGHT_BASE`, `ALIGHT_VERIFY_URL` | Alight Motion flow |
| `YTMUSIC_DL_API_KEY` | YouTube Music download API key |

See [`.env.example`](.env.example) for the complete, commented list.

---

## 🏗️ Project layout

```
hyper-go/
├── core/           ← entry point, runtime, WhatsApp I/O, storage, concurrency
├── plugins/        ← feature plugins (87 files)
├── owner/          ← owner-only commands
├── security/       ← anti-abuse, audit, hardening
├── util/           ← OS helpers, generic utilities
├── docs/
│   └── ARCHITECTURE.md   ← every file mapped to its layer
├── Makefile
├── go.mod / go.sum
└── .env.example
```

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the full file index.

---

## 🧩 Writing a plugin

A plugin is one file in `plugins/` that registers itself. Nothing else in the
codebase needs to change — no imports to add, no switch to extend, no menu
edit.

```go
package main

import (
    whatsmeow "github.com/polymorfa/hypermeow"
    "github.com/polymorfa/hypermeow/types/events"
)

func init() {
    RegisterPlugin(Plugin{
        Command:     "halo",
        Aliases:     []string{"hi", "hello"},
        Description: "Sapa balik",
        Category:    "Fun",
        Handler: func(client *whatsmeow.Client, msg *events.Message, cmd, args string) string {
            return "Halo! 👋"
        },
    })
}
```

That is the whole contract:

- **Command** — the name without the dot, lower-cased. `.halo` works.
- **Aliases** — alternative names; all map to the same handler.
- **Category** — the group it appears under in `.menu`.
- **OwnerOnly** — restrict it to the bot owner.
- **PreGate** — run *before* the self-mode / group-lock gate (admin tools).
- **Handler** — returns the reply text. Return `""` for a silent handler.

Plugins also get a **lifecycle**: idle plugins are auto-killed to free memory,
and an `OnKill` hook lets you release connections or stop goroutines.

Because every plugin talks to storage through `pgGet` / `pgExec` / `pgQuery` /
`pgQueryRow`, **your plugin works on PostgreSQL and SQLite alike** without
knowing which one is running.

---

## 🔒 Self mode & trust

The bot is locked down on purpose.

- **Self mode is ON by default.** In private chats only the owner gets an answer.
- **In groups the bot stays silent** until you unlock that specific group.
  - `.self off` *inside a group* opens it up for everyone.
  - `.trust @someone` opens it up for one person only.
- **Admin commands** (kick, promote, tagall, guard, …) always require the
  owner or a group admin — opening a group does not grant them.
- `.self on|off|status` — switch self mode globally or per group.

The bot also ships with per-group guards (anti-link, anti-spam, welcome
message, slow mode), rate limiting, an append-only audit log and a
path-traversal guard on `.getfile`.

---

## 🛠️ Make targets

```
make build            # flatten + build for $(GOOS)/$(GOARCH)
make build-local      # flatten + build for this machine
make build-debug      # flatten + build with debug symbols
make test             # flatten + run the whole test suite
make test-race        # flatten + run the tests with the race detector
make vet              # go vet on each subfolder
make fmt              # go fmt on each subfolder
make run              # build + run
make backup-archive   # timestamped tar.gz of the source
make clean            # remove the binary and .build/
make help             # list every target
```

Cross-compiling works, but CGO is enabled — so you also need a C cross
toolchain and must point `CC` at it:

```bash
make build GOOS=linux GOARCH=arm64 CC=aarch64-linux-gnu-gcc
```

---

## 📚 Documentation

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) — every file, its layer and
  its role.
- [`.env.example`](.env.example) — every environment variable, commented.

---

## ⚖️ License

MIT — see [`LICENSE`](LICENSE).
