# 给 AI / 脚本的快速上下文

> 读者是 AI 代理或写自动化脚本的人。只给结论、命令和坑，不给背景故事。
> 文中每条命令和路径都对着本仓库源码核对过；标 **【实测】** 的是在本机真跑过输出，
> 标 **【源码】** 的是读代码得出但未真跑下载。这个区分不要抹掉。

---

## 0. 一句话

`tdl` 是一个 Telegram 客户端工具，带两套完全不同的能力：

| 主线 | 命令 | 干什么 | 适合 |
|---|---|---|---|
| **单文件下载** | `tdl download`（别名 `dl`） | 按**消息链接**下载文件 | 知道具体是哪几条消息 |
| **归档系统** | `tdl archive ...` | 把**整个频道**抄进本地 SQLite + 文件库，再离线挑选 | 批量、全量、要索引 |

**要「批量下载一个频道的所有视频」，只能用第二条。**
`download` 只接受 `https://t.me/c/123/456` 这种带消息 id 的链接，给不了「整个频道」。
它也没有断点续传账本、没有媒体队列、没有去重。

一句话记法：**`dl` 是拿文件，`archive` 是拿库。库里有文件。**

---

## 1. 决策树

```
你要下什么？
├─ 几个具体消息里的文件
│    └─ tdl dl -u <链接> [-i mp4]
├─ 一个频道的全部/部分内容，之后要筛选、搜索、导出
│    ├─ 只要这一次                         → tdl archive sync <目标> --limit N
│    ├─ 要持续跟新（新视频自动下）          → tdl archive monitor add + tdl archive run
│    └─ 先建索引，视频稍后有选择地下        → monitor add --download-media=false，
│                                            再 media wake / 按 sha 单独取
└─ 已经归档过，只想把视频捞出来
     └─ tdl archive export --kind media --media-kind video --format jsonl
```

---

## 2. 前置：安装、登录

```bash
# 编译（本机已有 tdl 二进制时可跳过）
go build -o tdl .

# 装到 PATH（systemd unit 默认读 ~/.local/bin/tdl）
install -m 755 tdl ~/.local/bin/tdl

# 登录「收件账号」——这是你自己的用户账号，不是 bot token
tdl archive login          # 交互式：手机号 + 验证码
tdl archive login --qr     # 或者扫码
```

登录态存在 `~/.tdl/data`（bolt 存储，`--storage` 可改），**这个文件等于账号完全控制权**。
建议加密：`export TGA_SESSION_PASSPHRASE='...'`（写在 `~/.config/tdl/archive.env`，chmod 600）。

---

## 3. 数据落在哪

```
~/.tdl/archive/
├── tgarchive.db      SQLite：消息、媒体、监控源、任务、通知规则
├── objects/          媒体原件（内容寻址）
├── thumbnails/       缩略图
├── avatars/          头像缓存
├── staging/          下载中的半成品（gc 会清）
└── backups/          每日自动备份
```

**【实测】对象布局是两段前缀 + 完整 sha256，且没有扩展名：**

```
objects/7c/0e/7c0e5f80139c2c1c073ba1ee8ddf5cef130d0a8309ccf8828ea901f429d55fa4
objects/d9/38/d938f49deef9abb214468034c38c26d0aa46ddb6d82b572a989a23b92d1ac779
```

规则：`objects/<sha256[0:2]>/<sha256[2:4]>/<sha256>`

**【实测】`media` 表的 `original_path` 列存的就是上面那个绝对路径。**
所以「从 sha256 找文件」有两个办法：读这一列，或者自己拼这个路径。

**关键坑：文件名没有扩展名。** 拷到别处时必须按 `mime_type` 补扩展名，
否则播放器、图库、`ffmpeg -i` 全都不认。见第 7 节。

---

## 4. 命令全景

### 4.1 一次性 / 入库

