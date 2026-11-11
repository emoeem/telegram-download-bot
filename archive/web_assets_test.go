package archive

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 手机 WebView 里 UI 错位的一个根因：shell 资源没有 Cache-Control。
// theme.css 承载全部移动端布局（@media(max-width:650px) 里的 .mobile-nav
// 固定定位、.shell 单列网格、.feed 底部内边距），曾是唯一一个手写 handler、
// 只设 Content-Type 的资源。缺缓存头时 WebView 走启发式缓存，App 升级后
// index.html 是新的而 theme.css 还是旧的，两者对不上就是界面莫名错位。
func TestShellAssetsDeclareCachePolicy(t *testing.T) {
	ts := httptest.NewServer(NewWebServer(nil).Handler())
	defer ts.Close()

	for _, p := range []string{
		"/static/theme.css",
		"/static/app.css",
		"/manifest.webmanifest",
		"/sw.js",
	} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		cache := resp.Header.Get("Cache-Control")
		code := resp.StatusCode
		resp.Body.Close()

		if code != http.StatusOK {
			t.Fatalf("%s: status %d", p, code)
		}
		if cache == "" {
			t.Errorf("%s 缺少 Cache-Control：WebView 会走启发式缓存，升级后可能继续吃旧版", p)
		}
	}
}

// 前端只有一个 querySelector 助手 $()，选择器必须自带 # / . 前缀。
// 曾经出现 $("deletedBanner")（漏了 #）→ querySelector 返回 null →
// 每个 load* 函数第一行就抛异常，整个 Viewer 只显示统计面板。
// 这个测试就是防这一类回归。
var bareSelectorPattern = regexp.MustCompile(`\$\("([A-Za-z][A-Za-z0-9_-]*)"\)`)

func TestIndexHTMLSelectorsUseExplicitPrefix(t *testing.T) {
	matches := bareSelectorPattern.FindAllStringSubmatch(indexHTML, -1)
	if len(matches) > 0 {
		bad := make([]string, 0, len(matches))
		for _, m := range matches {
			bad = append(bad, `$("`+m[1]+`")`)
		}
		t.Fatalf("发现缺少 # / . 前缀的选择器（会返回 null）: %s", strings.Join(bad, ", "))
	}
}

// 没有头像时前端不得请求 /avatar/...（否则每条消息刷一次 404）。
func TestIndexHTMLSkipsAvatarRequestWithoutPath(t *testing.T) {
	if !strings.Contains(indexHTML, "x.AvatarPath") {
		t.Fatal("前端 avatar() 未检查 AvatarPath，缺失头像时会持续 404")
	}
	if strings.Contains(indexHTML, `src="'+avatar(x)+'"`) {
		t.Fatal("渲染路径仍在无条件拼接头像 URL，应使用 avatarTag(x)")
	}
	if !strings.Contains(indexHTML, "avatarTag(x)") {
		t.Fatal("未找到 avatarTag(x) 渲染函数")
	}
}

// 已删除消息的横幅必须按 id 定位（$("#deletedBanner")）。
func TestIndexHTMLBannerUsesIDSelector(t *testing.T) {
	if strings.Contains(indexHTML, `$("deletedBanner")`) {
		t.Fatal(`$("deletedBanner") 缺少 # 前缀`)
	}
	if !strings.Contains(indexHTML, `$("#deletedBanner")`) {
		t.Fatal("未找到 #deletedBanner 选择器")
	}
}

// 内嵌前端必须是自包含的：不能引用 CDN（离线/内网环境下会白屏）。
func TestIndexHTMLHasNoExternalAssets(t *testing.T) {
	for _, pattern := range []string{"http://", "https://"} {
		for _, line := range strings.Split(indexHTML, "\n") {
			if strings.Contains(line, pattern) &&
				(strings.Contains(line, "<script") || strings.Contains(line, "<link") || strings.Contains(line, "src=")) {
				t.Fatalf("前端引用了外部资源: %s", strings.TrimSpace(line))
			}
		}
	}
}

