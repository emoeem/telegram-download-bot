package archive

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
	"github.com/iyear/tdl/archive/store"
)

// publishStore builds a store with the raw messages a material is derived from,
// writes one material, and returns the store. Building the layer through the
// real materializer keeps this test honest: it fails if the store and the
// materializer ever disagree about what a material contains.
func publishStore(t *testing.T, msgs ...store.Message) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/archive.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	for _, m := range msgs {
		if err := s.UpsertMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := NewMaterializer(s).Build(ctx, BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildPublishPlanDefaultTemplate(t *testing.T) {
	s := publishStore(t, store.Message{
		ChatID: 10, MessageID: 1, Date: time.Unix(1700000000, 0),
		Text: "hello world",
	})
	plan, err := BuildPublishPlan(context.Background(), s, PublishOptions{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("items=%d", len(plan.Items))
	}
	if got := plan.Items[0].Text; !strings.Contains(got, "hello world") {
		t.Fatalf("text=%q", got)
	}
	if plan.Skipped != 0 {
		t.Fatalf("skipped=%d", plan.Skipped)
	}
	if plan.Items[0].Skip != "" {
		t.Fatalf("unexpected skip reason: %s", plan.Items[0].Skip)
	}
}

func TestBuildPublishPlanCustomTemplateHelpers(t *testing.T) {
	when := time.Date(2023, 11, 15, 12, 0, 0, 0, time.Local)
	s := publishStore(t, store.Message{
		ChatID: 10, MessageID: 1, Date: when,
		Text: strings.Repeat("字", 30),
	})
	tmpl := `{{date "2006-01-02" .Date}}|{{truncate 5 .Body}}`
	plan, err := BuildPublishPlan(context.Background(), s, PublishOptions{Limit: 5, Template: tmpl})
	if err != nil {
		t.Fatal(err)
	}
	got := plan.Items[0].Text
	if !strings.HasPrefix(got, "2023-11-15|") {
		t.Fatalf("date helper wrong: %q", got)
	}
	// truncate counts runes, so five Chinese characters plus the ellipsis.
	if !strings.Contains(got, "字字字字字…") {
		t.Fatalf("truncate must cut by rune, got %q", got)
	}
}

func TestBuildPublishPlanSkipsOversizedText(t *testing.T) {
	// 4096 characters is Telegram's hard message cap; a longer render must be
	// reported as skipped rather than truncated into half a post.
	s := publishStore(t, store.Message{
		ChatID: 10, MessageID: 1, Date: time.Unix(1700000000, 0),
		Text: strings.Repeat("x", 4200),
	})
	plan, err := BuildPublishPlan(context.Background(), s, PublishOptions{Limit: 5, Template: "{{.Body}}"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Skipped != 1 {
		t.Fatalf("skipped=%d, want 1", plan.Skipped)
	}
	if !strings.Contains(plan.Items[0].Skip, "4096") {
		t.Fatalf("skip reason=%q", plan.Items[0].Skip)
	}
}

func TestBuildPublishPlanSkipsEmptyRender(t *testing.T) {
	// A template that matches nothing produces an empty message; Telegram
	// rejects it, so the plan must say so before the batch starts.
	s := publishStore(t, store.Message{
		ChatID: 10, MessageID: 1, Date: time.Unix(1700000000, 0),
		Text: "body",
	})
	plan, err := BuildPublishPlan(context.Background(), s, PublishOptions{Limit: 5, Template: ""})
	if err != nil {
		t.Fatal(err)
	}
	// The default template still renders a body, so this asserts the non-empty
	// path; the empty case is covered below by an explicit empty template.
	if plan.Skipped != 0 {
		t.Fatalf("default template must not skip, skipped=%d (%s)", plan.Skipped, plan.Items[0].Skip)
	}

	empty, err := BuildPublishPlan(context.Background(), s, PublishOptions{Limit: 5, Template: "{{if false}}x{{end}}"})
	if err != nil {
		t.Fatal(err)
	}
	if empty.Skipped != 1 {
		t.Fatalf("an empty render must be skipped, skipped=%d", empty.Skipped)
	}
}

func TestBuildPublishPlanRejectsBadTemplate(t *testing.T) {
	s := publishStore(t, store.Message{ChatID: 10, MessageID: 1, Date: time.Unix(1, 0), Text: "x"})
	if _, err := BuildPublishPlan(context.Background(), s, PublishOptions{Template: "{{.Nope"}); err == nil {
		t.Fatal("a malformed template must fail at parse time, not mid-batch")
	}
}

func TestBuildPublishPlanRespectsLimitAndFilter(t *testing.T) {
	s := publishStore(t,
		store.Message{ChatID: 10, MessageID: 1, Date: time.Unix(3, 0), Text: "alpha"},
		store.Message{ChatID: 10, MessageID: 2, Date: time.Unix(4, 0), Text: "beta"},
		store.Message{ChatID: 20, MessageID: 3, Date: time.Unix(5, 0), Text: "gamma"},
	)
	all, err := BuildPublishPlan(context.Background(), s, PublishOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 1 {
		t.Fatalf("limit not applied: %d items", len(all.Items))
	}
	only, err := BuildPublishPlan(context.Background(), s, PublishOptions{Limit: 10, ChatID: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Items) != 1 || !strings.Contains(only.Items[0].Text, "gamma") {
		t.Fatalf("chat filter wrong: %+v", only.Items)
	}
}

func TestIsFatalSendError(t *testing.T) {
	if IsFatalSendError(nil) {
		t.Fatal("nil must not be fatal")
	}
	if !IsFatalSendError(tgerr.New(420, "FLOOD_WAIT_60")) {
		t.Fatal("a flood wait must stop the batch")
	}
	if !IsFatalSendError(tgerr.New(401, "AUTH_KEY_UNREGISTERED")) {
		t.Fatal("a lost authorization must stop the batch")
	}
	// A per-item rejection is the batch's job to skip, not to stop on.
	if IsFatalSendError(tgerr.New(400, "MEDIA_CAPTION_TOO_LONG")) {
		t.Fatal("a per-item rejection must not stop the batch")
	}
}
