#!/usr/bin/env bash
# TG归档 · 商业授权签发台（TUI）
#
# 把 `tdl license gen` 那套参数变成菜单操作：选设备码 → 选时长 → 选强度 → 出结果，
# 顺手把每次签发记进本地台账，避免"这个号发过没有""给谁签的"全靠脑子记。
#
# 主密钥（TDLSK1-…）只在本机用。脚本永远不打印它的内容，只报是否就位。
#
# 用法：
#   scripts/license-tui.sh                                       交互式
#   scripts/license-tui.sh --gen --device 7K3M9QX2 --days 30     无人值守签发
#   scripts/license-tui.sh --check <密钥|序列号>                  校验
#   scripts/license-tui.sh --ledger                              打印台账
#
# 环境变量：
#   TDL_ISSUER  主密钥路径（默认 ~/.tdl-issuer.txt）
#   TDL_LEDGER  台账路径（默认 ~/.tdl-license-issued.tsv）
#   TDL_BIN     tdl 二进制路径（默认自动探测）

set -euo pipefail

VERSION="1.0"
ISSUER="${TDL_ISSUER:-$HOME/.tdl-issuer.txt}"
LEDGER="${TDL_LEDGER:-$HOME/.tdl-license-issued.tsv}"
# 云端授权（可选）。设 TDL_LICENSE_SERVER= 空则整个跳过登记，纯本地验签。
LICENSE_SERVER="${TDL_LICENSE_SERVER-https://lic.emoe.eu.cc}"
ADMIN_ENV="${TDL_ADMIN_ENV:-$HOME/.tdl-license/admin.env}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

# ────────────────────────────── 颜色 / 宽度 ──────────────────────────────

if [ -t 1 ] && [ "${NO_COLOR:-}" = "" ]; then
  C_RESET=$'\033[0m'; C_DIM=$'\033[2m'; C_BOLD=$'\033[1m'
  C_CYAN=$'\033[36m'; C_GREEN=$'\033[32m'; C_RED=$'\033[31m'
  C_YELLOW=$'\033[33m'; C_GREY=$'\033[90m'
else
  C_RESET=; C_DIM=; C_BOLD=; C_CYAN=; C_GREEN=; C_RED=; C_YELLOW=; C_GREY=
fi

ui_cols() {
  local c
  c=$(tput cols 2>/dev/null || echo 72)
  [ "$c" -lt 46 ] && c=46
  [ "$c" -gt 78 ] && c=78
  echo "$c"
}
WIDTH=$(ui_cols)

hr() { printf '%s%s%s\n' "$C_GREY" "$(printf '─%.0s' $(seq 1 "$WIDTH"))" "$C_RESET"; }

clear_screen() {
  if [ -t 1 ]; then printf '\033[2J\033[H'; else printf '\n'; fi
}

banner() {
  hr
  printf '  %s%s%s  %s商业授权签发台 v%s%s\n' "$C_BOLD" "$C_CYAN" "TG归档" "$C_DIM" "$VERSION" "$C_RESET"
  hr
}

# ────────────────────────────── 基础输出 ──────────────────────────────

die()  { printf '%s✗ %s%s\n' "$C_RED" "$*" "$C_RESET" >&2; exit 1; }
ok()   { printf '%s✓ %s%s\n' "$C_GREEN" "$*" "$C_RESET"; }
warn() { printf '%s! %s%s\n' "$C_YELLOW" "$*" "$C_RESET"; }
note() { printf '%s%s%s\n' "$C_DIM" "$*" "$C_RESET"; }

# 提问：提示写 stderr，答案写 stdout，可直接 $(ask ...)
ask() {
  local prompt="$1" default="${2-}" reply
  if [ -n "$default" ]; then
    printf '  %s%s%s %s[%s]%s › ' "$C_BOLD" "$prompt" "$C_RESET" "$C_GREY" "$default" "$C_RESET" >&2
  else
    printf '  %s%s%s › ' "$C_BOLD" "$prompt" "$C_RESET" >&2
  fi
  IFS= read -r reply || reply=""
  [ -z "$reply" ] && reply="$default"
  printf '%s' "$reply"
}

pause_key() {
  printf '\n  %s按回车返回%s' "$C_DIM" "$C_RESET" >&2
  IFS= read -r _ || true
}

# ────────────────────────────── 依赖探测 ──────────────────────────────

find_tdl() {
  if [ -n "${TDL_BIN:-}" ]; then
    [ -x "$TDL_BIN" ] && { printf '%s' "$TDL_BIN"; return 0; }
    die "TDL_BIN=$TDL_BIN 不可执行"
  fi
  local c
  for c in "$PROJECT_DIR/tdl" "$SCRIPT_DIR/../tdl" "$PWD/tdl"; do
    [ -x "$c" ] && { printf '%s' "$c"; return 0; }
  done
  c=$(command -v tdl 2>/dev/null || true)
  [ -n "$c" ] && { printf '%s' "$c"; return 0; }
  die "找不到 tdl 二进制。用 TDL_BIN=/path/to/tdl 指定。"
}

issuer_ok() { [ -f "$ISSUER" ] && [ -r "$ISSUER" ]; }

issuer_status() {
  if issuer_ok; then
    printf '%s✓ 就位%s %s%s%s' "$C_GREEN" "$C_RESET" "$C_GREY" "${ISSUER/#$HOME/~}" "$C_RESET"
  else
    printf '%s✗ 缺失%s %s%s%s' "$C_RED" "$C_RESET" "$C_GREY" "${ISSUER/#$HOME/~}" "$C_RESET"
  fi
}

ledger_count() {
  [ -f "$LEDGER" ] || { printf '0'; return 0; }
  awk 'NR>1' "$LEDGER" | wc -l | tr -d ' '
}

clip_copy() {
  local text="$1" t
  if command -v wl-copy >/dev/null 2>&1; then
    printf '%s' "$text" | wl-copy 2>/dev/null && return 0
  fi
  for t in xclip xsel; do
    case "$t" in
      xclip) command -v xclip >/dev/null 2>&1 && printf '%s' "$text" | xclip -selection clipboard 2>/dev/null && return 0 ;;
      xsel)  command -v xsel  >/dev/null 2>&1 && printf '%s' "$text" | xsel --clipboard --input  2>/dev/null && return 0 ;;
    esac
  done
  return 1
}

