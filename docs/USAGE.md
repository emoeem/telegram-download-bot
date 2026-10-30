# 使用说明书 · 编号收件箱（archive）

这份文档假设你只有这一个问题：**"我的账号在哪里收东西，收到的第 1337 号放在哪，怎么拿、怎么发、怎么删。"**

---

## 0. 现在是什么状态

| 项 | 值 |
|---|---|
| 源码 | `~/code/telegram-download-bot`，工作区有未提交改动（本轮改动未提交） |
| 编译 | `go build -trimpath -ldflags "-s -w"` → 3.5s，**通过** |
| 测试 | `go vet` 干净；`go test ./archive/... ./cmd/... ./pkg/...` **全绿** |
| 二进制 | `./tdl` 与 `~/.local/bin/tdl`（同一 sha256 `a3f7864…`，101 MB，已 strip） |
| 旧二进制 | `./tdl.bak-20261027-1110`（85 MB，随时回滚） |
| 运行中的进程 | `./tdl archive serve --addr 127.0.0.1:8091`（pid 329953，**启动于替换之前**） |

> 关键：磁盘上的 `tdl` 已经是新版，但**正在跑的进程还是旧代码**。重启之前，`/inbox` 页面能打开、里面的数据接口返回 404。见第 8 节。

---

## 1. 三层结构（先理解这个，后面全是推论）

| 层 | 位置 | 性质 |
|---|---|---|
| **编号** | SQLite `media.serial` | 到达即分配，单调递增，**永不复用** |
| **字节** | `~/.tdl/archive/objects/<sha256>` | 内容寻址，同样内容只存一份 |
| **视图** | `~/.tdl/archive/objects/by-serial/001/001337-photo.jpg` | 硬链接，按编号直接打开，**不额外占磁盘** |

两条推论，决定了你遇到"编号有、文件没有"时该不该慌：

1. 编号在**消息落地那一刻**分配，不等下载完成 → 排队中、失败中的条目也在台账里，状态列会写 `pending` / `failed`。
2. 文件视图在**字节落盘之后**才出现 → 编号存在但没有 `by-serial` 文件 = 还没下载完，不是丢了。

---

## 2. 安装与升级

```bash
cd ~/code/telegram-download-bot

# 编译（当前方式，快，够用）
go build -trimpath -ldflags "-s -w" -o tdl .

# 装到 PATH（systemd unit 默认读这里）
install -Dm755 tdl ~/.local/bin/tdl

# 升级时的正确姿势：先备份，再替换
mv tdl tdl.bak-$(date +%Y%m%d-%H%M) && mv tdl.new tdl
```

三个坑，写在前面：

- **不要用 `cp` 覆盖正在运行的二进制**，会 `Text file busy`。用 `mv`（换 inode）。
- `make build` 走的是 goreleaser，产物在 `.tdl/dist/`，跟 `go build` 不是一条路；只为本机用，`go build` 更省事。
- `-ldflags "-s -w"` 只影响体积（131 MB → 101 MB），不影响功能。

---

## 3. 首次配置

```bash
# ① 登录"收件账号"（这是你自己的用户账号，不是 bot token）
tdl archive login            # 手机收验证码
tdl archive login --qr       # 或者扫码

# ② 会话加密（强烈建议：session 文件等同账号完全控制权）
tdl archive login --session-passphrase '你的口令'
# 也可以走环境变量，省得每次输：
echo 'TGA_SESSION_PASSPHRASE=你的口令' >> ~/.config/tdl/archive.env
chmod 600 ~/.config/tdl/archive.env
```

数据全在 `~/.tdl/archive/`：

```
tgarchive.db          # 主库（WAL），编号就在这
objects/<sha256>      # 字节
objects/by-serial/    # 编号视图（可删、可重建）
staging/              # 下载中的临时文件
avatars/  backups/
```

---

## 4. 五个核心动作

### 4.1 让它开始收：`watch`

```bash
tdl archive watch @some_bot
```

一条命令做三件事：登记这个会话 → 打开媒体下载 → 常驻收件。**这就是"账号在线、程序替你收"的形态**，一直到你 Ctrl-C。

```bash
tdl archive watch @some_bot --start r16      # 顺手打印绑定深链
tdl archive watch @some_bot --download-media=false   # 只编号不下载，之后再补
```

### 4.2 看台账：`inbox`

