# mcpgram

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Self-hosted Telegram Corporate MCP Server written in Go.

Allows employees to safely connect their AI agents (Cursor, Claude Desktop, OpenCode, CLI agents, etc.) to corporate Telegram group chats via **MCP (Model Context Protocol)** over Streamable HTTP — with strict Read-Only access and dynamic RBAC synchronized with Telegram chat membership.

> **Полное руководство (RU):** см. [docs/MANUAL.md](docs/MANUAL.md) — пошаговое развёртывание (`lite` и `pro`), справочник `.env`, подключение сотрудников, Day-2 эксплуатация и диагностика.

## Key Principles

- **Privacy by default:** all messages stay inside your infrastructure (local SQLite WAL + FTS5 in `lite` mode); no access to personal DMs.
- **Telegram as Source of Truth (RBAC):** users can query only the chats where they are active members (hybrid `chat_member` push + TTL-bounded live `getChatMember` verification, fail-closed on network errors).
- **Read-Only MCP:** no send/write/delete tools exposed to LLMs; bearer tokens live in the MCP client config, never in LLM prompts.
- **Long Polling only:** no public IP, domain, or inbound webhooks required; optional HTTP/SOCKS5 `TELEGRAM_PROXY_URL`.
- **Configurable history & retention policies:** `DEFAULT_HISTORY_POLICY=full_history` (or `since_join`, plus per-chat `/set_policy`), `RETENTION_DAYS=0` (keep forever by default), and `TOKEN_TTL_DAYS=90` (`0` for non-expiring tokens until revoked).

## System Requirements

| Deployment Profile | Components | Minimum Hardware | Recommended | Best For |
|---|---|---|---|---|
| **`lite`** *(core only)* | `mcpgram` (SQLite WAL + FTS5 + OAuth 2.1) | 1 vCPU, 256–512 MB RAM, 5 GB SSD | 1–2 vCPU, 512 MB–1 GB RAM, 10+ GB SSD | Cheap VPS ($4–5/mo), Raspberry Pi 4/5, NAS, fast start |
| **`lite` + External API** | `mcpgram` + cloud/corporate OpenAI-compatible LLM/Whisper API | 1 vCPU, 512 MB RAM, 5 GB SSD | 1–2 vCPU, 1 GB RAM, 10+ GB SSD | Digests, semantic search & Whisper without local GPU/RAM |
| **`pro` (Local CPU)** | `mcpgram` + Ollama (`bge-m3`, `qwen2.5:3b`/`7b`) + `faster-whisper-small` | 4 vCPU (AVX2 / ARM64), 8 GB RAM (`qwen2.5:3b`), 25 GB SSD | 6–8 vCPU, 16 GB RAM (`qwen2.5:7b`), 40+ GB SSD | 100% air-gapped / zero cloud APIs on a standard server |
| **`pro` (Local GPU / Apple Silicon)** | Same as `pro`, with Metal GPU (macOS native) or NVIDIA GPU | Apple Silicon M1+ (16 GB RAM) or NVIDIA GPU (8+ GB VRAM), 30 GB SSD | Apple Silicon M1–M4 (16–32 GB RAM) or RTX 3060 / L4 (12+ GB VRAM) | Fast voice transcription, embeddings & digests completely locally |

- **OS & Architecture:** Linux (`amd64`, `arm64`), macOS (`amd64`, `arm64`), or Windows (`amd64`). Standalone static binaries have zero runtime dependencies (`CGO_ENABLED=0`, pure-Go SQLite, embedded `time/tzdata`). For Docker deployments: Docker Engine 24+ and Docker Compose v2. Native `whisper-server` also requires `ffmpeg` to decode `.oga`/`.mp4`.
- **Disk Usage:** `mcpgram` stores text, FTS5 indexes, and optional vector chunks — media files are **not** stored permanently. Expect ~150–300 MB per 100,000 messages (plus ~5–8 GB for local ML models in `pro` mode).
- **Uptime & Network:** A 24/7 host is recommended (Telegram retains undelivered Long Polling updates for 24 hours; downtime > 20h triggers an admin DM alert). Only outbound HTTPS (`443`) to `api.telegram.org` (or via `TELEGRAM_PROXY_URL`) is needed for Telegram; MCP clients connect over LAN, VPN (Tailscale / WireGuard), or an HTTPS reverse proxy (required over the public internet and for production OAuth 2.1).

