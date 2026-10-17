# Telegram 归档子系统 — 状态文档

> 更新日期：2026-10-11
> 范围：`archive/` + `cmd/archive.go`（约 8800 行 Go 代码）+ 内嵌 Web UI（`archive/web/index.html`）
> 对照基线：[archive-system-proposal.md](archive-system-proposal.md)（提案 §8/§11/§13）

---

## 一、总体状态

**核心链路全部可用且经过实测**：扫码登录（浏览器/CLI）→ 聊天列表选择源 → 实时归档 → 周期巡检补漏 → 媒体自动下载 → FTS5 中文搜索 → Web 浏览/管理。

| 质量门 | 状态 |
|---|---|
| `go build ./...` | ✅ 通过 |
| `go test ./archive/... ./cmd/...` | ✅ 全绿 |
| `go test -race ./archive/...` | ✅ 全绿 |
| `go vet` / `gofmt -l` | ✅ 干净 |
| 单元测试 | store/collector/exporter/web auth/control/login 等 30+ 用例 |
| 端到端实测 | 真实账号登录、真实群归档 212 条消息 / 70 媒体，周期任务自动排空已验证 |

**当前部署**：`~/go/bin/tdl`（开发版）+ `./tdl`（82MB 静态单文件），serve 运行于 `127.0.0.1:8091`。

---

## 二、已完成功能

### M1 客户端层
- [x] gotd Runtime：recovery/retry/floodwait 中间件、peers 持久化缓存（存档 KV）
- [x] 登录三种方式：CLI 验证码（`login`）、CLI 扫码（`login --qr`）、**Web 浏览器内扫码**（二维码图片接口 + 30s 自动刷新 + 状态轮询）
- [x] 2FA 云密码支持（CLI 与 Web 两条路径）
- [x] 会话加密落盘（AES-GCM + scrypt；`--session-passphrase` / `TGA_SESSION_PASSPHRASE`；明文会话首次加密打开自动迁移；重登后自动 reseal；错误密码提前校验）

### M2 存储层（SQLite WAL + FTS5）
- [x] 双层存储：normalized 列 + `raw_json` 原始报文
- [x] 幂等 UPSERT `(chat_id, message_id)`——实时/回填/巡检三通道安全重放
- [x] FTS5 external content + gse 中文分词（词典内嵌二进制，容器/CGO_ENABLED=0 可用）
- [x] 编辑版本（`message_edits` 快照旧行，重投递去重）、软删除、事件表
- [x] 媒体表：入队即记录 type/mime/size（文档与照片），完成后回填 sha256/尺寸/路径
- [x] 单测：幂等重放、编辑快照回归、软删、链接、实体、分页、搜索语法等

### M3 采集器
- [x] 历史回填 `getHistory`（--limit / --since / --resume）
- [x] 实时监听（updates 分发 + 相册完整组提取）
- [x] 周期巡检（serve 托管模式 10 分钟一轮；`archive run` 同）
- [x] **reactions 采集**：实时 `updateMessageReactions` + 回填/巡检随消息重写聚合计数（👍/🔥/custom:<docid>），消息流以芯片展示
- [x] **views/forwards 定期刷新**：巡检对频道/超级群一次 `messages.getMessagesViews` 覆盖最近 200 条（提案 Phase 2 项）
- [x] **按 ChatID 解析回填目标**（私有群无 @username 也能同步——曾致 `empty target` 全阻断）
- [x] t.me 链接全形态解析：`@user`、`t.me/user`、`t.me/c/<id>`、`t.me/+<hash>`（已加入可解析，未加入给中文提示）
- [x] 关键词白名单/排除词过滤（`Accept`），Web 与 CLI 均有入口

### M3.5 关键词通知出口（提案 Phase 2 项）
- [x] `notify_rules` / `notify_log` 表 + 规则匹配语义与监控关键词一致（大小写不敏感子串、排除词优先、可选聊天范围）
- [x] 出口：Webhook（JSON POST）与 Telegram Bot API（sendMessage，超长截断 3500 字），可单配或同配
- [x] 限速 20 条/分钟/规则（滑动窗口）；投递异步且有并发上限，通知失败不影响归档
- [x] CLI：`tdl archive notify add|list|set|remove|test|log`（Bot Token 支持 `TGA_NOTIFY_BOT_TOKEN` env）
- [x] Web：`/api/notify/rules` CRUD + `/api/notify/test`；右栏「通知」面板（规则列表、添加表单、启停/删除/测试、最近投递）；Bot Token 只写不回显（返回掩码）
- [x] 仅实时新消息触发通知，历史回填不触发

