package archive

import "testing"

// The download concurrency used to be hardcoded to 1, which serialised the whole
// queue: a 1000-item backfill finished one file before starting the next, so a
// single 2 MB video blocked every photo behind it. These cases pin the default
// and the escape hatch, and they pin the *fallback* behaviour too — a bad tuning
// value must never stop archiving.
func TestMediaThreadsDefault(t *testing.T) {
	t.Setenv("TDL_MEDIA_THREADS", "")
	if got := MediaThreads(); got != mediaDefaultThreads {
		t.Fatalf("MediaThreads()=%d want default %d", got, mediaDefaultThreads)
	}
	if mediaDefaultThreads != 4 {
		t.Fatalf("mediaDefaultThreads=%d want 4", mediaDefaultThreads)
	}
}

func TestMediaThreadsEnvOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want int
	}{
		{"one keeps the old serial behaviour", "1", 1},
		{"in-range value is honoured", "8", 8},
		{"upper bound is inclusive", "16", 16},
		// Out-of-range values fall back instead of being obeyed: Telegram
		// rate-limits getFile per datacentre, and an accidental 200 would trade
		// throughput for a FLOOD_WAIT wall-clock penalty.
		{"above the cap falls back", "17", mediaDefaultThreads},
		{"zero falls back", "0", mediaDefaultThreads},
		{"negative falls back", "-3", mediaDefaultThreads},
		{"nonsense falls back", "lots", mediaDefaultThreads},
		{"whitespace is tolerated", "  6  ", 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("TDL_MEDIA_THREADS", c.env)
			if got := MediaThreads(); got != c.want {
				t.Fatalf("MediaThreads()=%d want %d (env=%q)", got, c.want, c.env)
			}
		})
	}
}

// The constructor must actually consume the setting; a knob that is read but
// never wired would look correct in tests of MediaThreads alone.
func TestNewMediaWorkerUsesConfiguredThreads(t *testing.T) {
	t.Setenv("TDL_MEDIA_THREADS", "6")
	w := NewMediaWorker(nil, nil, t.TempDir())
	if w.Threads != 6 {
		t.Fatalf("worker.Threads=%d want 6", w.Threads)
	}
}