## Quick Start

### Option A: Docker Compose (`lite` profile, prebuilt multi-arch GHCR image)

```bash
curl -O https://raw.githubusercontent.com/mytischi/mcpgram/main/docker-compose.yml
curl -O https://raw.githubusercontent.com/mytischi/mcpgram/main/.env.example
cp .env.example .env
# Edit BOT_TOKEN (from @BotFather with Privacy Mode disabled) and ADMIN_TG_IDS
docker compose --profile lite pull
docker compose --profile lite up -d
```

### Option B: Run locally from source (Go)

```bash
cp .env.example .env
# Set DATABASE_URL=./data/mcpgram.db for local macOS/Linux runs
go build -o bin/mcpgram ./cmd/mcpgram
./bin/mcpgram serve
```

## Bot Setup & Connecting MCP Clients

1. In `@BotFather`, create a bot, run `/setjoingroups` → **Enable** and `/setprivacy` → **Disable**.
2. Add the bot to your corporate Telegram groups as **administrator** (strip dangerous rights like delete/ban/pin).
3. **Option 1 — Manual Bearer token (`/auth`, default `OAUTH_ENABLED=false`, works over LAN/VPN HTTP & HTTPS):**
   - Employees open a DM with the bot and press **Start** (or `/connect`) — the bot silently verifies workspace chat membership and sends a safe prompt (`PASTE_TOKEN_HERE`, no secrets) using the bot's own username as the MCP server name.
   - Send `/auth` to obtain a personal Bearer token (`tgmcp_...`) + ready-to-copy `mcp.json` / `claude mcp add` snippets + a **"🗑 Hide token"** button.
4. **Option 2 — MCP OAuth 2.1 (zero manual tokens, requires HTTPS outside `localhost`):**
   - Set `OAUTH_ENABLED=true` and `PUBLIC_BASE_URL=https://tg-mcp.company.internal` (e.g. behind Caddy or `tailscale serve --bg 8080`; `http://localhost:8080` also works locally) in `.env`.
   - Employees press **Start** → paste the zero-secret prompt to Cursor / Claude Code / Hermes / OpenCode → click **"Confirm in Telegram"** → press **Start** in the bot → tokens are saved and rotated automatically.

```json
{
  "mcpServers": {
    "company-telegram": {
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer tgmcp_your_token_here"
      }
    }
  }
}
```

## Bot Commands (Private DM, English UI)

| Command | Who | Description |
|---|---|---|
| `/start` / `/connect` | Workspace members & admins | Silently verifies chat membership and sends clean setup steps + a ready-to-copy AI agent prompt named after the bot (never leaks server URL to outsiders or admin commands in onboarding) |
| `/auth` | Workspace members & admins | Issues a manual Bearer token with ready snippets + "🗑 Hide token" button |
| `/admin` | `ADMIN_TG_IDS` | Lists administrator commands (hidden from regular employees and public menu) |
| `/status` | `ADMIN_TG_IDS` | Shows bot diagnostics, OAuth status, Privacy Mode, workers, and tracked chats (`ws=...`) |
| `/digest [YYYY-MM-DD\|today]` | `ADMIN_TG_IDS` | Triggers nightly digest generation on demand (pending days or a specific date) |
| `/set_policy <ws_id> <full_history\|since_join\|default>` | `ADMIN_TG_IDS` | Sets or resets per-chat history visibility override |
| `/revoke_user <telegram_user_id>` | `ADMIN_TG_IDS` | Immediately revokes all active Bearer & OAuth refresh tokens for a user |
| *(send `result.json` or `messages.html`)* | `ADMIN_TG_IDS` | Imports a Telegram JSON (Desktop) or HTML (macOS / Desktop) export directly in bot DM without SSH (up to 20 MiB, deduplicated) |

