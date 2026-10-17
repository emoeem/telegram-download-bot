# Telegram Message Monitor & Archive System — 调研报告与架构方案

> 项目代号：**tgarchive**
> 定位：Telegram Message Monitoring & Archive Platform（不是 Download Bot）
> 状态：调研完成，等待架构确认后进入实现
> 日期：2026-10-04

---

## 1. 参考项目分析：connectingeverycorner/telegram-download-bot

本地仓库实为 **iyear/tdl 的定制版**（module `github.com/iyear/tdl`），技术栈：gotd/td (MTProto) + cobra + viper + bbolt + zap + Fyne。

### 结构摘要

| 层 | 内容 | 对新系统的价值 |
|---|---|---|
| `core/tclient` | MTProto client 工厂：proxy、NTP 时钟、重连 backoff、recovery/retry/floodwait 三个中间件（tclient.go:45,90） | **高**，可直接作为独立 Telegram Client 的设计蓝本 |
| `core/dcpool` | 按 DC 惰性建立的 invoker 连接池（dcpool.go:41） | **高**，media worker 并发下载直接需要 |
| `core/storage` | session（gotd session JSON）/ peers（access_hash 缓存）/ state（updates StateStorage，**已实现但零调用**） | **高**，state.go 正是新系统要接线的关键预留件 |
| `pkg/kv` | bbolt KV（namespace 隔离） | 中，可被 SQLite 取代或保留为 session/peers 存储 |
| `app/chat/ls.go` | 健壮的 dialogs 分页遍历（fetchDialogsWithErrorHandling，batch 100、去重、死循环保护） | **高**，monitor target 枚举/校验参考 |
| `pkg/tmessage` + `core/util/tutil` | 链接解析 → peer → getHistory 单条取消息 / album 按 grouped_id 聚合 | 中，backfill 补洞可参考 |
| `core/downloader` + `app/dl` | 下载引擎（errgroup 并发、fingerprint 断点、.tmp 原子写、OnProgress 回调） | **高**，media worker 直接借鉴 |
| `app/tgbot` | Bot 命令层（admin/allowlist、对话式登录、YAML 改写、下载回传） | **低**，只服务 Download Bot |
| `app/desktop` | Fyne GUI | **低**，不采用 |
| 登录 | code / QR / tdata 导入 / Bot 对话式登录，session 明文存 bbolt | 中，登录流程可复用，**加密需自建** |

### 关键结论（回答七个问题）

1. **可作接入层参考**：`core/tclient`（client 工厂 + 三中间件）、`core/dcpool`、`core/storage`（session/peers/state）、`app/chat/ls.go`（分页骨架）、`core/downloader`（下载引擎与进度抽象）。
2. **适合抽成独立 Telegram Client**：`tclient + dcpool + storage + pkg/tclient(凭据)` 这组无 CLI 依赖的代码，天然是一个 `pkg/tgclient` 包。
3. **只服务 Download Bot**：`app/tgbot`、`app/desktop`、serve 模式、上传回传逻辑、configManager 的 YAML 运行时改写。
4. **tdl/gotd 职责**：gotd 承担 MTProto 传输 + 高层 API（peers.Manager、query 迭代器、downloader/uploader、auth）；tdl 加了生产化中间件、KV 持久化与按需拉取业务。**tdl 在这里没有任何 update 同步**——它的 `core/storage/state.go` 实现了 gotd StateStorage 接口但无人调用。
5. **改造为长期 monitor 的重组方式**：抽 client 层为独立库；接上 gotd `telegram/updates` gap engine + state 持久化；backfill 与 realtime 成为两个共享同一 client 的 consumer；历史回填需新写 `messages.getHistory` 翻页（tdl 只有 20 条窗口）。
6. **MTProto user session 应作为核心采集能力**：是。user 账号可订阅任意 public channel 收到全部 updates；bot 账号只能收到被加入的 chat 的更新，无法主动监听频道。归档采集必须以 user account 为主。
7. **Bot API vs MTProto 分工**：采集（updates、getHistory、getDifference、media 下载）一律 MTProto；Bot API 仅适合未来"通知出口/遥控入口"（发消息给管理员），不进核心 pipeline。

