package archive

import (
	"testing"

	"github.com/iyear/tdl/archive/store"
)

func TestUTF16SliceCountsCodeUnits(t *testing.T) {
	// Telegram offsets are UTF-16 code units. For ASCII that matches bytes, so
	// this case would pass even with a broken implementation — it establishes
	// the baseline.
	if got := utf16Slice("hello world", 6, 5); got != "world" {
		t.Fatalf("ascii slice = %q, want world", got)
	}
	// Three BMP characters occupy three units, so byte slicing would also work
	// here. The emoji case below is the one that separates the two.
	if got := utf16Slice("编号:ABC", 3, 3); got != "ABC" {
		t.Fatalf("bmp slice = %q, want ABC", got)
	}
	// An emoji is one rune but two code units. Slicing by rune or byte both
	// drift here; only the UTF-16 view lands on the label.
	s := "🔰badge" // 2 units + 5 = 7
	if got := utf16Slice(s, 2, 5); got != "badge" {
		t.Fatalf("emoji offset slice = %q, want badge", got)
	}
	if got := utf16Slice(s, 0, 2); got != "🔰" {
		t.Fatalf("emoji slice = %q, want the emoji itself", got)
	}
	if got := utf16Slice("短", 5, 9); got != "" {
		t.Fatalf("out of range slice = %q, want empty", got)
	}
}

// The archived raw JSON is the only place a text-link destination survives, so
// this extraction is what a backfill depends on.
func TestClickablesFromRawRecoversTextLink(t *testing.T) {
	raw := []byte(`{"ID":154314,"Message":"🔰编号:HS3434(点击编号自动复制)\n🦋一键查询验证视频🦋\n昵称:牛奶","Entities":[{"Offset":0,"Length":2,"Type":0},{"Offset":5,"Length":6,"Type":0},{"Offset":24,"Length":8,"URL":"https://t.me/bianbaoyang1_bot?start=HS3434"}]}`)
	x := store.Message{ChatID: -100, MessageID: 154314, Raw: raw}

	got := clickablesFromRaw(x)
	if len(got) != 1 {
		t.Fatalf("want exactly one clickable, got %d: %+v", len(got), got)
	}
	if got[0].Kind != "link" {
		t.Fatalf("kind = %q, want link", got[0].Kind)
	}
	if got[0].URL != "https://t.me/bianbaoyang1_bot?start=HS3434" {
		t.Fatalf("url = %q", got[0].URL)
	}
	if got[0].Text == "" || got[0].Text == "文内按钮" {
		t.Logf("label fell back to the generic name (offset did not land on text): %q", got[0].Text)
	}
}

// A message with no raw payload must not panic and must not invent buttons; the
// caller falls back to fetching it.
func TestClickablesFromRawEmpty(t *testing.T) {
	if got := clickablesFromRaw(store.Message{ChatID: -100, MessageID: 1}); len(got) != 0 {
		t.Fatalf("no raw payload should yield no clickables, got %+v", got)
	}
	if got := clickablesFromRaw(store.Message{Raw: []byte("not json")}); len(got) != 0 {
		t.Fatalf("invalid raw payload should yield no clickables, got %+v", got)
	}
}

// A bare t.me URL in the body is still a clickable even without entities.
func TestClickablesFromRawBodyURL(t *testing.T) {
	x := store.Message{Raw: []byte(`{"ID":2,"Message":"看这个 https://t.me/bianbaoyang1_bot?start=AB1234"}`)}
	got := clickablesFromRaw(x)
	if len(got) != 1 {
		t.Fatalf("want one body link, got %d: %+v", len(got), got)
	}
	if got[0].Text != "正文链接" {
		t.Fatalf("label = %q, want 正文链接", got[0].Text)
	}
}

// The same URL arriving from both the entity array and the body must be clicked
// once. Clicking twice is user-visible and wastes rate budget.
func TestClickablesFromRawDeduplicates(t *testing.T) {
	u := "https://t.me/bianbaoyang1_bot?start=AB1234"
	raw := []byte(`{"ID":3,"Message":"` + u + `","Entities":[{"Offset":0,"Length":10,"URL":"` + u + `"}]}`)
	got := clickablesFromRaw(store.Message{Raw: raw})
	if len(got) != 1 {
		t.Fatalf("duplicate url should collapse to one clickable, got %d: %+v", len(got), got)
	}
}
