# Telegram 下载器 · 服务器部署说明

这个包把一个 Telegram 归档面板装到你的 Linux 服务器上。装完之后你用浏览器打开一个网址、输入账号密码，就能看到归档的聊天记录、图片、视频，并且可以继续登录 Telegram 抓取新内容。

---

## 一、先看清两件事

**它需要什么**

| 项 | 要求 |
|---|---|
| 系统 | Linux x86_64（Ubuntu / Debian / CentOS 都行） |
| 权限 | **普通用户即可，不需要 root** |
| 依赖 | **没有**。不需要 Node、不需要 Docker、不需要 Nginx、不需要 gcc |
| 磁盘 | 建议留 5 GB 以上（媒体文件很占地方） |
| 内存 | 512 MB 够用 |

**它不需要什么**

整套程序是**一个静态编译的二进制文件**（`tdl`），SQLite 数据库也编在里面（纯 Go 实现）。所以：

```
不需要 apt install 任何东西
不需要 npm install
不需要 docker pull
```

如果有人说"你这个是 Node 项目吧，得先装 Node" ——那不是这个包。

---

## 二、一分钟部署

把压缩包传到服务器（用 `scp` 或任何方式），然后：

```bash
tar -xzf Telegram下载器-Linux-x64-*.tar.gz
cd Telegram下载器-Linux-x64
./install.sh
```

就这三条。脚本会自己做完这些事：

1. 检查架构和端口
2. 安装二进制到 `~/tdl-archive/`
3. **生成一个 16 位随机密码**
4. 创建配置文件
5. 启动服务
6. 自检两个关键点：登录页能不能打开、**未登录访问首页会不会被拦**

跑完屏幕上会打印访问地址、账号、密码，长这样：

```
────────────────────────────────────────────
 Telegram 下载器 已部署
────────────────────────────────────────────
 本机地址 : http://127.0.0.1:8080
 公网地址 : http://203.0.113.10:8080   (需安全组放行 8080)
 账号     : admin
 密码     : kR7mQw2xNp4tLb9v
────────────────────────────────────────────
```

**把这个密码存到密码管理器里。** 它同时保存在服务器 `~/tdl-archive/.credentials`（权限 600，只有你自己能读）。

首次启动后，服务会把那个文件里的明文密码换成 **PBKDF2 哈希**（60 万轮），之后：

- 文件里再也读不到明文，`ps` 里也看不到（启动命令不再带密码）
- 密码仍然有效，**不用重新记**
- 想改密码，登进面板改（见下节），或 `RESET_PASSWORD=1 ./install.sh` 重新生成

### 在面板里改账号和密码

登录后，右侧面板的 **「账号」** 标签页：

| 字段 | 说明 |
|---|---|
| 当前密码 | 必填，改任何东西都要先证明是你 |
| 新账号 | 留空表示不改 |
| 新密码 / 确认新密码 | 留空表示不改；至少 8 位，不能含冒号 |

保存后：

- **其它设备上的登录立即失效**（Cookie 签名密钥由密码派生，改密码就换钥匙）
- **当前这一页自动续期**，不会被踢出去
- 密码以哈希存盘，服务端任何地方都不保留明文

连错 5 次当前密码会被限流（1 分钟起，逐次翻倍），和登录页同一套逻辑。

### 换端口 / 换账号名

```bash
PORT=9000 ./install.sh          # 换端口
USER_NAME=me ./install.sh       # 换账号名（会重新生成密码）
RESET_PASSWORD=1 ./install.sh   # 忘记密码时重设（会重新生成）
```

> 账号名和密码改了之后，再跑 `install.sh` 不会把它们改回去 —— 凭据文件是唯一真源，启动参数只在文件不存在时用来初始化。

---

## 三、第一次使用

### 1. 打开登录页

浏览器访问 `http://<服务器IP>:8080`，会看到：

- 账号、密码两个输入框
- 「账号或密码不正确」这样的错误提示会显示在表单上方
- 右下角有「切换主题」（深色 / 浅色）

登录成功后进入归档面板。

### 2. 让面板登录 Telegram

面板本身需要有一个 Telegram 账号身份才能抓取内容。这一步在浏览器里完成，**不需要在服务器上敲命令**：

