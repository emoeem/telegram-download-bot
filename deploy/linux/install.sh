#!/usr/bin/env bash
#
# Telegram 下载器 · 自托管部署脚本
#
#   ./install.sh                 默认端口 8080，账号 admin
#   PORT=9000 ./install.sh       换端口
#   USER_NAME=me ./install.sh    换账号名
#   RESET_PASSWORD=1 ./install.sh  忘记密码时重设
#
# 这个脚本不需要 root，也不需要目标机装 node / docker / nginx / gcc。
# 整套程序就是一个静态编译的二进制文件。

set -euo pipefail

# ------------------------------------------------------------------ 常量
APP_NAME="Telegram 下载器"
APP_DIR="${HOME}/tdl-archive"
BIN="${APP_DIR}/tdl"
PORT="${PORT:-8080}"
USER_NAME="${USER_NAME:-admin}"
BIND_ADDR="0.0.0.0:${PORT}"
CONF="${APP_DIR}/config.yaml"
CRED="${APP_DIR}/.credentials"
LOG="${APP_DIR}/tdl.log"
PIDFILE="${APP_DIR}/tdl.pid"

c_reset=$'\033[0m'; c_ok=$'\033[32m'; c_warn=$'\033[33m'; c_err=$'\033[31m'; c_dim=$'\033[2m'
ok()   { printf '%s✓%s %s\n' "$c_ok" "$c_reset" "$*"; }
warn() { printf '%s!%s %s\n' "$c_warn" "$c_reset" "$*"; }
err()  { printf '%s✗%s %s\n' "$c_err" "$c_reset" "$*" >&2; }
dim()  { printf '%s%s%s\n' "$c_dim" "$*" "$c_reset"; }

need() { command -v "$1" >/dev/null 2>&1 || { err "缺少命令: $1"; exit 1; }; }

# ------------------------------------------------------------------ 0. 环境
need tar
[ "$(uname -s)" = "Linux" ] || { err "这个脚本只用于 Linux"; exit 1; }

case "$(uname -m)" in
  x86_64|amd64) : ;;
  aarch64|arm64) err "这个包是 x86_64 版。ARM 机器请用 ARM 包，或自行用源码交叉编译。"; exit 1 ;;
  *) err "不支持的架构: $(uname -m)"; exit 1 ;;
esac

SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
[ -x "${SRC_DIR}/tdl" ] || { err "没找到 tdl 二进制，请在解压目录里运行 ./install.sh"; exit 1; }

mkdir -p "${APP_DIR}"
chmod 750 "${APP_DIR}"

# ------------------------------------------------------------------ 1. 停旧进程
if [ -f "${PIDFILE}" ]; then
  OLD="$(cat "${PIDFILE}" 2>/dev/null || true)"
  if [ -n "${OLD}" ] && kill -0 "${OLD}" 2>/dev/null; then
    printf '停止旧进程 %s … ' "${OLD}"
    kill "${OLD}" 2>/dev/null || true
    for _ in $(seq 1 20); do kill -0 "${OLD}" 2>/dev/null || break; sleep 0.25; done
    echo "已停止"
  fi
  rm -f "${PIDFILE}"
fi

# ------------------------------------------------------------------ 2. 端口检查
port_busy() {
  # 纯 bash 探测，避免依赖 ss/netstat 的格式差异
  (exec 3<>"/dev/tcp/127.0.0.1/${PORT}") 2>/dev/null && { exec 3<&-; return 0; } || return 1
}
if port_busy; then
  err "端口 ${PORT} 已被占用。换一个端口重试： PORT=9000 ./install.sh"
  exit 1
fi
ok "端口 ${PORT} 空闲"

# ------------------------------------------------------------------ 3. 安装二进制
cp "${SRC_DIR}/tdl" "${BIN}.new"
chmod 755 "${BIN}.new"
mv "${BIN}.new" "${BIN}"
ok "已安装二进制 → ${BIN}"

# 静态链接自检：如果这一步失败，说明拿错了包
if command -v file >/dev/null 2>&1; then
  desc="$(file "${BIN}")"
  case "${desc}" in
    *"statically linked"*) : ;;
    *) warn "该二进制不是静态链接，目标机可能缺库" ;;
  esac
fi

