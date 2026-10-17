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