# ────────────────────────────── 台账 ──────────────────────────────

ledger_init() {
  [ -f "$LEDGER" ] && return 0
  printf 'ts\tdevice\tkind\tterm\tserial\tkey\tnote\n' > "$LEDGER"
  chmod 600 "$LEDGER"
}

ledger_add() {
  local device="$1" kind="$2" term="$3" serial="$4" key="$5" note="$6"
  ledger_init
  note="${note//$'\t'/ }"; note="${note//$'\n'/ }"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$(date '+%Y-%m-%d %H:%M:%S')" "$device" "$kind" "$term" "$serial" "$key" "$note" >> "$LEDGER"
}

# ────────────────────────── 云端登记（admit）──────────────────────────
#
# 这一层很容易被漏掉，漏掉就是"签出来但客户激活不了"：
#   gen   只做本地签名 → 密钥在数学上有效
#   admit 把密钥写进服务端白名单 → 密钥在业务上可用
#
# App 的激活顺序是「先联网登记、后本地验签」（见 archive/license_gate.go），
# 所以没 admit 的密钥会拿到服务端 unknown_key，界面直接显示"密钥无效"，
# 客户的感受就是"你给的这个密钥是坏的"。

admin_token() {
  if [ -n "${TDL_ADMIN_TOKEN:-}" ]; then printf '%s' "$TDL_ADMIN_TOKEN"; return 0; fi
  local f t
  for f in "$ADMIN_ENV" "$HOME/.tdl-license/server.env"; do
    [ -r "$f" ] || continue
    t=$(sed -n 's/^TDL_ADMIN_TOKEN=//p' "$f" | head -1)
    [ -z "$t" ] && t=$(sed -n 's/^ADMIN_TOKEN=//p' "$f" | head -1)
    [ -n "$t" ] && { printf '%s' "$t"; return 0; }
  done
  return 1
}

# admit_key <key> <short|strong> <expiry_day:0=永久> <note>
# 返回 0=已登记 / 2=没配 token 跳过 / 3=网络或 HTTP 失败 / 4=服务端拒绝
admit_key() {
  local key="$1" kind="$2" expiry_day="$3" note_text="$4"
  local tok
  tok=$(admin_token) || return 2
  local payload
  payload=$(printf '{"key":"%s","kind":"%s","expiry_day":%s,"note":"%s"}' \
    "$key" "$kind" "$expiry_day" "${note_text//\"/}")
  local out
  # 隧道首连会慢（实测冷启动一次 23s，热连接 0.3s）。超时给短了，
  # 就会出现"签成功但登记失败"——客户拿到的是打不开的密钥。
  out=$(curl -sS --max-time 20 --retry 1 --retry-delay 1 --retry-connrefused \
        -X POST "$LICENSE_SERVER/v1/admit" \
        -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
        -d "$payload" 2>&1) || { ADMIT_ERROR="$out"; return 3; }
  case "$out" in
    *'"ok":true'*) return 0 ;;
    *) ADMIT_ERROR="$out"; return 4 ;;
  esac
}

# 启动时的静默探测缓存：后台跑，结果写文件，菜单渲染时读。
# 超时/失败就不留痕迹 —— 红点只在真的查到未登记时出现。
PENDING_CACHE=""
PENDING_JOB=""

# 列出"台账里有、服务器上没有"的密钥 —— 这些发给客户就是一句「密钥无效」。
# 传 count_only 时只打印条数，供后台探测使用。
do_pending() {
  local count_only="${1:-}"
  [ -f "$LEDGER" ] || { [ -n "$count_only" ] && echo 0; return 0; }
  [ -n "$LICENSE_SERVER" ] || { [ -z "$count_only" ] && warn "TDL_LICENSE_SERVER 为空，无法比对"; return 1; }
  local tok json admitted
  tok=$(admin_token) || { [ -z "$count_only" ] && warn "没有管理员 token（$ADMIN_ENV）"; return 1; }
  json=$(curl -sS --max-time "${PENDING_TIMEOUT:-20}" --retry 1 --retry-connrefused \
        -H "Authorization: Bearer $tok" "$LICENSE_SERVER/v1/keys" 2>/dev/null) || {
    [ -z "$count_only" ] && warn "拉不到服务器密钥列表"; return 1; }
  admitted=$(printf '%s' "$json" | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except Exception: sys.exit(1)
for k in d.get("keys",[]): print(k["key"])
' 2>/dev/null) || { [ -z "$count_only" ] && warn "服务器返回无法解析"; return 1; }

  local n=0 ts device kind term serial key note_text
  while IFS=$'\t' read -r ts device kind term serial key note_text; do
    [ "$ts" = "签发时间" ] && continue
    [ -z "$key" ] && continue
    if ! printf '%s\n' "$admitted" | grep -qxF "$key"; then
      if [ -z "$count_only" ]; then
        [ "$n" -eq 0 ] && printf '\n  %s未登记（客户激活会提示「密钥无效」）%s\n\n' "$C_BOLD$C_RED" "$C_RESET"
        printf '  序列号 %s · 设备 %s · %s %s · %s\n' "$serial" "$device" "$kind" "$term" "${note_text:-无备注}"
        printf '    %s补登记：scripts/license-tui.sh --admit %s%s\n' "$C_GREY" "$serial" "$C_RESET"
      fi
      n=$((n+1))
    fi
  done < <(tail -n +2 "$LEDGER")

  if [ -n "$count_only" ]; then printf '%s\n' "$n"; return 0; fi

  if [ "$n" -eq 0 ]; then
    ok "台账里 $(($(wc -l < "$LEDGER")-1)) 条密钥全部已登记"
  else
    printf '\n  共 %s 条未登记\n' "$n"
    return 2
  fi
}

# 后台静默探测：不阻塞菜单，跑完把条数写进 $PENDING_CACHE。
# 超时/网络失败就什么都不留 —— 菜单不会因此卡住或弹出错误。
pending_probe_bg() {
  [ -n "$LICENSE_SERVER" ] || return 0
  [ -f "$LEDGER" ] || return 0
  command -v mktemp >/dev/null 2>&1 || return 0
  PENDING_CACHE=$(mktemp -t tdl-pending.XXXXXX 2>/dev/null) || { PENDING_CACHE=""; return 0; }
  (
    PENDING_TIMEOUT="${PENDING_PROBE_TIMEOUT:-8}" do_pending count_only >"$PENDING_CACHE" 2>/dev/null </dev/null
  ) &
  PENDING_JOB=$!
  disown "$PENDING_JOB" 2>/dev/null || true
  return 0
}

# 菜单顶部的红点。探测没结果（还在跑 / 超时 / 全登记）就一个字符都不输出。
pending_badge() {
  [ -n "$PENDING_CACHE" ] && [ -s "$PENDING_CACHE" ] || return 0
  local n; n=$(cat "$PENDING_CACHE" 2>/dev/null) || return 0
  case "$n" in ''|*[!0-9]*) return 0 ;; esac
  [ "$n" -gt 0 ] || return 0
  printf '%s● %s 条未登记%s' "$C_RED" "$n" "$C_RESET"
}