# ------------------------------------------------------------------ 4. 密码
gen_password() {
  # 去掉易混字符（0/o/O/1/l/I），避免抄错。
  # 注意：不能写成 `tr -dc ... </dev/urandom | head -c 16` —— head 提前关闭
  # 管道会让 tr 收到 SIGPIPE，在 `set -o pipefail` 下整个脚本会中止。
  # 这里先把熵源一次性转完，再由 bash 截取，全程没有提前退出的读端。
  local raw
  raw="$(head -c 256 /dev/urandom | base64 | LC_ALL=C tr -dc 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789')"
  printf '%s' "${raw:0:16}"
}

# 凭据文件有两种形态：
#   1) 首次部署：user=… / password=…（明文，只在服务启动前的这一小段时间存在）
#   2) 服务启动后：JSON + PBKDF2 哈希。服务会把明文升级掉，之后这个文件
#      就是唯一真源，面板里改的密码也写在这里。
# 所以脚本只需要判断"文件在不在"，而不是"能不能读出明文"。
NEED_INIT=0
if [ "${RESET_PASSWORD:-0}" = "1" ] || [ ! -f "${CRED}" ]; then
  NEED_INIT=1
fi

if [ "${NEED_INIT}" = "1" ]; then
  PASS="$(gen_password)"
  umask 077
  printf 'user=%s\npassword=%s\n' "${USER_NAME}" "${PASS}" > "${CRED}"
  chmod 600 "${CRED}"
  ok "已生成登录密码（保存在 ${CRED}）"
else
  # shellcheck disable=SC1090
  . "${CRED}" 2>/dev/null || true
  USER_NAME="${user:-${USER_NAME}}"
  PASS="${password:-}"
  if [ -n "${PASS}" ]; then
    ok "沿用已有账号 ${USER_NAME}（重设密码： RESET_PASSWORD=1 ./install.sh）"
  else
    # 已经是哈希形态：读不回明文是正常状态，不是错误。
    ok "沿用已有账号 ${USER_NAME}（密码已哈希存储，无法回显）"
    dim "  改密码请登录面板 → 右侧「账号」，或 RESET_PASSWORD=1 ./install.sh 重新生成。"
  fi
fi

if [ -n "${PASS}" ]; then
  # 密码里有冒号会破坏 user:pass 的解析
  case "${PASS}" in
    *:*) err "密码包含冒号，无法用于登录参数。请执行 RESET_PASSWORD=1 ./install.sh"; exit 1 ;;
  esac
  if [ "${#PASS}" -lt 8 ]; then
    err "密码短于 8 位，服务会拒绝启动。请执行 RESET_PASSWORD=1 ./install.sh"
    exit 1
  fi
fi

# ------------------------------------------------------------------ 5. 配置
if [ ! -f "${CONF}" ]; then
  if [ -f "${SRC_DIR}/config.template.yaml" ]; then
    cp "${SRC_DIR}/config.template.yaml" "${CONF}"
    chmod 600 "${CONF}"
  else
    : > "${CONF}"
    chmod 600 "${CONF}"
  fi
  ok "已创建配置 ${CONF}"
else
  ok "沿用已有配置 ${CONF}"
fi

APP_ID="$(grep -E '^\s*app-id:' "${CONF}" 2>/dev/null | head -1 | awk '{print $2}' | tr -d '"' || true)"
if [ -z "${APP_ID}" ] || [ "${APP_ID}" = "0" ]; then
  warn "配置里没有 telegram app-id，面板将以「只读模式」运行"
  dim "  只读模式：可以浏览已归档内容，但不能登录 Telegram、不能下载新内容。"
  dim "  要完整功能：编辑 ${CONF} 填入 app-id / app-hash（从 https://my.telegram.org 申请），然后 ./restart.sh"
fi

# ------------------------------------------------------------------ 6. 启动
cat > "${APP_DIR}/run.sh" <<EOF
#!/usr/bin/env bash
# 启动归档面板（由 install.sh 生成）
set -euo pipefail
cd "${APP_DIR}"
# 凭据文件有两种形态：首次部署是明文（user=/password=），服务启动后会把它
# 升级成 JSON + PBKDF2 哈希。只有明文形态才需要 --auth-login，其余情况不传，
# 这样密码不会长期躺在命令行里被 ps 看到。
if head -c 1 "${CRED}" 2>/dev/null | grep -q '{'; then
  exec "${BIN}" archive serve \\
    --addr "${BIND_ADDR}" \\
    --auth-file "${CRED}"
fi
exec "${BIN}" archive serve \\
  --addr "${BIND_ADDR}" \\
  --auth-file "${CRED}" \\
  --auth-login "${USER_NAME}:${PASS}"
EOF
chmod 700 "${APP_DIR}/run.sh"