**禁止直接复制代码**——按以上设计结论重写，仅借鉴结构与中间件组合方式。

---

## 2. 开源项目调研（10 个）

| 项目 | 语言/API | 存储/搜索 | 增量/恢复 | 关键启示 |
|---|---|---|---|---|
| **Vendetta** (nixxoq/Vendetta) | Rust/grammers | SQLite WAL+FTS5，sha256 内容寻址+硬链接去重 | pts/qts/seq cursor + getDifference/getChannelDifference 双通道，环形缓冲溢出回退重扫 | **与本方案 1:1 同构**，其 docs/database.md、docs/mtproto.md 是最佳参照 |
| **GeiserX/Telegram-Archive** | Python/Telethon | SQLite 或 PG，媒体 symlink 去重 | 定时增量扫描 + 实时 listener **双通道互兜**；编辑全版本保留 + 软删除 + "What changed" feed | 产品形态参照：编辑历史/软删/变更 feed 均已验证可行 |
| **groupultra/telegram-search** (~4.1k★) | TS/gramjs | PG/PGlite + MinIO，中文分词+向量搜索 | CLI sync 任务模型 | 中文搜索的最成熟思路；但其向量/embedding 依赖超出 MVP |
| **knadh/tg-archive** (~1.5k★) | Python/Telethon | SQLite → 静态 HTML+RSS | last-id 定时增量 | 静态归档信息架构；无 FTS、无编辑删除，不够 |
| **iyear/tdl** (~7.6k★) | Go/gotd | 无库（JSON 导出） | fingerprint 断点续传 | Go+MTProto 工程标杆；session 管理/续传任务模型 |
| **telegram_media_downloader** (~4.9k★) | Python/Pyrogram | data.yaml 游标 | 逐 chat last id | 媒体过滤 DSL、下载状态机 |
| **momaek/tgchannel** | Go/MTProto | SQLite | serve 常驻监听 + fetch 分页回填 | Go 同栈小而完整的四件套参照（batch 100/delay 1-5s/retry 3） |
| **DyAxy/Telegram-Channel-Parser** | TS/gramjs | SQLite + Brotli 压缩 | 启动全量+实时 | 编辑/删除事件接入方式；压缩策略可考虑 |
| **tgarchive-viewer** | Python/React | 消费 Desktop 导出 + ES | 导入式 | viewer 与数据解耦思路；ES 栈不采用 |
| **Telethon / gram-js** | 库 | — | MessageBox gap 缓冲 0.5s 再 getDifference | gap 检测算法参照；gram-js 已归档，Go 直接用 gotd/td |

### 五个关键问题的答案

1. **长期运行+崩溃恢复+补洞**：Vendetta（pts cursor + 双 difference）和 GeiserX（定时重扫兜底实时）验证了正确模式：**实时 updates + 周期性 getHistory 重扫互相兜底**，cursor 持久化在数据库。
2. **SQLite+FTS5**：Vendetta 是唯一实践，证明可行；**中文 FTS5 是开源空白**——unicode61 对中文不可用，无现成 Go 方案，需自建分词层（见 §7）。
3. **内容寻址存储**：Vendetta（sha256+硬链接）、GeiserX（symlink）均验证可行。
4. **编辑历史+软删除**：GeiserX 最完整（版本表 + deleted 标记 + 变更 feed）。
5. **Web UI**：GeiserX 的类 Telegram viewer（搜索/主题/媒体库/变更 feed）是功能集参照；Go 内嵌静态资源实现单二进制。

---

## 3. Telegram API 调研要点

### Updates 体系