# 补登记：给"已经签出但没进服务器白名单"的旧密钥用，密钥本身不用重发。
do_admit() {
  local q="$1" row
  [ -f "$LEDGER" ] || { warn "台账不存在：$LEDGER"; return 1; }
  row=$(awk -F'\t' -v q="$q" 'NR>1 && (tolower($5)==tolower(q) || index($6,q)) { print; exit }' "$LEDGER")
  if [ -z "$row" ]; then
    # 也认设备码：客户报的一般是设备码，不是序列号。同一设备多条时取最新一条。
    row=$(awk -F'\t' -v q="$q" 'NR>1 && tolower($2)==tolower(q) { r=$0 } END { if (r) print r }' "$LEDGER")
    [ -n "$row" ] && note "  按设备码匹配到最新一条（同设备多条时只处理最近这条）"
  fi
  [ -z "$row" ] && { warn "台账里没找到：$q（序列号、设备码、密钥片段都试过了）"; return 1; }

  local ts device kind term serial key note_text
  IFS=$'\t' read -r ts device kind term serial key note_text <<< "$row"

  local server_kind="short" server_days="0"
  [ "$kind" = "强" ] && server_kind="strong"
  case "$term" in
    永久) server_days="0" ;;
    天*) server_days="${term%% *}" ;;
    *天) server_days="${term%%天*}" ;;
    *) server_days="0" ;;
  esac

  printf '  序列号  %s\n' "$serial"
  printf '  设备码  %s\n' "$device"
  printf '  类型    %s / %s\n' "$kind" "$term"
  printf '  服务器  %s\n\n' "${LICENSE_SERVER:-（未配置）}"

  [ -n "$LICENSE_SERVER" ] || { warn "TDL_LICENSE_SERVER 为空，没有可登记的服务器"; return 1; }

  local rc=0
  admit_key "$key" "$server_kind" "$server_days" "$note_text" || rc=$?
  if [ "$rc" -eq 0 ]; then
    ok "已登记 —— 客户现在可以激活了"
    return 0
  fi
  case "$rc" in
    2) warn "没有管理员 token"
       note "  放到 $ADMIN_ENV（TDL_ADMIN_TOKEN=…）或用环境变量传入" ;;
    3) warn "连不上授权服务器"
       printf '%s\n' "${ADMIT_ERROR:-}" | sed 's/^/    /' ;;
    *) warn "授权服务器拒绝了登记"
       printf '%s\n' "${ADMIT_ERROR:-}" | sed 's/^/    /' ;;
  esac
  return 1
}

# 按密钥或序列号在台账里找完整密钥
ledger_find() {
  local q="$1"
  [ -f "$LEDGER" ] || return 0
  awk -F'\t' -v q="$q" 'NR>1 && (index($6,q) || index(tolower($5),tolower(q))) { print $6 }' "$LEDGER"
}

ledger_show() {
  clear_screen; banner
  if [ ! -f "$LEDGER" ] || [ "$(ledger_count)" = "0" ]; then
    printf '\n'; note "  台账还是空的 —— 签发过的密钥会自动记在这里。"; printf '\n'
    pause_key; return 0
  fi
  printf '\n'
  # 表头手写补空格：printf 的 %-Ns 按字节算，中文列名会短 3 个显示宽
  printf '  %s%s%s\n' "$C_BOLD" \
    "时间                设备码    类型  期限     序列号    尾号   备注" "$C_RESET"
  printf '  %s%s%s\n' "$C_GREY" "$(printf '─%.0s' $(seq 1 $((WIDTH-4))))" "$C_RESET"
  awk -F'\t' 'NR>1 {
      n = $7; if (length(n) > 12) n = substr(n,1,12) "…";
      k = $6; tail = (length(k) >= 4) ? substr(k, length(k)-3) : k;
      printf "  %-19s %-9s %-5s %-8s %-9s %-6s %s\n", $1, $2, $3, $4, $5, tail, n
    }' "$LEDGER" | tail -n 40
  printf '\n'
  note "  共 $(ledger_count) 条（只显示最近 40 条）· 完整台账：${LEDGER/#$HOME/~}"
  printf '\n'
  printf '  %s输入序列号%s → 看完整密钥 + 客户话术（可复制）\n' "$C_CYAN$C_BOLD" "$C_RESET"
  printf '  %s[d]%s 删除一条  %s[q]%s 返回\n' "$C_CYAN" "$C_RESET" "$C_CYAN" "$C_RESET"
  printf '  › '
  local sel; IFS= read -r sel || return 0
  case "$sel" in
    ""|q|Q) return 0 ;;
    d|D) ledger_delete || true ;;
    *) ledger_view "$sel" || true ;;
  esac
  return 0
}

