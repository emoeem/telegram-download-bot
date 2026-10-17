package archive

import (
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

func TestMapForwardedMessageIDsUsesRandomID(t *testing.T) {
	sourceIDs := []int{101, 102, 103}
	randomIDs := []int64{9001, 9002, 9003}
	res := &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessageID{RandomID: 9002, ID: 502},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 502}},
		&tg.UpdateMessageID{RandomID: 9001, ID: 501},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 501}},
		&tg.UpdateMessageID{RandomID: 9003, ID: 503},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 503}},
	}}
	got, err := mapForwardedMessageIDs(res, sourceIDs, randomIDs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{101: 501, 102: 502, 103: 503}
	if len(got) != len(want) {
		t.Fatalf("mapping=%v", got)
	}
	for id, wantID := range want {
		if got[id] != wantID {
			t.Fatalf("source %d mapped to %d, want %d", id, got[id], wantID)
		}
	}
}

func TestMissingMediaGroupMessagesIsIdempotent(t *testing.T) {
	group := []store.Message{
		{ChatID: 1, MessageID: 11, GroupedID: 700, Date: time.Unix(1, 0)},
		{ChatID: 1, MessageID: 12, GroupedID: 700, Date: time.Unix(2, 0)},
		{ChatID: 1, MessageID: 13, GroupedID: 700, Date: time.Unix(3, 0)},
	}
	missing, existing := missingMediaGroupMessages(group, map[int]int64{12: 9002})
	if len(missing) != 2 || missing[0].MessageID != 11 || missing[1].MessageID != 13 {
		t.Fatalf("missing=%+v", missing)
	}
	if existing[12] != 9002 {
		t.Fatalf("existing=%v", existing)
	}
}

func TestMarkExportsBatchStoresGroupedID(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/archive.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	_, err = s.DB().ExecContext(ctx, "INSERT INTO archive_destinations(name,kind,chat_id,created_at,updated_at) VALUES('album','telegram',100,1,1)")
	if err != nil {
		t.Fatal(err)
	}
	var dest int64
	if err = s.DB().QueryRowContext(ctx, "SELECT id FROM archive_destinations WHERE name='album'").Scan(&dest); err != nil {
		t.Fatal(err)
	}
	if err = s.UpsertMessage(ctx, store.Message{ChatID: 1, MessageID: 11, GroupedID: 700, Date: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	row, err := s.RowID(ctx, 1, 11)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkExportsBatch(ctx, []store.ExportMapping{{RowID: row, DestinationID: dest, DestinationMessageID: 501, GroupedID: 700}}); err != nil {
		t.Fatal(err)
	}
	var grouped int64
	if err = s.DB().QueryRowContext(ctx, "SELECT grouped_id FROM archive_exports WHERE message_rowid=?", row).Scan(&grouped); err != nil {
		t.Fatal(err)
	}
	if grouped != 700 {
		t.Fatalf("grouped_id=%d", grouped)
	}
}

func TestFallbackTextPrefersTextThenCaption(t *testing.T) {
	cases := []struct {
		name string
		view store.MessageView
		want string
	}{
		{"text wins", store.MessageView{Text: "  正文内容  ", Caption: "caption"}, "正文内容"},
		{"caption fallback", store.MessageView{Text: "   ", Caption: " media caption "}, "media caption"},
		{"empty", store.MessageView{Text: "", Caption: ""}, ""},
	}
	for _, tc := range cases {
		if got := fallbackText(tc.view); got != tc.want {
			t.Fatalf("%s: fallbackText=%q want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct {
		in           string
		rest, invite string
		channelID    int64
	}{
		{"@durov", "@durov", "", 0},
		{"durov", "durov", "", 0},
		{"https://t.me/durov", "durov", "", 0},
		{"t.me/durov/", "durov", "", 0},
		{"https://t.me/+5GQbFmtzAQljNzFl", "", "5GQbFmtzAQljNzFl", 0},
		{"t.me/joinchat/AbCdEf", "", "AbCdEf", 0},
		{"https://t.me/c/2226601852/10", "", "", 2226601852},
		{"-1001234567890", "-1001234567890", "", 0},
	}
	for _, tc := range cases {
		rest, invite, channelID := parseTarget(tc.in)
		if rest != tc.rest || invite != tc.invite || channelID != tc.channelID {
			t.Fatalf("parseTarget(%q) = (%q,%q,%d), want (%q,%q,%d)", tc.in, rest, invite, channelID, tc.rest, tc.invite, tc.channelID)
		}
	}
}
