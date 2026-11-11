#!/usr/bin/env bash
# 把 docs/用户手册.md 渲染成可以直接发给客户的 PDF。
#
# 为什么要有这个脚本：交付给客户的文档必须是 PDF——Markdown 在微信/QQ 里
# 打开是一堆井号和竖线，客户第一眼就认为这东西不专业。手点一遍 pandoc 谁都会，
# 但下次改了手册没人记得住当时的 CSS 和字体，所以参数固化在这里。
#
# 依赖：pandoc + weasyprint + 思源黑体/ Maple Mono NF CN（含中文的等宽字体）。
#   - 正文 Noto Sans CJK SC：无衬线、屏幕与打印都清楚
#   - 代码块 Maple Mono NF CN：等宽且含中文，手册里的 ASCII 示意图靠它才不会错位
#
# 用法：tools/gen-user-manual-pdf.sh [输出路径]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/docs/用户手册.md"
OUT="${1:-$ROOT/dist/docs/TG归档-用户手册.pdf}"

[[ -f "$SRC" ]] || { echo "缺少源文件: $SRC" >&2; exit 1; }
mkdir -p "$(dirname "$OUT")"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# 标题取第一行 "# xxx"，失败就退回默认名
TITLE="$(sed -n '1s/^#[[:space:]]*//p' "$SRC")"
[[ -n "$TITLE" ]] || TITLE="用户手册"
# 版本号必须和客户手上那个 APK 一致，否则封面就是在骗人。
# 取最新构建的 APK 文件名里的版本（dist/android/TG归档-Android-arm64-2.8.0-cloud.apk → 2.8.0）；
# 没用 git describe：App 版本来自 dist 里的发行包，而 tag 只在发版时打，
# 两者不一定同时更新（历史上 tag 停在 2.5.0，App 已经发到 2.8.3）。
# 想覆盖时用 MANUAL_VERSION=2.8.1 tools/gen-user-manual-pdf.sh
VERSION="${MANUAL_VERSION:-}"
if [[ -z "$VERSION" ]]; then
  VERSION="$(ls -1 "$ROOT"/dist/android/*.apk 2>/dev/null     | sed -n 's/.*-\([0-9]\+\.[0-9]\+\.[0-9]\+\)[^0-9]*\.apk$/\1/p'     | sort -t. -k1,1n -k2,2n -k3,3n | tail -1 || true)"
fi
[[ -n "$VERSION" ]] || VERSION="1.0"
TODAY="$(date +%Y-%m-%d)"

# 正文：去掉首个 h1（封面已经有文档名，正文再出现一次就是重复），
# 并且用 -s 让 pandoc 真的产出 <nav id="TOC">——非 standalone 模式下 --toc 是空转，
# 第一版就这么交付过一份没有目录的 PDF。
sed '1{/^#[[:space:]]/d}' "$SRC" > "$TMP/src.md"

pandoc "$TMP/src.md" \
  --from gfm+pipe_tables \
  --to html5 \
  --standalone \
  --toc --toc-depth=3 \
  --wrap=none \
  --metadata title="$TITLE" \
  -o "$TMP/full.html"

