# mcpgram

Self-hosted Telegram Corporate MCP Server written in Go.

Allows employees to safely connect their AI agents (Cursor, Claude Desktop, OpenCode, CLI agents, etc.) to corporate Telegram group chats via **MCP (Model Context Protocol)** over Streamable HTTP — with strict Read-Only access and dynamic RBAC synchronized with Telegram chat membership.

## Key Principles

- **Privacy by default:** all messages stay inside your infrastructure (local SQLite in `lite` mode); no access to personal DMs.
- **Telegram as Source of Truth (RBAC):** users can query only the chats where they are active members (hybrid `chat_member` push + TTL-bounded live `getChatMember` verification).
- **Read-Only MCP:** no send/write/delete tools exposed to LLMs; bearer tokens live in the MCP client config, never in LLM prompts.
- **Long Polling only:** no public IP, domain, or inbound webhooks required; optional `TELEGRAM_PROXY_URL`.
- **Configurable history policy:** `DEFAULT_HISTORY_POLICY=full_history` by default (access full chat archive), switchable to `since_join` globally or per chat.

## Project Status

Currently under active development (v0.1 MVP):

- [x] Stage 0: Repository scaffold, Go module, configuration & validation
- [x] Stage 1: SQLite WAL + FTS5 storage layer, migrations, group migration handling, `#offrecord`, TTL retention, RBAC SQL queries & unit tests
- [ ] Stage 2: Telegram Bot API client, Long Polling ingestor & bot commands (`/auth`, `/status`, `/revoke_user`)
- [ ] Stage 3: Hybrid RBAC authorizer (`chat_member` + TTL `getChatMember`)
- [ ] Stage 4: Streamable HTTP MCP server (`list_chats`, `search_messages`, `get_messages`, `get_thread`)
- [ ] Stage 5: Telegram Desktop JSON (`result.json`) importer & Docker packaging (`lite` profile)

## Quick Start (Development)

```bash
cp .env.example .env
go test -race ./...
go build -o bin/mcpgram ./cmd/mcpgram
```