```bash
tdl archive inbox                    # 最新 50 条入站
tdl archive inbox --all              # 含本账号发出的
tdl archive inbox --kind video       # 只看视频
tdl archive inbox --status pending   # 只看没下完的
tdl archive inbox --since 2026-10-20 --limit 200
tdl archive inbox --json             # 给脚本吃
```

典型输出里每行就是一条：编号 / 时间 / 方向（← 入 / → 出）/ 类型 / 大小 / 状态 / 来源。

### 4.3 取文件：`file`

```bash
tdl archive file 1337          # 打印本地绝对路径
tdl archive file 1337 --json   # 连文件名、MIME、来源、状态一起给
```

也可以完全不进命令行走文件系统直取：

```bash
open ~/.tdl/archive/objects/by-serial/001/001337-photo.jpg
```

### 4.4 变成可发布内容：`post`

```bash
tdl archive post 1337                      # 渲染成发布文本（只渲染，不发送）
tdl archive post 1337 --template post.tmpl # 自己的模板
tdl archive post 1337 --json               # 只要结构化数据
```

### 4.5 删：`purge`（默认 dry run）

```bash
tdl archive purge --serial 1337                 # 预览会删什么
tdl archive purge --serial 1337 --yes           # 真删
tdl archive purge --kind video --until 2026-01-01 --keep-messages --yes
```

- `--keep-messages` = 删媒体和字节、留消息文字（"腾空间、留记录"）。
- 删除顺序是**先删行、再删文件**：中途中断只会留下孤儿对象（`tdl archive gc` 回收），不会留下指向空文件的记录。

---

## 5. 深链：把编号编进 `?start=`

```bash
tdl archive deeplink @some_bot --serial 1337
# https://t.me/some_bot?start=r16        （r + base36）

tdl archive deeplink @some_bot --serial 1337 --count 5   # 连发 5 条编号
tdl archive deeplink @some_bot --serial 1337 --open      # 直接开浏览器
tdl archive deeplink @some_bot --payload mycustom        # 自己指定 payload
```

为什么要这个：`start` payload 会出现在 bot 收到的 `/start r16` 里，于是"它回过来的媒体"和"你发出去的请求"能对上号。配合已有的 `autoclick`（识别消息里的 `t.me/...` 链接并打开），链路是：

> 点链接 → bot 回媒体 → 落地即编号 → `inbox` 里查编号

---

## 6. 命令全表

| 命令 | 干什么 |
|---|---|
| `archive login [--qr]` | 登录收件账号 |
| `archive watch <@bot\|chat-id>` | 登记 + 常驻收件（最常用） |
| `archive run` | 只跑常驻循环（不登记新目标），systemd 用这个 |
| `archive monitor add/list/remove/set` | 管理被监控会话、过滤器、大小上限 |
| `archive inbox` | 编号台账 |
| `archive file <serial>` | 编号 → 本地路径 |
| `archive post <serial>` | 编号 → 素材 + 渲染文本 |
| `archive purge` | 按编号/会话/日期删除（默认 dry run） |
| `archive relink` | 给历史数据补 `by-serial` 视图 |
| `archive deeplink <@bot>` | 生成带编号的 `?start=` 链接 |
| `archive search <query>` | 本地全文检索（`--chat --media --after --before --sender`） |
| `archive sync <target>` | 重扫一个会话补空洞（`--resume` 从最旧继续） |
| `archive material` | 原始消息折叠成素材（相册合并、重复标记） |
| `archive publish` | 素材 → 消息并发送（不加 `--apply` 只打印） |
| `archive export` | 导 CSV/JSON/JSONL（`--kind --fields --map`），或 `--format tgjson` 导 Telegram Desktop 布局 |
| `archive notify add/list/set/remove/test/log` | 关键词 → Webhook / Bot 通知 |
| `archive media pending/retry` | 看队列、重排失败任务 |
| `archive destination create-channel/add/attach/...` | 镜像到 Telegram 频道 |
| `archive serve` | 本地 Web UI |
| `archive stats` / `doctor` | 计数器 / 自检 |
| `archive gc` | 回收孤儿对象、清理 staging（`--apply` 才动手） |

### 导出为 Telegram Desktop 格式