> On startup, if server downtime exceeded **20 hours**, `mcpgram` automatically alerts `ADMIN_TG_IDS` in Telegram DM so any gap beyond Telegram's 24h update window can be backfilled by dropping `result.json` or `messages.html` into the bot chat. `/healthz` also returns `503` if polling has been stale for > 5 minutes.

## Nightly Chat Digests (`get_digests`)

To let AI agents scan weeks of chat history cheaply without pulling raw logs, enable nightly digests in `.env` using any OpenAI-compatible LLM endpoint (local Ollama, llama.cpp, vLLM, or OpenAI):

```env
DIGEST_MODE=openai_compatible
DIGEST_BASE_URL=http://127.0.0.1:11434/v1
DIGEST_API_KEY=
DIGEST_MODEL=qwen2.5:7b
DIGEST_LANGUAGE=ru
DIGEST_SCHEDULE_HOUR=3
DIGEST_TIMEZONE=Europe/Moscow
DIGEST_MIN_MESSAGES=3
DIGEST_LOOKBACK_DAYS=7
```

You can also trigger digest generation manually via CLI or `/digest` in Telegram:

```bash
./bin/mcpgram digest                 # generate missing digests for the last DIGEST_LOOKBACK_DAYS days
./bin/mcpgram digest --today --force # summarize today's messages immediately
```

## Hybrid Semantic Search (`EMBEDDING_MODE`) & Whisper (`WHISPER_MODE`)

In `lite` mode both are `off` (zero ML overhead, pure SQLite FTS5). To enable fully local semantic search (dialog window chunking + RBAC pre-filtered vector RRF fusion) and voice/video_note transcription, enable the `pro` Compose profile (includes local Ollama + `faster-whisper-server` sidecars) and configure `.env`:

```env
EMBEDDING_MODE=openai_compatible
EMBEDDING_BASE_URL=http://ollama:11434/v1
EMBEDDING_MODEL=bge-m3

WHISPER_MODE=openai_compatible
WHISPER_BASE_URL=http://whisper:8000/v1
WHISPER_MODEL=Systran/faster-whisper-small
WHISPER_LANGUAGE=ru
```

```bash
docker compose --profile pro up -d
# Optional manual semantic indexing after importing old archives:
./bin/mcpgram reindex
```

## Import Historical Archive (JSON or HTML)

Export chat history from **Telegram Desktop** (`result.json` or `messages.html`) or **Telegram for macOS** (`messages.html`, uncheck media) and either drop the file directly into the bot DM (up to 20 MiB) or run the CLI:

```bash
./bin/mcpgram import --file /path/to/result.json
./bin/mcpgram import --file /path/to/messages.html --tz Europe/Moscow
# Optional: override Bot API chat ID or limit age with --retention-days:
./bin/mcpgram import --file /path/to/messages.html --chat-id -1001234567890 --retention-days 0
```

> For HTML exports (`messages.html`), `mcpgram` automatically matches the chat title to an already-tracked workspace chat (or uses `--chat-id` / numeric `chat_id` in the Telegram file caption).

## MCP Tools (Read-Only)

- `list_chats` — accessible corporate chats, Forum Topics, and last message timestamp.
- `get_digests` — precomputed nightly daily summaries (overview, key decisions, action items, topics) over a date range.
- `search_messages` — RBAC-filtered FTS5 search across accessible chats or a specific chat/topic.
- `get_messages` — chronological messages filtered by date range, topic, and author.
- `get_thread` — full reply chain reconstruction around a `message_id`.

## Development & Tests

```bash
go vet ./...
go test -race ./...
```

## License

Copyright 2026 Matvei Naumov. Licensed under the [Apache License 2.0](LICENSE).