// themeCSS reads the embedded stylesheet for the layout regression tests.
func themeCSS(t *testing.T) string {
	t.Helper()
	b, err := webAssets.ReadFile("web/static/theme.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 滚动修复回归：.shell 用 overflow:hidden 锁死整页，滚动全部下发给 .feed。
// 一旦 .main 丢掉 min-height:0 或 .feed 丢掉 flex/min-height，内容溢出会被
// .shell 裁剪，主页表现为完全无法滚动。
func TestThemeCSSScrollChain(t *testing.T) {
	css := themeCSS(t)
	for _, want := range []string{
		".shell{height:100vh;height:100dvh;", // dvh：移动端地址栏跳动
		".main{min-width:0;min-height:0;",    // grid item 的高度约束
		".feed{flex:1 1 auto;min-height:0;",  // 唯一滚动容器的弹性尺寸
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("theme.css 缺少滚动约束 %q，主页可能无法滚动", want)
		}
	}
}

// 自动点击的 UI 接线必须完整：右栏 tab、面板、弹窗字段与加载函数。
func TestIndexHTMLAutoClickWiring(t *testing.T) {
	for _, want := range []string{
		`id="rightTabClick"`,
		`id="clickPanel"`,
		`id="editAutoClick"`,
		`id="addAutoClick"`,
		`function loadAutoClick(`,
		`function saveAutoClick(`,
		`function testAutoClick(`,
		`function kbButtons(`,
		"api/autoclick/log",
		"api/autoclick/test",
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("index.html 缺少自动点击接线: %s", want)
		}
	}
}

// 视频缩略预览是服务端从视频里抽出来的 jpeg 帧，服务端按 image/jpeg 返回。
// 把它当成 <video src> 时浏览器收到图片，直接报「没有找到支持的视频格式和
// MIME 类型」，视频页每一格都打不开。openMedia 必须只在图片上使用
// /media/preview/，视频/音频/文件一律走 /media/<sha> 原文件。
func TestIndexHTMLVideoNeverUsesPreviewAsSource(t *testing.T) {
	for _, want := range []string{
		`isImage=mime.startsWith("image/")`,
		`src=(isImage&&x.PreviewPath)`,
		`poster="/media/preview/`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("openMedia 的媒体源选择缺少 %q：视频会拿到 jpeg 预览而无法播放", want)
		}
	}
	if strings.Contains(indexHTML, `src=x.PreviewPath?"/media/preview/"+sha:"/media/"+sha`) {
		t.Fatal("openMedia 仍在无条件把 preview 当作 src，视频必然报 MIME 不支持")
	}
}

// 视频卡片有封面帧时应该用起来，而不是一律显示灰底占位符。
func TestThemeCSSVideoPosterLayer(t *testing.T) {
	css := themeCSS(t)
	for _, want := range []string{
		".media-video-placeholder img.poster{position:absolute",
		".media-video-placeholder span.play-badge{position:absolute",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("theme.css 缺少视频封面叠层规则 %q", want)
		}
	}
}

// 消息卡片里的视频只渲染一个「▶ 打开视频」按钮、不显示封面帧时，用户看到
// 的是一片空白 —— 归档了 1000 条视频，消息流里一张图都没有，会被当成「下载
// 失败」。媒体网格视图一直是有封面的，两处必须一致。
func TestIndexHTMLMessageVideoRendersPoster(t *testing.T) {
	for _, want := range []string{
		`class="msg-video`,                       // 独立的封面容器
		`class="poster"`,                         // 封面 img
		`src="/media/preview/'+x.MediaSHA256+'"`, // 封面走 preview 变体
		`<span class="play-badge">▶</span>`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("消息卡片视频缺少 %q：视频消息会显示成空白", want)
		}
	}
	// 旧的纯按钮写法必须消失，否则两条渲染路径同时存在，改一处不生效。
	if strings.Contains(indexHTML, `>▶ 打开视频</button>`) {
		t.Fatal("消息卡片仍在用纯文字按钮渲染视频，封面帧不会被显示")
	}
}

// 视频封面叠层要有自己的样式，不能依赖媒体网格那套 .media-video-placeholder
// （容器选择器不同，套用后封面会塌成 0 高度）。
func TestThemeCSSMessageVideoLayer(t *testing.T) {
	css := themeCSS(t)
	for _, want := range []string{
		".msg-video{position:relative",
		".msg-video .poster{position:absolute",
		".msg-video .play-badge{position:absolute",
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("theme.css 缺少消息视频封面规则 %q", want)
		}
	}
}

