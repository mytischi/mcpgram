# mcpgram

Self-hosted Telegram Corporate MCP Server written in Go.

Allows employees to safely connect their AI agents (Cursor, Claude Desktop, OpenCode, CLI agents, etc.) to corporate Telegram group chats via **MCP (Model Context Protocol)** over Streamable HTTP — with strict Read-Only access and dynamic RBAC synchronized with Telegram chat membership.

## Key Principles

- **Privacy by default:** all messages stay inside your infrastructure (local SQLite WAL + FTS5 in `lite` mode); no access to personal DMs.
- **Telegram as Source of Truth (RBAC):** users can query only the chats where they are active members (hybrid `chat_member` push + TTL-bounded live `getChatMember` verification, fail-closed on network errors).
- **Read-Only MCP:** no send/write/delete tools exposed to LLMs; bearer tokens live in the MCP client config, never in LLM prompts.
- **Long Polling only:** no public IP, domain, or inbound webhooks required; optional HTTP/SOCKS5 `TELEGRAM_PROXY_URL`.
- **Configurable history & retention policies:** `DEFAULT_HISTORY_POLICY=full_history` (or `since_join`, plus per-chat `/set_policy`), `RETENTION_DAYS=0` (keep forever by default), and `TOKEN_TTL_DAYS=90` (`0` for non-expiring tokens until revoked).

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
3. Send `/auth` to the bot in a private DM to obtain your personal Bearer token (`tgmcp_...`).
4. Add `mcpgram` to your MCP client config (`mcp.json` / `claude_desktop_config.json`):

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

## Bot Commands (Private DM)

| Command | Who | Description |
|---|---|---|
| `/start` | Anyone | Shows your numeric `Telegram ID` and available commands |
| `/auth` | Corporate chat members & admins | Issues a personal Bearer token (revokes any previous token) |
| `/status` | `ADMIN_TG_IDS` | Shows bot diagnostics, Privacy Mode status, digest status, and tracked chats (`ws=...`) |
| `/digest [YYYY-MM-DD\|today]` | `ADMIN_TG_IDS` | Triggers nightly digest generation on demand (pending days or a specific date) |
| `/set_policy <ws_id> <full_history\|since_join\|default>` | `ADMIN_TG_IDS` | Sets or resets per-chat history visibility override |
| `/revoke_user <telegram_user_id>` | `ADMIN_TG_IDS` | Immediately revokes all active tokens for a user |

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

## Import Historical Archive (Telegram Desktop JSON)

Export chat history to JSON in Telegram Desktop (`result.json`) and run:

```bash
./bin/mcpgram import --file /path/to/result.json
# Optional: override Bot API chat ID or limit age with --retention-days:
./bin/mcpgram import --file /path/to/result.json --chat-id -1001234567890 --retention-days 0
```

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