| 命令 | 干什么 |
|---|---|
| `archive monitor add <@channel\|chat-id> --limit N --max-size 2GB` | 登记 + 回填 N 条历史（默认 1000），**媒体入队等消费** |
| `archive monitor list [--json]` | 看登记了哪些、是否启用 |
| `archive monitor set <chat-id> --keywords a,b --exclude c --max-size 50MB --download-media=false` | 改过滤器 / 大小上限 / 下载开关（只改传了的） |
| `archive monitor remove <chat-id>` | 停止监控（数据保留） |
| `archive sync <目标> --limit N [--resume]` | **回填 + 同步下媒体 + 导出 + 抓头像，跑完退出** |
| `archive run` | 常驻：实时收 + 每 2 分钟清媒体队列 + 每 10 分钟补洞 |

### 4.2 看 / 取

| 命令 | 干什么 |
|---|---|
| `archive stats [--json]` | `{"messages":N,"media":N,"monitors":N,"database":"..."}` **【源码】** |
| `archive inbox [--kind video] [--status completed] [--json] [--limit N]` | 编号台账（收件视角） |
| `archive file <serial> [--json]` | 编号 → 本地绝对路径 |
| `archive search <query> --media video --after 2026-01-01 --chat ID` | 本地全文检索 |
| `archive message <...>` / `archive post <serial>` | 单条详情 / 渲染成可发布文本 |
| `archive media pending [--json]` | 排队中和失败的媒体任务 |
| `archive media retry <job-id>` | 重排一个失败任务 |
| `archive media wake [--chat ID]` | 唤醒「入库时该会话还没被监控」而被搁置的下载 |

### 4.3 导出

| 命令 | 干什么 |
|---|---|
| `archive export --kind messages\|media\|links\|deleted\|edited\|materials` | 默认 CSV |
| `--format csv\|json\|jsonl\|tgjson` | tgjson = Telegram Desktop 布局 |
| `--fields id,title:name,body:content` | 选列 + 改名（冒号改） |
| `--map schema.json` | 目标站 schema 映射（优先于 `--fields`） |
| `--media-kind image\|video\|file` | **只有这三个值**，且默认 `image` |
| `--limit N` / `--chat ID` / `--topic ID` / `-q 关键词` | 过滤 |
| `--format tgjson [--with-media] [--split] -o DIR` | `-o` 是**目录** |

### 4.4 维护

| 命令 | 干什么 |
|---|---|
| `archive serve --addr 127.0.0.1:8080 [--auth-basic u:p \| --auth-token T]` | Web UI + 只读 JSON API |
| `archive gc [--apply] [--repair]` | 回收孤儿对象、清 staging（默认 dry run） |
| `archive notify add/list/set/remove/test/log` | 关键词 → Webhook / Telegram Bot |
| `archive doctor` | 自检 |
| `archive purge` | 按编号/会话/日期删（默认 dry run） |

### 4.5 传统单文件下载

```bash
tdl dl -u https://t.me/c/1234567890/42 -i mp4 -d ./videos
tdl dl -u <链接1> -u <链接2> -i mp4,mp3 --group -d ./out   # --group 连相册一起下
```

`-i/-e` 按**文件名**判断（不是 MIME）。`--rewrite-ext` 按文件头修正扩展名。

---

## 5. 批量下载视频：四种方案

### 方案 A — 一次性全量，跑完退出（推荐给脚本）

```bash
# 前提前提：先把它登记成监控源，否则 sync 的媒体会被"搁置"
tdl archive monitor add @somechannel --limit 1

# 回填 N 条 + 下完这些消息的媒体 + 导出 + 抓头像，然后退出
tdl archive sync @somechannel --limit 50000
```

**【源码】** `sync` 的实现末尾依次调用 `Backfill → MediaWorker.Run → Exporter.ExportPending
→ AvatarWorker.Run`，是**同步**的：媒体没下完它不会退出。
这是唯一「下完就退出」的命令，适合 cron。

`--resume` 从最旧的消息继续，否则从最新往回扫。全量抓老内容时要配合它。

### 方案 B — 持续跟新（新视频自动落库）

