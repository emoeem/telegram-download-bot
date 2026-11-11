# telegram-download-bot

[English](README.md) | 中文说明

## 桌面端

![桌面端](./docs/images/desktop_zh.png)

下载地址：[GitHub Releases](https://github.com/ConnectingEveryCorner/telegram-download-bot/releases) https://pan.quark.cn/s/b0206663ed62

### macOS 首次打开

当前 macOS 应用未进行 Apple 签名与公证，若系统提示应用已损坏，请将应用移动到“应用程序”目录后在终端执行

```bash
xattr -cr "/Applications/TelegramDownloadBotDesktop.app"
```

telegram-download-bot 是一个基于 [tdl](https://github.com/iyear/tdl) 实现的 Telegram MTProto 下载回传机器人 用户把 Telegram 消息链接发给机器人后，机器人会使用已登录的 Telegram 用户账号下载原消息内容，再通过机器人把文本、图片、视频、文件或相册回传给当前聊天

![Usage demo](./docs/images/demo.gif)

## 功能

- 通过 Telegram Bot 接收 Telegram 消息链接
- 使用已登录的 MTProto 用户账号下载消息内容
- 通过机器人回传文本、图片、视频、文件和相册
- 在机器人流程内登录 Telegram 用户账号
- 仅管理员可管理授权
- 通过配置固定机器人语言，目前支持英文和中文

## 配置

先复制配置：

```bash
cp config.example.yaml config.yaml
```

然后编辑 `config.yaml`：

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

重要字段：

- `bot.token`：来自 BotFather 的 Bot Token
- `bot.language`：必填的机器人语言，只支持 `en` 和 `zh`
- `telegram.app-id` 和 `telegram.app-hash`：Telegram API 凭据
- `auth.admin-chat-id`：主管理员 chat id
- `auth.allowed-chat-ids`：允许使用机器人的普通用户
- `tdl.proxy`：可选代理，例如 `socks5://127.0.0.1:7890`

如何获取 `telegram.app-id` 和 `telegram.app-hash`：

1. 打开 [my.telegram.org](https://my.telegram.org/) 并使用 Telegram 手机号登录
2. 进入 **API development tools**
3. 如果还没有应用，先创建一个应用
4. 把页面显示的 `api_id` 填到 `telegram.app-id`，把 `api_hash` 填到 `telegram.app-hash`

语言是全局配置，用户不能在聊天里选择或切换语言 如果 `bot.language` 是 `en`，机器人提示使用英文 如果是 `zh`，机器人提示使用中文

## Docker Compose 部署

https://youtu.be/3rKaZDRmR5Q?si=rX-s2s5y2Ay-4ENg

从本仓库手动下载 `docker-compose.yml` 和 `config.example.yaml`，并把它们放到同一个部署目录

复制 `config.example.yaml` 为 `config.yaml`，然后在 `config.yaml` 中填写 Bot Token、Telegram API 凭据和管理员 chat id

启动机器人：

```bash
docker compose up -d
```

## 机器人命令

- `/start`：显示帮助
- `/help`：显示帮助
- `/myid`：显示当前 chat id
- `/login`：主管理员登录共享的 Telegram 账号
- `/login +4475834875`：主管理员直接带手机号登录共享账号

`/login` 仅允许 `auth.admin-chat-id` 配置的主管理员执行。登录成功后，所有已授权的机器人用户共享该 Telegram 登录状态，无需分别登录
- `/status`：检查登录状态
- `/logout`：清除当前账号会话
- `/cancel`：取消当前登录流程
- `/grant <chat_id>`：主管理员授权用户
- `/revoke <chat_id>`：主管理员撤销用户授权
- `/users`：主管理员查看授权状态

登录后，直接发送 Telegram 消息链接即可下载并回传

## 联系方式 💬

- 🌐 官网: [ceckit.com](https://ceckit.com/)
- 🤖 机器人: [@ConnectingEveryCornerCNBot](https://telegram.me/ConnectingEveryCornerCNBot)
- 👤 Telegram: [@ConnectingEveryCorner](https://telegram.me/ConnectingEveryCorner)
- 📢 频道: [CECCNBoard](https://telegram.me/CECCNBoard)

## 许可证

本项目使用 AGPL-3.0 许可 完整 AGPL-3.0 许可证文本归档在 `licenses/LICENSE.upstream-AGPL-3.0.txt` 项目许可说明见当前 `LICENSE` 文件

## 消息监控与归档（archive 子系统）

项目在原有「下载机器人」之外，同时提供长期运行的 **Telegram 消息监控与归档**能力。
采集走 **MTProto 用户账号**，与 Bot Token 入口分离：只要命令行出现 `archive`，即使
`config.yaml` 里配置了 `bot.token` 也不会启动 Bot（见 `main.go` 的 `isLocalOnlyCommand`）。

> 只想知道"怎么用"：看 [使用说明书（编号收件箱）](docs/USAGE.md) —— 编译、登录、收件、取编号、发布、删除、常驻、排障一条线走完。
>
> **要拿这个软件给别人用（卖 / 送 / 授权）**：用户看 [用户手册](docs/用户手册.md)，你自己看 [签发手册](docs/签发手册.md)。
> 前者讲"这软件干什么、撤回为什么救得回来、怎么上手"；后者讲"怎么签密钥、怎么登记、怎么撤销换机"。
>
> 用户手册有**三个出口，同一个源文件** `docs/用户手册.md`：
>
> | 出口 | 生成命令 | 产物 |
> |---|---|---|
> | 仓库内阅读 | — | `docs/用户手册.md` |
> | 发给客户的 PDF | `tools/gen-user-manual-pdf.sh` | `dist/docs/TG归档-用户手册.pdf`（A4、带目录页码、字体内嵌） |
> | App 内「⋯ → 使用说明」 | `tools/gen_help.py` | 写进 `archive/web/index.html` |
>
> 改了手册要同时重跑这两个脚本；`tools/gen_help.py --check` 校验弹窗与手册是否同步
> （`archive/web_assets_test.go` 里也有对应断言，走神了 `go test` 会红）。
> 手册里不要用空格或 ASCII 框线做对齐——PDF 与手机弹窗的字体宽度不受你控制，会散架。
>
> 想写脚本、或让别的 AI 接手这个项目：看 [给 AI / 脚本的快速上下文](docs/AI-QUICKSTART.md) —— 命令全景、批量下载视频的四种方案、机器可读输出、12 条已知坑。

数据默认落在 `~/.tdl/archive/`：

| 路径 | 内容 |
|---|---|
| `tgarchive.db` | SQLite（WAL）主库：消息、编辑版本、删除状态、实体、链接、reactions、媒体任务、监控目标、通知规则、同步游标 |
| `objects/<sha256 前 2/4 位>/…` | 媒体内容寻址存储（CAS，sha256 去重） |
| `staging/` | 媒体下载临时文件（完成后 rename 进 CAS） |

全文检索使用 SQLite FTS5 + `gse` 中文分词（写入时预分词为 `fts_text`，查询用同一分词器），
因此中文、英文、中英混排都可以直接搜。

### 首次使用

```bash
tdl archive login                                         # 登录归档用的用户账号（验证码）
tdl archive login --qr                                    # 扫码登录
tdl archive monitor add @channel --since 2026-01-01 --limit 1000
tdl archive run                                           # 常驻：实时监听 + 周期巡检 + 媒体 worker
```

`archive run` 会同时运行实时 updates/gap recovery、周期性历史巡检与媒体下载 worker。
消息写入使用 `(chat_id, message_id)` 幂等键，因此实时更新、历史回填、周期补扫可以安全重复执行。

### Web 控制台（登录 + 管理 + 浏览三合一）

在 `config.yaml` 配置 `telegram.app-id` / `app-hash` 后，`tdl archive serve` 会自动启用
**托管模式**：顶栏账号芯片实时显示登录状态，未登录时点击即可在浏览器里**扫码登录**
（手机 Telegram → 设置 → 设备 → 链接桌面设备；开启了两步验证的账号会提示输入云密码）。
登录成功后 serve 进程即作为实时归档器运行。网页上还可以：

- **添加 / 移除 / 补洞同步监控源**：左栏「＋ 添加源」，保存后自动后台回填历史，
  进度显示在右栏「任务」面板；每个源带 ↻（同步）与 ✕（移除）按钮；
- **按源浏览**：选中某个监控源后，消息 / 图片 / 视频 / 文件 / 导出都只显示该聊天；
- **时间线滑块翻页**：底部分页条是可拖动的进度条，拖到哪就浏览到那段历史，
  也可以直接输入页码精确跳转；
- **论坛话题**：话题群组会在顶部显示话题芯片栏（标题 + 消息数），点击只看该话题，
  消息内的 `#话题` 标签同样可以点击；
- 浏览消息 / 图片 / 视频 / 文件 / 链接 / 已删除 / 编辑历史 / 目的地频道；
- 「Storage」视图：数据库 / WAL / 媒体对象 / 备份 / 磁盘剩余空间一览。

未配置 app-id/hash 时 serve 以**只读模式**运行（仅浏览归档，顶栏芯片会提示如何开启托管）。
所有管理接口与查看接口共用同一套认证（`--auth-token` / `--auth-basic`），对外暴露时务必加认证。
网页右上角「❓ 说明」内置了完整使用指南。

### 媒体下载与消息正文里的链接

归档一条消息时，媒体任务分三类，各有各的取字节路径：

| 链接形态 | 处理方式 |
| --- | --- |
| 消息自带的图片 / 视频 / 圆视频 / 语音 / 贴纸 / 文件 | Telegram API 直取，sha256 内容寻址（CAS）去重 |
| 正文里的 YouTube / B 站 / X / 抖音等外链视频 | 交给 `yt-dlp`（需自行安装）；默认选渐进式 mp4，不依赖 ffmpeg |
| 正文里的 `t.me/<频道>/<消息id>` 或 `t.me/c/<id>/<id>` | 走 Telegram API：解析目标 peer → 读那一条 post → 下载它的附件 |

t.me 这一类是**引用别人的帖**，字节不在当前聊天里，所以不能靠历史扫描找到，
也不该交给 yt-dlp。它读的是被引用消息自身的附件，入库后与站内附件完全一致：
同一套 sha256、同一份缩略图、同一个导出触发。

**不会自动跟随的链接**（只记进 `message_links`，不入队）：裸频道链接
`t.me/<频道>`、邀请链接 `t.me/+hash`、机器人链接 `t.me/<bot>?start=...`。
裸频道链接尤其故意不跟随——一句话引用整个频道，会把它变成一次全量采集。

```bash
tdl archive media pending              # 看队列（含 tme 任务及其 source_url）
tdl archive media retry <job-id>       # 单个失败任务重排
```

外链视频的开关跟随监控源的「下载媒体」：`monitor add ... --download-media=false`
时两类外链都只归档链接文本。被引用的 post 若没有附件（纯文字、投票、别人的预览），
任务会以 `unsupported` 停下并写明原因，不会反复重试。

### 会话加密与自动备份（提案 §8/§9 落地）

- **会话加密落盘**：gotd session 等同账号完全控制权，`tdl archive login` 会交互式引导
  设置加密密码（AES-GCM + scrypt 派生），也可用 `--session-passphrase` 或
  `export TGA_SESSION_PASSPHRASE=...`；已存在的明文会话在首次加密打开时自动迁移，
  无需重新登录。之后所有 archive 命令读取会话时需提供同一密码（env 最方便）。
- **每日自动备份**：`archive run` 与 `serve` 托管模式每 24 小时执行一次
  `VACUUM INTO` 快照到 `~/.tdl/archive/backups/`，自动保留最近 7 份。
- **磁盘水位**：媒体下载前检查可用空间，低于 1 GiB 时暂停下载队列并提示，
  消息归档不受影响（GSE 词典已内嵌二进制，容器内分词同样可用）。

### 关键词通知（Webhook / Telegram Bot API）

匹配监控源关键词的**实时新消息**可以推送到外部出口——适合告警、抢购、资源发布等
「来了就要知道」的场景：

- 规则包含关键词白名单、排除词、可选的聊天范围（留空 = 全部监控源）；
- 出口二选一或同时配：**Webhook**（JSON POST，含 `text/chat/chat_id/message_id/date/rule`）
  与 **Telegram Bot API**（bot 发送文本到指定 chat_id，超长自动截断）；
- 每条规则限速 20 条/分钟，防止高频频道把出口打成 firehose；投递结果记录在
  `notify_log`（`tdl archive notify log` 可查），通知失败不影响归档本身；
- 历史回填不触发通知，只有实时新消息才会；Bot Token 支持 `TGA_NOTIFY_BOT_TOKEN`
  环境变量，避免出现在 shell 历史里。

右侧「通知」面板（或 `tdl archive notify` 命令）可增删规则、启停、一键发送测试通知。

### 自动点击（按钮 / t.me 链接代点）

在监控源（Web 控制台的添加/编辑弹窗）里开启**自动点击**后，频道新消息里可点的内容
由你的账号自动处理，结果直接推送到你：

- **内联 callback 按钮**：用用户账号调用 `messages.getBotCallbackAnswer` 直接点击，
  按钮的提示/弹窗文本原样带回；
- **`t.me/<bot>?start=…` 深链**：自动向该 bot 发送 `/start <参数>`，bot 的回复会自动
  捕获（约 25 秒窗口）并推送；
- **t.me 消息链接**（正文或 URL 按钮）：自动拉取原消息（含相册）归档入库并触发媒体下载；
- **外部网页链接**：出于安全考虑不会自动打开，只把链接推送给你。

点击结果通过右栏「点击」面板配置的**专用 Bot Token + chat_id**（Telegram Bot API）
发送，面板支持一键发送测试消息验证出口；全部动作记录在 `autoclick_log`（Web 面板可见
最近 50 条，重启后同一消息也不会重复点击）。每个监控源可设关键词过滤（匹配按钮
文字/链接，正文链接额外匹配消息正文，空=全部点击）；每源限速 20 次/分钟；外部网页
链接不会自动打开，只推送链接给你。消息卡片与详情页会渲染消息自带的内联按钮，方便回看。

### reactions 与浏览数

消息的 **reactions**（👍 / 🔥 / 自定义表情等聚合计数）随实时流、回填与巡检自动采集，
消息流里直接以芯片展示；频道/超级群的 **views / forwards** 计数由巡检通过
`messages.getMessagesViews` 定期刷新（每聊天每轮一次请求，覆盖最近 200 条），
即使实时流漏掉了计数更新也不会失真。

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
```

### HTTP API（脚本 / 自动化用）

`tdl archive serve` 同时是一套只读 JSON API，认证方式与页面一致（`--auth-token` 用
`Authorization: Bearer <token>`；`--auth-basic` 用 `-u user:pass`）。一旦配置了认证，
**包括 `/healthz`、`/readyz` 在内的所有路径都需要认证**（探针请带上同样的凭据）。

| 端点 | 说明 |
|---|---|
| `GET /healthz` / `GET /readyz` | 探针（配置认证后需要同样凭据） |
| `GET /api/stats` | 消息数 / 媒体数 / 监控源数 |
| `GET /api/messages/page?page=&page_size=&q=&chat=&topic=` | 分页消息列表（支持全部搜索语法，可按聊天与论坛话题过滤） |
| `GET /api/topics?chat=&limit=` | 论坛话题列表（标题、消息数、起止时间） |
| `GET /api/search?q=&limit=&offset=&chat=` | 搜索结果（limit ≤ 200） |
| `GET /api/message/rowid?rowid=` / `GET /api/message?chat=&id=` | 单条消息 |
| `GET /api/media?kind=image\|video\|file&page=&page_size=` | 媒体列表 |
| `GET /api/links?page=&page_size=&q=&domain=` / `GET /api/link_domains?q=` | 链接与域名聚合 |
| `GET /api/deleted` / `GET /api/edited` / `GET /api/edits?chat=&id=` | 删除 / 编辑历史 |
| `GET /api/export?kind=messages\|media\|links\|deleted\|edited\|materials\|serials&format=csv\|json\|jsonl&q=&chat=&topic=&media_kind=&domain=` | 流式导出，上限 5 万行 |
| `GET /api/storage` / `GET /api/jobs` / `GET /api/destinations` | 存储用量 / 任务进度 / 目的地 |
| `GET /api/targets` | 监控源列表 |
| `POST /api/targets` / `PATCH /api/targets` / `DELETE /api/targets?chat_id=` | 增 / 改（关键词、排除词、大小上限、下载与启用开关）/ 删监控源 |
| `POST /api/monitor/sync` | 触发补洞同步 |
| `GET /api/notify/rules` / `POST` / `PATCH` / `DELETE ?id=` | 通知规则的增删改查（Bot Token 只写不回显，仅返回掩码） |
| `POST /api/notify/test` | 向指定规则的出口发送测试通知 |
| `GET /api/auth/status`、`/api/login/qr/*`、`/api/login/password` | 登录状态与扫码登录 |
| `GET /media/<sha256>`、`/media/thumb|preview/<sha256>`、`/avatar/<kind>/<id>` | 媒体与头像（只读归档目录，带 `nosniff`） |

**导出成别的工具能读的格式**：`tdl archive export` 默认出 CSV/JSON/JSONL（`--kind` 选视图、
`--fields a,b:out` 选列并改名）。加 `--format tgjson` 则产出 Telegram Desktop 自己的导出
布局，能直接被 `telegram-chat-parser`、`tg-archive` 这类工具打开：

```bash
tdl archive export --format tgjson --with-media -o ./export/          # 全部会话
tdl archive export --format tgjson --split --with-media -o ./export/  # 每会话一个目录
```

媒体优先硬链接（同盘零拷贝），`--with-media` 没开时媒体键仍写出客户端那句「文件未包含」
提示——「有媒体但没导」和「没有媒体」必须能区分。细节见 [docs/USAGE.md](docs/USAGE.md#导出为-telegram-desktop-格式)。

写操作（`POST`/`PATCH`/`DELETE`）只在 serve 以托管模式启动时可用，即
`config.yaml` 配好 `telegram.app-id` / `app-hash`。

### 本地 Web Viewer

```bash
tdl archive serve
# 默认 http://127.0.0.1:8080
```

Viewer 支持：按监控来源浏览消息、中文全文搜索、发送者/频道头像、正文与 caption、
时间、用户名、置顶/删除状态，以及图片预览、视频播放、音频播放和附件直接打开。

`serve` 默认只监听 127.0.0.1。绑定到非本机地址时必须配置 `--auth-token` 或
`--auth-basic`（也可用 `TDL_ARCHIVE_AUTH_TOKEN` / `TDL_ARCHIVE_AUTH_BASIC`），
否则启动会被拒绝；`--insecure` 才会放行未认证的非本机绑定。

两种认证的分工（实测）：

| 方式 | 浏览器 | 脚本 / curl | 说明 |
|---|---|---|---|
| `--auth-login user:pass` | ✅ 页面登录表单 + 会话 Cookie | ⚠️ 需先 `POST /login` 拿 Cookie | 浏览器访问推荐；密码至少 8 位 |
| `--auth-basic user:pass` | ✅ 浏览器原生凭据弹窗 | ✅ `-u user:pass` | 简单，但密码会随每个请求发送 |
| `--auth-token <token>` | ❌ 只返回 401（没有登录表单） | ✅ `-H "Authorization: Bearer <token>"` | 适合 API/自动化 |
| 不配置 | ✅（仅 127.0.0.1） | ✅ | 只在本机使用时的默认姿态 |

配置任一认证方式后，`/healthz` 与 `/readyz` 也需要凭据（探针请一并配置）。
非本机绑定时若未配置认证会被拒绝启动；`--insecure` 才会放行。

写接口（`POST`/`PATCH`/`DELETE`）要求 `Content-Type: application/json`，并拒绝跨站
`Origin`；`/media/` 里的 SVG/HTML 一律以附件下载，不会被浏览器当作页面执行。

`/media/` 与 `/avatar/` 只允许读取归档数据目录内的文件，不接受任意路径。

### Telegram 归档目的地（可选）

本地 SQLite 归档始终保留；如果还希望把内容复制到 Telegram 频道，可以配置一个专用频道作为
「第二份可直接在 Telegram 中浏览的归档副本」：

```bash
tdl archive destination create-channel "我的消息归档"   # 新建并启用
tdl archive destination add @my_archive_channel        # 或用已有频道
tdl archive destination status                          # 查看启用状态
tdl archive destination disable <id>                    # 暂停同步
tdl archive destination enable <id>                     # 恢复同步
```

- 目的地使用独立的 `archive_exports` 幂等记录：同一条源消息不会重复发送到同一个目的地。
- `--mode telegram` 为低本地磁盘模式（不下载媒体，只同步文本）；
  `--mode both`（或默认 `local` + 后续 `destination attach`）则本地先落 CAS，再上传到目的频道。
- 媒体始终先进入本地 CAS，再上传；本地归档不受目的地开关影响。
- **受保护内容（禁止转发的聊天）**：本地归档不受影响（成员账号经 MTProto 照常收到全部
  数据）；镜像到目的频道时原生复制会被服务端拒绝，此时自动降级为「从本地重新上传媒体 /
  重发文本」——降级原因记录在 `archive_exports` 表中，可在数据库中审计。
  系统只归档当前账号有权访问的内容，不做任何绕过 Telegram 权限的设计。