### M4 媒体
- [x] 媒体 worker：CAS 内容寻址（`objects/ab/cd/<sha256>`）、staging 原子写、失败指数退避重试
- [x] **serve 托管模式周期下载**（2 分钟一轮，每轮最多 500 个，排空即停）——修复「文本有、图片不下」
- [x] 缩略图/预览图生成、头像 worker
- [x] 单文件大小上限：入队即知 size，**超限下载前直接 skip**
- [x] 磁盘水位：低于 1 GiB 暂停下载（消息归档不受影响）

### M5 CLI（`tdl archive …`）
- [x] login[--qr] / monitor add|list|set|remove / sync[--resume] / run / search / message / media pending|retry / stats / doctor / serve / destination / gc / notify add|list|set|remove|test|log …
- [x] search 过滤：`--chat --limit --after --before --sender --media any|image|video|file|audio`
- [x] monitor add 规则：`--keywords --exclude --max-size 200MB`
- [x] doctor 四项体检：`PRAGMA quick_check`、会话可读性（含加密/被锁提示）、磁盘水位、最近备份

### M6 Web 控制台（http://127.0.0.1:8091）
- [x] 浏览：消息 / 图片 / 视频 / 文件 / 链接 / 已删除 / 编辑历史 / 目的地 / Storage 面板（磁盘与库用量）；消息流内嵌 reactions 芯片
- [x] 搜索：`from:@user in:@channel has:image before:/after:` + **BM25 相关度排序**
- [x] 管理：聊天列表选择器（可搜索、显示已监控标记）、添加/移除/↻同步源、关键词/排除词/大小上限表单、任务进度面板、右栏「通知」面板（规则 CRUD + 测试 + 投递日志）
- [x] **按源过滤**：选中监控源后消息/图片/视频/文件/导出全部只显示该聊天（第五轮修复）
- [x] **可拖动时间线分页**：滑块拖动浏览整个历史 + 页码精确跳转（第五轮）
- [x] **论坛话题**：话题芯片栏（标题+消息数）、话题筛选、消息内话题标签可点击（第五轮）
- [x] 认证：`--auth-token` / `--auth-basic`、未认证禁止绑非本机地址（`:8080` 空 host 也拦截）、401 浏览器提示页
- [x] 安全加固：媒体响应头白名单 + `nosniff`（防存储型 XSS）、`esc()` 转义引号、外链 `safeUrl` 白名单、未知 `/api/*` 返回 404、请求体 1MiB 上限、Server 超时、500 不泄漏 SQL 细节、17 个只读端点 method 校验、notify webhook 仅限 http(s)

### M7 工程化
- [x] 基准：10K 插入 ~2.6s（~257µs/条）、FTS 搜索 ~18ms（提案要求 P99<20ms）
- [x] 每日自动备份：`VACUUM INTO` 快照保留 7 份（run 与托管 serve 均有）
- [x] SIGTERM 优雅退出（docker stop 可用）
- [x] Docker 静态构建可用（gse 词典内嵌修复；Dockerfile COPY 路径齐全）
- [x] 性能：分页 COUNT 单查询化（原 500 批循环全表扫）、链接域名空查询 GROUP BY 下推

---

## 三、本轮修复的 Bug 记录（均已实测）