```bash
tdl archive monitor add @somechannel --limit 50000 --max-size 2GB

# 前台跑，Ctrl-C 停；数据全保留
tdl archive run
```

生产用 systemd user unit（`deploy/tdl-archive.service`，`ExecStart=tdl archive run`）：

```bash
cp deploy/tdl-archive.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now tdl-archive
loginctl enable-linger "$USER"
journalctl --user -u tdl-archive -f
```

**【源码】`archive run` 的两个 ticker：**
- 媒体队列 **每 2 分钟**清一轮，每轮最多 5 批 × 100 任务 = 500 个
- 补洞与镜像 **每 10 分钟**一次

所以「入库 done」和「媒体下完」之间**天然有几十分钟的差**。这不是卡住，是设计。
脚本判断完成要看 `media pending`，不要看 job 状态。

`archive serve` 在托管模式下（配了 `telegram.app-id`/`app-hash`）**也会跑同样的 ticker**，
所以只开面板不跑 `run` 也能下媒体。

### 方案 C — 先建索引，之后有选择地下

```bash
# 只记消息、记 sha256，不下载字节
tdl archive monitor add @somechannel --limit 100000 --download-media=false

# 挑完之后再唤醒（或者按 chat 唤醒）
tdl archive media wake --chat -1001234567890
tdl archive run
```

适合：先看有哪些、再决定下哪些、或者只下某一个大小区间。

### 方案 D — 已经归档过，只把视频捞出来

```bash
# 1) 导出视频清单（注意 --media-kind video，默认是 image）
tdl archive export --kind media --media-kind video --format jsonl -o /tmp/videos.jsonl

# 2) 按 mime_type 补扩展名，扁平拷到一个目录
jq -r '[.media_sha256, .media_mime] | @tsv' /tmp/videos.jsonl | while IFS=$'\t' read -r sha mime; do
  src="$HOME/.tdl/archive/objects/${sha:0:2}/${sha:2:2}/$sha"
  [ -f "$src" ] || { echo "missing: $sha" >&2; continue; }
  case "$mime" in
    video/mp4) ext=mp4 ;; video/webm) ext=webm ;; video/quicktime) ext=mov ;;
    *) ext=${mime#*/} ;;
  esac
  cp -n "$src" "./videos/${sha}.${ext}"
done
```

**【实测】`--kind media` 导出的列是：**

```
row_id, chat_id, message_id, date, sender, username, text,
media_sha256, media_mime, deleted, pinned, media_status
```

**没有 `original_path`。** 想要路径就自己拼（上面那样），或者用 `archive file <serial>`。
`media_status` 的 `completed` 才代表字节在磁盘上。

---

## 6. 给脚本吃的机器可读输出

| 命令 | 结构 |
|---|---|
| `archive stats --json` | `{"messages":int,"media":int,"monitors":int,"database":path}` **【源码】** |
| `archive inbox --json [--kind video] [--status completed]` | 台账数组，编号在消息落地时就分配 |
| `archive media pending --json` | 未完成的媒体任务（可判断"下完了没"） |
| `archive monitor list --json` | 监控源 |
| `archive file <serial> --json` | 单条解析结果（含本地路径） |
| `archive export --format jsonl` | 每行一条，最适合流式处理 |

**判断「下载完成了」的正确写法**（不要用 job 的 `done`）：

```bash
while :; do
  n=$(tdl archive media pending --json | jq 'length')
  [ "$n" -eq 0 ] && break
  echo "media pending: $n"
  sleep 30
done
```