- `updateNewMessage`（私聊/基本群，全局 pts）/ `updateNewChannelMessage`（channel/supergroup，**独立 channel pts**）。
- `updateShortMessage` / `updateShortChatMessage`：带宽优化格式，**不携带 entities**，必须靠本地 peer 缓存（access_hash）补全——这是必须持久化 peers 的直接原因。
- `updateEditMessage` / `updateEditChannelMessage`（编辑）、`updateDeleteChannelMessages`（channel，带 pts）/ `updateDeleteMessages`（全局）。
- reactions（`updateMessageReactions`）、pinned、views/forwards（快照字段，需 `messages.getMessagesViews` 定期刷新）。

### pts 与差量恢复

- 校验式：`local_pts + pts_count == new_pts`，不等即 gap。
- `updates.getState` → 四元组（pts/qts/seq/date）；`updates.getDifference`（全局）/ `updates.getChannelDifference`（channel，limit 100）。
- `ChannelDifferenceTooLong`：增量不可行，需整段重拉。
- gotd 的 `telegram/updates` gap engine（新版，取代 contrib Engine）处理以上全部；**坑**：自己发起的 `affectedMessages` 类 RPC 必须用 updhook 推进本地 pts（gotd#1782）。
- Telethon MessageBox 算法参照：gap 缓冲 0.5s 再 getDifference；15 分钟无更新强制校验。

### gotd/td 具体支持

- `telegram/updates.New(...)` + `gaps.Run(ctx, api, selfID, AuthOptions)`：需要装在 `Options.UpdateHandler` + `updhook.UpdateHook` 中间件。
- `telegram/peers.Manager` + contrib storage：从每条 update 自动学习 peers（`storage.UpdateHook`）。
- `query.Messages(api).GetHistory(peer)`：`offset_id/offset_date/add_offset` 从新到旧分页。
- `channels.getMessages(ids)`：按 ID 精确补洞。
- `tgerr.FloodWait` / contrib `floodwait.NewWaiter()` + `ratelimit`：FloodWait 正确姿势。
- `ChannelDifferenceTooLong` 无法增量恢复 → 回退 getHistory 全量重扫（新系统必须实现此回退路径）。

### access_hash

peer = id + access_hash；username 可变、resolveUsername 有严格 FloodWait。**每次响应/更新中的 users/chats entities 必须落库**；冷启动先 CollectPeers（拉 dialogs 预热）。

---

## 4. 架构方案

```
                    Telegram (MTProto, user account)
                       │
                       ▼
            ┌──────────────────────────┐
            │  pkg/tgclient            │   client 工厂（借鉴 tclient+dcpool）
            │  gotd/td + updates gap   │   recovery/retry/floodwait/ratelimit
            │  engine + peers.Manager  │
            └───────────┬──────────────┘
                        │  normalized updates + raw TL objects
                        ▼
            ┌──────────────────────────┐
            │  collector               │
            │  ├ realtime  (gaps.Run)  │   实时流
            │  ├ backfill  (getHistory)│   历史回填/首添全量
            │  └ rescan    (补洞巡检)  │   周期性对齐 max msg_id
            └───────────┬──────────────┘
                        │  同一条幂等写路径 (UPSERT)
                        ▼
            ┌──────────────────────────┐
            │  store (SQLite, WAL)     │
            │  raw + normalized 双层   │   FTS5 external content
            └──────┬───────────┬───────┘
                   │           │
          ┌────────▼───┐   ┌───▼─────────────┐
          │ media      │   │ 查询层           │
          │ worker 池  │   │ search/browse/  │
          │ (dcpool +  │   │ export/stats    │
          │  CAS 落盘) │   └───┬──────┬───────┘
          └────────────┘       ▼      ▼
                          CLI (cobra)  Web UI (embed)
```

**基础设施决策（刻意保持简单）**：