// 归档任务的状态只反映「消息回填」，媒体是异步队列。实测一次真实回填：
// 任务在 06:48:13 就 done(1000/1000)，媒体却一直下到 07:13:55 —— 中间 25 分钟
// 面板写着「✓ 完成」，而消息里的图片位置全是空的。任务卡片必须把两件事分开
// 显示，并且在媒体未清空时继续轮询。
func TestIndexHTMLJobSeparatesMessageAndMediaProgress(t *testing.T) {
	for _, want := range []string{
		`fetch("/api/downloads/summary")`, // 任务卡片要读到媒体进度
		`busy=j.state==="done"&&mdPend>0`, // done 且有排队/在跑 = 还没真正完
		`✓ 消息完成`,                          // 不再谎报「完成」
		`媒体 '+mdDone+'/'+mdTotal`,         // 具体数字
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("任务卡片缺少媒体进度接线 %q：任务会谎报完成", want)
		}
	}
	// 轮询条件必须包含媒体队列，否则任务一 done 就不再刷新，进度永远停在旧值。
	if !strings.Contains(indexHTML, `(js.some(j=>j.state==="running")||mdPend>0)&&!jobsTimer`) {
		t.Fatal("任务轮询未把媒体队列纳入条件，任务 done 后进度不再更新")
	}
}

// jsFunctionBody 返回 index.html 内联脚本里某个函数的函数体（含花括号）。
func jsFunctionBody(t *testing.T, name string) string {
	t.Helper()
	marker := "function " + name + "("
	i := strings.Index(indexHTML, marker)
	if i < 0 {
		t.Fatalf("index.html 找不到函数 %s", name)
	}
	open := strings.Index(indexHTML[i:], "{")
	if open < 0 {
		t.Fatalf("函数 %s 没有函数体", name)
	}
	open += i
	depth := 0
	for j := open; j < len(indexHTML); j++ {
		switch indexHTML[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return indexHTML[open : j+1]
			}
		}
	}
	t.Fatalf("函数 %s 的花括号不闭合", name)
	return ""
}

// 「全部功能 / 添加监控源 / Telegram 账号」必须是 main 里的普通视图，
// 不能再是 position:fixed 的浮层。浮层方案下，从「设置」里点添加源时
// 打开的是叠在「全部」页面之下的另一个全屏层：用户要先关掉「全部」才能
// 看到表单。三者现在和图片/链接/编号共用同一套视图栈（openPage）。
func TestIndexHTMLPagesAreViewsNotOverlays(t *testing.T) {
	for _, want := range []string{
		`id="moreView" class="page-view"`,
		`id="addView" class="page-view"`,
		`id="accountsView" class="page-view"`,
	} {
		if !strings.Contains(indexHTML, want) {
			t.Fatalf("缺少页面型视图 %q：子页面又退回浮层了", want)
		}
	}
	for _, dead := range []string{`id="mPage"`, `id="addModal"`, `id="accountsModal"`} {
		if strings.Contains(indexHTML, dead) {
			t.Fatalf("%q 仍然存在：页面与浮层同时维护会再次出现层叠问题", dead)
		}
	}
	if !strings.Contains(jsFunctionBody(t, "openAdd"), `openPage("add")`) {
		t.Fatal("添加监控源没有走视图导航 openPage(\"add\")")
	}
	if !strings.Contains(jsFunctionBody(t, "openAccounts"), `openPage("accounts")`) {
		t.Fatal("账号管理没有走视图导航 openPage(\"accounts\")")
	}
	if !strings.Contains(jsFunctionBody(t, "mobileNav"), `replaceState`) {
		t.Fatal("视图切换丢了 replaceState：返回键会堆出十几条历史")
	}
}

// 同理，「我的账号」行里的「登出」→ openLogin 也走同一条路，必须一并收口。
func TestIndexHTMLLogoutUsesOpenLogin(t *testing.T) {
	if !strings.Contains(jsFunctionBody(t, "logoutAccount"), "openLogin()") {
		t.Fatal("登出后未回到登录流程，用户会卡在无登录态")
	}
}