`tdl archive serve` 还提供只读 HTTP API（`/api/messages/page`、`/api/media`、
`/api/export`、`/media/<sha256>` 等），脚本可以直接吃 JSON，不用调 CLI。
详见 [README.zh.md](../README.zh.md#http-api脚本--自动化用)。

---

## 7. 坑（每条都有症状，别踩）

| # | 坑 | 症状 | 正确做法 |
|---|---|---|---|
| 1 | **媒体是异步的** | job 显示 `done`/100%，但文件还没下 | 用 `media pending` 判断，不是 job 状态 |
| 2 | **队列 2 分钟才扫一轮** | 刚 add 完「没动静」 | 等一轮，或 `sync` 走同步路径 |
| 3 | **`--kind media` 默认 `image`** | 想要视频，导出来全是图 | 必须带 `--media-kind video` |
| 4 | **`--media-kind` 只有 image/video/file** | 传 `audio`/`document` 无效 | help 文本里那个 "audio, document, ..." 是过期的 |
| 5 | **对象文件没有扩展名** | 拷出来播放器不认 | 按 `media_mime` 补扩展名 |
| 6 | **`export` 没有 original_path** | 找不到磁盘路径 | 拼 `objects/xx/yy/<sha>` 或用 `archive file` |
| 7 | **`monitor add` 只排队不下载** | `add` 完文件不见 | 需要 `run` / `serve` / `sync` 之一在跑 |
| 8 | **`--max-size` 是逐文件** | 大视频被静默跳过 | 先看 `media pending`，超限的会有记录 |
| 9 | **Telegram 限速** | 报 `FLOOD_WAIT` | 别盲目加并发；`TDL_MEDIA_THREADS` 上限 16 |
| 10 | **`tdl` 与 `archive login` 是同一个会话** | 换账号会互相踢 | 用 `-n <namespace>` 隔离 |
| 11 | **`--limit` 是单次扫描上限** | 以为抓全了其实只有 1000 条 | 按需要显式给 `--limit` |
| 12 | **tgjson 的 `-o` 是目录** | 报错或写错位置 | `-o ./export/`，不是 `-o x.json` |

**【源码】媒体下载并发**：默认 4，`TDL_MEDIA_THREADS` 可调，<1 或 >16 会忽略并取默认。
单批次内的文件并发；`mediaGate` 限制的是同时进行的**批次**数，不是文件数。

---

## 8. 给 AI 的硬约束

1. **不要凭 help 文本猜枚举值。** `--media-kind` 的 help 写了 "audio, document, ..."，
   实际只有 `image`/`video`/`file`（`archive/store/media_list.go`）。
2. **不要用 job 状态判断文件到了没。** 用 `media pending` / `media_status=completed`。
3. **不要在 `export` 结果里找文件路径。** 只有 sha256；路径要拼或用 `archive file`。
4. **不要给「整个频道」调 `tdl dl`。** 它只吃带消息 id 的链接。
5. **不要为了提速把 `TDL_MEDIA_THREADS` 调到 16 以上。** 会被忽略，而且 Telegram 会罚
   `FLOOD_WAIT`（按墙钟时间，比你省下的时间更贵）。
6. **改完数据先 `archive gc` 的 dry run**，确认要删什么再 `--apply`。
7. **`purge` 默认 dry run**，别当它已经删了。

---

## 9. 一页速查

```bash
# 登录
tdl archive login

# 登记 + 回填 5 万条
tdl archive monitor add @chan --limit 50000 --max-size 2GB

# 常驻（新内容自动落库 + 下媒体）
tdl archive run

# 或者一次性跑完退出
tdl archive sync @chan --limit 50000

# 看进度
tdl archive stats --json
tdl archive media pending --json

# 捞视频清单
tdl archive export --kind media --media-kind video --format jsonl -o videos.jsonl

# 单条取文件
tdl archive file 42 --json

# 面板 + JSON API
tdl archive serve --addr 127.0.0.1:8080 --auth-token "$TOKEN"
```

---

## 附：相关文档

- [USAGE.md](USAGE.md) — 编号收件箱完整说明（`watch`/`inbox`/`file`/`post`/深链）
- [ARCHIVE-INBOX.md](ARCHIVE-INBOX.md) — 收件箱协议
- [../README.zh.md](../README.zh.md) — 归档子系统概览 + HTTP API 全表
- [../deploy/linux/README.md](../deploy/linux/README.md) — 生产部署（systemd、面板账号、升级）