| 问题 | 决策 |
|---|---|
| message queue? | **否**。Go channel + 单写者批事务足够；SQLite 单写锁即天然串行化 |
| worker pool? | **是**，仅 media worker（errgroup SetLimit，借鉴 tdl downloader） |
| Redis? | **否**。队列即 SQLite 表 `media_jobs`，崩溃天然持久 |
| PostgreSQL? | **否**（第一阶段）。触发迁移条件见 §7 |
| 独立 media worker 进程? | 第一阶段同进程内 goroutine 组 + graceful shutdown；表结构已支持未来拆进程 |
| 独立 search service? | **否**。FTS5 内嵌 |
| Bot API? | **否**（第一阶段）。CLI + Web UI 即入口 |

可靠性设计（详见 §8）全部内建，不靠外部组件。

---

## 5. 数据库 Schema（SQLite）

```
accounts(id PK, phone, api_id, api_hash, session_blob_encrypted,
         status, created_at, updated_at)            -- 多账户预留，MVP 单账户

chats(id PK,                        -- 标准化 -100xxxx / -xxxx / +xxxx
      type,                         -- user|chat|channel|supergroup
      username, title, access_hash, -- access_hash 必存
      last_msg_id,                  -- 巡检对齐用
      raw_json,                    -- 原始 tg.Chat/User
      created_at, updated_at)

users(id PK, username, first_name, last_name, phone, is_bot,
      access_hash, raw_json, created_at, updated_at)

messages(id INTEGER PK,            -- SQLite rowid
         chat_id, message_id,      -- UNIQUE(chat_id, message_id) 幂等键
         sender_id,
         date, edit_date,
         text, caption,            -- 原文保留（重建索引必需）
         reply_to_message_id, reply_to_top_id, reply_to_peer_id,
         thread_id, grouped_id,
         forward_from_id, forward_from_chat_id, forward_from_message_id,
         forward_from_name, forward_post_author,
         views, forwards, reply_count,
         has_media, has_entities,
         pinned,
         deleted, deleted_at,      -- 软删除
         raw_json,                 -- 原始 tg.Message 完整序列化（双层存储）
         fts_text,                 -- gse 预分词后的空格分隔文本
         created_at, updated_at)
  INDEX (chat_id, message_id), (chat_id, date), (grouped_id), (sender_id)

message_edits(id PK, message_rowid FK, version, text, caption,
              raw_json, edit_date, archived_at)     -- 完整版本历史

message_links(id PK, message_rowid FK, url, domain, expanded_url)

message_entities(id PK, message_rowid FK, type, offset, length, value)
                 -- bold/mention/hashtag/url 等，value 为可提取载荷

message_reactions(id PK, message_rowid FK, emoji, count, raw_json)

media(id PK, message_rowid FK,
      type,                       -- photo|video|audio|voice|document|sticker...
      file_id_unique,             -- Telegram file_unique_id（跨消息稳定）
      file_name, mime_type, size,
      width, height, duration,
      sha256,                     -- 内容寻址 key（下载完成后回填）
      ref_count,                  -- 同一 sha 多消息引用计数（物理去重）
      status,                     -- pending|queued|downloading|completed|failed|skipped
      error, attempts, next_retry_at,
      downloaded_at, created_at, updated_at)
  UNIQUE(message_rowid), INDEX (sha256), INDEX (status)

files(sha256 PK, size, mime_type, state, ref_count,
      created_at)                  -- 物理对象登记表，media 指向它

sync_cursors(account_id, chat_id,                  -- 主键复合
             channel_pts, pts,
             backlog_state,                        -- pending|running|done
             backlog_oldest_msg_id,                -- 回填游标（从新到旧）
             last_msg_id, last_seen_at,
             updated_at)

monitor_targets(chat_id PK, username, enabled,
                archive_text, archive_media, archive_files,
                media_max_size, download_media,
                keywords_json, exclude_keywords_json,
                since_date,                        -- 历史起点
                created_at, updated_at)

archive_jobs(id PK, chat_id, kind, state, params_json,
             error, started_at, finished_at)      -- backfill/rescan 任务

tags(id PK, name UNIQUE); message_tags(message_rowid, tag_id)
```

