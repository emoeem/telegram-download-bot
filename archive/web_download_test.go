package archive

import (
	"context"
	"encoding/json"
	"github.com/iyear/tdl/archive/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadsSummaryEmpty(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &Controller{Store: s, ctx: context.Background(), jobs: map[int64]*Job{}}
	ts := httptest.NewServer((&WebServer{Store: s, Ctl: c}).Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/downloads/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var v store.DownloadSummary
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if v.Running != 0 || v.Queued != 0 || v.Completed != 0 || v.Failed != 0 || v.Paused != 0 {
		t.Fatalf("summary=%+v", v)
	}
}
func TestJobsAPIEmptyWrapper(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := &Controller{Store: s, ctx: context.Background(), jobs: map[int64]*Job{}}
	ts := httptest.NewServer((&WebServer{Store: s, Ctl: c}).Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var v jobsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(v.Items) != 0 {
		t.Fatalf("jobs=%+v", v.Items)
	}
}

// 任务卡片上的「归档 N 条」取自 live jobs map，服务一重启就归零 —— 面板会出现
// 「✓ 消息完成 · 归档 0 条」这种自相矛盾的卡片（实测于 2026-10-28 部署后）。
// DB 里的 progress_done 就是同一件事，必须能兜底。
func TestJobsAPIArchivedFallsBackToProgressDone(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	total := 1000
	started := time.Now()
	chatID := int64(3826678107)
	id, err := s.CreateJob(ctx, store.Job{
		Kind: "backfill", ChatID: &chatID, Status: "done",
		ProgressDone: 1000, ProgressTotal: &total, StartedAt: &started,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishJob(ctx, id, "done", ""); err != nil {
		t.Fatal(err)
	}
	// live map 故意留空：模拟进程重启后的状态
	c := &Controller{Store: s, ctx: ctx, jobs: map[int64]*Job{}}
	ts := httptest.NewServer((&WebServer{Store: s, Ctl: c}).Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/jobs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v jobsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(v.Items) != 1 {
		t.Fatalf("jobs=%+v", v.Items)
	}
	if v.Items[0].Archived != 1000 {
		t.Fatalf("archived=%d want 1000（重启后必须从 progress_done 兜底）", v.Items[0].Archived)
	}
	if v.Items[0].ProgressDone != 1000 {
		t.Fatalf("progress_done=%d", v.Items[0].ProgressDone)
	}
}
