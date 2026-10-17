package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
)

type MessageEvent struct {
	ID           int64
	MessageRowID int64
	ChatID       int64
	MessageID    int
	EventType    string
	ActorID      int64
	SubjectID    int64
	PayloadJSON  string
	Date         int64
	CreatedAt    int64
}

type parsedMessageEvent struct {
	EventType string
	ActorID   int64
	SubjectID int64
	Payload   map[string]any
	Text      string
}

func MessageEventChatID(m *tg.MessageService) int64 {
	if m == nil {
		return 0
	}
	return peerID(m.PeerID)
}

func ParseMessageService(m *tg.MessageService) (MessageEvent, error) {
	if m == nil {
		return MessageEvent{}, fmt.Errorf("nil service message")
	}
	chatID := peerID(m.PeerID)
	actorID := peerID(m.FromID)
	p := parseMessageAction(m.Action, actorID)
	payload, err := json.Marshal(p.Payload)
	if err != nil {
		return MessageEvent{}, err
	}
	return MessageEvent{ChatID: chatID, MessageID: m.ID, EventType: p.EventType, ActorID: p.ActorID, SubjectID: p.SubjectID, PayloadJSON: string(payload), Date: int64(m.Date)}, nil
}

func parseMessageAction(action tg.MessageActionClass, actorID int64) parsedMessageEvent {
	p := parsedMessageEvent{EventType: "other", ActorID: actorID, Payload: map[string]any{}}
	if action == nil {
		p.Payload["type"] = "nil"
		p.Text = "未识别的系统事件"
		return p
	}
	p.Payload["type"] = action.TypeName()
	switch x := action.(type) {
	case *tg.MessageActionChatCreate:
		p.EventType, p.Text = "chat_created", "创建群组："+x.Title
		p.Payload["title"], p.Payload["users"] = x.Title, x.Users
	case *tg.MessageActionChatEditTitle:
		p.EventType, p.Text = "title_changed", "群组名称变更为："+x.Title
		p.Payload["title"] = x.Title
	case *tg.MessageActionChatEditPhoto, *tg.MessageActionChatDeletePhoto:
		p.EventType, p.Text = "photo_changed", "群组头像已变更"
	case *tg.MessageActionChatAddUser:
		p.EventType, p.Text = "user_added", "用户加入了群组"
		p.Payload["users"] = x.Users
		if len(x.Users) > 0 {
			p.SubjectID = x.Users[0]
		}
	case *tg.MessageActionChatDeleteUser:
		p.SubjectID = x.UserID
		p.Payload["user_id"] = x.UserID
		if actorID != 0 && actorID == x.UserID {
			p.EventType, p.Text = "user_left", "用户离开了群组"
		} else {
			p.EventType, p.Text = "user_removed", "用户被移出群组"
		}
	case *tg.MessageActionChatJoinedByLink:
		p.EventType, p.SubjectID, p.Text = "user_joined", actorID, "用户通过邀请链接加入了群组"
		p.Payload["inviter_id"] = x.InviterID
	case *tg.MessageActionChatJoinedByRequest:
		p.EventType, p.SubjectID, p.Text = "user_joined", actorID, "用户通过入群申请加入了群组"
	case *tg.MessageActionPinMessage:
		p.EventType, p.Text = "pinned", "消息已置顶"
	case *tg.MessageActionTopicCreate:
		p.EventType, p.Text = "topic_created", "创建了话题："+x.Title
		p.Payload["title"] = x.Title
		p.Payload["icon_color"] = x.IconColor
	case *tg.MessageActionTopicEdit:
		p.EventType, p.Text = "topic_edited", "话题信息已编辑"
		if x.Title != "" {
			p.Payload["title"] = x.Title
			p.Text = "话题标题改为：" + x.Title
		}
		if !x.Closed {
			p.Payload["closed"] = false
		} else {
			p.Payload["closed"] = true
			p.Text = "话题已关闭"
		}
	case *tg.MessageActionChannelCreate:
		p.EventType, p.Text = "channel_created", "创建频道："+x.Title
		p.Payload["title"] = x.Title
	case *tg.MessageActionChatMigrateTo:
		p.EventType, p.Text = "channel_migrated", fmt.Sprintf("群组迁移到频道 #%d", x.ChannelID)
		p.Payload["channel_id"] = x.ChannelID
	case *tg.MessageActionChannelMigrateFrom:
		p.EventType, p.Text = "channel_migrated", fmt.Sprintf("频道由群组 #%d 迁移而来", x.ChatID)
		p.Payload["chat_id"] = x.ChatID
	case *tg.MessageActionHistoryClear:
		p.EventType, p.Text = "history_cleared", "历史消息已清除"
	case *tg.MessageActionChangeCreator, *tg.MessageActionNewCreatorPending:
		p.EventType, p.Text = "admin_changed", "群组管理员/创建者发生变更"
	default:
		p.Text = "系统事件：" + action.TypeName()
	}
	return p
}