// 兜底：即使将来有别的入口同时打开两个弹窗，登录弹窗也必须压在最上层。
//
// 层级改成 token 后（批次 4：8 个裸 z-index 收成 --z-* 一套），这里不能再直接
// 从规则里抠数字，要先解析 var(--z-xxx) 再回查 :root 的 token 值。
func TestThemeCSSLoginModalAboveAccounts(t *testing.T) {
	css := themeCSS(t)

	// 规则 -> 它引用的层级 token 名
	refOf := func(selector string) (string, bool) {
		re := regexp.MustCompile(`(?m)` + regexp.QuoteMeta(selector) + `\{[^}]*z-index:var\((--z-[a-z-]+)\)`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			return "", false
		}
		return m[1], true
	}
	// token -> 数值
	valOf := func(tok string) (int, bool) {
		re := regexp.MustCompile(regexp.QuoteMeta(tok) + `:(\d+)`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			return 0, false
		}
		v, err := strconv.Atoi(m[1])
		return v, err == nil
	}

	baseTok, ok := refOf(".modal")
	if !ok {
		t.Fatal("theme.css 找不到 .modal 的 z-index token 引用")
	}
	topTok, ok := refOf("#loginModal")
	if !ok {
		t.Fatal("theme.css 缺少 #loginModal 的 z-index：登录弹窗会被后声明的 .modal 覆盖")
	}
	baseZ, ok1 := valOf(baseTok)
	topZ, ok2 := valOf(topTok)
	if !ok1 || !ok2 {
		t.Fatalf("层级 token 未在 :root 定义: %s / %s", baseTok, topTok)
	}
	if topZ <= baseZ {
		t.Fatalf("#loginModal z-index=%d(%s) 未高于 .modal 基线 %d(%s)：两个弹窗同开时登录框会被盖住",
			topZ, topTok, baseZ, baseTok)
	}
}

// 「使用说明」弹窗的内容由 tools/gen_help.py 从 docs/用户手册.md 生成。
// 这些断言盯的是「内容被悄悄换掉」：手册改一句、弹窗里那句就过期，
// 而弹窗是客户在 App 里唯一能读到的说明——PDF 与 App 里说法不一致时，
// 客户会信 App 里那份，然后按过期的步骤操作。
func TestHelpModalMatchesUserManual(t *testing.T) {
	help, ok := sliceDivByID(indexHTML, "helpModal")
	if !ok {
		t.Fatal("index.html 找不到 #helpModal 或它的 div 不配平")
	}

	if !strings.Contains(help, "BEGIN help-manual") || !strings.Contains(help, "END help-manual") {
		t.Fatal("帮助正文缺少生成标记：请运行 tools/gen_help.py，不要手写这段")
	}

	// 核心卖点与关键流程：这三句没了，说明手册被换成了别的东西
	for _, want := range []string{
		"已删除",   // 撤回/删除留存的入口
		"设备码",   // 激活流程的起点
		"激活",    // 授权
		"添加监控源", // 上手最后一步
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("使用说明里缺少关键词 %q：手册内容被替换或裁剪了？", want)
		}
	}

	// 边界说明必须留着。这是文档里最容易被"为了好看"删掉的一段，
	// 但它正是防止客户以为"什么删了都能恢复"、然后来投诉的那道保险。
	for _, want := range []string{"收不到", "Secret Chat", "清空归档"} {
		if !strings.Contains(help, want) {
			t.Fatalf("使用说明缺少边界声明 %q：删除这段会让客户形成错误预期", want)
		}
	}

	// 旧版弹窗的标志性内容，必须已经消失
	if strings.Contains(help, "CLI 命令参考") {
		t.Fatal("使用说明仍是旧的 CLI 版内容：请运行 tools/gen_help.py")
	}

	// 表格必须成对闭合，否则弹窗里会漏出一堆标签
	if open, closed := strings.Count(help, "<table>"), strings.Count(help, "</table>"); open != closed {
		t.Fatalf("帮助正文 table 标签不配平: %d 开 / %d 闭", open, closed)
	}
	if strings.Count(help, "<table>") == 0 {
		t.Fatal("帮助正文没有任何表格：手册里的边界/功能/语法表都被吃掉了")
	}
}

// sliceDivByID 返回含 id="<id>" 的那个 div 的完整片段（含自身标签）。
// 为什么不用「截到下一个 id=」：helpModal 在 loginModal 之后、脚本之前，
// 按相邻元素取边界一旦有人调整弹窗顺序就会静默截错；这里按 <div> 深度配平。
// 前缀用 "<div" 而非 "<div " 是有意的：内联开关标签的写法不统一，
// 而 index.html 里不存在 <divfoo 这种标签，配平不会误判。
func sliceDivByID(doc, id string) (string, bool) {
	marker := `id="` + id + `"`
	i := strings.Index(doc, marker)
	if i < 0 {
		return "", false
	}
	start := strings.LastIndex(doc[:i], "<div")
	if start < 0 {
		return "", false
	}
	depth, pos := 1, i
	for depth > 0 {
		nextOpen := strings.Index(doc[pos:], "<div")
		nextClose := strings.Index(doc[pos:], "</div>")
		if nextClose < 0 {
			return "", false
		}
		if nextOpen >= 0 && nextOpen < nextClose {
			depth++
			pos += nextOpen + len("<div")
			continue
		}
		depth--
		pos += nextClose + len("</div>")
	}
	return doc[start:pos], true
}