cat > "${APP_DIR}/stop.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
if [ -f "${PIDFILE}" ]; then
  PID="\$(cat "${PIDFILE}")"
  kill "\${PID}" 2>/dev/null && echo "已停止 \${PID}" || echo "进程不存在"
  rm -f "${PIDFILE}"
else
  echo "没有 PID 文件，未运行"
fi
EOF
chmod 700 "${APP_DIR}/stop.sh"

cat > "${APP_DIR}/restart.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
"\$(dirname "\${BASH_SOURCE[0]}")/stop.sh" >/dev/null 2>&1 || true
"\$(dirname "\${BASH_SOURCE[0]}")/start.sh"
EOF
chmod 700 "${APP_DIR}/restart.sh"

cat > "${APP_DIR}/start.sh" <<EOF
#!/usr/bin/env bash
# 后台启动并写 PID（由 install.sh 生成）
set -euo pipefail
cd "${APP_DIR}"
if [ -f "${PIDFILE}" ] && kill -0 "\$(cat "${PIDFILE}")" 2>/dev/null; then
  echo "已在运行 (PID \$(cat "${PIDFILE}"))"
  exit 0
fi
nohup "${APP_DIR}/run.sh" >> "${LOG}" 2>&1 &
echo \$! > "${PIDFILE}"
sleep 1
if kill -0 "\$(cat "${PIDFILE}")" 2>/dev/null; then
  echo "已启动 (PID \$(cat "${PIDFILE}"))，日志: ${LOG}"
else
  echo "启动失败，最后 20 行日志:"
  tail -20 "${LOG}" || true
  exit 1
fi
EOF
chmod 700 "${APP_DIR}/start.sh"

printf '启动服务 … '
"${APP_DIR}/start.sh" >/dev/null || {
  err "启动失败。日志："
  tail -20 "${LOG}" >&2 || true
  exit 1
}
echo "完成"

# ------------------------------------------------------------------ 7. 自检
ready=0
for _ in $(seq 1 40); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/login" 2>/dev/null || echo 000)"
  if [ "${code}" = "200" ]; then ready=1; break; fi
  sleep 0.25
done

if [ "${ready}" = "1" ]; then
  ok "自检通过：登录页 /login 返回 200"
else
  err "自检失败（40 次探测内 /login 未就绪）"
  dim "  最后 20 行日志:"
  tail -20 "${LOG}" >&2 || true
  exit 1
fi

# 未登录必须被挡住 —— 这条是部署是否安全的分水岭
guard="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/" 2>/dev/null || echo 000)"
if [ "${guard}" = "303" ] || [ "${guard}" = "302" ]; then
  ok "自检通过：未登录访问首页被重定向到登录页"
else
  err "未登录访问首页返回 ${guard}，登录保护没有生效，请检查启动参数"
  exit 1
fi

# ------------------------------------------------------------------ 8. 访问信息
PUBLIC_IP="$(curl -s --max-time 3 https://api.ipify.org 2>/dev/null || true)"

echo
echo "────────────────────────────────────────────"
echo " ${APP_NAME} 已部署"
echo "────────────────────────────────────────────"
echo " 本机地址 : http://127.0.0.1:${PORT}"
if [ -n "${PUBLIC_IP}" ]; then
  echo " 公网地址 : http://${PUBLIC_IP}:${PORT}   ${c_dim}(需安全组放行 ${PORT})${c_reset}"
fi
echo " 账号     : ${USER_NAME}"
echo " 密码     : ${PASS}"
echo " 安装目录 : ${APP_DIR}"
echo " 日志     : ${LOG}"
echo "────────────────────────────────────────────"
echo
echo "推荐用 SSH 隧道访问（加密，且不需要开放端口）："
dim "  ssh -L ${PORT}:127.0.0.1:${PORT} $(whoami)@<服务器地址>"
dim "  然后浏览器打开 http://127.0.0.1:${PORT}"
echo
echo "常用命令："
dim "  ${APP_DIR}/start.sh     启动"
dim "  ${APP_DIR}/stop.sh      停止"
dim "  ${APP_DIR}/restart.sh   重启（改完 config.yaml 后执行）"
dim "  tail -f ${LOG}          看日志"
echo
if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${PORT}/" 2>/dev/null)" = "303" ] && [ -n "${PUBLIC_IP}" ]; then
  warn "公网端口一旦放行，登录密码就是唯一防线。"
  dim "  没有 HTTPS 时密码是明文传输 —— 优先用上面的 SSH 隧道，或按文档「方式三」加 TLS。"
fi