# 从台账取回一条：完整密钥 + 可原样发给客户的话术 + 复制
ledger_view() {
  local q="$1" raw="${2:-}" row ts device kind term serial key note_text
  [ -f "$LEDGER" ] || return 1
  row=$(awk -F'\t' -v q="$q" 'NR>1 && tolower($5)==tolower(q) { print; exit }' "$LEDGER")
  [ -z "$row" ] && row=$(awk -F'\t' -v q="$q" 'NR>1 && index($6,q) { print; exit }' "$LEDGER")
  if [ -z "$row" ]; then
    if [ "$raw" = "raw" ]; then
      printf '台账里没有「%s」\n' "$q" >&2; return 1
    fi
    printf '\n'; warn "台账里没有「$q」（按序列号或密钥片段查）"
    pause_key; return 1
  fi
  IFS=$'\t' read -r ts device kind term serial key note_text <<< "$row"

  if [ "$raw" != "raw" ]; then clear_screen; banner; fi
  printf '\n'
  printf '  %s序列号%s  %s\n' "$C_BOLD" "$C_RESET" "$serial"
  printf '  %s设备码%s  %s\n' "$C_BOLD" "$C_RESET" "$device"
  printf '  %s有效期%s  %s\n' "$C_BOLD" "$C_RESET" "$term"
  printf '  %s强度%s    %s\n' "$C_BOLD" "$C_RESET" "$kind"
  [ -n "$note_text" ] && printf '  %s备注%s    %s\n' "$C_BOLD" "$C_RESET" "$note_text"
  printf '  %s签发于%s  %s\n' "$C_BOLD" "$C_RESET" "$ts"
  printf '\n'
  printf '  %s完整密钥%s\n' "$C_BOLD" "$C_RESET"
  printf '  %s%s%s\n' "$C_BOLD$C_GREEN" "$key" "$C_RESET"
  printf '\n'

  local msg
  msg=$(build_message "$key" "$device" "$term" "$kind")
  printf '  %s────────────── 发给客户的话 ──────────────%s\n' "$C_GREY" "$C_RESET"
  printf '%s\n' "$msg" | sed 's/^/  /'
  printf '  %s──────────────────────────────────────────%s\n' "$C_GREY" "$C_RESET"
  printf '\n'
  [ "$raw" = "raw" ] && return 0
  printf '  %s[c]%s 复制密钥  %s[m]%s 复制整段话术  %s[q]%s 返回\n' \
    "$C_CYAN" "$C_RESET" "$C_CYAN" "$C_RESET" "$C_CYAN" "$C_RESET"
  printf '  › '
  local sel; IFS= read -r sel || return 0
  case "$sel" in
    c|C) if clip_copy "$key"; then ok "密钥已复制到剪贴板"; else note "没找到剪贴板工具（wl-copy/xclip/xsel）"; fi ;;
    m|M) if clip_copy "$msg"; then ok "整段话术已复制到剪贴板"; else note "没找到剪贴板工具（wl-copy/xclip/xsel）"; fi ;;
  esac
  return 0
}

ledger_delete() {
  local serial tmp
  serial=$(ask "要删除的序列号（只删台账记录，不影响已发出的密钥）")
  [ -z "$serial" ] && return 0
  grep -qi -- "$serial" "$LEDGER" || { printf '\n'; warn "台账里没有序列号 $serial"; pause_key; return 0; }
  tmp=$(mktemp)
  awk -F'\t' -v s="$serial" 'NR==1 || tolower($5) != tolower(s)' "$LEDGER" > "$tmp"
  mv -- "$tmp" "$LEDGER"; chmod 600 "$LEDGER"
  printf '\n'; ok "已从台账移除 $serial"
  pause_key
}

# ────────────────────────────── 输出解析 ──────────────────────────────

# tdl license gen 输出形态：
#   MF1P1RE7-...-JBYC4CWR
#       序列号 A7SMN4GZ · 至 2026-11-06（7 天） · 首激绑定设备
# 每行密钥后跟一行信息。用 bash 正则解析（awk 的 match 对中文按字节，不可靠）。
parse_gen_output() {
  local raw="$1" line cur_key="" serial="" rest=""
  while IFS= read -r line; do
    if [[ "$line" =~ ^([A-Z0-9][A-Z0-9-]{18,})$ ]]; then
      cur_key="${BASH_REMATCH[1]}"
      continue
    fi
    if [ -n "$cur_key" ] && [[ "$line" == *序列号* ]]; then
      if [[ "$line" =~ 序列号[[:space:]]+([A-Z0-9]+) ]]; then
        serial="${BASH_REMATCH[1]}"
        rest="${line#*"$serial"}"
        # 去掉前导的 " · "，否则会打印成 "· 序列号 X · · 至 ..."
        rest=$(printf '%s' "$rest" | sed 's/^[[:space:]]*·[[:space:]]*//')
        printf '%s\t%s\t%s\n' "$cur_key" "$serial" "$rest"
      fi
      cur_key=""
    fi
  done <<< "$raw"
}

# ────────────────────────────── 校验 ──────────────────────────────

do_check() {
  local key="${1:-}"
  if [ -z "$key" ]; then
    clear_screen; banner
    printf '\n'
    note "  粘贴要校验的密钥，或输入台账里的序列号"
    printf '\n'
    key=$(ask "密钥或序列号")
    [ -z "$key" ] && return 0
    key=$(printf '%s' "$key" | tr -d ' \t')
  fi

  # 长的像完整密钥就直接用；否则当序列号去台账换
  if [[ ! "$key" =~ ^(MH|MF|ME|TDL2)[A-Z0-9-]{15,}$ ]]; then
    local found
    found=$(ledger_find "$key" | head -1)
    if [ -n "$found" ]; then
      clear_screen; banner; printf '\n'
      note "  序列号 $key → 台账里换到完整密钥"
      key="$found"
    fi
  fi

  local out rc=0
  out=$("$TDL" license check "$key" --key-file "$ISSUER" 2>&1) || rc=$?
  if [ ! -t 1 ]; then
    # 非交互：原样输出，方便管道
    [ "$rc" -eq 0 ] && { printf '%s\n' "$out"; return 0; }
    printf '%s\n' "$out" >&2; return 1
  fi

  clear_screen; banner; printf '\n'
  if [ "$rc" -ne 0 ]; then
    warn "校验失败（退出码 $rc）"
    printf '\n'; printf '%s\n' "$out" | sed 's/^/    /'
  else
    ok "校验通过"
    printf '\n'; printf '%s\n' "$out" | sed 's/^/    /'
  fi

  if [ -f "$LEDGER" ]; then
    local row
    row=$(awk -F'\t' -v k="$key" 'NR>1 && $6==k { print $1 " · " $2 " · " $7 }' "$LEDGER" | head -1)
    [ -n "$row" ] && { printf '\n'; note "  台账记录：$row"; }
  fi
  printf '\n'
  pause_key
  return 0
}

# ────────────────────────────── 客户话术 ──────────────────────────────