| Bug | 根因 | 修复 |
|---|---|---|
| 图片不下载（用户实测） | ① 托管 serve 无周期任务，媒体队列无人消费；② `Backfill` 按 `Username` 解析，私有群无 username → `empty target` | taskLoop 周期任务 + 按 ChatID 解析回填目标 |
| 编辑历史丢失原始文本（数据丢失级） | `RecordEdit` 存的是编辑后新文本，且先于主表更新 | 改为快照当前旧行 + 重投递去重；回归测试锁定语义 |
| 两个 serve 进程写同一个库 | 8090 旧演示实例未关 | 已停 8090，仅保留 8091 |
| Job 状态数据竞争 | goroutine 裸写 job 字段 | 写入统一收进 `jobsMu` |
| `--media audio` 静默返回全库 | SearchAdvanced 无 audio 分支 | 补 `mime LIKE 'audio/%'` 分支 + file 分支排除 audio |
| `destination attach` 无效 | 导出器跳过 `archive_mode=local` | attach 自动提升为 `both` |
| `--addr :8080` 绕过认证守卫 | 空 host 未按 0.0.0.0 处理 | 已修 |
| 加密会话重登后残留明文 | 补封只在首次建 salt 时执行 | 登录成功后无条件 `Reseal` + 错误密码提前校验 |
| `/media/<sha>` 存储型 XSS | ServeFile 内容嗅探可回 text/html | mime 白名单 + octet-stream/attachment 兜底 + nosniff |
| `esc()` 引号注入 / safeUrl 死代码 | innerHTML 不转义引号；接线替换静默失败 | esc 补 `"'` 转义；safeUrl 真正接入外链 |
| 未知 `/api/*` 返回 HTML | catch-all 无条件回 index | `/api/` 前缀 404 |
| 私有群 `sync` 必失败 | 同「图片不下载」② | 同上 |
| Docker 构建阻断 | Fyne GUI 需 cgo | `gui` 命令构建标签隔离（`cgo` / `!cgo` 桩） |

---

## 四、已修复与剩余问题

### 第五轮：浏览体验修复 + 话题支持（2026-10-11 下午）

用户实测反馈两个问题，排查后发现是一个真 bug + 一个体验缺口：

| 问题（用户反馈） | 根因 | 修复与验证 |
|---|---|---|
| 「监控一个群组时无法拖动进度条，看不到后面发生了什么」 | ① 前端 `loadMessages`/`loadMedia` 从未把选中的源传给 API，后端 `/api/messages/page` 也根本不解析 `chat` 参数——选了群组，消息流仍是全库混排，翻页翻的是整个库；② 分页只有上一页/下一页按钮，几百页的历史只能一页页点 | 前后端接线补齐（chat/topic 参数贯通 messages/page、media、export）；分页改为**可拖动时间线滑块**（拖动即时预览、松手加载、数值框可精确跳页），实测拖到第 2 页正确出现更早的消息 |
| 「群组里有话题，无法查看话题的内容」 | 话题（forum topics）从未建模：`MessageActionTopicCreate` 事件被归为"系统事件"且标题不入库，消息虽有 thread_id 但无任何浏览入口 | 新增 `chats.is_forum` 列（从 Channel.Forum 学习）；`topic_created`/`topic_edited` 事件正确解析标题；`store.Topics` 聚合话题（标题取事件、缺根时回退最早消息文本）；`/api/topics` + 消息/媒体/导出全链路 `topic` 过滤；Web 顶部话题芯片栏 + 消息内 `#话题 N` 标签可点击筛选 |

**端到端实测**（临时库 + 真实 serve + 浏览器）：选源后 feed 只显示该群（修复前两个群混排）；话题栏显示「故障反馈 40 / 资源分享 40 / 日常灌水 0」，点击芯片只剩该话题消息（41 条 = 40 回复 + 根消息）；滑块拖到最右翻到第 2 页出现更早消息。

补充说明：用户原话的"进度条"若指视频播放进度条——`/media/<sha>` 用 `http.ServeFile` 实现，HTTP Range 本来就支持，视频可拖动；无法拖动浏览的实为消息历史本身，已如上修复。

### 第四轮：Phase 2 首批功能（2026-10-11）

本轮从提案 §12/§13 的 Phase 2 清单中落地三项，全部带测试：

| 功能 | 实现 | 验证 |
|---|---|---|
| reactions 采集 + views/forwards 定期刷新 | `message_reactions` 表；`FromTG` 提取、`RefreshMetadata` 随消息重写、实时 `updateMessageReactions` 直写；巡检新增 `RefreshChatStats`（getMessagesViews，每聊天 1 请求/轮） | store 单测（提取/重放幂等/视图附加/ApplyMessageViews）+ race 全绿 |
| 关键词通知出口 | `notify_rules`/`notify_log` + `Notifier`（webhook + Bot API、滑动窗口限速、异步并发上限、投递日志自动裁剪）；CLI `notify` 五个子命令；Web CRUD + 测试端点 + 「通知」面板 | 单测覆盖匹配语义/webhook 投递/失败记录/限速；Web CRUD 含 token 掩码与凭据保全；httptest 端到端 |
| run/serve 媒体节奏统一 | 抽出共享常量 `archive.MediaTick`（2 分钟）与 `DrainMediaQueue`（含 `media_paused_until` 全局闸门）；`archive run` 新增独立媒体 ticker——此前 run 只在 10 分钟巡检里捎带下载，且完全不读暂停闸门 | `TestDrainMediaQueue`（空队列即停、暂停闸门短路）|

