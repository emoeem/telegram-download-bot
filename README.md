# telegram-download-bot

English | [中文说明](README.zh.md)

## Desktop app

![Desktop app](./docs/images/desktop_en.png)

Download: [GitHub Releases](https://github.com/ConnectingEveryCorner/telegram-download-bot/releases)

### First launch on macOS

The macOS app is not currently signed or notarized by Apple. If macOS says that the app is damaged, move it to Applications and run:

```bash
xattr -cr "/Applications/TelegramDownloadBotDesktop.app"
```

telegram-download-bot is a Telegram MTProto download-and-send-back bot built on top of [tdl](https://github.com/iyear/tdl). Send a Telegram message link to the bot, and it will download the original message content with a logged-in Telegram user account, then send text, photos, videos, documents, or albums back through the bot.

**This repository is a heavily extended fork.** tdl's manual one-shot downloader is turned into a realtime, delete-proof archiving system, and on top of it sit a web (PWA) console, an Android client built from the same Go core, and an online licence system. See the comparison below.

![Usage demo](./docs/images/demo.gif)

## Features

### Original vs this fork

| Capability | tdl / the original bot | This fork |
|---|---|---|
| Downloading | send a link, fetch one message at a time | realtime capture: media is stored the moment a message arrives, nothing to babysit |
| Recalls & deletions | content is gone | a recall only adds a "deleted" tag: text, media and timestamps stay byte-identical (sha256 verified) |
| Edit history | not tracked | every edit is kept as a version |
| Media storage | loose files | content-addressed store (`objects/<sha256>`) with dedupe, staging dir and `gc` |
| History backfill | manual, per link | batch backfill by date/count, idempotent keyed writes |
| Interface | CLI / TUI | web console (installable PWA) + Android app |
| Search | none | SQLite FTS5 + embedded gse Chinese tokenizer; `from:` `has:` `before:` syntax |
| Export | none | streaming CSV/JSON (50k-row cap) and Telegram Desktop format |
| Notifications | none | keyword rules pushed via Webhook / Bot API |
| Licensing | none | Ed25519 online activation with device binding |

The original bot flow is unchanged: message link in, content back out, admin-only authorization, fixed bot language (`en`/`zh`).

### Realtime archive core (the download pipeline, rebuilt)

- A media worker runs next to the MTProto connection: new messages are written to SQLite and their attachments downloaded to the CAS store immediately, so a later recall cannot take them away, including in protected (no-forward) chats the account is a member of.
- Concurrency is deliberately paced (batched jobs, `TDL_MEDIA_THREADS` knob) so the account never trips `FLOOD_WAIT`; a typo in the knob falls back to the default instead of stopping archiving.
- Quoted `t.me` message links are resolved through the Telegram API itself and archived through the same CAS path as native media.
- Downloads pause automatically below 1 GiB free disk (message archiving never stops); the database is snapshotted daily (`VACUUM INTO`, last 7 kept).
- Channel buttons and `t.me` links can be auto-clicked by the account, with results archived.

### Web client (PWA)

- `tdl archive serve` serves the whole console from the binary (embedded, works as an installed PWA: manifest, service worker, icons, Tailwind-built stylesheet).
- **Managed mode**: scan a QR code in the browser to log the account in (2FA supported), manage monitor sources, trigger gap-fill syncs with progress, inspect storage.
- Multi-account archive viewer with draggable timeline scrubber, forum topic chips, deleted/edited views, serial inbox, media gallery and full search syntax.
- Streaming export, keyword notification rules (bot token write-only, masked), storage usage view, built-in guide.
- Hardened: CSRF protection, basic/token auth, dedicated activation page, secure data-wipe endpoint.

### Android client (same core, no server needed)

- The whole Go archive core is compiled into an AAR with `gomobile bind` (`android/build-aar.sh`): not a subprocess, not Termux, not a website wrapper.
- `ArchiveService` foreground service keeps the MTProto connection alive 24/7; a boot receiver restarts it after reboot.
- The WebView loads the exact same embedded web UI from `127.0.0.1`, so phone and desktop share one frontend (mobile breakpoints included).
- Session keys live in the app's private `filesDir`; the device id binds into the licence system.
- See [android/README.md](android/README.md) for architecture and build steps.

### Online licence system

- Ed25519-signed licence keys in Crockford encoding, bound to a device id at first activation.
- A standalone licence server (`tdl license server`) records issuance and can revoke/release keys; revocation takes effect on the next heartbeat.
- The CLI gate and the web activation page both enforce it; keys can be embedded at build time (`tdl license build`).
- Design, issuing and deployment guides: [docs/授权体系.md](docs/授权体系.md), [docs/签发手册.md](docs/签发手册.md), [docs/在线授权部署.md](docs/在线授权部署.md).

## Documentation

| Doc | Content |
|---|---|
| [docs/用户手册.md](docs/用户手册.md) | end-user manual (Chinese, what/why/how) |
| [docs/USAGE.md](docs/USAGE.md) | operator command reference |
| [docs/ARCHIVE-INBOX.md](docs/ARCHIVE-INBOX.md) | serial inbox workflow |
| [docs/ui-spec.md](docs/ui-spec.md) | web UI specification |
| [docs/授权体系.md](docs/授权体系.md) / [docs/签发手册.md](docs/签发手册.md) / [docs/在线授权部署.md](docs/在线授权部署.md) | licence design / issuing / server deployment |
| [docs/AI-QUICKSTART.md](docs/AI-QUICKSTART.md) | quickstart for AI assistants |

## Configuration

Copy the example config first:

```bash
cp config.example.yaml config.yaml
```

Then edit `config.yaml`:

```yaml
bot:
  token: "YOUR_TELEGRAM_BOT_TOKEN"
  debug: false
  language: "en"

telegram:
  app-id: 123456
  app-hash: "YOUR_TELEGRAM_APP_HASH"

auth:
  admin-chat-id: 123456789
  allowed-chat-ids: []

tdl:
  proxy: ""
  ntp: ""
  reconnect-timeout: "5m"
  threads: 4
  limit: 2
  pool: 8
  delay: "0s"
```

Important fields:

- `bot.token`: Bot token from BotFather
- `bot.language`: required bot language, only `en` and `zh` are supported
- `telegram.app-id` and `telegram.app-hash`: Telegram API credentials
- `auth.admin-chat-id`: the primary admin chat id
- `auth.allowed-chat-ids`: regular users allowed to use the bot
- `tdl.proxy`: optional proxy, for example `socks5://127.0.0.1:7890`

How to get `telegram.app-id` and `telegram.app-hash`:

1. Open [my.telegram.org](https://my.telegram.org/) and log in with your Telegram phone number.
2. Go to **API development tools**.
3. Create an application if you do not already have one.
4. Copy the displayed `api_id` into `telegram.app-id` and `api_hash` into `telegram.app-hash`.

The language is global and fixed by config. Users cannot choose or switch language in chat. If `bot.language` is `en`, all bot prompts use English; if it is `zh`, all bot prompts use Chinese.

## Docker Compose Deployment

Download `docker-compose.yml` and `config.example.yaml` from this repository, then place them in the same deployment directory.

Copy `config.example.yaml` to `config.yaml`, then edit `config.yaml` with your bot token, Telegram API credentials, and admin chat id.

Start the bot:

```bash
docker compose up -d
```

## Bot Commands

- `/start`: show help
- `/help`: show help
- `/myid`: show current chat id
- `/login`: primary admin logs in the shared Telegram account
- `/login +4475834875`: primary admin logs in the shared account with a phone number
- `/status`: check login status
- `/logout`: clear the current account session
- `/cancel`: cancel the current login flow
- `/grant <chat_id>`: primary admin authorizes a user
- `/revoke <chat_id>`: primary admin revokes a user
- `/users`: primary admin views authorization status

After login, send a Telegram message link directly to download and send it back.

Only the primary admin configured by `auth.admin-chat-id` can run `/login`. Once logged in, all authorized bot users share the same Telegram account session and do not need to log in separately.

## Message Monitoring & Archive (archive subsystem)

Besides the download bot, the project ships a long-running **Telegram message
monitor/archive** driven by an MTProto **user account** (not the Bot API).
Any command starting with `archive` bypasses the bot entry point, even when
`bot.token` is configured.

Data lives in `~/.tdl/archive/` (`tgarchive.db` — SQLite WAL with FTS5 + `gse`
Chinese tokenization; `objects/<sha256>` — content-addressed media;
`staging/` — in-flight downloads).

```bash
tdl archive login                          # log in the archiving user account (code)
tdl archive login --qr                     # or scan a QR code from the Telegram app
tdl archive monitor add @channel --since 2026-01-01 --limit 1000
tdl archive run                            # realtime + periodic rescan + media worker
tdl archive search "关键词" --chat <chat-id>
tdl archive stats
tdl archive serve                          # local viewer on http://127.0.0.1:8080
```

Messages are written through a single idempotent path keyed by
`(chat_id, message_id)`, so realtime updates, history backfill and periodic
rescans can safely repeat. Optional Telegram destination channels mirror the
archive into Telegram itself (`tdl archive destination ...`). Local archiving
covers channels, groups and private chats — including protected
(no-forwards) chats the account is a member of. When mirroring such sources,
the native copy is rejected server-side, so the exporter automatically falls
back to re-uploading media from the local CAS store / re-sending text as a
fresh message, recording the fallback reason in `archive_exports`.

**Web console (login + management + browsing).** Once `telegram.app-id` /
`app-hash` are configured, `tdl archive serve` starts in **managed mode**: the
top bar shows the account state, and when logged out you can **scan a QR code
in the browser** (Telegram app → Settings → Devices → Link Desktop Device;
accounts with 2FA are prompted for the cloud password). After login the serve
process archives in realtime, and the web UI can add/remove monitor sources,
trigger gap-fill syncs (backfill progress shows in the Tasks panel) and inspect
disk/database usage in the Storage view. Picking a source filters every view
(messages, media, export) to that chat; the pager is a **draggable timeline
slider** so months of history can be scrubbed directly; forum topics get a
chip bar (title + message count) and messages carry clickable topic tags.
Without credentials serve stays a
**read-only viewer**. All control endpoints share the viewer authentication —
always add `--auth-basic user:pass` when exposing the port. A built-in guide
("❓ 说明") ships inside the web UI.

**Session encryption & daily backups.** The archive session (full account
control) is encrypted at rest with AES-GCM under a scrypt-derived key:
`tdl archive login` interactively offers to set a passphrase, or use
`--session-passphrase` / `export TGA_SESSION_PASSPHRASE=...`. Existing
plaintext sessions migrate automatically on the first encrypted open — no
re-login needed. `archive run` and managed `serve` snapshot the database daily
(`VACUUM INTO`, keeps the last 7 in `~/.tdl/archive/backups/`), and media
downloads pause automatically below 1 GiB of free disk space (message archiving
never stops). The gse dictionary is embedded in the binary, so tokenization
works in containers with `CGO_ENABLED=0`.

**Keyword notifications (Webhook / Telegram Bot API).** Realtime messages that
match a rule's keywords can be pushed outward — for alerting, drops, releases:

- rules carry a keyword whitelist, exclude words and an optional chat scope;
- outlets: a **webhook** (JSON POST with `text/chat/chat_id/message_id/date/rule`)
  and/or the **Telegram Bot API** (bot sends the text to a chat id, truncated);
- each rule is rate-limited to 20 notifications/minute; deliveries are logged
  (`tdl archive notify log`) and notification failures never affect archiving;
- history backfill does not notify — only realtime messages do. The bot token
  can come from `TGA_NOTIFY_BOT_TOKEN` to keep it out of shell history.

Manage rules via the web UI's 通知 (Notify) panel or
`tdl archive notify add|list|set|remove|test|log`.

**Reactions & view counters.** Message **reactions** (aggregated emoji /
custom-emoji counts) are collected from realtime updates, backfill and rescans,
and render as chips in the message feed; channel/supergroup **views/forwards**
are refreshed each sweep with one `messages.getMessagesViews` call per chat
(newest 200 messages), so counters stay accurate even when the realtime stream
missed them.

See [README.zh.md](README.zh.md) for the full Chinese documentation, including
the `archive monitor`, `media`, `destination` and `serve` command reference.

The local viewer (`tdl archive serve`) binds to `127.0.0.1` by default. When you
expose it, use `--auth-basic user:pass` for browser access (native credential
prompt); `--auth-token` is header-based (`Authorization: Bearer ...`) and is
meant for scripts/curl — a plain browser cannot log in with it.

### HTTP API (for scripts)

`tdl archive serve` also exposes a read-only JSON API, guarded by the same
credentials as the pages. Useful endpoints:

| Endpoint | Purpose |
|---|---|
| `GET /api/stats` | message / media / monitor counters |
| `GET /api/messages/page?page=&page_size=&q=&chat=` | paged messages, full search syntax |
| `GET /api/search?q=&limit=&offset=` | search results (limit <= 200) |
| `GET /api/export?kind=messages\|media\|links\|deleted\|edited&format=csv\|json` | streaming export, 50k-row cap |
| `GET /api/media?kind=image\|video\|file` | media listing |
| `GET /api/targets`, `POST`/`PATCH`/`DELETE /api/targets` | monitor management (managed mode only) |
| `POST /api/monitor/sync` | trigger a gap-closing rescan |
| `GET /api/notify/rules`, `POST`/`PATCH`/`DELETE ?id=` | notification rule CRUD (bot token is write-only, returned masked) |
| `POST /api/notify/test` | send a test notification through one rule's outlets |

`tdl archive gc [--apply]` reclaims unreferenced media objects; it is a dry run
unless `--apply` is passed. Most read commands accept `--json`.

## Contact 💬

- 🌐 Website: [ceckit.com](https://ceckit.com/)
- 🤖 Bot: [@ConnectingEveryCornerBot](https://telegram.me/ConnectingEveryCornerBot)
- 👤 Telegram: [@ConnectingEveryCorner](https://telegram.me/ConnectingEveryCorner)
- 📢 Channel: [CECBoard](https://telegram.me/CECBoard)

## License

This project is licensed under AGPL-3.0. The full AGPL-3.0 license text is archived at `licenses/LICENSE.upstream-AGPL-3.0.txt`. Read the current `LICENSE` file for project license notes.