1. 面板右上角点「登录」
2. 屏幕出现二维码
3. 手机 Telegram → 设置 → 设备 → 连接设备 → 扫这个码
4. 确认后，面板就拿到了账号身份

> 前提：配置文件里必须填了 `app-id` / `app-hash`。没填的话面板是「只读模式」，只能看已有内容，不能登录、不能下载。

### 3. 填 app-id / app-hash

编辑 `~/tdl-archive/config.yaml`：

```yaml
telegram:
  app-id: 1234567
  app-hash: "0123456789abcdef0123456789abcdef"
```

**怎么拿这两个值**（免费，5 分钟）：

1. 浏览器打开 https://my.telegram.org
2. 输入 Telegram 手机号（带国家码），收验证码登录
3. 点 **API development tools**
4. 应用名随便填，平台选 **Other**
5. 页面上会显示 `api_id` 和 `api_hash`

填完重启：

```bash
~/tdl-archive/restart.sh
```

### 4. 开始用

登录后，面板里可以：

- 粘贴 Telegram 消息链接 → 下载那条消息的图片/视频/文件
- 给频道或群加「监控」→ 自动归档新消息
- 按类型浏览：消息 / 图片 / 视频 / 文件 / 链接 / 材料
- 全文搜索

---

## 四、怎么让外面访问到（三种方式，选一个）

服务器默认只在 `8080` 端口监听，能不能从外面访问取决于两件事：**防火墙/安全组** 和 **你有没有开 TLS**。

### 方式一：SSH 隧道（推荐，最安全）

**不用开放任何端口，不用配 TLS，流量全程加密。**

在你自己的电脑上执行：

```bash
ssh -L 8080:127.0.0.1:8080 deployer@<服务器IP>
```

保持这个 SSH 窗口开着，然后**在你自己的电脑上**打开：

```
http://127.0.0.1:8080
```

就这样。密码在 SSH 加密通道里传输，不经过公网明文。

> 这是唯一一个"零配置且安全"的方案。如果只是你自己用，选这个就够了，后面两种可以跳过。

### 方式二：直接开放端口（最省事，但密码明文传输）

1. **云厂商安全组**放行 8080（AWS：EC2 → 安全组 → 入站规则 → 添加 TCP 8080，来源建议填你自己的固定 IP，不要填 `0.0.0.0/0`）
2. 服务器本机防火墙（如果有 `ufw`）：
   ```bash
   sudo ufw allow 8080/tcp    # 需要 root；没有 root 就只能靠安全组
   ```
3. 浏览器访问 `http://<服务器IP>:8080`

**这条路的代价**：HTTP 明文，登录密码在网络上是可见的。在自己家宽带里风险不大，在咖啡厅/机场 WiFi 上等于把密码写给同网段的人看。

### 方式三：加一层 HTTPS（要域名）

如果没有 root、装不了 Nginx，有两个不需要 root 的办法：

**A. cloudflared**（推荐，免费，不用开放端口）

```bash
# 下载静态二进制（不需要 root）
curl -L -o ~/cloudflared https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64
chmod +x ~/cloudflared

# 临时隧道，跑起来会给你一个 https://xxx.trycloudflare.com 地址
~/cloudflared tunnel --url http://127.0.0.1:8080
```

把那个 `https://` 地址发给要访问的人即可。它会自动处理证书。

> 注意：这种方式你的服务会暴露在一个随机公网域名上，**登录密码就是唯一防线**。用强密码，别用 `123456`。

**B. 有 root 的话**：装 Caddy（自动 HTTPS，配置两行）

```
your-domain.com {
    reverse_proxy 127.0.0.1:8080
}
```

---

## 五、登录保护是怎么实现的

不是浏览器弹的那种 `Basic Auth` 小框，是**真正的登录页面 + 会话 Cookie**：

| 机制 | 说明 |
|---|---|
| 密码存储 | 不落盘，只在启动参数里，进程内存中比对 |
| 密码比对 | 常量时间比较（先哈希再比），不会通过响应时间泄露密码长度 |
| 会话 Cookie | `HttpOnly`（JS 读不到）+ `SameSite=Lax`（防跨站提交）+ 12 小时过期 |
| Cookie 签名 | HMAC-SHA256，篡改任意一个字节都会被拒 |
| 换密码 | 所有已发出的会话**立即失效**（签名密钥由密码派生） |
| 暴力破解 | 连续输错 3 次开始锁定，时间逐次翻倍，最长 15 分钟 |
| 跳转保护 | 登录后跳回原页面，但只允许站内路径（`//evil.com` 这类会被拒） |
| API 保护 | 未登录访问 `/api/*` 返回 `401`，不会重定向成 HTML |

