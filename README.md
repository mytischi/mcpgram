# mcpgram

Self-hosted Telegram Corporate MCP Server written in Go.

Allows employees to safely connect their AI agents (Cursor, Claude Desktop, OpenCode, CLI agents, etc.) to corporate Telegram group chats via **MCP (Model Context Protocol)** over Streamable HTTP — with strict Read-Only access and dynamic RBAC synchronized with Telegram chat membership.

## Key Principles

- **Privacy by default:** all messages stay inside your infrastructure (local SQLite WAL + FTS5 in `lite` mode); no access to personal DMs.
- **Telegram as Source of Truth (RBAC):** users can query only the chats where they are active members (hybrid `chat_member` push + TTL-bounded live `getChatMember` verification, fail-closed on network errors).
- **Read-Only MCP:** no send/write/delete tools exposed to LLMs; bearer tokens live in the MCP client config, never in LLM prompts.
- **Long Polling only:** no public IP, domain, or inbound webhooks required; optional HTTP/SOCKS5 `TELEGRAM_PROXY_URL`.
- **Configurable history policy:** `DEFAULT_HISTORY_POLICY=full_history` by default (access full chat archive), switchable to `since_join` globally or per chat.

## v0.1 MVP Status

- [x] Stage 0: Repository scaffold, Go module, configuration & validation
- [x] Stage 1: SQLite WAL + FTS5 storage layer, migrations, group migration handling, `#offrecord`, TTL retention, RBAC SQL queries & unit tests
- [x] Stage 2: Telegram Bot API client, Long Polling ingestor & bot commands (`/auth`, `/status`, `/revoke_user`)
- [x] Stage 3: Hybrid RBAC authorizer (`chat_member` + TTL `getChatMember` + fail-closed SQL freshness)
- [x] Stage 4: Streamable HTTP MCP server (`/mcp`, `/healthz`, Bearer auth, `list_chats`, `search_messages`, `get_messages`, `get_thread`, background TTL retention)
- [x] Stage 5: Telegram Desktop JSON (`result.json`) importer & Docker packaging (`lite` profile)

## Quick Start

### 1. Configure `.env`

```bash
cp .env.example .env
# Edit BOT_TOKEN (from @BotFather with Privacy Mode disabled) and ADMIN_TG_IDS
```

### 2. Run locally (Go)

```bash
go build -o bin/mcpgram ./cmd/mcpgram
./bin/mcpgram serve
```

Or run via Docker Compose (`lite` profile):

```bash
docker compose --profile lite up -d --build
```

### 3. Add bot to corporate chats & connect MCP client

1. Add the bot to your corporate Telegram groups as **administrator** (strip dangerous rights like delete/ban).
2. Send `/auth` to the bot in a private DM to obtain your personal Bearer token (`tgmcp_...`).
3. Configure your MCP client (e.g. `mcp.json`):

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

### 4. Import historical archive (optional)

Export chat history to JSON in Telegram Desktop (`result.json`) and run:

```bash
./bin/mcpgram import --file /path/to/result.json
# Optional: override Bot API chat ID or import older than RETENTION_DAYS:
./bin/mcpgram import --file /path/to/result.json --chat-id -1001234567890 --retention-days 0
```

## MCP Tools (Read-Only)

- `list_chats` — accessible corporate chats, Forum Topics, and last message timestamp.
- `search_messages` — RBAC-filtered FTS5 search across accessible chats or a specific chat/topic.
- `get_messages` — chronological messages filtered by date range, topic, and author.
- `get_thread` — full reply chain reconstruction around a `message_id`.

## Development & Tests

```bash
go vet ./...
go test -race ./...
```
