#!/usr/bin/env bash
#
# 构建 Linux 分发包（自托管部署用）
#
#   单个静态二进制  +  部署脚本  +  使用说明  →  一个 tar.gz
#
# 用法:
#   ./hack/build-linux.sh              # 版本号取 git describe
#   ./hack/build-linux.sh 2.1.0
#
# 产物:
#   dist/linux-amd64/Telegram下载器-Linux-x64-<版本>.tar.gz
#   dist/linux-amd64/Telegram下载器-Linux-x64-<版本>.tar.gz.sha256
#
# 为什么不需要目标机装任何东西:
#   sqlite 驱动是 modernc.org/sqlite（纯 Go 实现），所以 CGO_ENABLED=0 能
#   编出完全静态的 ELF。目标机只要有 glibc 甚至不要 glibc —— 内核能跑 ELF
#   就行，不需要 node / docker / gcc / nginx。

set -euo pipefail

source ./hack/lib.sh

VERSION="${1:-}"
if [ -z "${VERSION}" ]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

# `tdl version` prints Commit/CommitDate too. Leaving them as the "unknown"
# default makes a user bug report impossible to pin to a revision, so inject all
# three the same way the Dockerfile does.
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
COMMIT_DATE="$(git log -1 --format=%cI 2>/dev/null || echo unknown)"

PKG_NAME="Telegram下载器-Linux-x64"
OUT_DIR="dist/linux-amd64"
STAGE="${OUT_DIR}/${PKG_NAME}"
TAR_PATH="${OUT_DIR}/${PKG_NAME}-${VERSION}.tar.gz"
ASSETS="deploy/linux"

log_info "==> 构建 Linux 版本: ${VERSION}"

# ---------------------------------------------------------------- 依赖检查
command -v go >/dev/null 2>&1 || { log_error "缺少 go"; exit 1; }
command -v file >/dev/null 2>&1 || { log_error "缺少 file（用于校验静态链接）"; exit 1; }

rm -rf "${STAGE}"
mkdir -p "${STAGE}"

# ---------------------------------------------------------------- 编译
log_info "编译 linux/amd64（CGO 关闭 → 完全静态）"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X github.com/iyear/tdl/pkg/consts.Version=${VERSION} -X github.com/iyear/tdl/pkg/consts.Commit=${COMMIT} -X github.com/iyear/tdl/pkg/consts.CommitDate=${COMMIT_DATE}" \
  -o "${STAGE}/tdl" .

if [ ! -f "${STAGE}/tdl" ]; then
  log_error "编译产物缺失"
  exit 1
fi

# ---------------------------------------------------------------- 静态性校验
# 动态链接的二进制到了目标机可能报 "no such file or directory" 或缺 libgcc，
# 这两种失败在部署现场极难判断，所以在构建期就拦掉。
# 用变量接住输出而不是 `file | grep -q`：grep 匹配到即退出会让 file 吃到
# SIGPIPE，在 pipefail 下把成功判成失败。
fdesc="$(file "${STAGE}/tdl")"
case "${fdesc}" in
  *"statically linked"*) : ;;
  *)
    log_error "产物不是静态链接，目标机可能无法运行："
    printf '%s\n' "${fdesc}"
    exit 1
    ;;
esac
if command -v ldd >/dev/null 2>&1; then
  lddesc="$(ldd "${STAGE}/tdl" 2>&1 || true)"
  case "${lddesc}" in
    *libgcc*|*libstdc++*|*libpthread*|*"not found"*)
      log_error "产物带动态依赖："
      printf '%s\n' "${lddesc}"
      exit 1
      ;;
  esac
fi
log_succ "静态链接校验通过（$(stat -c%s "${STAGE}/tdl" | numfmt --to=iec)）"

# ---------------------------------------------------------------- 组装
cp "${ASSETS}/install.sh" "${STAGE}/install.sh"
cp "${ASSETS}/README.md" "${STAGE}/使用说明.md"
cp "${ASSETS}/config.template.yaml" "${STAGE}/config.template.yaml"
cp "${ASSETS}/tdl-serve.service" "${STAGE}/tdl-serve.service.template"

chmod +x "${STAGE}/tdl" "${STAGE}/install.sh"

# 行尾与可执行位在 Windows 解压/传输后常被破坏，这里固化成 LF。
STAGE="${STAGE}" python3 - <<'PY'
import os, pathlib
stage = pathlib.Path(os.environ["STAGE"])
n = 0
for p in stage.rglob("*.sh"):
    b = p.read_bytes()
    fixed = b.replace(b"\r\n", b"\n")
    if fixed != b:
        p.write_bytes(fixed)
        n += 1
print(f"normalised {n} shell script(s) to LF")
PY

# ---------------------------------------------------------------- 打包
log_info "打包 ${TAR_PATH}"
# -C 到父目录并用包名做根，避免解压后多出 dist/linux-amd64 三层
tar -czf "${TAR_PATH}" -C "${OUT_DIR}" "${PKG_NAME}"

tops="$(tar -tzf "${TAR_PATH}" | awk -F/ '{print $1}' | sort -u)"
if [ "${tops}" != "${PKG_NAME}" ]; then
  log_error "tar 根目录不对：${tops}"
  exit 1
fi

( cd "${OUT_DIR}" && sha256sum "$(basename "${TAR_PATH}")" > "$(basename "${TAR_PATH}").sha256" )

log_succ "完成"
echo
echo "  包     : ${TAR_PATH}"
echo "  大小   : $(du -h "${TAR_PATH}" | cut -f1)"
echo "  sha256 : $(cut -d' ' -f1 "${TAR_PATH}.sha256")"
echo
echo "  目标机（无需 root、无需装任何依赖）:"
echo "    tar -xzf $(basename "${TAR_PATH}")"
echo "    cd ${PKG_NAME} && ./install.sh"
