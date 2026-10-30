package archive

import (
	"regexp"
	"strings"
	"testing"
)

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