// 打开登录弹窗会自动启动扫码流程。若切到「手机号」页签时没有取消它，
// 点「发送验证码」必然被后端用「另一个登录流程正在进行，请先取消」拒绝，
// 而界面上没有任何办法把这个残留流程清掉 —— 手机号登录等于不可用。
func TestIndexHTMLLoginTabSwitchCancelsStaleFlow(t *testing.T) {
	window := func(anchor string, size int) string {
		idx := strings.Index(indexHTML, anchor)
		if idx < 0 {
			t.Fatalf("找不到 %s", anchor)
		}
		end := idx + size
		if end > len(indexHTML) {
			end = len(indexHTML)
		}
		return indexHTML[idx:end]
	}

	pane := window("async function loginPane(tab){", 700)
	if !strings.Contains(pane, `loginPaneCur==="qr"&&tab!=="qr"`) {
		t.Fatal("切离扫码页签时没有判断来源页签，手机号登录会被残留扫码流程卡死")
	}
	if !strings.Contains(pane, "/api/login/qr/cancel") {
		t.Fatal("切离扫码页签时未调用 /api/login/qr/cancel")
	}

	open := window("async function openLogin(){", 300)
	if !strings.Contains(open, "/api/login/qr/cancel") {
		t.Fatal("打开登录弹窗前未清理残留流程，重复打开会一直撞「另一个登录流程正在进行」")
	}
}

// 手机端冷启动的竞态：Go 侧在 MTProto 握手完成前一直报 state=connecting，
// 而 WebView 恰好在这个窗口里加载页面。旧版 loadAuth 只问一次，把
// "connecting" 当成 "没登录" 钉死在顶栏，之后再也不会刷新 —— 用户看到
// 顶栏「未登录」而账号面板「已登录」，两个界面都没说谎，读的是不同时刻。
//
// 这条测试锁住三件事：connecting 必须显示为连接中且自动重试、no_account
// 不能伪装成未登录、空态文案必须区分"没登录"和"登录了但没监控源"。
func TestIndexHTMLLoginRaceNotReportedAsLoggedOut(t *testing.T) {
	body := indexHTML
	for _, want := range []struct{ needle, why string }{
		{`a.state==="connecting"`, "connecting 必须单独分支，否则会被当成未登录"},
		{`setTimeout(loadAuth,1500)`, "connecting 必须自动重试，否则冷启动竞态永久定格"},
		{`window.__auth=a`, "空态渲染需要最新的登录状态"},
		{`已登录 "+who+"，但还没有监控源`, "已登录但零监控源是另一种空态，不能劝用户再登录一次"},
		{`a.state==="no_account"`, "无账号要引导去添加，不是未登录"},
	} {
		if !strings.Contains(body, want.needle) {
			t.Errorf("index.html 缺少 %q —— %s", want.needle, want.why)
		}
	}
	// 反向断言：catch 兜底里也要重试，否则网络抖动一次就永久"状态未知"。
	if !strings.Contains(body, `el.textContent="● 状态未知";if(authRetry++<40)setTimeout(loadAuth,2000)`) {
		t.Error("catch 分支必须带重试")
	}
}

// 登录状态从 connecting 翻到 authorized 时，#feed 里那份空态是用旧状态渲染的。
// 只更新顶栏而不重绘空态，用户会看到「已登录 @xxx」旁边挂着「登录 Telegram
// 账号」按钮 —— 两处自相矛盾，正是这个 bug 的现场。
func TestIndexHTMLRefreshesEmptyStateOnAuthFlip(t *testing.T) {
	body := indexHTML
	if !strings.Contains(body, `let flipped=authWas!==true`) {
		t.Error("authorized 分支必须检测状态翻转")
	}
	if !strings.Contains(body, `if(flipped)setTimeout(refresh,0)`) {
		t.Error("状态翻转后必须重绘，否则空态停留在旧文案")
	}
	if strings.Contains(body, `if(flipped)setTimeout(loadAuth`) {
		t.Error("翻转后调 loadAuth 会自激循环")
	}
}
