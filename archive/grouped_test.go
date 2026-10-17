package archive

import (
	"testing"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
)

func newAlbumMessage(channelID int64, id int, groupedID int64) *tg.Message {
	m := &tg.Message{
		ID:      id,
		PeerID:  &tg.PeerChannel{ChannelID: channelID},
		Date:    1700000000,
		Message: "photo",
	}
	m.SetGroupedID(groupedID)
	return m
}

func TestSplitGroupedUpdatesExtractsCompleteAlbum(t *testing.T) {
	album := []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: newAlbumMessage(7, 1, 55)},
		&tg.UpdateNewChannelMessage{Message: newAlbumMessage(7, 2, 55)},
		&tg.UpdateNewChannelMessage{Message: &tg.Message{ID: 3, PeerID: &tg.PeerChannel{ChannelID: 7}, Date: 1, Message: "solo"}},
	}
	filtered, groups := splitGroupedUpdates(&tg.Updates{Updates: album, Date: 1, Seq: 1})

	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if len(groups[0]) != 2 || groups[0][0].ID != 1 || groups[0][1].ID != 2 {
		t.Fatalf("album messages = %+v", groups[0])
	}

	updates, ok := filtered.(*tg.Updates)
	if !ok {
		t.Fatalf("filtered type = %T, want *tg.Updates", filtered)
	}
	if len(updates.Updates) != 1 {
		t.Fatalf("remaining updates = %d, want 1 (album removed)", len(updates.Updates))
	}
	if _, ok := updates.Updates[0].(*tg.UpdateNewChannelMessage); !ok {
		t.Fatalf("remaining update = %T", updates.Updates[0])
	}
}

// 单个 grouped_id（容器里只有一条）不算完整相册，必须原样交给 dispatcher。
func TestSplitGroupedUpdatesKeepsPartialAlbum(t *testing.T) {
	updates := []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: newAlbumMessage(7, 1, 99)},
		&tg.UpdateNewMessage{Message: &tg.Message{ID: 2, PeerID: &tg.PeerUser{UserID: 5}, Date: 1, Message: "dm"}},
	}
	in := &tg.Updates{Updates: updates, Date: 1, Seq: 1}
	filtered, groups := splitGroupedUpdates(in)
	if len(groups) != 0 {
		t.Fatalf("groups = %+v, want none", groups)
	}
	if filtered != tg.UpdatesClass(in) {
		t.Fatal("container with no complete album must be returned unchanged")
	}
}

// 两个不同 chat 使用相同 grouped_id 时不得互相串组。
func TestSplitGroupedUpdatesGroupsByChatAndGroup(t *testing.T) {
	updates := []tg.UpdateClass{
		&tg.UpdateNewChannelMessage{Message: newAlbumMessage(1, 10, 77)},
		&tg.UpdateNewChannelMessage{Message: newAlbumMessage(2, 11, 77)},
		&tg.UpdateNewChannelMessage{Message: newAlbumMessage(1, 12, 77)},
	}
	filtered, groups := splitGroupedUpdates(&tg.Updates{Updates: updates, Date: 1, Seq: 1})
	// 只有 chat 1 的 10/12 构成完整相册；chat 2 的 11 是同 grouped_id 的碎片，必须保留在流里。
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(groups))
	}
	if got := store.FromTG(groups[0][0]).ChatID; got != store.FromTG(newAlbumMessage(1, 10, 77)).ChatID {
		t.Fatalf("album chat = %d", got)
	}
	if len(groups[0]) != 2 || groups[0][0].ID != 10 || groups[0][1].ID != 12 {
		t.Fatalf("album = %+v", groups[0])
	}
	rest := filtered.(*tg.Updates)
	if len(rest.Updates) != 1 {
		t.Fatalf("remaining updates = %d, want 1 (the partial album message)", len(rest.Updates))
	}
	msg, ok := rest.Updates[0].(*tg.UpdateNewChannelMessage)
	if !ok {
		t.Fatalf("remaining update type = %T", rest.Updates[0])
	}
	if store.FromTG(msg.Message.(*tg.Message)).MessageID != 11 {
		t.Fatalf("wrong message kept: %+v", msg.Message)
	}
}

func TestSplitGroupedUpdatesIgnoresNonUpdateContainers(t *testing.T) {
	in := &tg.UpdatesCombined{Updates: nil, Date: 1, Seq: 1}
	filtered, groups := splitGroupedUpdates(in)
	if len(groups) != 0 {
		t.Fatalf("groups = %+v", groups)
	}
	if filtered != tg.UpdatesClass(in) {
		t.Fatal("empty container must be returned unchanged")
	}

	short := &tg.UpdateShortMessage{ID: 1, UserID: 2, Message: "x", Date: 1}
	filtered, groups = splitGroupedUpdates(short)
	if len(groups) != 0 || filtered != tg.UpdatesClass(short) {
		t.Fatalf("short update must be passed through: %v %v", filtered, groups)
	}
}

func TestUpdateEntitiesExtractsPeers(t *testing.T) {
	u := &tg.Updates{
		Users: []tg.UserClass{&tg.User{ID: 11, AccessHash: 22, Username: "ada", FirstName: "Ada", Photo: &tg.UserProfilePhotoEmpty{}}},
		Chats: []tg.ChatClass{&tg.Channel{ID: 33, AccessHash: 44, Title: "频道", Photo: &tg.ChatPhotoEmpty{}}},
	}
	e := updateEntities(u)
	if len(e.Users) != 1 || e.Users[11] == nil {
		t.Fatalf("users = %+v", e.Users)
	}
	if e.Users[11].AccessHash != 22 {
		t.Fatalf("user access hash = %d, want 22", e.Users[11].AccessHash)
	}
	if len(e.Channels) != 1 || e.Channels[33] == nil || e.Channels[33].AccessHash != 44 {
		t.Fatalf("channels = %+v", e.Channels)
	}

	if got := updateEntities(&tg.UpdateShortMessage{}); len(got.Users) != 0 || len(got.Chats) != 0 {
		t.Fatalf("unexpected entities for short update: %+v", got)
	}
}
