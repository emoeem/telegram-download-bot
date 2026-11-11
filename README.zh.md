# telegram-download-bot

[English](README.md) | 中文说明

> **实时、防删除的 Telegram 归档系统**：Web 控制台（PWA）+ Android 客户端 + 在线授权体系。
> 基于 [tdl](https://github.com/iyear/tdl) 内核深度重构；原「发链接下载回传」机器人保留为附加模式（见文末）。

![Usage demo](./docs/images/demo.gif)

## 与原版对比

| 能力 | tdl / 原版机器人 | 本项目 |
|---|---|---|
| 下载方式 | 手动发链接，一次一取 | 实时抓取：消息到达即存，无需人守 |
| 撤回与删除 | 撤回即丢 | 撤回只是打个"已删除"标记：正文、媒体、时间戳逐字节保留（sha256 校验） |
| 编辑记录 | 不记录 | 每次编辑都留版本 |
| 媒体存储 | 散落文件 | 内容寻址存储（`objects/<sha256>`）自动去重，配 staging 与 `gc` |
| 历史回填 | 手动逐条 | 按日期/条数批量回填，幂等写入 |
| 界面 | CLI / TUI | Web 控制台（可安装的 PWA）+ Android App |
| 搜索 | 无 | SQLite FTS5 + 内嵌 gse 中文分词，支持 `from:` `has:` `before:` 语法 |
| 导出 | 无 | CSV/JSON/JSONL 流式导出（5 万行上限）+ Telegram Desktop 格式 |
| 通知 | 无 | 关键词规则推送 Webhook / Bot API |
| 授权 | 无 | Ed25519 在线激活 + 设备绑定 |

## 桌面端

![桌面端](./docs/images/desktop_zh.png)

下载地址：[GitHub Releases](https://github.com/emoeem/telegram-download-bot/releases) https://pan.quark.cn/s/b0206663ed62

### macOS 首次打开

当前 macOS 应用未进行 Apple 签名与公证，若系统提示应用已损坏，请将应用移动到"应用程序"目录后在终端执行

```bash
xattr -cr "/Applications/TelegramDownloadBotDesktop.app"
```

## 归档系统（项目主体）

**一句话：把你 Telegram 里看到的内容实时复制一份到自己设备上，撤回和删除对这份拷贝无效。**
消息到达的那一刻（不是你点开的那一刻），全文和媒体就已落库；对方事后撤回，本地只是多一个"已删除"标记。

### 30 秒上手

```bash
tdl archive login                                         # 登录归档用的用户账号（验证码）
tdl archive login --qr                                    # 扫码登录
tdl archive monitor add @channel --since 2026-01-01 --limit 1000
tdl archive run                                           # 常驻：实时监听 + 周期巡检 + 媒体 worker
tdl archive serve                                         # Web 控制台 http://127.0.0.1:8080
```

`archive run` 同时运行实时 updates/gap recovery、周期性历史巡检与媒体下载 worker。
消息写入使用 `(chat_id, message_id)` 幂等键，实时更新、历史回填、周期补扫可以安全重复执行。

### 数据落在哪

数据默认落在 `~/.tdl/archive/`：

| 路径 | 内容 |
|---|---|
| `tgarchive.db` | SQLite（WAL）主库：消息、编辑版本、删除状态、实体、链接、reactions、媒体任务、监控目标、通知规则、同步游标 |
| `objects/<sha256 前 2/4 位>/…` | 媒体内容寻址存储（CAS，sha256 去重） |
| `staging/` | 媒体下载临时文件（完成后 rename 进 CAS） |

全文检索使用 SQLite FTS5 + `gse` 中文分词（写入时预分词，查询用同一分词器），
中文、英文、中英混排都可以直接搜。

### Web 控制台（登录 + 管理 + 浏览三合一，可安装的 PWA）

整套控制台从二进制内嵌提供（manifest、service worker、图标、Tailwind 样式全内嵌），可安装到手机/桌面主屏。
配置 `telegram.app-id` / `app-hash` 后，`tdl archive serve` 自动启用**托管模式**：顶栏账号芯片实时显示
登录状态，未登录时点击即可在浏览器里**扫码登录**（手机 Telegram → 设置 → 设备 → 链接桌面设备；
开启两步验证的账号会提示输入云密码）。登录成功后 serve 进程即作为实时归档器运行。网页上还可以：

- **添加 / 移除 / 补洞同步监控源**：保存后自动后台回填历史，进度显示在「任务」面板；
- **按源浏览**：选中某个监控源后，消息 / 图片 / 视频 / 文件 / 导出都只显示该聊天；
- **时间线滑块翻页**：底部分页条是可拖动的进度条，也可以直接输入页码精确跳转；
- **论坛话题**：话题芯片栏（标题 + 消息数），点击只看该话题，消息内 `#话题` 标签同样可点；
- 浏览消息 / 图片 / 视频 / 文件 / 链接 / 已删除 / 编辑历史 / 目的地频道；
- 「Storage」视图：数据库 / WAL / 媒体对象 / 备份 / 磁盘剩余空间一览。

未配置 app-id/hash 时 serve 以**只读模式**运行。所有管理接口与查看接口共用同一套认证
（`--auth-token` / `--auth-basic` / `--auth-login`），对外暴露时务必加认证。
网页右上角「❓ 说明」内置了完整使用指南。

### 媒体下载与消息正文里的链接

归档一条消息时，媒体任务分三类，各有各的取字节路径：

| 链接形态 | 处理方式 |
| --- | --- |
| 消息自带的图片 / 视频 / 圆视频 / 语音 / 贴纸 / 文件 | Telegram API 直取，sha256 内容寻址（CAS）去重 |
| 正文里的 YouTube / B 站 / X / 抖音等外链视频 | 交给 `yt-dlp`（需自行安装）；默认选渐进式 mp4，不依赖 ffmpeg |
| 正文里的 `t.me/<频道>/<消息id>` 或 `t.me/c/<id>/<id>` | 走 Telegram API：解析目标 peer → 读那一条 post → 下载它的附件 |

t.me 这一类是**引用别人的帖**，字节不在当前聊天里，所以不能靠历史扫描找到，也不该交给 yt-dlp。
它读的是被引用消息自身的附件，入库后与站内附件完全一致：同一套 sha256、同一份缩略图、同一个导出触发。

**不会自动跟随的链接**（只记进 `message_links`，不入队）：裸频道链接 `t.me/<频道>`、邀请链接
`t.me/+hash`、机器人链接 `t.me/<bot>?start=...`。裸频道链接尤其故意不跟随：一句话引用整个频道，
会把它变成一次全量采集。

```bash
tdl archive media pending              # 看队列（含 tme 任务及其 source_url）
tdl archive media retry <job-id>       # 单个失败任务重排
```

媒体下载并发刻意限速（分批任务 + `TDL_MEDIA_THREADS` 调节），避免触发 `FLOOD_WAIT`；
参数写错只会回落默认值，不会停掉归档。

### 会话加密与自动备份

- **会话加密落盘**：gotd session 等同账号完全控制权，`tdl archive login` 会交互式引导设置加密密码
  （AES-GCM + scrypt 派生），也可用 `--session-passphrase` 或 `export TGA_SESSION_PASSPHRASE=...`；
  已存在的明文会话在首次加密打开时自动迁移，无需重新登录。
- **每日自动备份**：`archive run` 与 `serve` 托管模式每 24 小时执行一次 `VACUUM INTO` 快照到
  `~/.tdl/archive/backups/`，自动保留最近 7 份。
- **磁盘水位**：媒体下载前检查可用空间，低于 1 GiB 时暂停下载队列并提示，消息归档不受影响
  （gse 词典已内嵌二进制，容器内分词同样可用）。

### 关键词通知（Webhook / Telegram Bot API）

匹配监控源关键词的**实时新消息**可以推送到外部出口，适合告警、抢购、资源发布等场景：

- 规则包含关键词白名单、排除词、可选的聊天范围（留空 = 全部监控源）；
- 出口二选一或同时配：**Webhook**（JSON POST，含 `text/chat/chat_id/message_id/date/rule`）
  与 **Telegram Bot API**（bot 发送文本到指定 chat_id，超长自动截断）；
- 每条规则限速 20 条/分钟；投递结果记录在 `notify_log`，通知失败不影响归档本身；
- 历史回填不触发通知，只有实时新消息才会；Bot Token 支持 `TGA_NOTIFY_BOT_TOKEN` 环境变量，
  避免出现在 shell 历史里。

右侧「通知」面板（或 `tdl archive notify` 命令）可增删规则、启停、一键发送测试通知。

### 自动点击（按钮 / t.me 链接代点）

在监控源里开启**自动点击**后，频道新消息里可点的内容由你的账号自动处理，结果直接推送给你：

- **内联 callback 按钮**：用用户账号调用 `messages.getBotCallbackAnswer` 直接点击，提示/弹窗文本原样带回；
- **`t.me/<bot>?start=…` 深链**：自动向该 bot 发送 `/start <参数>`，回复自动捕获（约 25 秒窗口）并推送；
- **t.me 消息链接**（正文或 URL 按钮）：自动拉取原消息（含相册）归档入库并触发媒体下载；
- **外部网页链接**：不会自动打开，只把链接推送给你。

点击结果通过「点击」面板配置的**专用 Bot Token + chat_id** 发送；全部动作记录在 `autoclick_log`
（面板可见最近 50 条，重启后同一消息不会重复点击）。每源可设关键词过滤，限速 20 次/分钟。

### reactions 与浏览数

消息的 **reactions**（👍 / 🔥 / 自定义表情等聚合计数）随实时流、回填与巡检自动采集，消息流里直接
以芯片展示；频道/超级群的 **views / forwards** 计数由巡检通过 `messages.getMessagesViews` 定期刷新，
即使实时流漏掉了计数更新也不会失真。

### 归档到 Telegram 频道（目的地，可选）

本地 SQLite 归档始终保留；还可以配置一个专用频道作为「第二份可直接在 Telegram 里浏览的副本」：

```bash
tdl archive destination create-channel "我的消息归档"   # 新建并启用
tdl archive destination add @my_archive_channel        # 或用已有频道
tdl archive destination status                          # 查看启用状态
```

- 目的地使用独立的 `archive_exports` 幂等记录：同一条源消息不会重复发送到同一个目的地；
- `--mode telegram` 为低本地磁盘模式（不下载媒体，只同步文本）；`--mode both` 则本地先落 CAS 再上传；
- **受保护内容（禁止转发的聊天）**：本地归档不受影响；镜像到目的频道时原生复制会被服务端拒绝，
  此时自动降级为「从本地重新上传媒体 / 重发文本」，降级原因记录在 `archive_exports` 表中可审计。
  系统只归档当前账号有权访问的内容。

### 命令一览

```bash
tdl archive login [--qr]                                  # 验证码或扫码登录（交互设置会话加密密码）
tdl archive monitor add <@username|t.me链接|chat-id> [--limit N] [--since DATE] [--download-media=false]
                                           [--mode local|telegram|both] [--telegram-channel <@chan>]
tdl archive monitor list
tdl archive monitor remove <chat-id>
tdl archive sync <@username|chat-id> [--limit N]
tdl archive run
tdl archive search "关键词" [--chat <chat-id>] [--limit N] [--after 2026-01-01] [--before 2026-10-01]
                                           [--sender @user] [--media any|image|video|file|audio]
tdl archive message <chat-id> <message-id>
tdl archive media pending
tdl archive media retry <job-id>
tdl archive notify add --name 告警 --keywords 宕机,故障 --webhook https://example.com/hook
tdl archive notify add --name 推送 --keywords 福利 --bot-token $TGA_NOTIFY_BOT_TOKEN --bot-chat-id 123456
tdl archive notify list | set <id> | remove <id> | test <id> | log
tdl archive destination create-channel <标题>
tdl archive destination add <@channel|chat-id>
tdl archive destination list | enable <id> | disable <id> | attach <源 chat-id> <目的地 id> | status
tdl archive stats
tdl archive doctor
tdl archive serve [--addr 127.0.0.1:8080] \
  [--auth-token <token> | --auth-basic user:pass | --auth-login user:pass | --auth-file <path>] [--insecure]
tdl archive gc [--apply]                                  # 回收无引用媒体对象，默认 dry run
```

### HTTP API（脚本 / 自动化用）

`tdl archive serve` 同时是一套只读 JSON API，认证方式与页面一致。一旦配置了认证，
**包括 `/healthz`、`/readyz` 在内的所有路径都需要认证**（探针请带上同样凭据）。

| 端点 | 说明 |
|---|---|
| `GET /healthz` / `GET /readyz` | 探针（配置认证后需要同样凭据） |
| `GET /api/stats` | 消息数 / 媒体数 / 监控源数 |
| `GET /api/messages/page?page=&page_size=&q=&chat=&topic=` | 分页消息列表（全部搜索语法） |
| `GET /api/topics?chat=&limit=` | 论坛话题列表 |
| `GET /api/search?q=&limit=&offset=&chat=` | 搜索结果（limit ≤ 200） |
| `GET /api/message/rowid?rowid=` / `GET /api/message?chat=&id=` | 单条消息 |
| `GET /api/media?kind=image\|video\|file&page=&page_size=` | 媒体列表 |
| `GET /api/links?page=&page_size=&q=&domain=` / `GET /api/link_domains?q=` | 链接与域名聚合 |
| `GET /api/deleted` / `GET /api/edited` / `GET /api/edits?chat=&id=` | 删除 / 编辑历史 |
| `GET /api/export?kind=messages\|media\|links\|deleted\|edited\|materials\|serials&format=csv\|json\|jsonl` | 流式导出，上限 5 万行 |
| `GET /api/storage` / `GET /api/jobs` / `GET /api/destinations` | 存储用量 / 任务进度 / 目的地 |
| `GET /api/targets`，`POST` / `PATCH` / `DELETE /api/targets` | 监控源增删改（托管模式） |
| `POST /api/monitor/sync` | 触发补洞同步 |
| `GET /api/notify/rules` / `POST` / `PATCH` / `DELETE ?id=` | 通知规则增删改查（Bot Token 只写不回显） |
| `POST /api/notify/test` | 向指定规则的出口发送测试通知 |
| `GET /api/auth/status`、`/api/login/qr/*`、`/api/login/password` | 登录状态与扫码登录 |
| `GET /media/<sha256>`、`/media/thumb\|preview/<sha256>`、`/avatar/<kind>/<id>` | 媒体与头像（只读归档目录） |

**导出成别的工具能读的格式**：`tdl archive export` 默认出 CSV/JSON/JSONL；加 `--format tgjson`
则产出 Telegram Desktop 自己的导出布局，能直接被 `telegram-chat-parser`、`tg-archive` 这类工具打开：

```bash
tdl archive export --format tgjson --with-media -o ./export/          # 全部会话
tdl archive export --format tgjson --split --with-media -o ./export/  # 每会话一个目录
```

媒体优先硬链接（同盘零拷贝），`--with-media` 没开时媒体键仍写出客户端那句「文件未包含」提示：
「有媒体但没导」和「没有媒体」必须能区分。细节见 [docs/USAGE.md](docs/USAGE.md)。

### 认证与安全

`serve` 默认只监听 127.0.0.1。绑定到非本机地址时必须配置认证，否则启动被拒绝（`--insecure` 才放行）。

| 方式 | 浏览器 | 脚本 / curl | 说明 |
|---|---|---|---|
| `--auth-login user:pass` | ✅ 页面登录表单 + 会话 Cookie | ⚠️ 需先 `POST /login` 拿 Cookie | 浏览器访问推荐；密码至少 8 位 |
| `--auth-basic user:pass` | ✅ 浏览器原生凭据弹窗 | ✅ `-u user:pass` | 简单，但密码随每个请求发送 |
| `--auth-token <token>` | ❌ 只返回 401 | ✅ `-H "Authorization: Bearer <token>"` | 适合 API/自动化 |
| 不配置 | ✅（仅 127.0.0.1） | ✅ | 只在本机使用时的默认姿态 |

写接口要求 `Content-Type: application/json` 并拒绝跨站 `Origin`（CSRF 防护）；`/media/` 里的
SVG/HTML 一律以附件下载；`/media/` 与 `/avatar/` 只允许读取归档数据目录内的文件。
另有独立的授权激活页与安全擦除接口。

## Android 客户端（同一核心，不需要服务器）

- 整个 Go 归档核心用 `gomobile bind` 编进 AAR（`android/build-aar.sh`）：不是子进程、不是 Termux、不是网站套壳；
- `ArchiveService` 前台服务保持 MTProto 长连接 7×24 在线，开机由 BootReceiver 自启；
- WebView 加载的就是同一份内嵌 Web 界面（127.0.0.1 回环），手机与桌面共用一个前端（含移动端断点适配）；
- Telegram 会话密钥存放在应用私有 `filesDir`；设备码接入授权体系；
- 架构与构建见 [android/README.md](android/README.md)。

## 在线授权体系

- Ed25519 签发、Crockford 编码的密钥，首次激活时绑定设备码；
- 独立授权服务器（`tdl license server`）记录签发台账，支持撤销/解绑；撤销在下次心跳即生效；
- CLI 门禁与 Web 激活页双重校验；支持构建时把密钥编译进二进制（`tdl license build`）；
- 设计、签发与部署文档：[docs/授权体系.md](docs/授权体系.md)、[docs/签发手册.md](docs/签发手册.md)、[docs/在线授权部署.md](docs/在线授权部署.md)。

## 文档

| 文档 | 内容 |
|---|---|
| [docs/用户手册.md](docs/用户手册.md) | 使用者手册（这个软件是什么、为什么撤回救得回来、怎么上手） |
| [docs/USAGE.md](docs/USAGE.md) | 编号收件箱操作手册：编译、登录、收件、取编号、发布、常驻、排障 |
| [docs/ARCHIVE-INBOX.md](docs/ARCHIVE-INBOX.md) | 收件箱工作流说明 |
| [docs/ui-spec.md](docs/ui-spec.md) | Web 界面规范 |
| [docs/授权体系.md](docs/授权体系.md) / [docs/签发手册.md](docs/签发手册.md) / [docs/在线授权部署.md](docs/在线授权部署.md) | 授权设计 / 签发 / 服务器部署 |
| [docs/AI-QUICKSTART.md](docs/AI-QUICKSTART.md) | 给 AI / 脚本的快速上下文：命令全景、批量下载方案、机器可读输出、已知坑 |

手册的三个出口（同一源文件 `docs/用户手册.md`）：仓库内直接读；`tools/gen-user-manual-pdf.sh`
生成发给客户的 PDF；`tools/gen_help.py` 写进 App 内「⋯ → 使用说明」弹窗。

## 机器人链接下载模式（附加功能，原版保留）

除了归档系统，项目保留原版用法：把 Telegram 消息链接发给机器人，机器人用已登录的用户账号下载
原消息内容，再通过机器人把文本、图片、视频、文件或相册回传给当前聊天。仅管理员可管理授权，
语言由配置固定（`en` / `zh`）。

复制 `config.example.yaml` 为 `config.yaml` 并填写：

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

`telegram.app-id` / `app-hash` 在 [my.telegram.org](https://my.telegram.org/) → API development tools
创建应用获取。Docker 部署：下载 `docker-compose.yml` 与 `config.example.yaml` 放同一目录，
改好 `config.yaml` 后 `docker compose up -d`。

Bot 命令：`/start` `/help` 帮助；`/myid` 查当前 chat id；`/login [手机号]` 管理员登录共享账号；
`/status` `/logout` `/cancel` 会话管理；`/grant <chat_id>` `/revoke <chat_id>` `/users` 授权管理。
登录后直接发消息链接即可下载回传。归档命令（`archive` 开头）不受 `bot.token` 影响，始终走本地入口。

## 联系方式 💬

- 🌐 网站: [ceckit.com](https://ceckit.com/)
- 🤖 Bot: [@ConnectingEveryCornerBot](https://telegram.me/ConnectingEveryCornerBot)
- 👤 Telegram: [@ConnectingEveryCorner](https://telegram.me/ConnectingEveryCorner)
- 📢 频道: [CECCNBoard](https://telegram.me/CECCNBoard)

## 许可证

本项目使用 AGPL-3.0 许可 完整 AGPL-3.0 许可证文本归档在 `licenses/LICENSE.upstream-AGPL-3.0.txt` 项目许可说明见当前 `LICENSE` 文件