build_message() {
  local key="$1" device="$2" term="$3" kind="$4" bind_line
  if [ "$device" = "any" ]; then
    bind_line="这份密钥在第一次激活时会自动绑定那台手机，之后不能换机使用。"
  else
    bind_line="这份密钥已绑定设备 ${device}，换手机需要重新签发。"
  fi
  printf '您的「TG归档」授权密钥：\n\n%s\n\n' "$key"
  printf '激活步骤：\n'
  printf '1. 打开 App，会停在激活页（页面上显示 8 位设备码）\n'
  printf '2. 把上面这串密钥完整复制粘贴进输入框\n'
  printf '3. 点「激活」\n\n'
  printf '有效期：%s。\n%s\n' "$term" "$bind_line"
  if [ "$kind" = "强" ]; then
    printf '注意：这串密钥比较长，务必整段复制，不要手打。\n'
  fi
  printf '\n激活遇到问题，把激活页上的设备码发给我。\n'
}

# ────────────────────────────── 设备码 ──────────────────────────────

normalize_device() {
  local d upper
  d=$(printf '%s' "$1" | tr -d ' ')
  upper=$(printf '%s' "$d" | tr '[:lower:]' '[:upper:]')
  # any 是关键字，不能被大写化，否则 device_valid 认不出来
  if [ "$upper" = "ANY" ]; then printf 'any'; return 0; fi
  printf '%s' "$upper"
}

device_valid() {
  local d="$1"
  [ "$d" = "any" ] && return 0
  [[ "$d" =~ ^[A-Z0-9]{8}$ ]]
}

# ────────────────────────────── 交互：签发 ──────────────────────────────

# 注意：这两个选择器不用 $(...) 返回值，改用全局变量。
# 因为它们的画面输出走 stdout，一旦被命令替换捕获，"--days" 会收到整屏文字。
PICK_DAYS=""; PICK_LABEL=""
PICK_KIND=""; PICK_KIND_LABEL=""

pick_term() {
  PICK_DAYS=""; PICK_LABEL=""
  clear_screen; banner
  printf '\n  %s有效期%s\n\n' "$C_BOLD" "$C_RESET"
  printf '    %s[1]%s 7 天      %s[2]%s 30 天     %s[3]%s 90 天\n' "$C_CYAN" "$C_RESET" "$C_CYAN" "$C_RESET" "$C_CYAN" "$C_RESET"
  printf '    %s[4]%s 180 天    %s[5]%s 365 天    %s[6]%s 永久\n' "$C_CYAN" "$C_RESET" "$C_CYAN" "$C_RESET" "$C_CYAN" "$C_RESET"
  printf '    %s[7]%s 自定义天数\n' "$C_CYAN" "$C_RESET"
  printf '\n'
  local sel n
  sel=$(ask "选 (1-7)" "2")
  case "$sel" in
    1) PICK_DAYS="7";    PICK_LABEL="7 天" ;;
    2) PICK_DAYS="30";   PICK_LABEL="30 天" ;;
    3) PICK_DAYS="90";   PICK_LABEL="90 天" ;;
    4) PICK_DAYS="180";  PICK_LABEL="180 天" ;;
    5) PICK_DAYS="365";  PICK_LABEL="365 天" ;;
    6) PICK_DAYS="forever"; PICK_LABEL="永久" ;;
    7)
      n=$(ask "天数")
      if ! [[ "$n" =~ ^[1-9][0-9]*$ ]]; then printf '\n'; warn "天数必须是正整数"; return 1; fi
      PICK_DAYS="$n"; PICK_LABEL="$n 天"
      ;;
    *) printf '\n'; warn "没这个选项"; return 1 ;;
  esac
  return 0
}

pick_kind() {
  PICK_KIND=""; PICK_KIND_LABEL=""
  clear_screen; banner
  printf '\n  %s密钥强度%s\n\n' "$C_BOLD" "$C_RESET"
  printf '    %s[1]%s 标准 · 40 字符 · 客户能手输 · 挡"随手编"和"改字符"\n' "$C_CYAN" "$C_RESET"
  printf '    %s[2]%s 强   · 127 字符 · 只能粘贴 · 从 APK 里挖不出校验串\n' "$C_CYAN" "$C_RESET"
  printf '\n'
  note "  高价值客户选 2；随手卖个 30 天选 1 就够。"
  printf '\n'
  local sel
  sel=$(ask "选 (1-2)" "1")
  case "$sel" in
    1) PICK_KIND="short";  PICK_KIND_LABEL="标准" ;;
    2) PICK_KIND="strong"; PICK_KIND_LABEL="强" ;;
    *) printf '\n'; warn "没这个选项"; return 1 ;;
  esac
  return 0
}

do_gen_interactive() {
  clear_screen; banner
  printf '\n  %s设备码%s\n\n' "$C_BOLD" "$C_RESET"
  note "  客户首次打开 App，激活页上显示 8 位大写字母数字，例如 CT84B4MD。"
  note "  填 any = 不预先绑定，客户第一次激活时自动绑到那台机器。"
  printf '\n'
  printf '    · 绑定设备码 → 换机即失效，最严，得先问客户要码\n'
  printf '    · any        → 好卖，不用问，但技术上换机更容易（要 root）\n'
  printf '\n'
  local device
  device=$(normalize_device "$(ask "设备码（回车 = any）" "any")")
  if ! device_valid "$device"; then
    printf '\n'; warn "设备码 \"$device\" 不是 8 位字母数字，也不是 any。"; pause_key; return 1
  fi

  local term_days term_label
  pick_term || { pause_key; return 1; }
  term_days="$PICK_DAYS"; term_label="$PICK_LABEL"

  local kind kind_label flag_strong=""
  pick_kind || { pause_key; return 1; }
  kind="$PICK_KIND"; kind_label="$PICK_KIND_LABEL"
  [ "$kind" = "strong" ] && flag_strong="--strong"

  local count
  count=$(ask "签发数量" "1")
  if ! [[ "$count" =~ ^[1-9][0-9]*$ ]] || [ "$count" -gt 100 ]; then
    printf '\n'; warn "数量 1-100"; pause_key; return 1
  fi

  local note_text
  note_text=$(ask "备注（只记本地台账，例如「张三 微信zhang3」）" "")

  # 确认页
  clear_screen; banner
  printf '\n  %s即将签发%s\n\n' "$C_BOLD" "$C_RESET"
  printf '    设备码    %s%s%s\n' "$C_CYAN" "$device" "$C_RESET"
  printf '    有效期    %s\n' "$term_label"
  printf '    强度      %s\n' "$kind_label"
  printf '    数量      %s\n' "$count"
  [ -n "$note_text" ] && printf '    备注      %s\n' "$note_text"
  printf '\n'
  local go
  go=$(ask "确认签发？(Y/n)" "y")
  case "$go" in
    y|Y|yes|YES|"") ;;
    *) printf '\n'; note "  已取消。"; sleep 0.5; return 0 ;;
  esac

  gen_and_report "$device" "$term_days" "$term_label" "$flag_strong" "$count" "$note_text" || { pause_key; return 1; }
  # 停一下再回主菜单：否则签名结果连同话术会被 clear_screen 立刻抹掉
  pause_key
  return 0
}

