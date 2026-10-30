#!/usr/bin/env bash
#
# 构建 Windows 分发包
#
#   桌面版 GUI  +  命令行/归档面板  +  使用说明  →  一个 zip
#
# 用法:
#   ./hack/build-windows.sh              # 版本号取 git describe
#   ./hack/build-windows.sh 2.1.0        # 指定版本号
#
# 依赖:
#   sudo apt install gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64 zip
#
# 产物:
#   dist/windows-amd64/Telegram下载器-Windows-x64-<版本>.zip
#   dist/windows-amd64/Telegram下载器-Windows-x64-<版本>.zip.sha256

set -euo pipefail

source ./hack/lib.sh

VERSION="${1:-}"
if [ -z "${VERSION}" ]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
fi

# `tdl version` prints Commit/CommitDate too; inject them so a user bug report
# pins to a revision instead of "unknown". Same three values the Dockerfile uses.
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
COMMIT_DATE="$(git log -1 --format=%cI 2>/dev/null || echo unknown)"

APP_NAME="Telegram下载器"
PKG_NAME="${APP_NAME}-Windows-x64"
OUT_DIR="dist/windows-amd64"
STAGE="${OUT_DIR}/${PKG_NAME}"
ZIP_PATH="${OUT_DIR}/${PKG_NAME}-${VERSION}.zip"
ASSETS="deploy/windows"

log_info "==> 构建 Windows 版本: ${VERSION}"

# ---------------------------------------------------------------- 依赖检查
for bin in x86_64-w64-mingw32-gcc x86_64-w64-mingw32-objdump iconv python3; do
  command -v "${bin}" >/dev/null 2>&1 || {
    log_error "缺少 ${bin}。安装: sudo apt install gcc-mingw-w64-x86-64 g++-mingw-w64-x86-64"
    exit 1
  }
done

[ -d "${ASSETS}" ] || { log_error "找不到打包资源目录 ${ASSETS}"; exit 1; }

rm -rf "${STAGE}" "${ZIP_PATH}" "${ZIP_PATH}.sha256"
mkdir -p "${STAGE}"

# ------------------------------------------------- 桌面版（Fyne，需要 CGO）
# 入口必须是 ./desktop 而不是 . —— 根入口在没有参数时会打印 CLI 帮助就退出，
# 双击根本进不了界面。-H windowsgui 去掉黑色控制台窗口。
log_info "==> 编译桌面版 ${APP_NAME}.exe（CGO + mingw，较慢）"
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
  CC=x86_64-w64-mingw32-gcc CXX=x86_64-w64-mingw32-g++ \
  go build -trimpath \
    -ldflags "-s -w -H windowsgui -X github.com/iyear/tdl/app/desktop.Version=${VERSION}" \
    -o "${STAGE}/${APP_NAME}.exe" ./desktop

# ---------------------------------------- CLI + 归档面板（纯静态，不需要 CGO）
log_info "==> 编译 tdl.exe（CGO_ENABLED=0，纯静态）"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath \
    -ldflags "-s -w -X github.com/iyear/tdl/pkg/consts.Version=${VERSION} -X github.com/iyear/tdl/pkg/consts.Commit=${COMMIT} -X github.com/iyear/tdl/pkg/consts.CommitDate=${COMMIT_DATE}" \
    -o "${STAGE}/tdl.exe" .

# ------------------------------------------------------------------ 资源文件
log_info "==> 复制说明文档与配置"
cp "${ASSETS}"/*.html "${ASSETS}"/*.md "${ASSETS}"/*.txt "${ASSETS}"/config.yaml "${STAGE}/"

# .bat 必须是 GBK + CRLF。UTF-8 的 bat 在 cmd.exe 里中文会乱码，
# 严重时连命令都解析错（cmd 按当前代码页逐字节读取）。
for f in "${ASSETS}"/*.bat; do
  base="$(basename "${f}")"
  sed 's/$/\r/' "${f}" | iconv -f UTF-8 -t GBK > "${STAGE}/${base}"
done
log_succ "    bat 已转为 GBK + CRLF"

# ------------------------------------------------------------ 导入表校验
# 这是最容易翻车的地方：mingw 默认动态链接 gcc 运行时，
# 对方机器没装就会弹「找不到 libgcc_s_seh-1.dll」。必须在出厂前拦住。
log_info "==> 校验 DLL 依赖"
for exe in "${STAGE}/${APP_NAME}.exe" "${STAGE}/tdl.exe"; do
  if x86_64-w64-mingw32-objdump -p "${exe}" 2>/dev/null \
      | grep -qiE "DLL Name: (libgcc|libstdc|libwinpthread)"; then
    log_error "$(basename "${exe}") 依赖 mingw 运行时 DLL —— 用户机器上会缺库"
    exit 1
  fi
  # 只允许系统 DLL
  bad="$(x86_64-w64-mingw32-objdump -p "${exe}" 2>/dev/null \
      | awk '/DLL Name:/ {print $3}' \
      | grep -viE '^(api-ms-win-|kernel32|user32|gdi32|opengl32|shell32|advapi32|ws2_32|ole32|oleaut32|comdlg32|winmm|imm32|dwmapi|uxtheme|bcrypt|crypt32|netapi32|iphlpapi|secur32|shlwapi|version|winspool|mpr|wtsapi32|psapi|ntdll)' \
      | sort -u || true)"
  if [ -n "${bad}" ]; then
    log_warn "$(basename "${exe}") 有非系统 DLL: ${bad}"
  fi
done
log_succ "    导入表干净：只依赖系统 DLL"

# -------------------------------------------------------------------- 打包
log_info "==> 打包 zip（UTF-8 文件名标记，Windows 解压不乱码）"
# 注意 arcname：zip 内只能有「包名/文件」两层。
# 直接用 p.as_posix() 会把 dist/windows-amd64/... 这段构建路径一起塞进去，
# 用户解压后得点进三层目录才看到 exe。
python3 - "${STAGE}" "${ZIP_PATH}" "${PKG_NAME}" <<'PY'
import sys, zipfile, pathlib

stage = pathlib.Path(sys.argv[1])
out = pathlib.Path(sys.argv[2])
pkg = sys.argv[3]

files = sorted(p for p in stage.rglob("*") if p.is_file())
with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
    for p in files:
        arc = pathlib.PurePosixPath(pkg) / pathlib.PurePosixPath(p.relative_to(stage).as_posix())
        z.write(p, arc.as_posix())

with zipfile.ZipFile(out) as z:
    assert z.testzip() is None, "zip CRC 校验失败"
    tops = {i.filename.split("/")[0] for i in z.infolist()}
    assert tops == {pkg}, f"zip 根目录应只有 {pkg}，实际 {tops}"
    for i in z.infolist():
        assert i.flag_bits & 0x800, f"文件名缺少 UTF-8 标记: {i.filename}"
print(f"    {len(files)} 个文件, {out.stat().st_size:,} 字节, 根目录 {pkg}/")
PY

sha256sum "${ZIP_PATH}" | sed "s|  .*/|  |" > "${ZIP_PATH}.sha256"

log_succ ""
log_succ "==> 完成"
log_succ "    ${ZIP_PATH}"
log_succ "    $(cat "${ZIP_PATH}.sha256")"
