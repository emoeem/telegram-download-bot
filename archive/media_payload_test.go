package archive

import (
	"context"
	"strings"
	"testing"

	"github.com/gotd/td/telegram/query/messages"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

// The bug this file pins down: gotd's File() only checks the media *kind*, so a
// payload whose object is present but nil reaches a field read on a nil struct
// and takes the process down. If this test ever stops panicking, upstream grew a
// nil check and the guard in messageFile became redundant — that is worth
// knowing, so it is asserted rather than assumed.
func TestUpstreamFilePanicsOnNilMediaObject(t *testing.T) {
	hostile := &tg.Message{ID: 7, Media: (*tg.MessageMediaPhoto)(nil)}
	defer func() {
		if recover() == nil {
			t.Fatal("messages.Elem.File returned on a typed-nil photo instead of panicking: " +
				"messageFile's nil guards can be re-examined")
		}
	}()
	_, _ = messages.Elem{Msg: hostile}.File()
}

// A typed-nil *tg.Photo also slips through upstream: (*tg.Photo).AsNotEmpty has
// no receiver check and reports ok=true for a nil pointer. Same panic, one level
// deeper — this is the case the kind-only nil check would miss.
func TestUpstreamFilePanicsOnTypedNilPhoto(t *testing.T) {
	hostile := &tg.Message{ID: 8, Media: &tg.MessageMediaPhoto{Photo: (*tg.Photo)(nil)}}
	defer func() {
		if recover() == nil {
			t.Fatal("messages.Elem.File returned on a typed-nil *tg.Photo instead of panicking")
		}
	}()
	_, _ = messages.Elem{Msg: hostile}.File()
}

// Every incomplete shape the history walk can hand us must come back as
// ok=false. No shape may escape as a panic: this code runs in the media
// worker, where a panic ends the whole archive process.
func TestMessageFileRejectsIncompletePayloads(t *testing.T) {
	cases := []struct {
		name string
		msg  tg.NotEmptyMessage
	}{
		{"typed-nil MessageMediaPhoto", &tg.Message{ID: 1, Media: (*tg.MessageMediaPhoto)(nil)}},
		{"photo media with nil photo", &tg.Message{ID: 2, Media: &tg.MessageMediaPhoto{}}},
		{"photo media with typed-nil photo", &tg.Message{ID: 3, Media: &tg.MessageMediaPhoto{Photo: (*tg.Photo)(nil)}}},
		{"photo media with empty photo", &tg.Message{ID: 4, Media: &tg.MessageMediaPhoto{Photo: &tg.PhotoEmpty{ID: 9}}}},
		{"typed-nil MessageMediaDocument", &tg.Message{ID: 5, Media: (*tg.MessageMediaDocument)(nil)}},
		{"document media with nil document", &tg.Message{ID: 6, Media: &tg.MessageMediaDocument{}}},
		{"document media with typed-nil document", &tg.Message{ID: 7, Media: &tg.MessageMediaDocument{Document: (*tg.Document)(nil)}}},
		{"media-less message", &tg.Message{ID: 8}},
		{"typed-nil *tg.Message as the element", (*tg.Message)(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := messageFile(tc.msg)
			if ok {
				t.Fatalf("messageFile accepted an incomplete payload: %+v", f)
			}
			if f.Location != nil {
				t.Fatalf("rejected payload still came back with a location: %T", f.Location)
			}
		})
	}
}

// The guard must not cost the valid cases: a real photo and a real document
// still resolve, with a usable location.
func TestMessageFileResolvesValidPayloads(t *testing.T) {
	photo := &tg.Message{ID: 10, Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{
		ID:    4242,
		Date:  1600000000,
		Sizes: []tg.PhotoSizeClass{&tg.PhotoSize{Type: "x", W: 800, H: 600}},
	}}}
	f, ok := messageFile(photo)
	if !ok {
		t.Fatal("valid photo was rejected")
	}
	if f.Location == nil {
		t.Fatal("valid photo produced no download location")
	}
	if !strings.HasPrefix(f.Name, "photo4242_") {
		t.Fatalf("unexpected photo file name %q", f.Name)
	}

	doc := &tg.Message{ID: 11, Media: &tg.MessageMediaDocument{Document: &tg.Document{
		ID:       5353,
		MimeType: "video/mp4",
		Attributes: []tg.DocumentAttributeClass{
			&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
		},
	}}}
	f, ok = messageFile(doc)
	if !ok {
		t.Fatal("valid document was rejected")
	}
	if f.Location == nil {
		t.Fatal("valid document produced no download location")
	}
	if f.Name != "clip.mp4" {
		t.Fatalf("unexpected document file name %q", f.Name)
	}
}

// The poster path runs over the same untrusted payload; losing a thumbnail is
// acceptable, losing the process is not.
func TestThumbLocationSurvivesIncompletePayloads(t *testing.T) {
	cases := []struct {
		name string
		msg  tg.NotEmptyMessage
	}{
		{"typed-nil MessageMediaDocument", &tg.Message{ID: 1, Media: (*tg.MessageMediaDocument)(nil)}},
		{"document media with typed-nil document", &tg.Message{ID: 2, Media: &tg.MessageMediaDocument{Document: (*tg.Document)(nil)}}},
		{"typed-nil *tg.Message", (*tg.Message)(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if loc := thumbLocation(tc.msg); loc != nil {
				t.Fatalf("incomplete payload produced a thumbnail location: %T", loc)
			}
		})
	}
}

// A panicking job is contained and parked. Leaving it pending would make the
// worker panic again on the next pass, which is a crash loop, not a retry.
func TestMediaJobPanicIsContainedAndParked(t *testing.T) {
	s, id := mediaJobStore(t)
	w := &MediaWorker{Store: s}
	job := store.MediaJob{ID: id}

	task := w.runJob(context.Background(), job, func() error {
		var nilPhoto *tg.Photo
		_ = nilPhoto.ID // the same shape that killed the worker
		return nil
	})
	if err := task(); err != nil {
		t.Fatalf("contained panic still propagated: %v", err)
	}

	status, attempts := mediaStatus(t, s, id)
	if status != "skipped" {
		t.Fatalf("panicking job must be parked, got status=%q attempts=%d", status, attempts)
	}
}