### 双层存储（raw + normalized）

- **raw**：每条消息的完整 `tg.Message` 序列化为 JSON 存 `raw_json`（chats/users 同理）。Telegram API 结构未来变化时可以**重新归一化（re-normalize）**而不丢数据。
- **normalized**：上表的全部结构化列 + FTS。
- FTS5 表：`messages_fts` **external content**（`content='messages', content_rowid='id'`），索引 `fts_text` 列，`tokenize='unicode61'`，BM25 排序，写入走同一事务的触发器。

### ER 概览

```
accounts ──< sync_cursors >── chats ──< messages >── media >── files
                                   │        │
                                   │        ├──< message_edits   (1:N 版本)
                                   │        ├──< message_links
                                   │        ├──< message_entities
                                   │        ├──< message_reactions
                                   │        └──< message_tags >── tags
                                   └── monitor_targets (chat_id 关联)
users (sender 反查)      archive_jobs (chat_id 关联)
```

---

## 6. 存储设计

```
data/
├── tgarchive.db            # SQLite（WAL, synchronous=NORMAL）
├── tgarchive.db-wal
├── sessions/
│   └── default.bin         # 加密的 gotd session（见 §9）
├── backups/                # VACUUM INTO 每日快照
└── archive/
    ├── objects/ab/cd/<sha256>       # 内容寻址，2 级目录分片
    ├── staging/<sha256>.tmp         # 下载原子写（完成后 rename）
    └── thumbnails/<sha256>.jpg      # 缩略图（可选）
```

- **内容寻址**：sha256 为对象 key，同一文件被 N 条消息引用只在 `files.ref_count` 计数，物理仅一份（Vendetta 验证）。
- **消息与媒体解耦**：`media.sha256` 为空表示未下载；下载完成后回填。目录结构不含 chat/message_id，避免强绑定。
- 删除消息**不删对象**（ref_count 归零后由未来的 GC 决策，MVP 不做 GC）。

---

## 7. 搜索设计

调研结论：FTS5 在数千万级短文本下 P99 <20ms，索引为原文 25-50%，完全够用；ES/Meilisearch 在 <1 亿条时是纯负担。

**中文是关键决策点**。SQLite unicode61 对中文不可用；`wangfenjing/simple` 需要 cgo（破坏纯 Go 交叉编译）。选定方案：

1. **主索引**：写入时用 **gse**（纯 Go jieba 实现，9.2MB/s，线程安全单例）`CutSearch` 预分词，存空格分隔的 `fts_text`，FTS5 用 `unicode61` 索引。中英混排天然友好，1-2 字词可搜。
2. **查询端**：同一 gse 单例对 query 分词，拼 `AND/OR` FTS5 查询串。
3. **约束**：分词器版本随 schema 版本固定；保留 `text` 原文列 → 换分词器 = 重建 FTS 表，数据无损。
4. **辅索引（Phase 2 可选）**：trigram FTS5 表做任意子串搜索（≥3 字命中，1-2 字 LIKE 兜底）。
5. 过滤维度（非 FTS）：chat_id、sender_id、date 范围、media.type、has_media、tags、message_id —— 全部走 messages 表索引 + FTS 结果 join。

**迁移 PostgreSQL 的触发条件**：多进程并发写需求、单库 >50-100GB、查询 P99 持续 >500ms。届时目标：PG + zhparser 或 Meilisearch 单节点。

---

## 8. 可靠性设计（断线与补偿）

```
启动
 ├─ 加载 session → 验证有效 → 无效则要求 login
 ├─ gaps.Run：getState → getDifference/getChannelDifference 回放 → 实时流
 ├─ 启动时对每个 target：比较 DB.max_msg_id 与服务端最新
 │    差距 > 阈值 → 触发 backfill（getHistory 从 DB 缺口处拉）
 └─ rescan 巡检循环（每 N 分钟）：max(channel.last_msg_id) 对齐，漏了就补
```