func (s *Store) SaveMessageService(ctx context.Context, m *tg.MessageService) error {
	event, err := ParseMessageService(m)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	msg := Message{ChatID: event.ChatID, MessageID: event.MessageID, SenderID: event.ActorID, Date: time.Unix(event.Date, 0), Text: eventTextWithNames(ctx, s, event)}
	msg.Raw, _ = json.Marshal(m)
	if err := s.UpsertMessage(ctx, msg); err != nil {
		return err
	}
	rowID, err := s.RowID(ctx, event.ChatID, event.MessageID)
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx, "DELETE FROM message_events WHERE message_rowid=?", rowID); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO message_events(message_rowid,chat_id,message_id,event_type,actor_id,subject_id,payload_json,date,created_at) VALUES(?,?,?,?,?,?,?,?,?)", rowID, event.ChatID, event.MessageID, event.EventType, nullableInt64(event.ActorID), nullableInt64(event.SubjectID), event.PayloadJSON, event.Date, now)
	return err
}

func eventTextWithNames(ctx context.Context, s *Store, e MessageEvent) string {
	var actor, subject string
	if e.ActorID != 0 {
		_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(TRIM(first_name||' '||last_name),''),NULLIF(username,''),?) FROM users WHERE id=?", "用户", e.ActorID).Scan(&actor)
	}
	if e.SubjectID != 0 {
		_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(NULLIF(TRIM(first_name||' '||last_name),''),NULLIF(username,''),?) FROM users WHERE id=?", "用户", e.SubjectID).Scan(&subject)
	}
	if e.EventType == "user_joined" || e.EventType == "user_left" || e.EventType == "user_removed" || e.EventType == "user_added" {
		if subject == "" {
			subject = actor
		}
		switch e.EventType {
		case "user_joined":
			return subject + " 加入了群组"
		case "user_left":
			return subject + " 离开了群组"
		case "user_removed":
			return subject + " 被移出群组"
		case "user_added":
			return subject + " 被加入了群组"
		}
	}
	var p map[string]any
	if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil {
		if title, ok := p["title"].(string); ok && title != "" {
			switch e.EventType {
			case "title_changed":
				return "群组名称变更为：" + title
			case "chat_created":
				return "创建群组：" + title
			case "channel_created":
				return "创建频道：" + title
			}
		}
	}
	return "系统事件：" + e.EventType
}

func (s *Store) ListMessageEvents(ctx context.Context, chatID int64, since, until *time.Time, limit int) ([]MessageEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := []string{"chat_id=?"}
	args := []any{chatID}
	if since != nil {
		where = append(where, "date>=?")
		args = append(args, since.Unix())
	}
	if until != nil {
		where = append(where, "date<?")
		args = append(args, until.Unix())
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, "SELECT id,message_rowid,chat_id,message_id,event_type,COALESCE(actor_id,0),COALESCE(subject_id,0),COALESCE(payload_json,''),date,created_at FROM message_events WHERE "+joinAnd(where)+" ORDER BY date DESC,id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageEvent
	for rows.Next() {
		var e MessageEvent
		if err := rows.Scan(&e.ID, &e.MessageRowID, &e.ChatID, &e.MessageID, &e.EventType, &e.ActorID, &e.SubjectID, &e.PayloadJSON, &e.Date, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) GetMessageEventByMessageRowID(ctx context.Context, rowid int64) (MessageEvent, error) {
	var e MessageEvent
	err := s.db.QueryRowContext(ctx, "SELECT id,message_rowid,chat_id,message_id,event_type,COALESCE(actor_id,0),COALESCE(subject_id,0),COALESCE(payload_json,''),date,created_at FROM message_events WHERE message_rowid=? ORDER BY id DESC LIMIT 1", rowid).Scan(&e.ID, &e.MessageRowID, &e.ChatID, &e.MessageID, &e.EventType, &e.ActorID, &e.SubjectID, &e.PayloadJSON, &e.Date, &e.CreatedAt)
	return e, err
}
func joinAnd(parts []string) string {
	if len(parts) == 0 {
		return "1=1"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += " AND " + p
	}
	return out
}