`archive export --format tgjson` 产出的是 Telegram Desktop 自己的导出布局，不是本项目的
扁平表。字段名、会话类型枚举、以及「纯文本的 `text` 是字符串、带格式才是数组」这条规则
都照客户端导出器的写法，因此产物能被已经支持「Telegram 导出」的工具直接打开
（`telegram-chat-parser`、`tg-archive`、`telegram-export-viewer` 等）。

```bash
# 单文件：result.json + 媒体目录（全部会话）
tdl archive export --format tgjson --with-media -o ./export/

# 每个会话一个目录：result.json 顶层就是该会话对象（单聊布局）
tdl archive export --format tgjson --split --with-media -o ./export/

# 只导一个会话，每个会话最多 500 条
tdl archive export --format tgjson --chat 1002003005 --limit 500 -o ./export/
```

产物：

```
export/
  result.json
  photos/              photo_2@24-10-2025_08-12-00.jpg
  video_files/         video_3@24-10-2025_08-13-00.mp4
  voice_messages/      audio_4@24-10-2025_08-15-00.ogg
  round_video_messages/  music/  stickers/  animations/  files/
```

| 情况 | 行为 |
|---|---|
| 不加 `--with-media` | 媒体键照样写出，值是 `(File not included. Change data exporting settings to download.)`。**「有媒体但没导出」和「这条没有媒体」是两种事实**，混在一起会让缺文件的导出看起来是完整的 |
| 对象文件在库里但磁盘上没了 | 值是 `(File unavailable, please try again later)`，并把该条计入「跳过」计数 |
| 时间 | 一律 UTC。客户端写本地时间；归档可能在任何时区的服务器上导出，稳定偏移比隐式本地时间可靠 |
| 媒体落地 | 优先硬链接（同一文件系统下等于零拷贝，2 GB 导出是瞬时的），跨文件系统才退回复制 |
| `--fields` / `--map` | 不适用于 tgjson；字段名由格式固定，传了直接报错，不会静默忽略 |
| `-o` | tgjson 下是**目录**，不存在会自动创建 |

JSON 先写 `.tmp`、`fsync` 之后再改名。中途失败不会留下半个 `result.json`——半份文件比
没有文件更糟，每个读取方都会把它当成完整导出，然后把丢掉的尾部报告成「消息丢失」。

同一条消息的 `from_id` 一定伴随 `from`：归档里可能没有该发送者的 users 行（频道以自己
身份发帖就是这种情况），此时 `from` 按客户端显示逻辑回退到会话名，再回退到 peer id。
悬空的 `from_id` 会让读取方的用户表构建直接抛错，整份文件都读不了。

---

## 7. Web UI

```bash
tdl archive serve --addr 127.0.0.1:8091
# 打开 http://127.0.0.1:8091/inbox
```

| 页面/接口 | 用途 |
|---|---|
| `/inbox` | 编号台账：编号 / 时间 / 方向 / 类型 / 大小 / 状态 / 文件 / 来源 |
| 顶部 **⤓ CSV / ⤓ JSON** | 导出当前台账 |
| 行内 **路径** | 复制本地绝对路径 |
| 行内 **删除** | 二次确认后执行（走 `POST /api/purge`） |
| `GET /api/serials` | 台账 + 汇总（`?limit&kind&status&since&max_serial`） |
| `GET /api/serial?serial=N` | 单号解析 + 渲染文本 |
| `POST /api/purge` | 删除；**不带 `confirm` 就是 dry run** |
| `GET /api/export?kind=serials&format=csv` | 台账导出 |

鉴权（要暴露到 localhost 之外时必配）：

```bash
tdl archive serve --addr 0.0.0.0:8091 --auth-token 'long-random-token'
tdl archive serve --addr 0.0.0.0:8091 --auth-basic 'user:pass'
# 绑定非回环地址且不带鉴权时，必须显式加 --insecure
```

---

## 8. 常驻（重启后才会用上新代码）

现在跑的是旧进程，先做这一步：

```bash
systemctl --user stop tdl-archive 2>/dev/null || kill 329953
sleep 2

cd ~/code/telegram-download-bot
./tdl archive relink      # 给旧进程期间下载完的文件补编号视图
```

然后装成 systemd user unit：

```bash
mkdir -p ~/.config/systemd/user
cp deploy/tdl-archive.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now tdl-archive

loginctl enable-linger "$USER"          # 登出后继续收
journalctl --user -u tdl-archive -f     # 看日志
```