cat >"$TMP/doc.html" <<HTML
<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>${TITLE}</title>
<style>
@page {
  size: A4;
  margin: 20mm 18mm 18mm;
  @bottom-center { content: "第 " counter(page) " 页 / 共 " counter(pages) " 页";
                   font-family: "Noto Sans CJK SC"; font-size: 8.5pt; color: #8b97a8; }
  @top-right { content: "${TITLE}"; font-family: "Noto Sans CJK SC";
               font-size: 8pt; color: #b6c0cd; }
}
@page cover { margin: 0; @bottom-center { content: none } @top-right { content: none } }

html { font-family: "Noto Sans CJK SC", "Source Han Sans SC", sans-serif;
       font-size: 10.5pt; color: #1c2430; line-height: 1.72; }
body { margin: 0; }

/* ── 封面 ── */
.cover { page: cover; height: 297mm; box-sizing: border-box;
         padding: 62mm 22mm 24mm; background: #0d1117; color: #eef3f9;
         position: relative; page-break-after: always; }
.cover .bar { position: absolute; left: 0; top: 0; width: 100%; height: 6mm;
              background: #2AABEE; }
.cover h1 { font-size: 30pt; line-height: 1.25; margin: 0 0 6mm; color: #fff;
            font-weight: 700; letter-spacing: .5pt; }
.cover .sub { font-size: 12.5pt; color: #9fb0c4; margin: 0 0 16mm; line-height: 1.9; }
.cover .pitch { font-size: 11pt; color: #cfe0f0; border-left: 3pt solid #2AABEE;
                padding-left: 6mm; margin: 0 0 20mm; line-height: 1.9; }
.cover .toc-hint { margin: 0 0 22mm; font-size: 9.6pt; color: #a9bdd2; }
.cover .toc-hint-title { color: #4d8fc0; margin-bottom: 4mm; }
.cover .toc-hint ol { margin: 0; padding-left: 5.5mm; }
.cover .toc-hint li { margin: 2.6mm 0; line-height: 1.65; }
.cover .toc-hint b { color: #dceaf7; font-weight: 600; }
.cover .meta { position: absolute; left: 22mm; bottom: 24mm; font-size: 9pt;
               color: #7d8da1; line-height: 2; }
.cover .meta b { color: #cfe0f0; font-weight: 500; }

/* ── 目录 ── */
nav#TOC { page-break-after: always; }
nav#TOC > ul { list-style: none; padding-left: 0; }
nav#TOC ul ul { list-style: none; padding-left: 8mm; }
nav#TOC li { margin: 2.4mm 0; font-size: 10pt; }
nav#TOC a { color: #1c2430; text-decoration: none; }
nav#TOC > ul > li > a { font-weight: 600; color: #0f1620; font-size: 11pt; }
/* 目录带页码与引导点：纸质文档没有页码就没法引用（"见第 5 页"），
   而客户打电话问问题时会这么说。WeasyPrint 用 target-counter 从锚点反查页码。 */
nav#TOC a::after { content: " " leader('.') " " target-counter(attr(href), page);
                   color: #7b8b9e; font-weight: 400; font-size: 9pt; }
nav#TOC ul ul li { font-size: 9.6pt; margin: 1.8mm 0; }
nav#TOC ul ul a { color: #48586c; }
nav#TOC::before { content: "目 录"; display: block; font-size: 17pt; font-weight: 700;
                  color: #0f1620; margin: 0 0 9mm; padding-bottom: 3mm;
                  border-bottom: 2pt solid #2AABEE; }

/* ── 正文 ── */
h1 { font-size: 19pt; margin: 0 0 7mm; padding-bottom: 3mm; color: #0f1620;
     border-bottom: 2pt solid #2AABEE; page-break-after: avoid; }
h2 { font-size: 14.5pt; margin: 9mm 0 3.5mm; color: #0f1620; page-break-after: avoid;
     border-left: 3pt solid #2AABEE; padding-left: 4mm; }
h3 { font-size: 11.5pt; margin: 6mm 0 2.5mm; color: #1a6ea8; page-break-after: avoid; }
p { margin: 0 0 2.6mm; }
strong { color: #0f1620; font-weight: 700; }
hr { border: none; border-top: 1px solid #dde4ec; margin: 7mm 0; }
a { color: #1a6ea8; text-decoration: none; }

ul, ol { margin: 0 0 3mm; padding-left: 7mm; }
li { margin: 1.4mm 0; }

blockquote { margin: 4mm 0; padding: 3mm 5mm; background: #f0f7fd;
             border-left: 3pt solid #2AABEE; color: #33465c; font-size: 10pt; }
blockquote p { margin: 0 0 1.5mm; }
blockquote p:last-child { margin-bottom: 0; }

code { font-family: "Maple Mono NF CN", "Noto Sans Mono CJK SC", monospace;
       font-size: 9pt; background: #eef2f7; padding: 0.4mm 1.4mm;
       border-radius: 1mm; color: #b02a3c; }
/* 长代码块允许跨页：死守 page-break-inside:avoid 会把一个 30 行的示意图整块
   推到下一页，上一页只剩半页空白。宁可截断，也不要那种排版事故。 */
pre { background: #f5f7fa; border: 1px solid #e1e7ef; border-radius: 1.6mm;
      padding: 3.5mm 4mm; margin: 3mm 0; page-break-inside: auto; overflow: visible; }
pre code { background: none; padding: 0; color: #24313f; font-size: 8.6pt;
           line-height: 1.55; white-space: pre; }

table { border-collapse: collapse; width: 100%; margin: 3.5mm 0 4.5mm;
        font-size: 9.2pt; page-break-inside: avoid; }
thead th { background: #0d1117; color: #fff; font-weight: 600; text-align: left;
           padding: 2mm 2.6mm; }
tbody td { padding: 2mm 2.6mm; border-bottom: 1px solid #e4eaf1; vertical-align: top; }
tbody tr:nth-child(even) { background: #f7f9fc; }
td code, th code { background: #e8eef6; color: #1a6ea8; }

/* 表格里出现长中文不要让浏览器把词撕开 */
td, th { word-break: break-word; }

/* 收尾：最后一段不要贴着页脚 */
body > *:last-child { margin-bottom: 0; }
</style></head>
<body>
<div class="cover">
  <div class="bar"></div>
  <h1>${TITLE}</h1>
  <p class="sub">Telegram Archive · 本地留存客户端</p>
  <p class="pitch">对方撤回、管理员删除、频道清空——<br>保存在你手机里的那一份，不受影响。</p>
  <div class="toc-hint">
    <div class="toc-hint-title">本手册讲这四件事</div>
    <ol>
      <li><b>它替你留住什么</b>——以及哪些情况留不住</li>
      <li><b>撤回和删除为什么对它无效</b>——原理与你能自证的证据</li>
      <li><b>从安装到上手</b>——激活、登录、添加监控源</li>
      <li><b>出问题怎么反馈</b>——三样东西省掉一半沟通</li>
    </ol>
  </div>
  <div class="meta">
    版本 <b>${VERSION}</b>　·　生成日期 <b>${TODAY}</b><br>
    本文档随软件一同交付，请妥善保存
  </div>
</div>
BODY_PLACEHOLDER
</body></html>
HTML

# 从 pandoc 的完整输出里取出 <nav id="TOC"> 与 <body> 内容，塞进我们的模板。
# 不直接用 pandoc 的默认模板：它的字体、分页、表格样式都不是我们要的。
python3 - "$TMP/doc.html" "$TMP/full.html" <<'PY'
import sys, pathlib, re
doc  = pathlib.Path(sys.argv[1]).read_text(encoding="utf-8")
full = pathlib.Path(sys.argv[2]).read_text(encoding="utf-8")

toc = re.search(r'<nav id="TOC".*?</nav>', full, re.S)
toc = toc.group(0) if toc else ""

body = re.search(r'<body[^>]*>(.*)</body>', full, re.S)
body = body.group(1) if body else full
body = re.sub(r'<header id="title-block-header">.*?</header>', '', body, flags=re.S)
body = body.replace(toc, "")          # TOC 已单独提到前面
body = re.sub(r'<nav id="TOC".*?</nav>', '', body, flags=re.S)

doc = doc.replace('BODY_PLACEHOLDER', toc + "\n" + body)
pathlib.Path(sys.argv[1]).write_text(doc, encoding="utf-8")
PY

weasyprint "$TMP/doc.html" "$OUT" 2>/dev/null

echo "PDF: $OUT"
ls -l "$OUT"