顺带修复：

| 问题 | 根因 | 修复 |
|---|---|---|
| `TestStorageEndpoint` 失败（db_bytes=0） | `/api/storage` 写死 `consts.DataDir` 全局路径，测试库开在临时目录 → 之前纯靠开发机恰好存在真实库文件才通过 | 路径全部从 `w.Store.Path()` 推导（与 backups 同一约定）；实测正确报告任意库路径 |
| `UpdateNotifyRule`（新写）SET 子句误用 `joinAnd` | WHERE 专用的 AND 拼接被用于 SET 列表，SQLite 把 `a=? AND b=?` 解析成单个布尔表达式赋值 | 改 `strings.Join(sets, ", ")` + 测试锁定 |

### 第三轮：实测验证与限流修复（2026-10-05 下午）

重启到新二进制后实测，发现并修复了一个**导致媒体队列永久卡死**的真 bug，以及限流下的行为缺陷：

| 现象（实测） | 根因 | 修复与验证 |
|---|---|---|
| 22 条媒体任务永远 `pending`、`attempts=0` | `fail()` 用**已经超时取消的 context** 写数据库，UPDATE 被拒绝 → 失败状态从未落库，任务每次巡检被重新取出 | 终态写入改用 `context.WithoutCancel` + 10s 超时；实测 22 条立刻变成 `failed/attempts=1` 并带 `next_retry_at` |
| 单任务 5 分钟超时形同虚设 | 同上的连带后果（写不进去就像没发生） | 已随上条修复；`attempts` 现在正常递增并退避 |
| 每轮 5×100 任务的突发请求 | 每个媒体文件单独调一次 `messages.getHistory` | 改为**每个聊天一次历史扫描**（`locateFiles`，500 条上限 + 未命中回退单条查询）；实测日志出现 `located=true`，一次扫描覆盖多个任务 |
| 触发 FLOOD_WAIT 后反复无效重试 | 无全局闸门，每次轮询都撞限流 | 新增 `archive_settings.media_paused_until` 全局暂停闸门（限流/超时后暂停 10 分钟）；实测已写入 `media_paused_until` |
| 超时被误判为「文件不存在」 | 超时与「找不到」走同一分支 | 限流与超时永不判死刑（只退避，`attempts` 只用于拉长退避），只有明确的永久失败才 `skipped` |

**结论**：媒体队列现在会正确记录状态、指数退避、限流时全局停手，不会再出现「静默卡死」。当前账号仍处于 Telegram 的 FLOOD_WAIT 限制中，媒体下载会在限流解除后自动恢复（无需人工干预）。

### 第二轮修复（全部有测试或实测覆盖）