# 真正调用 gen + 落台账 + 打印结果。交互与无人值守共用。
gen_and_report() {
  local device="$1" term_days="$2" term_label="$3" flag_strong="$4" count="$5" note_text="$6"
  local args=(license gen --key-file "$ISSUER" --device "$device" --count "$count")
  if [ "$term_days" = "forever" ]; then
    args+=(--forever)
  else
    args+=(--days "$term_days")
  fi
  [ -n "$flag_strong" ] && args+=("$flag_strong")

  local raw rc=0
  raw=$("$TDL" "${args[@]}" 2>&1) || rc=$?
  if [ "$rc" -ne 0 ]; then
    clear_screen; banner; printf '\n'
    warn "签发失败（退出码 $rc）"
    printf '\n'; printf '%s\n' "$raw" | sed 's/^/    /'; printf '\n'
    case "$raw" in
      *"no such file"*|*"open "*|*"找不到"*) note "  主密钥可能在 $ISSUER 读不到。用主密钥管理 (5) 检查。" ;;
    esac
    return 1
  fi

  local parsed
  parsed=$(parse_gen_output "$raw")
  if [ -z "$parsed" ]; then
    clear_screen; banner; printf '\n'
    warn "gen 成功但输出无法解析，原文："
    printf '\n'; printf '%s\n' "$raw" | sed 's/^/    /'
    return 1
  fi

  local key serial rest first_key="" first_serial="" all_keys=""
  local server_kind="short" server_days="0"
  local admit_rc=0 admit_ok=0 admit_bad=0 arc
  [ -n "$flag_strong" ] && server_kind="strong"
  [ "$term_days" != "forever" ] && server_days="$term_days"
  while IFS=$'\t' read -r key serial rest; do
    [ -z "$key" ] && continue
    ledger_add "$device" "${kind_label:-$([ -n "$flag_strong" ] && echo 强 || echo 标准)}" "$term_label" "$serial" "$key" "$note_text"
    if [ -n "$LICENSE_SERVER" ]; then
      if admit_key "$key" "$server_kind" "$server_days" "$note_text"; then
        admit_ok=$((admit_ok+1))
      else
        arc=$?
        admit_rc="$arc"; admit_bad=$((admit_bad+1))
      fi
    fi
    if [ -z "$first_key" ]; then first_key="$key"; first_serial="$serial"; fi
    all_keys+="$key"$'\n'
  done <<< "$parsed"

  local kind_name="标准"
  [ -n "$flag_strong" ] && kind_name="强"

  clear_screen; banner
  printf '\n'
  ok "签发成功"
  printf '\n'
  while IFS=$'\t' read -r key serial rest; do
    [ -z "$key" ] && continue
    printf '  %s%s%s\n' "$C_BOLD$C_GREEN" "$key" "$C_RESET"
    printf '  %s· 序列号 %s · %s%s\n\n' "$C_DIM" "$serial" "$rest" "$C_RESET"
  done <<< "$parsed"

  local msg
  msg=$(build_message "$first_key" "$device" "$term_label" "$kind_name")

  printf '  %s────────────── 发给客户的话 ──────────────%s\n' "$C_GREY" "$C_RESET"
  printf '%s\n' "$msg" | sed 's/^/  /'
  printf '  %s──────────────────────────────────────────%s\n' "$C_GREY" "$C_RESET"
  printf '\n'

  # 云端登记结果。这一行决定客户到底能不能激活，不能只当脚注。
  local admit_ready=0
  if [ -z "$LICENSE_SERVER" ]; then
    note "离线模式（TDL_LICENSE_SERVER 为空）：App 走本地验签，不登记服务器"
    admit_ready=1
  elif [ "$admit_bad" -eq 0 ]; then
    ok "已登记到授权服务器（$admit_ok 把）—— 客户可以直接激活"
    admit_ready=1
  else
    printf '\n'
    case "$admit_rc" in
      2) warn "没有管理员 token，这把密钥没登记到服务器"
         note "  客户激活时会看到「密钥无效」。密钥本身没坏，是服务器不认识它。" ;;
      3) warn "连不上授权服务器，这把密钥没登记"
         note "  本地密钥有效，但云端模式下客户激活会失败。" ;;
      *) warn "授权服务器拒绝了登记"
         note "  ${ADMIT_ERROR:-未知原因}" ;;
    esac
    note "  补登记（不用重发密钥）：scripts/license-tui.sh --admit $first_serial"
    printf '\n'
  fi

  if [ "$admit_ready" -ne 1 ]; then
    printf '  %s密钥先别发：登记没成功，客户拿去也是提示「密钥无效」。%s\n\n' "$C_RED" "$C_RESET"
  elif [ "$count" = "1" ]; then
    if clip_copy "$first_key"; then ok "密钥已复制到剪贴板"
    else note "没找到剪贴板工具（wl-copy/xclip/xsel），手动复制吧。"; fi
  else
    if clip_copy "$all_keys"; then ok "$count 个密钥已复制到剪贴板（每行一个）"
    else note "没找到剪贴板工具，手动复制吧。"; fi
  fi
  note "已记入台账：序列号 $first_serial"
  return 0
}

# ────────────────────────────── 主密钥 / 设备码页 ──────────────────────────────