unit 的三个要点：`Restart=always`（掉线自愈）、`TimeoutStopSec=30`（让在写的文件写完）、`Environment=TDL_LOG_FORMAT=json`（结构化日志）。会话有口令时写进 `~/.config/tdl/archive.env`：

```
TGA_SESSION_PASSPHRASE=你的口令
```

Web UI 另起一个（unit 跑的是 `run`，不打 UI）：

```bash
./tdl archive serve --addr 127.0.0.1:8091 --auth-token "$(openssl rand -hex 16)"
```

---

## 9. 排障

| 症状 | 先查 | 处理 |
|---|---|---|
| 编号有、文件没有 | `inbox --status pending` | 正常（没下完）；卡住看 `media pending` |
| 下载失败 | `media pending` 里有 job-id | `tdl archive media retry <job-id>` |
| 大批媒体卡在 `skipped`，错误含 `no rows in result set` | 该 chat 当时还不是监控目标 | `tdl archive media wake`（只修已监控的 chat）；给 chat 建目标后会自动唤醒它的队列 |
| 编号视图缺失 | `find ~/.tdl/archive/objects/by-serial -type f \| wc -l` 对比 `media` 里 completed 数 | `tdl archive relink`（视图是派生数据，可反复重建） |
| 磁盘变大但没新内容 | `tdl archive gc` | 看到孤儿对象后 `--apply`；`--repair --apply` 会顺带重排"记录在、文件没了"的条目 |
| 编号有空洞 | `archive stats` | 正常：`skipped`（超大小上限等）也占编号，空洞不是丢数据 |
| Web 页面空白 / 接口 404 | `curl -s -o /dev/null -w '%{http_code}' 127.0.0.1:8091/api/serials` | 404 = 跑的是旧二进制，重启 |
| 会话掉线 | `journalctl --user -u tdl-archive -f` | 重新 `tdl archive login` |
| 全面自检 | `tdl archive doctor` / `archive stats --json` | — |

**升级迁移**：旧库首次被新版打开时自动加列、按消息时间回填编号、建唯一索引，**只跑一次**；之后 `relink` 补文件视图。回填不动原有行，可安全重复。

---

## 10. 边界与注意

| 项 | 说明 |
|---|---|
| 数据库 | `~/.tdl/archive/tgarchive.db` 里有你的私聊内容，**不要提交到任何仓库**、不要放进同步盘 |
| session | `~/.tdl/data` 里的 session 等同账号完全控制权，比密码更敏感；建议开 `--session-passphrase` |
| `purge` | 不可恢复。默认 dry run 是故意的，先看再 `--yes` |
| `serve` | 默认只绑 `127.0.0.1`；对外必须加 `--auth-token` / `--auth-basic` |
| `relink` / `material --rebuild` | 都是**派生数据**，删掉可重建，不会伤到原始消息 |
| 删除消息 | 不级联清 `materials`（`anchor_rowid` 不是外键），下次 `material` 刷新会重建 |

---

## 附：最短上手路径

```bash
tdl archive login                       # 1. 登录收件账号
tdl archive watch @some_bot             # 2. 开始收（常驻）
#  —— 让 bot 给你发点东西 ——
tdl archive inbox                       # 3. 看编号
tdl archive file 1                      # 4. 拿路径
tdl archive post 1                      # 5. 变成可用文本
```

## 点按钮让机器人把内容发给你

页面上的内联按钮（🔘 开头）现在**可以点**。点一下不是打开网页，而是服务端用你的账号
向机器人发出同一个交互，机器人回过来的消息（文字、图片、视频都算）会进入归档并分配编号。

1. 先保证那个机器人所在的会话在监控源里（页面左下「监控源」→ 添加，或
   `tdl archive watch @bot`）——否则消息到不了库。
2. 消息列表里找到带 🔘 按钮的那条，点按钮。
3. 页面提示「已替你点击」，几秒后刷新，机器人回的内容就是一条新消息，
   `tdl archive inbox` 里能看到它的编号。

底层做三件事：`channels.getMessages` 取回按钮的 callback 数据 → 以你的账号发出
`getBotCallbackAnswer` → 把这个机器人登记为监控源（否则它私聊回复会被收集器丢掉）。

只有 callback 按钮走服务端；链接按钮（🔗）直接打开，不需要代按。