| 原问题 | 处理 |
|---|---|
| 无 GC / retention | 新增 `tdl archive gc`（默认 dry run，`--apply` 才删，`--repair` 还可把「对象丢失但记录为 completed」的媒体重新入队）；周期巡检顺带清理 `staging/` 过期残片 |
| `sync_cursors` 未接线 | 游标真正持久化：`Backfill` 读写 `backlog_state` / `backlog_oldest_msg_id` / `last_msg_id`，`--resume` 优先用游标；`last_msg_id` 单调不回退，旧写入者无法回退水位 |
| 真·缺口巡检缺失 | 新增 `RescanTargets`：先比服务端最新 msg_id 与本地最大（一次 getHistory(limit=1)），落后就补尾部缺口，否则走常规巡检；`chats.last_msg_id` 由 `SetChatLastMsgID` upsert 落库（以前是静默无效的 UPDATE） |
| 分页 N+1 | `MessageViewsByRowIDs` 一次 IN 查询取代每页 50 次 `GetMessageByRowID`；`editCounts` 一次 GROUP BY 取代每条一次 COUNT；三条重复的巨型 SELECT 合并为 `messageViewColumns` 单一来源 |
| `ListLinks` / `ListLinkDomains` 全量候选 + N+1 | 都改为单条 SQL：域名聚合 `GROUP BY` 下推、链接列表分页 + `COUNT(*)`，不再把全库候选拉进 Go |
| media 元数据缺口 | 类型细分为 `photo`/`video`/`animation`/`audio`/`voice`/`sticker`/`document`（按 DocumentAttribute 判定，mime 兜底）；`file_name`/`file_id_unique`/`size`/宽高/时长入队即落库 |
| 无文件媒体被误入队 | `MediaInfo` 对 geo/poll/contact/webpage/dice 等返回 `ok=false`，不再产生永远无法成功的下载任务 |
| 媒体队列被卡死 | 找不到文件、失去访问权等一律走 `fail()`：指数退避重试，5 次后标记 `skipped` 并退出队列——修复「22 条 pending 且 attempts=0 永久滞留」 |
| 关键词规则无法改 | `tdl archive monitor set <chat-id> --keywords/--exclude/--max-size/--download-media/--enable/--disable`；Web `PATCH /api/targets` + 每个源的 ⚙ 编辑表单 |
| Web 无导出 | `GET /api/export?kind=messages|media|links|deleted|edited&format=csv|json`（流式、上限 5 万行）+ 顶栏 ⤓ CSV / ⤓ JSON 按钮，跟随当前视图与筛选 |
| 孤儿接口 | `/api/search` 支持 `limit`/`offset`；三个「孤儿」端点统一为脚本 API 并写入 README |
| CLI 自动化 | 补 Short/Example；`stats`/`monitor list`/`media pending`/`destination list`/`gc` 支持 `--json`；补全由 cobra `completion` 提供 |
| 移动端 | 窄屏右下角 📊 抽屉可展开统计/任务/目的地面板 |

### 仍然未做（诚实分级）

1. **尾部以外的历史缺口**：缺口巡检只比对「服务端最新 vs 本地最大」，能覆盖离线期间的尾部缺失；更早的中间空洞要靠 `tdl archive sync <目标> --resume` 深度回填（游标已持久化，可跨次续传）。
2. **Phase 2 剩余项**（提案 §12；reactions/views 刷新与通知出口已于第四轮完成）：
   - HTML/JSONL 静态导出（CSV/JSON 已有）
   - 多账户
   - trigram 辅索引（任意子串搜索）
   - 转发关系图可视化
   - Web RBAC / 完整 REST API、MCP 入口
   - 时间分库、PG/Meilisearch 迁移路径
   - 监控规则引擎的进一步细化（通知规则已覆盖「关键词→推送」，尚无定时/聚合类规则）
3. **仓库卫生**：archive 子系统仍未 commit（新增 CI workflow 已就位但未入库生效）。
4. **示例凭据（已产生实际代价）**：`config.yaml` 仍用 Telegram Desktop 公开示例 app-id/hash（2040/…）。这组凭据被大量公开项目共用，请求共享同一限流桶——此前媒体队列卡死的直接触发因素就是 FLOOD_WAIT。建议改用 my.telegram.org 申领的私有凭据；用户此前明确暂不更换，此处仅记录实测影响。
5. **通知出口的凭据管理**：规则创建后 Bot Token 不可修改（只写不回显），换 Token 需重建规则；Webhook 无重试（失败仅记录）。可作为后续迭代。

---

## 五、怎么运行

```bash
cd ~/code/telegram-download-bot
~/go/bin/tdl archive serve --addr 127.0.0.1:8091   # 托管模式：登录+归档+查看全在浏览器
# 浏览器打开 http://127.0.0.1:8091 → 点红色芯片扫码 → 「＋添加源」→ 从聊天列表选择
```

```bash
# 常驻完整形态（与 serve 二选一）
~/go/bin/tdl archive run
# 常用命令
~/go/bin/tdl archive search "关键词" --after 2026-10-01 --media image
~/go/bin/tdl archive doctor        # 体检：库完整性/会话/磁盘/备份
~/go/bin/tdl archive sync <目标> --resume   # 断点续传式补洞
~/go/bin/tdl archive notify add --name 告警 --keywords 宕机 --webhook https://example.com/hook
~/go/bin/tdl archive notify test 1 # 发送测试通知验证出口
```

数据位置：`~/.tdl/archive/`（`tgarchive.db` + `objects/` 内容寻址媒体 + `backups/` 每日快照）。
迁移 = 拷贝二进制 + `~/.tdl` 目录。
