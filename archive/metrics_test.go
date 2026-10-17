package archive

import (
	"context"
	"testing"
)

func TestMetricsCacheTTL(t *testing.T) {
	s := openTestStore(t)
	w := NewWebServer(s)
	first, err := w.metricsSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().ExecContext(context.Background(), "INSERT INTO jobs(kind,status,created_at,updated_at) VALUES(?,?,unixepoch(),unixepoch())", "test", "running"); err != nil {
		t.Fatal(err)
	}
	second, err := w.metricsSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Jobs.Running != second.Jobs.Running {
		t.Fatalf("cache miss inside TTL: first=%+v second=%+v", first, second)
	}
}
