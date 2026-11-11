# telegram-download-bot

English | [中文说明](README.zh.md)

> **A realtime, delete-proof Telegram archiving system**: web console (PWA) + Android client + online licence system.
> Heavily rebuilt on top of the [tdl](https://github.com/iyear/tdl) core; the original "send a link, get the content back" bot is kept as an add-on mode (see the bottom).

![Usage demo](./docs/images/demo.gif)

## Original vs this fork

| Capability | tdl / the original bot | This fork |
|---|---|---|
| Downloading | send a link, fetch one message at a time | realtime capture: media is stored the moment a message arrives, nothing to babysit |
| Recalls & deletions | content is gone | a recall only adds a "deleted" tag: text, media and timestamps stay byte-identical (sha256 verified) |
| Edit history | not tracked | every edit is kept as a version |
| Media storage | loose files | content-addressed store (`objects/<sha256>`) with dedupe, staging dir and `gc` |
| History backfill | manual, per link | batch backfill by date/count, idempotent keyed writes |
| Interface | CLI / TUI | web console (installable PWA) + Android app |
| Search | none | SQLite FTS5 + embedded gse Chinese tokenizer; `from:` `has:` `before:` syntax |
| Export | none | streaming CSV/JSON/JSONL (50k-row cap) and Telegram Desktop format |
| Notifications | none | keyword rules pushed via Webhook / Bot API |
| Licensing | none | Ed25519 online activation with device binding |

## Desktop app

![Desktop app](./docs/images/desktop_en.png)

Download: [GitHub Releases](https://github.com/emoeem/telegram-download-bot/releases)

### First launch on macOS

The macOS app is not currently signed or notarized by Apple. If macOS says that the app is damaged, move it to Applications and run:

```bash
xattr -cr "/Applications/TelegramDownloadBotDesktop.app"
```

## The archive system (the core of this project)

**In one sentence: everything you see in Telegram is copied to your own device in realtime, and recalls or deletions cannot touch that copy.**
The moment a message arrives (not when you open it), its full text and media are already stored; if the sender later recalls it, your local copy just gains a "deleted" tag.

### Quick start

```bash
tdl archive login                          # log in the archiving user account (code)
tdl archive login --qr                     # or scan a QR code from the Telegram app
tdl archive monitor add @channel --since 2026-01-01 --limit 1000
tdl archive run                            # realtime + periodic rescan + media worker
tdl archive serve                          # web console on http://127.0.0.1:8080
```

`archive run` runs realtime updates/gap recovery, periodic history rescans and the media download worker together.
Messages are written through a single idempotent path keyed by `(chat_id, message_id)`, so realtime
updates, history backfill and periodic rescans can safely repeat.

### Where the data lives

Data lives in `~/.tdl/archive/`:

| Path | Content |
|---|---|
| `tgarchive.db` | SQLite (WAL) main database: messages, edit versions, deletion state, entities, links, reactions, media jobs, monitors, notify rules, sync cursors |
| `objects/<sha256 prefix>/…` | content-addressed media store (CAS, sha256 dedupe) |
| `staging/` | in-flight downloads (renamed into the CAS store when complete) |

Full-text search uses SQLite FTS5 + the embedded `gse` Chinese tokenizer, so Chinese, English and mixed text are all searchable out of the box.

### Web console (login + management + browsing, installable PWA)

The whole console is served from the binary (manifest, service worker, icons and the Tailwind stylesheet are embedded) and can be installed to a phone/desktop home screen.
With `telegram.app-id` / `app-hash` configured, `tdl archive serve` starts in **managed mode**: the
top bar shows the account state, and when logged out you can **scan a QR code in the browser**
(Telegram app → Settings → Devices → Link Desktop Device; 2FA accounts are prompted for the cloud
password). After login the serve process archives in realtime. The web UI can also:

- **add / remove monitor sources and trigger gap-fill syncs**, with backfill progress in the Tasks panel;
- **filter every view by source**: messages, media and export all scope to the picked chat;
- **scrub history with the draggable timeline slider**, or jump to an exact page number;
- **browse forum topics** via a chip bar (title + message count); `#topic` tags in messages are clickable;
- browse messages / images / videos / files / links / deleted / edit history / destination channels;
- inspect database/WAL/media/backup/disk usage in the Storage view.

Without credentials serve stays a **read-only viewer**. All control endpoints share the viewer
authentication (`--auth-token` / `--auth-basic` / `--auth-login`); always add authentication when
exposing the port. A built-in guide ("❓ Help") ships inside the web UI.

### Media download and links inside message text

When a message is archived, media jobs fall into three classes, each with its own byte path:

| Link form | How it is fetched |
| --- | --- |
| Native photos / videos / video notes / voice / stickers / files | straight from the Telegram API into the sha256 CAS store |
| External videos in the text (YouTube / Bilibili / X / TikTok…) | handed to `yt-dlp` (install it yourself); progressive mp4 preferred, no ffmpeg needed |
| `t.me/<channel>/<id>` or `t.me/c/<id>/<id>` quoted posts | through the Telegram API: resolve the peer → read that one post → download its attachment |

Quoted `t.me` posts are **someone else's message**: the bytes are not in the current chat, so history
scans cannot find them and yt-dlp is the wrong tool. The quoted post's own attachment is archived
through the exact same CAS path as native media: same sha256, same thumbnail, same export trigger.

**Links that are deliberately not followed** (recorded in `message_links` only): bare channel links
`t.me/<channel>`, invite links `t.me/+hash`, bot links `t.me/<bot>?start=...`. Bare channel links
especially: quoting a whole channel in one message would turn it into a full crawl.

```bash
tdl archive media pending              # queue (incl. tme jobs with their source_url)
tdl archive media retry <job-id>       # requeue a single failed job
```

Media concurrency is deliberately paced (batched jobs, `TDL_MEDIA_THREADS` knob) so the account never
trips `FLOOD_WAIT`; a typo in the knob falls back to the default instead of stopping archiving.

### Session encryption and daily backups

- **Encrypted session at rest**: the gotd session equals full account control, so `tdl archive login`
  interactively offers a passphrase (AES-GCM under a scrypt-derived key), or use `--session-passphrase`
  / `export TGA_SESSION_PASSPHRASE=...`. Existing plaintext sessions migrate automatically on the first
  encrypted open, no re-login needed.
- **Daily backups**: `archive run` and managed `serve` snapshot the database every 24 hours
  (`VACUUM INTO`, last 7 kept in `~/.tdl/archive/backups/`).
- **Disk floor**: media downloads pause automatically below 1 GiB of free space (message archiving
  never stops). The gse dictionary is embedded, so tokenization works in containers with `CGO_ENABLED=0`.

### Keyword notifications (Webhook / Telegram Bot API)

Realtime messages matching a rule's keywords can be pushed outward, for alerts, drops and releases:

- rules carry a keyword whitelist, exclude words and an optional chat scope (empty = all monitors);
- outlets, either or both: a **webhook** (JSON POST with `text/chat/chat_id/message_id/date/rule`)
  and the **Telegram Bot API** (bot sends the text to a chat id, truncated when too long);
- each rule is rate-limited to 20 notifications/minute; deliveries are logged (`tdl archive notify log`)
  and notification failures never affect archiving;
- history backfill does not notify, only realtime messages do; the bot token can come from
  `TGA_NOTIFY_BOT_TOKEN` to keep it out of shell history.

Manage rules in the web Notify panel or via `tdl archive notify add|list|set|remove|test|log`.

### Auto-click (buttons / t.me links)

With auto-click enabled on a monitor source, clickable content in new channel messages is handled by
your account automatically and the result is pushed back to you:

- **inline callback buttons**: clicked via `messages.getBotCallbackAnswer`, the button's alert/text comes back verbatim;
- **`t.me/<bot>?start=…` deep links**: `/start <payload>` is sent to the bot automatically, its reply is captured (~25s window) and pushed;
- **t.me message links** (in text or URL buttons): the quoted message (incl. albums) is fetched, archived and its media downloaded;
- **external web links**: never opened automatically, only pushed to you.

Results are delivered through a dedicated bot token + chat id configured in the Clicks panel; every
action is logged in `autoclick_log` (last 50 visible in the panel, no re-click of the same message
after restart). Per-source keyword filters and a 20 clicks/minute rate limit apply.

### Reactions and view counters

Message **reactions** (aggregated emoji / custom-emoji counts) are collected from realtime updates,
backfill and rescans, and render as chips in the feed; channel/supergroup **views/forwards** are
refreshed each sweep with one `messages.getMessagesViews` call per chat, so counters stay accurate
even when the realtime stream missed them.

### Mirror the archive into a Telegram channel (optional, destinations)

The local SQLite archive always exists; you can additionally configure a dedicated channel as a
second, Telegram-browsable copy:

```bash
tdl archive destination create-channel "My archive"   # create and enable
tdl archive destination add @my_archive_channel       # or use an existing channel
tdl archive destination status
```

- destinations use their own `archive_exports` idempotent ledger: one source message is never sent twice to the same destination;
- `--mode telegram` is the low-local-disk mode (text only); `--mode both` stores into the local CAS first, then uploads;
- **protected (no-forward) chats**: local archiving is unaffected; mirroring falls back to re-uploading media from the local CAS / re-sending text, with the fallback reason recorded in `archive_exports` for audit. The system only archives content the account can already access.

### Command reference

```bash
tdl archive login [--qr]                                  # code or QR login (interactive session passphrase)
tdl archive monitor add <@username|t.me link|chat-id> [--limit N] [--since DATE] [--download-media=false]
                                           [--mode local|telegram|both] [--telegram-channel <@chan>]
tdl archive monitor list
tdl archive monitor remove <chat-id>
tdl archive sync <@username|chat-id> [--limit N]
tdl archive run
tdl archive search "keyword" [--chat <chat-id>] [--limit N] [--after 2026-01-01] [--before 2026-10-01]
                                           [--sender @user] [--media any|image|video|file|audio]
tdl archive message <chat-id> <message-id>
tdl archive media pending
tdl archive media retry <job-id>
tdl archive notify add --name alerts --keywords outage,failure --webhook https://example.com/hook
tdl archive notify list | set <id> | remove <id> | test <id> | log
tdl archive destination create-channel <title>
tdl archive destination add <@channel|chat-id>
tdl archive destination list | enable <id> | disable <id> | attach <source chat-id> <destination id> | status
tdl archive stats
tdl archive doctor
tdl archive serve [--addr 127.0.0.1:8080] \
  [--auth-token <token> | --auth-basic user:pass | --auth-login user:pass | --auth-file <path>] [--insecure]
tdl archive gc [--apply]                                  # reclaim unreferenced media objects, dry run by default
```

### HTTP API (for scripts)

`tdl archive serve` also exposes a read-only JSON API guarded by the same credentials as the pages.
Once authentication is configured, **every path including `/healthz` and `/readyz` requires it**
(probes need the same credentials).

| Endpoint | Purpose |
|---|---|
| `GET /healthz` / `GET /readyz` | probes (auth required once configured) |
| `GET /api/stats` | message / media / monitor counters |
| `GET /api/messages/page?page=&page_size=&q=&chat=&topic=` | paged messages, full search syntax |
| `GET /api/topics?chat=&limit=` | forum topics |
| `GET /api/search?q=&limit=&offset=&chat=` | search results (limit ≤ 200) |
| `GET /api/message/rowid?rowid=` / `GET /api/message?chat=&id=` | single message |
| `GET /api/media?kind=image\|video\|file&page=&page_size=` | media listing |
| `GET /api/links?page=&page_size=&q=&domain=` / `GET /api/link_domains?q=` | links and domain aggregation |
| `GET /api/deleted` / `GET /api/edited` / `GET /api/edits?chat=&id=` | deleted / edit history |
| `GET /api/export?kind=messages\|media\|links\|deleted\|edited\|materials\|serials&format=csv\|json\|jsonl` | streaming export, 50k-row cap |
| `GET /api/storage` / `GET /api/jobs` / `GET /api/destinations` | storage usage / job progress / destinations |
| `GET /api/targets`, `POST` / `PATCH` / `DELETE /api/targets` | monitor management (managed mode) |
| `POST /api/monitor/sync` | trigger a gap-closing rescan |
| `GET /api/notify/rules`, `POST` / `PATCH` / `DELETE ?id=` | notification rule CRUD (bot token write-only, masked) |
| `POST /api/notify/test` | send a test notification through one rule's outlets |
| `GET /api/auth/status`, `/api/login/qr/*`, `/api/login/password` | login state and QR login |
| `GET /media/<sha256>`, `/media/thumb\|preview/<sha256>`, `/avatar/<kind>/<id>` | media and avatars (read-only, `nosniff`) |

**Export into other tools' formats**: `tdl archive export` emits CSV/JSON/JSONL by default
(`--kind` picks the view, `--fields a,b:out` renames columns). With `--format tgjson` it produces
Telegram Desktop's own export layout, openable by `telegram-chat-parser`, `tg-archive` and friends:

```bash
tdl archive export --format tgjson --with-media -o ./export/          # all sessions
tdl archive export --format tgjson --split --with-media -o ./export/  # one directory per session
```

Media is hard-linked when possible (zero-copy on the same disk); without `--with-media` the media
key still carries the client's "file not included" note, so "exported without media" and "no media"
stay distinguishable. Details in [docs/USAGE.md](docs/USAGE.md).

Write endpoints (`POST`/`PATCH`/`DELETE`) require `Content-Type: application/json` and reject
cross-site `Origin` (CSRF); SVG/HTML under `/media/` is always served as an attachment; `/media/`
and `/avatar/` only read inside the archive data directory. A dedicated licence activation page and
a secure data-wipe endpoint round it off.

### Viewer authentication

`serve` binds to 127.0.0.1 by default. Binding a non-local address requires authentication or startup
is refused (`--insecure` overrides).

| Method | Browser | Script / curl | Notes |
|---|---|---|---|
| `--auth-login user:pass` | ✅ login form + session cookie | ⚠️ needs `POST /login` first | recommended for browsers; password ≥ 8 chars |
| `--auth-basic user:pass` | ✅ native credentials prompt | ✅ `-u user:pass` | simple; password sent on every request |
| `--auth-token <token>` | ❌ 401 only (no login form) | ✅ `-H "Authorization: Bearer <token>"` | for APIs/automation |
| none | ✅ (127.0.0.1 only) | ✅ | default posture for local use |

## Android client (same core, no server needed)

- The whole Go archive core is compiled into an AAR with `gomobile bind` (`android/build-aar.sh`): not a subprocess, not Termux, not a website wrapper.
- The `ArchiveService` foreground service keeps the MTProto connection alive 24/7; a boot receiver restarts it after reboot.
- The WebView loads the exact same embedded web UI from `127.0.0.1`, so phone and desktop share one frontend (mobile breakpoints included).
- Session keys live in the app's private `filesDir`; the device id binds into the licence system.
- Architecture and build steps: [android/README.md](android/README.md).

## Online licence system

- Ed25519-signed licence keys in Crockford encoding, bound to a device id at first activation.
- A standalone licence server (`tdl license server`) records issuance and can revoke/release keys; revocation takes effect on the next heartbeat.
- The CLI gate and the web activation page both enforce it; keys can be embedded at build time (`tdl license build`).
- Design, issuing and deployment guides: [docs/授权体系.md](docs/授权体系.md), [docs/签发手册.md](docs/签发手册.md), [docs/在线授权部署.md](docs/在线授权部署.md).

## Documentation

| Doc | Content |
|---|---|
| [docs/用户手册.md](docs/用户手册.md) | end-user manual (what it is, why recalls cannot destroy the copy, how to start) |
| [docs/USAGE.md](docs/USAGE.md) | serial-inbox operator manual: build, login, receive, pick serials, publish, run persistently, troubleshoot |
| [docs/ARCHIVE-INBOX.md](docs/ARCHIVE-INBOX.md) | serial inbox workflow |
| [docs/ui-spec.md](docs/ui-spec.md) | web UI specification |
| [docs/授权体系.md](docs/授权体系.md) / [docs/签发手册.md](docs/签发手册.md) / [docs/在线授权部署.md](docs/在线授权部署.md) | licence design / issuing / server deployment |
| [docs/AI-QUICKSTART.md](docs/AI-QUICKSTART.md) | quick context for AI assistants and scripts: command map, batch download recipes, machine-readable output, known pitfalls |

The user manual has three outputs from one source file (`docs/用户手册.md`): read it in the repo;
`tools/gen-user-manual-pdf.sh` renders the customer PDF; `tools/gen_help.py` embeds it into the
app's "⋯ → Help" dialog.

## Bot link-download mode (add-on, kept from the original)

Besides the archive system, the original usage is kept: send a Telegram message link to the bot and
it downloads the message content with a logged-in user account, then sends text, photos, videos,
documents or albums back through the bot. Admin-only authorization management; bot language fixed by
config (`en` / `zh`).

Copy `config.example.yaml` to `config.yaml` and fill in:

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

Get `telegram.app-id` / `app-hash` at [my.telegram.org](https://my.telegram.org/) → API development
tools. Docker deployment: put `docker-compose.yml` and `config.example.yaml` in one directory, edit
`config.yaml`, then `docker compose up -d`.

Bot commands: `/start` `/help` help; `/myid` current chat id; `/login [phone]` admin logs in the
shared account; `/status` `/logout` `/cancel` session management; `/grant <chat_id>` `/revoke
<chat_id>` `/users` authorization. After login, send a message link directly. Commands starting with
`archive` always take the local entry point, regardless of `bot.token`.

## Contact 💬

- 🌐 Website: [ceckit.com](https://ceckit.com/)
- 🤖 Bot: [@ConnectingEveryCornerBot](https://telegram.me/ConnectingEveryCornerBot)
- 👤 Telegram: [@ConnectingEveryCorner](https://telegram.me/ConnectingEveryCorner)
- 📢 Channel: [CECBoard](https://telegram.me/CECBoard)

## License

This project is licensed under AGPL-3.0. The full AGPL-3.0 license text is archived at
`licenses/LICENSE.upstream-AGPL-3.0.txt`. Read the current `LICENSE` file for project license notes.