- **三通道合一**：realtime（gap engine）、backfill（首添/大缺口）、rescan（周期兜底）。三者走**同一条幂等写路径**：`UPSERT ... ON CONFLICT(chat_id, message_id)`，天然无重复归档。
- **编辑**：update 到达时，若 `edit_date` 变化 → 当前版本快照进 `message_edits`，再更新主表。rescan 对比 `edit_date` 可补偿漏掉的编辑。
- **删除**：软删（`deleted=1, deleted_at`），数据永不物理删除。private chat 对端删除不可靠——如实接受（MTProto 限制）。
- **崩溃恢复**：cursor（channel_pts、backfill_oldest_msg_id）与消息同库同事务提交；重启后从断点继续，`ChannelDifferenceTooLong` 回退 getHistory。
- **FloodWait**：contrib floodwait waiter + ratelimit 中间件 + 指数退避；backfill 批间主动 sleep（batch 100 / sleep 1-2s，参考 tgchannel 与 tdl）。
- **重试**：media 下载失败入 retry 队列（attempts、next_retry_at、指数退避）。
- **磁盘满**：media worker 写前检查可用空间，低于阈值暂停队列并告警，消息归档不受影响。
- **DB 完整性**：启动 `PRAGMA quick_check`；每日 `VACUUM INTO` 备份；WAL checkpoint 管理。
- **graceful shutdown**：SIGTERM → 停收 update → 冲刷批事务 → media worker 等待在途任务 → 关闭。

---

## 9. 安全设计

- **session 加密**：gotd session（等同账户完全控制权）不以明文落盘 —— 用主密码通过 HKDF/scrypt 派生密钥，AES-GCM 加密后存 `sessions/`；启动时解锁（CLI 交互输入或 `TGA_SESSION_PASSPHRASE` env）。
- **API ID/Hash**：不进 config.yaml 明文 → 存 `accounts` 表（同受 DB 权限保护）；config.yaml 仅含非敏感项。
- **Web 认证**：Phase 2 —— 单管理员模式：首次启动生成 token，或 HTTP Basic + HTTPS 反代；MVP 阶段 Web 仅监听 127.0.0.1。
- **日志脱敏**：zap 层过滤 phone/验证码/token 字段。
- **权限边界**：系统只归档**当前账号有权访问的内容**，不做任何绕过 Telegram 权限的设计（合规边界）。
- **备份**：session 文件 + DB 快照 + objects/ 三件套即可完整迁移部署。

---

## 10. 技术栈选型

| 候选 | 评估 |
|---|---|
| **Go + gotd/td** | 参考项目验证过的组合；单二进制部署；goroutine 天然适合长驻监听 + worker；gotd 的 updates gap engine / peers / downloader 是三大生态独有优势；tdl 已示范工程化姿势 |
| Rust + grammers | grammers 成熟度低于 gotd；开发效率低；无对应 peers/updates engine 完整件 |
| Python + Telethon | Telethon 最成熟（MessageBox 参照来源），但长期运行内存/GIL/部署劣势，且放弃与参考项目的经验复用 |

**选定：Go 1.25 + gotd/td + modernc.org/sqlite（纯 Go，无 cgo）+ gse + cobra + embed Web UI。**

关键库：
- `github.com/gotd/td` + `github.com/gotd/contrib`（floodwait、peers storage、ratelimit）
- `modernc.org/sqlite`（纯 Go 驱动，FTS5 内建支持）
- `github.com/go-ego/gse`（中文分词）
- `github.com/spf13/cobra`、`gopkg.in/yaml.v3`（配置）
- `go.uber.org/zap`

---

## 11. MVP 范围（Phase 1）

