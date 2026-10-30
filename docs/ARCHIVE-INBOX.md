# 编号收件箱（inbox）

给一个长期在线的账号加一条**收货台账**：bot 私聊里发来的每个媒体，落地即编号，按编号可查、可取、可发、可删。

## 三层结构

| 层 | 位置 | 作用 |
|---|---|---|
| 编号 | `media.serial`（SQLite） | 入站即分配，单调递增，永不复用 |
| 字节 | `~/.tdl/archive/objects/<sha>` | 内容寻址，天然去重 |
| 视图 | `~/.tdl/archive/objects/by-serial/001/001337-photo.jpg` | 硬链接，按编号直接打开，零额外磁盘 |

编号在**消息落地那一刻**分配，不等下载完成——所以排队中、失败中的条目也在台账里。文件视图在字节落盘后出现。

## 常用命令

```bash
tdl archive watch @some_bot          # 登记 + 常驻收件（bot 私聊）
tdl archive inbox                    # 台账（默认只入站）
tdl archive inbox --all --json       # 含出站、机器可读
tdl archive file 1337                # 编号 → 本地路径
tdl archive post 1337                # 编号 → 可发布文本
tdl archive relink                   # 给历史数据补编号视图
```

## 深链（?start=）

把编号编进 bot 深链，回答就能对回请求：

```bash
tdl archive deeplink @some_bot --serial 1337
# https://t.me/some_bot?start=r16      (r + base36)
tdl archive deeplink @some_bot --serial 1337 --count 5   # 连发 5 条
```

`watch @bot --start r16` 会在登记后直接打印绑定链接。已有 `autoclick` 能识别消息里的 `t.me/...` 链接并打开，两者叠加就是"点链接 → bot 回媒体 → 自动编号"。

## 删除

默认 **dry run**，确认后加 `--yes`：

```bash
tdl archive purge --serial 1337                 # 预览
tdl archive purge --serial 1337 --yes           # 删媒体+消息+文件+视图
tdl archive purge --kind video --until 2026-01-01 --keep-messages --yes
```

`--keep-messages`：删媒体和字节，保留消息文字（"腾空间、留记录"）。
删除顺序是**先删行、再删文件**：中断只会留下孤儿对象（`tdl archive gc` 可回收），不会留下指向空文件的行。

## Web UI

`tdl archive serve` 后打开 `/inbox`：

- 台账表格（编号 / 时间 / 方向 / 类型 / 大小 / 状态 / 文件 / 来源）
- 顶部 **⤓ CSV / ⤓ JSON** 导出当前台账
- 每行 **路径**（复制本地绝对路径）/ **删除**（二次确认后执行）

对应接口：

| 接口 | 方法 | 用途 |
|---|---|---|
| `/api/serials` | GET | 台账 + 汇总 |
| `/api/serial?serial=N` | GET | 单号解析 + 渲染后的发布文本 |
| `/api/purge` | POST | 删除；不带 `confirm` 即 dry run |
| `/api/export?kind=serials&format=csv` | GET | 台账导出 |

## 常驻

见 `deploy/tdl-archive.service`（systemd user unit）：

```bash
cp deploy/tdl-archive.service ~/.config/systemd/user/
systemctl --user daemon-reload && systemctl --user enable --now tdl-archive
loginctl enable-linger "$USER"     # 登出后继续收
```

## 迁移说明

旧库首次被新版打开时会自动：加 `media.serial`、加 `messages.out`、按**消息时间**回填编号、建唯一索引。回填只跑一次；`tdl archive relink` 再把已下载的文件补成编号视图。