do_keymgmt() {
  clear_screen; banner
  printf '\n  %s主密钥%s  %s\n\n' "$C_BOLD" "$C_RESET" "$(issuer_status)"
  note "  主密钥是唯一需要保密的东西：它同时派生短密钥校验串和强密钥签名私钥。"
  note "  它只在你本机，不进仓库、不发客户、不打印内容。"
  printf '\n'
  printf '  %s[1]%s 生成一把新的（仅首次）\n' "$C_CYAN" "$C_RESET"
  printf '  %s[2]%s 体检（位置 / 权限 / 是否在仓库里）\n' "$C_CYAN" "$C_RESET"
  printf '  %s[q]%s 返回\n' "$C_CYAN" "$C_RESET"
  printf '\n  › '
  local sel; IFS= read -r sel || return 0
  case "$sel" in
    1)
      if issuer_ok; then
        printf '\n'
        warn "已经有一把了。重新生成会让**所有已发出的密钥全部失效**。"
        local c
        c=$(ask "确实要覆盖？输入 OVERWRITE")
        [ "$c" = "OVERWRITE" ] || { note "  已取消。"; sleep 0.6; return 0; }
        mv -- "$ISSUER" "$ISSUER.bak.$(date +%s)"
        note "  旧主密钥已备份为 $(basename "$ISSUER").bak.*"
      fi
      printf '\n'
      "$TDL" license keygen --out "$ISSUER" 2>&1 | sed 's/^/    /' || { warn "keygen 失败"; pause_key; return 0; }
      chmod 600 "$ISSUER" 2>/dev/null || true
      printf '\n'; ok "主密钥已写入 ${ISSUER/#$HOME/~}（权限 600）"
      warn "立刻离线备份它。丢了签不出续期密钥，已发出的密钥仍可用。"
      pause_key
      ;;
    2)
      printf '\n'
      printf '  文件位置   %s\n' "${ISSUER/#$HOME/~}"
      printf '  权限       %s\n' "$(stat -c '%a' "$ISSUER" 2>/dev/null || echo '?')"
      printf '  大小       %s 字节\n' "$(stat -c '%s' "$ISSUER" 2>/dev/null || echo '?')"
      local ingit="否，安全"
      case "$ISSUER" in "$PROJECT_DIR"/*) ingit="⚠ 在仓库里，立刻移出去" ;; esac
      printf '  是否在仓库内  %s\n' "$ingit"
      printf '\n'
      note "  备份方式：抄进密码管理器，或者拷到离线 U 盘。"
      note "  不要放进 git、不要贴进聊天记录、不要截图。"
      pause_key
      ;;
  esac
  return 0
}

do_device_hint() {
  clear_screen; banner
  printf '\n  %s客户从哪拿设备码%s\n\n' "$C_BOLD" "$C_RESET"
  printf '  1. 客户装好 APK，第一次打开会停在 %s激活页%s\n' "$C_BOLD" "$C_RESET"
  printf '  2. 页面上有一行「设备码  XXXX-XXXX」（8 位大写字母数字）\n'
  printf '  3. 让他把这 8 位发给你\n'
  printf '  4. 你在 (1) 里填进去，签发，把密钥发回去\n'
  printf '\n'
  hr
  printf '  %s本机设备码%s（你自己这台，不是客户的）\n\n' "$C_BOLD" "$C_RESET"
  "$TDL" license device 2>&1 | sed 's/^/    /' || warn "读取失败"
  printf '\n'
  hr
  printf '  %s绑定 vs 不绑定%s\n\n' "$C_BOLD" "$C_RESET"
  printf '  %-10s %-32s %s\n' "模式" "换机" "适合"
  printf '  %-10s %-32s %s\n' "设备码" "签名里绑定，换机即失效" "能联系上的客户"
  printf '  %-10s %-32s %s\n' "any" "首激绑定，root 可改文件绕过" "批量卖、懒得问"
  printf '\n'
  pause_key
  return 0
}

# ────────────────────────────── 主菜单 ──────────────────────────────

main_menu() {
  while true; do
    clear_screen; banner
    printf '\n'
    printf '  主密钥  %s\n' "$(issuer_status)"
    local badge; badge=$(pending_badge)
    if [ -n "$badge" ]; then
      printf '  台账    %s 条 %s%s%s   %s\n' "$(ledger_count)" "$C_GREY" "(${LEDGER/#$HOME/~})" "$C_RESET" "$badge"
    else
      printf '  台账    %s 条 %s%s%s\n' "$(ledger_count)" "$C_GREY" "(${LEDGER/#$HOME/~})" "$C_RESET"
    fi
    printf '\n'
    hr
    printf '\n'
    printf '   %s[1]%s  签发新密钥\n' "$C_CYAN$C_BOLD" "$C_RESET"
    printf '   %s[2]%s  校验密钥 / 查序列号\n' "$C_CYAN$C_BOLD" "$C_RESET"
    printf '   %s[3]%s  签发台账\n' "$C_CYAN$C_BOLD" "$C_RESET"
    printf '   %s[4]%s  设备码怎么拿（客户指引）\n' "$C_CYAN$C_BOLD" "$C_RESET"
    printf '   %s[5]%s  主密钥管理\n' "$C_CYAN$C_BOLD" "$C_RESET"
    printf '   %s[6]%s  检查云端登记（谁还没生效）\n' "$C_CYAN$C_BOLD" "$C_RESET"
    printf '\n'
    printf '   %s[q]%s  退出\n' "$C_GREY" "$C_RESET"
    printf '\n'
    hr
    printf '  › '
    local sel; IFS= read -r sel || { printf '\n'; return 0; }
    case "$sel" in
      1) do_gen_interactive || true ;;
      2) do_check || true ;;
      3) ledger_show || true ;;
      4) do_device_hint || true ;;
      5) do_keymgmt || true ;;
      6) do_pending || true; pending_probe_bg; pause_key ;;
      q|Q) return 0 ;;
      *) ;;
    esac
  done
}

# ────────────────────────────── 参数 ──────────────────────────────

usage() {
  cat <<'EOF'
TG归档 · 商业授权签发台

用法：
  license-tui.sh                        交互式菜单
  license-tui.sh --gen [选项]           直接签发（无人值守）
  license-tui.sh --check <密钥|序列号>  校验
  license-tui.sh --ledger               打印台账（纯文本 TSV，含完整密钥）
  license-tui.sh --show <序列号>        取回完整密钥 + 客户话术
  license-tui.sh --device               打印本机设备码
  license-tui.sh --help                 本页

签发选项（配合 --gen）：
  --device <8位|any>   设备码，默认 any
  --days <N>           有效天数
  --forever            永久
  --strong             强密钥（127 字符，只能粘贴）
  --count <N>          数量，默认 1
  --note <文本>        本地台账备注

环境变量：
  TDL_ISSUER  主密钥路径（默认 ~/.tdl-issuer.txt）
  TDL_LEDGER  台账路径（默认 ~/.tdl-license-issued.tsv）
  TDL_BIN     tdl 二进制路径

例子：
  license-tui.sh --gen --device 7K3M9QX2 --days 30
  license-tui.sh --gen --device any --forever --strong --note "张三"
  license-tui.sh --check MF1P1RE7-GSK5NNHJ-XM67AH0S-MSVRGNAE-JBYC4CWR
  license-tui.sh --admit B7BHKHMR              补登记到授权服务器（序列号/设备码/密钥都认）
  license-tui.sh --pending                     列出"签了但服务器不认识"的密钥
EOF
}

MODE=""; CHECK_KEY=""; SHOW_SERIAL=""; ADMIT_Q=""; UA_DEVICE=""; UA_DAYS=""; UA_FOREVER=0
UA_STRONG=0; UA_COUNT=""; UA_NOTE=""

# 先看有没有 --gen，决定 --device 是"签发参数"还是"打印设备码"
for a in "$@"; do [ "$a" = "--gen" ] && MODE="gen" && break; done

while [ $# -gt 0 ]; do
  case "$1" in
    --gen) shift ;;
    --check)
      MODE="check"; CHECK_KEY="${2:-}"
      if [ $# -ge 2 ]; then shift 2; else shift; fi ;;
    --check=*) MODE="check"; CHECK_KEY="${1#*=}"; shift ;;
    --ledger) MODE="ledger"; shift ;;
    --show)
      MODE="show"; SHOW_SERIAL="${2:-}"
      if [ $# -ge 2 ]; then shift 2; else shift; fi ;;
    --show=*) MODE="show"; SHOW_SERIAL="${1#*=}"; shift ;;
    --admit)
      MODE="admit"; ADMIT_Q="${2:-}"
      if [ $# -ge 2 ]; then shift 2; else shift; fi ;;
    --admit=*) MODE="admit"; ADMIT_Q="${1#*=}"; shift ;;
    --pending) MODE="pending"; shift ;;
    --device)
      if [ "$MODE" = "gen" ]; then UA_DEVICE="${2:-}"; shift 2; else MODE="device"; shift; fi ;;
    --device=*) UA_DEVICE="${1#*=}"; shift ;;
    --days) UA_DAYS="${2:-}"; shift 2 ;;
    --days=*) UA_DAYS="${1#*=}"; shift ;;
    --forever) UA_FOREVER=1; shift ;;
    --strong) UA_STRONG=1; shift ;;
    --count) UA_COUNT="${2:-1}"; shift 2 ;;
    --count=*) UA_COUNT="${1#*=}"; shift ;;
    --note) UA_NOTE="${2:-}"; shift 2 ;;
    --note=*) UA_NOTE="${1#*=}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "未知参数：$1（--help 看用法）" ;;
  esac
done

TDL=$(find_tdl)

# ────────────────────────────── 入口 ──────────────────────────────

case "$MODE" in
  ledger)
    if [ ! -f "$LEDGER" ]; then echo "台账为空（$LEDGER 不存在）"; exit 0; fi
    cat "$LEDGER"
    exit 0
    ;;
  show)
    [ -n "$SHOW_SERIAL" ] || die "--show 需要序列号（先跑 --ledger 看有哪些）"
    ledger_view "$SHOW_SERIAL" raw
    exit $?
    ;;
  device)
    "$TDL" license device
    exit 0
    ;;
  check)
    do_check "${CHECK_KEY:-}"
    exit $?
    ;;
  admit)
    [ -n "$ADMIT_Q" ] || die "--admit 需要序列号 / 设备码 / 密钥（先跑 --ledger 看有哪些）"
    do_admit "$ADMIT_Q"
    exit $?
    ;;
  pending)
    do_pending
    exit $?
    ;;
  gen)
    issuer_ok || die "主密钥缺失：$ISSUER（先跑一次交互模式生成）"
    UA_DEVICE=$(normalize_device "${UA_DEVICE:-any}")
    device_valid "$UA_DEVICE" || die "设备码格式不对：$UA_DEVICE（8 位字母数字或 any）"
    if [ -z "$UA_DAYS" ] && [ "$UA_FOREVER" != "1" ]; then
      die "要么给 --days N，要么给 --forever"
    fi
    if [ -n "$UA_DAYS" ] && ! [[ "$UA_DAYS" =~ ^[1-9][0-9]*$ ]]; then
      die "--days 必须是正整数（收到：$UA_DAYS）"
    fi
    local_count="${UA_COUNT:-1}"
    [[ "$local_count" =~ ^[1-9][0-9]*$ ]] || die "--count 必须是正整数"
    flag_strong=""
    [ "$UA_STRONG" = "1" ] && flag_strong="--strong"
    term_days="$UA_DAYS"; term_label="${UA_DAYS} 天"
    if [ "$UA_FOREVER" = "1" ]; then term_days="forever"; term_label="永久"; fi
    gen_and_report "$UA_DEVICE" "$term_days" "$term_label" "$flag_strong" "$local_count" "$UA_NOTE"
    exit 0
    ;;
  *)
    if ! issuer_ok; then
      clear_screen; banner
      printf '\n'; warn "主密钥还没生成：$ISSUER"
      printf '\n'
      note "  这是签发密钥的唯一凭据，只需要生成一次。"
      note "  它会写进 ${ISSUER/#$HOME/~}（权限 600），之后所有签发都用它。"
      printf '\n'
      first_go=$(ask "现在生成？(Y/n)" "y")
      case "$first_go" in
        y|Y|"")
          "$TDL" license keygen --out "$ISSUER" 2>&1 | sed 's/^/    /' || die "keygen 失败"
          chmod 600 "$ISSUER" 2>/dev/null || true
          printf '\n'; ok "已生成，权限已设为 600"
          warn "立刻离线备份 —— 丢了签不出续期密钥。"
          pause_key
          ;;
        *) die "没有主密钥无法签发" ;;
      esac
    fi
    pending_probe_bg
    trap 'rm -f "$PENDING_CACHE"' EXIT
    main_menu
    ;;
esac
