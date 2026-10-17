package archive

import (
	"context"
	"encoding/json"
	"github.com/iyear/tdl/archive/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
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