不想用页面登录、想用命令行调接口时，可以改用 Bearer Token 模式：

```bash
# 停掉当前服务，换成 token 模式
~/tdl-archive/tdl archive serve --addr 0.0.0.0:8080 --auth-token "your-long-random-token"

# 调用
curl -H "Authorization: Bearer your-long-random-token" http://127.0.0.1:8080/api/stats
```

三种鉴权模式**互斥**，同时配两个会直接报错，不会静默选一个。

---

## 六、日常运维

安装脚本会在 `~/tdl-archive/` 生成这些文件：

| 文件 | 作用 |
|---|---|
| `tdl` | 主程序（静态二进制） |
| `config.yaml` | 配置（app-id / app-hash / 代理） |
| `.credentials` | 登录账号密码（权限 600） |
| `run.sh` | 实际启动命令（含密码，权限 700） |
| `start.sh` / `stop.sh` / `restart.sh` | 启停 |
| `tdl.log` | 日志 |
| `tdl.pid` | 进程号 |

### 常用命令

```bash
~/tdl-archive/start.sh          # 启动
~/tdl-archive/stop.sh           # 停止
~/tdl-archive/restart.sh        # 重启（改完配置后）

tail -f ~/tdl-archive/tdl.log   # 实时看日志
tail -100 ~/tdl-archive/tdl.log # 看最近 100 行
```

### 调下载速度

一次回填的下载并发默认是 **4 个文件同时下**。单文件内部另有一条字节分片通道，
所以实际连接数比 4 更多，但对 Telegram 来说是正常量级。

```bash
# 临时提高（写进 run.sh 或 systemd 的 Environment 才会持久）
TDL_MEDIA_THREADS=8 ~/tdl-archive/start.sh
```

| 值 | 适用场景 |
|---|---|
| 1 | 服务器同时在跑别的重活，或网络很差 |
| **4** | 默认，交互性和速度的平衡点 |
| 8 | 只有归档在跑、带宽充足 |
| >16 | 会被忽略并回退到 4 —— 再多换来的不是吞吐，是 FLOOD_WAIT |

改 systemd 的话：

```bash
sudo systemctl edit tdl-archive      # 加一行 Environment=TDL_MEDIA_THREADS=8
sudo systemctl restart tdl-archive
```

### 缩略图布局跳动（宽高缺失）

面板的图片墙靠每张图的宽高比预留位置。**旧版本归档的照片没有记录宽高，
所以图片一到、布局就跳。** 数据库里其实一直存着原始报文，可以在本地补回来，
不需要重新下载、也不需要连 Telegram：

```bash
~/tdl-archive/stop.sh
~/tdl-archive/tdl archive media backfill-dims --dry-run   # 先看有多少条要补
~/tdl-archive/tdl archive media backfill-dims             # 实际补（可重复跑）
~/tdl-archive/start.sh
```

实测 2682 条照片 **1.2 秒**补完。再跑一次会显示 `examined 0`，说明是幂等的。

### 数据在哪

| 内容 | 路径 |
|---|---|
| 归档数据库 + 媒体文件 | `~/.tdl/` |
| 程序与配置 | `~/tdl-archive/` |

**备份**就是打包这两个目录：

```bash
tar -czf ~/backup-$(date +%F).tar.gz ~/.tdl ~/tdl-archive/config.yaml ~/tdl-archive/.credentials
```

### 升级

```bash
~/tdl-archive/stop.sh
# 上传新版本的 tdl 覆盖旧文件
cp /path/to/new/tdl ~/tdl-archive/tdl && chmod 755 ~/tdl-archive/tdl
~/tdl-archive/start.sh
```

数据库会自动迁移，配置和密码都保留。

### 卸载

```bash
~/tdl-archive/stop.sh
rm -rf ~/tdl-archive ~/.tdl
```

（第二条会删掉所有归档内容和 Telegram 登录状态，确认后再执行。）

---

## 七、常见问题

