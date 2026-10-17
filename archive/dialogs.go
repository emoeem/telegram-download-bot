package archive

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
)

// Dialog is one entry of the account's chat list: a joined channel/group or a
// private conversation, in newest-message-first Telegram order.
type Dialog struct {
	ChatID    int64  `json:"id"`
	Type      string `json:"type"` // user|chat|channel|supergroup
	Title     string `json:"title"`
	Username  string `json:"username"`
	Monitored bool   `json:"monitored"`
}

// ListDialogs enumerates up to limit dialogs of the account via paginated
// messages.getDialogs, carrying the last dialog's (date, id, peer) cursor.
func ListDialogs(ctx context.Context, rt *Runtime, limit int) ([]Dialog, error) {
	if limit <= 0 {
		limit = 200
	}
	out := make([]Dialog, 0, 64)
	var (
		offsetDate int
		offsetID   int
		offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	)
	for len(out) < limit {
		res, err := rt.API.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      100,
		})
		if err != nil {
			return out, err
		}
		var (
			dialogs []tg.DialogClass
			rawMsgs []tg.MessageClass
			rawUser []tg.UserClass
			rawChat []tg.ChatClass
		)
		switch x := res.(type) {
		case *tg.MessagesDialogs:
			dialogs, rawMsgs, rawUser, rawChat = x.Dialogs, x.Messages, x.Users, x.Chats
		case *tg.MessagesDialogsSlice:
			dialogs, rawMsgs, rawUser, rawChat = x.Dialogs, x.Messages, x.Users, x.Chats
		case *tg.MessagesDialogsNotModified:
			return out, nil
		default:
			return out, fmt.Errorf("unexpected getDialogs response %T", res)
		}
		userOf := map[int64]*tg.User{}
		for _, u := range rawUser {
			if uu, ok := u.(*tg.User); ok {
				userOf[uu.ID] = uu
			}
		}
		chatOf := map[int64]*tg.Chat{}
		channelOf := map[int64]*tg.Channel{}
		for _, c := range rawChat {
			switch ch := c.(type) {
			case *tg.Chat:
				chatOf[ch.ID] = ch
			case *tg.Channel:
				channelOf[ch.ID] = ch
			}
		}
		dateOf := map[int]int{}
		for _, m := range rawMsgs {
			if mm, ok := m.(*tg.Message); ok {
				dateOf[mm.ID] = mm.Date
			}
		}
		if len(dialogs) == 0 {
			break
		}
		progressed := false
		for _, dcl := range dialogs {
			d, ok := dcl.(*tg.Dialog)
			if !ok {
				continue
			}
			switch p := d.Peer.(type) {
			case *tg.PeerUser:
				u := userOf[p.UserID]
				if u == nil {
					continue
				}
				title := strings.TrimSpace(u.FirstName + " " + u.LastName)
				switch {
				case title == "" && u.Username != "":
					title = u.Username
				case title == "":
					title = fmt.Sprintf("用户 %d", p.UserID)
				}
				out = append(out, Dialog{ChatID: p.UserID, Type: "user", Title: title, Username: u.Username})
				offsetPeer = &tg.InputPeerUser{UserID: p.UserID, AccessHash: u.AccessHash}
			case *tg.PeerChat:
				c := chatOf[p.ChatID]
				if c == nil {
					continue
				}
				title := c.Title
				if title == "" {
					title = fmt.Sprintf("群组 %d", p.ChatID)
				}
				out = append(out, Dialog{ChatID: p.ChatID, Type: "chat", Title: title})
				offsetPeer = &tg.InputPeerChat{ChatID: p.ChatID}
			case *tg.PeerChannel:
				ch := channelOf[p.ChannelID]
				if ch == nil {
					continue
				}
				typ := "channel"
				if !ch.Broadcast {
					typ = "supergroup"
				}
				title := ch.Title
				if title == "" {
					title = fmt.Sprintf("%s %d", typ, p.ChannelID)
				}
				out = append(out, Dialog{ChatID: p.ChannelID, Type: typ, Title: title, Username: ch.Username})
				offsetPeer = &tg.InputPeerChannel{ChannelID: p.ChannelID, AccessHash: ch.AccessHash}
			}
			offsetID = d.TopMessage
			offsetDate = dateOf[d.TopMessage]
			progressed = true
			if len(out) >= limit {
				break
			}
		}
		if !progressed || len(dialogs) < 100 {
			break
		}
	}
	return out, nil
}