```
tgarchive login                    # code/QR 两种方式（tdata 导入列为 Phase 1.5）
tgarchive monitor add @channel     # 落库 + 立即触发历史回填（可 --limit/--since）
tgarchive monitor list/remove
tgarchive sync @channel            # 手动触发回填/补洞
tgarchive run                      # 长驻：实时监听 + rescan 巡检 + media worker
tgarchive search "关键词" [--chat --after --before --sender --media]
tgarchive message <chat> <msg_id>  # 查看单条（含 raw）
tgarchive media pending/retry
tgarchive stats
tgarchive doctor                   # session/DB/磁盘/配置体检
tgarchive serve                    # Web UI (embed, 127.0.0.1)
```

MVP 覆盖：user login、monitor targets、历史回填（--since/--limit/--resume）、实时监听（gap engine + cursor）、双层消息归档（文本/caption/photo/file/video 元数据、链接、sender、chat、时间、reply、forward、grouped、entities）、编辑版本、软删除、FTS5 中文搜索、media 独立 worker（CAS 存储）、Web UI 基础（Dashboard/Sources/Messages/Search/Media/Storage）。

**明确不做（Phase 1）**：通知系统、监控规则引擎、Bot API 入口、多账户、reactions 采集细化、trigram 辅索引、导出、GC。

---

## 12. Phase 2+

- 通知出口（Telegram Bot API / Webhook）、监控规则引擎（关键词→动作）
- trigram 辅索引、导出（HTML/JSONL）、多账户
- reactions/views 定期刷新、转发关系图可视化
- Web 多用户/RBAC、REST API、MCP 入口
- 时间分库（跨库搜索 fan-out）、PG/Meilisearch 迁移路径

---

## 13. 实施路线图

| 阶段 | 内容 | 验收 |
|---|---|---|
| **M0 骨架** | 新仓库 `tgarchive/`：go module、目录骨架（cmd/core/store/collector/media/web）、config、zap、cobra 根命令 | `tgarchive version/doctor` 可运行 |
| **M1 tgclient** | pkg/tgclient：client 工厂（recovery/retry/floodwait/ratelimit）、登录（code/QR）、加密 session 存储、peers 落库 | `tgarchive login` 完成，session 加密落盘并可复用 |
| **M2 store** | SQLite schema 全量 migration（可重复执行）、双层消息模型、幂等 UPSERT、FTS5 external content + gse 分词、单测（消息/编辑/软删/FTS） | store 层单测全绿；重复写入无副作用 |
| **M3 collector** | backfill（getHistory 分页 + cursor + --since/--limit/resume）、realtime（gaps.Run + state 持久化）、rescan 巡检、三者幂等合并；FloodWait/退避/断线重连 | mock MTProto server 集成测试：回填→实时→崩溃恢复→补洞 |
| **M4 media** | media worker 池（dcpool）、CAS 落盘（staging→rename）、retry 队列、ref_count 去重、磁盘水位 | 重复媒体物理去重；失败重试；graceful shutdown |
| **M5 CLI 完备** | monitor/sync/search/message/media/stats/doctor 全命令 | CLI 端到端用例通过 |
| **M6 Web UI** | embed 静态前端：Dashboard/Sources/Messages/Search/Media/Storage；API 分页查询 | 本地浏览 + 搜索可用 |
| **M7 测试与基准** | benchmark（10K/100K/1M 插入与搜索延迟）、恢复演练脚本、Docker、备份任务 | 基准报告 + 镜像可运行 |

每阶段结束：`go build && go test ./...` 全绿，无 TODO 冒充完成。

---

## 附：关键调研来源

- gotd 文档：updates-recovery / peers / middleware / query-iterators（gotd.dev）
- core.telegram.org/type/Update、/Updates
- Vendetta docs（database.md / mtproto.md）、GeiserX/Telegram-Archive、groupultra/telegram-search、knadh/tg-archive
- SQLite FTS5 实测数据（Cloudflare D1 生产、Travis Horn、sqlite-sparse 基准）
- gse 分词器、wangfenjing/simple（cgo 路径，未采用）