**Q: `./install.sh` 报 `Permission denied`**
`chmod +x install.sh` 再跑一次。

**Q: 报「端口 8080 已被占用」**
`PORT=9000 ./install.sh` 换一个。

**Q: 启动成功，但浏览器打不开**
三个可能，按顺序排查：
1. `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/login` —— 在**服务器上**跑。返回 200 说明服务正常，问题在网络层
2. 云厂商安全组没放行端口
3. `--addr` 绑的是 `127.0.0.1` 而不是 `0.0.0.0`（install.sh 默认绑 `0.0.0.0`）

**Q: 登录页能打开，但登录后一片空白**
打开浏览器开发者工具看 Console。多半是接口返回了 401 —— 检查 Cookie 是否被浏览器拦了（用了 `http://` 但 Cookie 标了 `Secure` 时会这样；本方案只在检测到 HTTPS 时才加 `Secure`，正常不会触发）。

**Q: 忘了密码**
```bash
RESET_PASSWORD=1 ./install.sh
```

**Q: 面板显示「只读模式」**
`config.yaml` 里 `app-id` 还是 `0`。填上你的 `api_id` / `api_hash` 后 `restart.sh`。

**Q: 扫码登录 Telegram 一直失败**
- 服务器能不能访问 Telegram？`curl -s -o /dev/null -w '%{http_code}\n' https://core.telegram.org`
- 不通的话在 `config.yaml` 的 `tdl.proxy` 填代理
- 二维码有时效（约 30 秒），过期了点「刷新」

**Q: 想开机自动启动**
`install.sh` 用的是 `nohup`，重启服务器后需要手动 `start.sh`。要开机自启，按 `tdl-serve.service.template` 头部的注释配 systemd user 单元（不需要 root），再执行 `loginctl enable-linger "$USER"`。

**Q: 磁盘满了**
`~/.tdl/` 里的媒体文件是主要占用。删掉不需要的监控目标，或用面板里的「存储」页面清理。

---

## 八、安全须知（请务必看完）

1. **登录密码是唯一防线。** 一旦公网可达，任何人访问那个地址都会撞到登录页。用脚本生成的 16 位随机密码，不要改成 `admin123`。

2. **优先用 SSH 隧道（方式一）。** 不开端口 = 攻击面为零，且密码全程加密。只有确实要分享给别人用时才考虑开放端口。

3. **`~/.tdl/` 里存着 Telegram 账号的登录状态（session）。** 拿到这个文件等于拿到你的号——不需要验证码就能登录。所以：
   - 不要把 `~/.tdl/` 打包发给别人
   - 不要放到网盘 / git 仓库
   - 多人共用的机器上别装

4. **`config.yaml` 里的 `api_hash` 和 `~/tdl-archive/.credentials` 同样是敏感文件。** 权限已经设成 600/700，不要改成 644。

5. **这个面板能看到你 Telegram 账号能看到的一切**：私聊、私有群、受保护的频道。它不是"只能导公开内容"的工具。分享访问权限前想清楚这一点。

6. **服务器被入侵时**：先 `stop.sh`，然后去 Telegram 手机端 → 设置 → 设备 → 把那个会话踢掉。这样即使 session 文件被拿走也失效了。

---

## 附：手动启动（不用 install.sh）

```bash
# 前台跑，方便看日志
./tdl archive serve --addr 0.0.0.0:8080 --auth-login "admin:你的密码"

# 后台跑
nohup ./tdl archive serve --addr 0.0.0.0:8080 --auth-login "admin:你的密码" \
  > tdl.log 2>&1 &
```

参数说明：

| 参数 | 说明 |
|---|---|
| `--addr` | 监听地址。`127.0.0.1:8080` 只本机能访问；`0.0.0.0:8080` 所有网卡 |
| `--auth-login user:pass` | 页面登录（本说明介绍的方式），密码至少 8 位 |
| `--auth-token TOKEN` | Bearer Token，给脚本调接口用 |
| `--auth-basic user:pass` | 浏览器原生弹框（旧方式） |
| `--insecure` | 允许无鉴权绑公网。**除非你清楚在做什么，不要加** |

不带任何鉴权参数时，程序会强制只绑 `127.0.0.1` 并打印警告 —— 这是故意的，防止有人把没有任何保护的面板直接暴露到公网。
