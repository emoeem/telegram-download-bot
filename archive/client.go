package archive

import (
	"context"
	"fmt"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive/store"
	cstorage "github.com/iyear/tdl/core/storage"
	tclientcore "github.com/iyear/tdl/core/tclient"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/spf13/viper"
	"log/slog"
	"strconv"
	"strings"
)

type Runtime struct {
	Client *telegram.Client
	API    *tg.Client
	Peers  *peers.Manager
	Gaps   *updates.Manager
	Ready  chan struct{}
}

type updateGroupKey struct {
	chatID    int64
	groupedID int64
}

func updateEntities(u tg.UpdatesClass) tg.Entities {
	switch x := u.(type) {
	case *tg.Updates:
		chats := x.MapChats()
		return tg.Entities{Users: x.MapUsers().NotEmptyToMap(), Chats: chats.ChatToMap(), Channels: chats.ChannelToMap()}
	case *tg.UpdatesCombined:
		chats := x.MapChats()
		return tg.Entities{Users: x.MapUsers().NotEmptyToMap(), Chats: chats.ChatToMap(), Channels: chats.ChannelToMap()}
	default:
		return tg.Entities{}
	}
}

// splitGroupedUpdates extracts only complete grouped_id sets present in the same
// Telegram update container. No time window is guessed: grouping is based solely
// on Telegram's grouped_id and the update package boundary.
func splitGroupedUpdates(u tg.UpdatesClass) (tg.UpdatesClass, [][]*tg.Message) {
	var updates []tg.UpdateClass
	switch x := u.(type) {
	case *tg.Updates:
		updates = x.Updates
	case *tg.UpdatesCombined:
		updates = x.Updates
	default:
		return u, nil
	}
	groups := make(map[updateGroupKey][]*tg.Message)
	order := make([]updateGroupKey, 0)
	for _, update := range updates {
		var msg *tg.Message
		switch x := update.(type) {
		case *tg.UpdateNewMessage:
			msg, _ = x.Message.(*tg.Message)
		case *tg.UpdateNewChannelMessage:
			msg, _ = x.Message.(*tg.Message)
		}
		if msg == nil {
			continue
		}
		m := store.FromTG(msg)
		if m.GroupedID == 0 {
			continue
		}
		key := updateGroupKey{chatID: m.ChatID, groupedID: m.GroupedID}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], msg)
	}
	complete := make(map[updateGroupKey]bool)
	var outGroups [][]*tg.Message
	for _, key := range order {
		if len(groups[key]) >= 2 {
			complete[key] = true
			outGroups = append(outGroups, groups[key])
		}
	}
	if len(outGroups) == 0 {
		return u, nil
	}
	filtered := make([]tg.UpdateClass, 0, len(updates))
	for _, update := range updates {
		var msg *tg.Message
		switch x := update.(type) {
		case *tg.UpdateNewMessage:
			msg, _ = x.Message.(*tg.Message)
		case *tg.UpdateNewChannelMessage:
			msg, _ = x.Message.(*tg.Message)
		}
		if msg != nil {
			m := store.FromTG(msg)
			if complete[updateGroupKey{chatID: m.ChatID, groupedID: m.GroupedID}] {
				continue
			}
		}
		filtered = append(filtered, update)
	}
	switch x := u.(type) {
	case *tg.Updates:
		y := *x
		y.Updates = filtered
		return &y, outGroups
	case *tg.UpdatesCombined:
		y := *x
		y.Updates = filtered
		return &y, outGroups
	}
	return u, nil
}

func NewRuntime(ctx context.Context, kvd cstorage.Storage, appID int, appHash string, onUpdate func(context.Context, tg.Entities, *tg.Message) error, onGroup func(context.Context, tg.Entities, []*tg.Message) error, onService func(context.Context, tg.Entities, *tg.MessageService) error, onEdit func(context.Context, tg.Entities, *tg.Message) error, onDelete func(context.Context, int64, []int) error, onReactions func(context.Context, *tg.UpdateMessageReactions) error) (*Runtime, error) {
	d := tg.NewUpdateDispatcher()
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		switch m := u.Message.(type) {
		case *tg.Message:
			return onUpdate(ctx, e, m)
		case *tg.MessageService:
			if onService != nil {
				return onService(ctx, e, m)
			}
		}
		return nil
	})
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		switch m := u.Message.(type) {
		case *tg.Message:
			return onUpdate(ctx, e, m)
		case *tg.MessageService:
			if onService != nil {
				return onService(ctx, e, m)
			}
		}
		return nil
	})
	d.OnEditMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditMessage) error {
		m, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}
		return onEdit(ctx, e, m)
	})
	d.OnEditChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateEditChannelMessage) error {
		m, ok := u.Message.(*tg.Message)
		if !ok {
			return nil
		}
		return onEdit(ctx, e, m)
	})
	d.OnDeleteMessages(func(ctx context.Context, e tg.Entities, u *tg.UpdateDeleteMessages) error {
		return onDelete(ctx, 0, u.Messages)
	})
	d.OnDeleteChannelMessages(func(ctx context.Context, e tg.Entities, u *tg.UpdateDeleteChannelMessages) error {
		return onDelete(ctx, u.ChannelID, u.Messages)
	})
	d.OnMessageReactions(func(ctx context.Context, e tg.Entities, u *tg.UpdateMessageReactions) error {
		if onReactions == nil {
			return nil
		}
		return onReactions(ctx, u)
	})
	var h telegram.UpdateHandler
	client, err := tclientcore.New(ctx, tclientcore.Options{AppID: appID, AppHash: appHash, Session: cstorage.NewSession(kvd, false), Proxy: viper.GetString(consts.FlagProxy), ProxyInsecure: viper.GetBool(consts.FlagProxyInsecure), ReconnectTimeout: viper.GetDuration(consts.FlagReconnectTimeout), UpdateHandler: telegram.UpdateHandlerFunc(func(ctx context.Context, u tg.UpdatesClass) error {
		if h == nil {
			return nil
		}
		filtered, groups := splitGroupedUpdates(u)
		if onGroup != nil {
			e := updateEntities(u)
			for _, group := range groups {
				if err := onGroup(ctx, e, group); err != nil {
					return err
				}
			}
		}
		if len(groups) > 0 {
			switch x := filtered.(type) {
			case *tg.Updates:
				if len(x.Updates) == 0 {
					return nil
				}
			case *tg.UpdatesCombined:
				if len(x.Updates) == 0 {
					return nil
				}
			}
		}
		return h.Handle(ctx, filtered)
	}), Middlewares: nil})
	if err != nil {
		return nil, err
	}
	pm := peers.Options{Storage: cstorage.NewPeers(kvd)}.Build(client.API())
	gaps := updates.New(updates.Config{Handler: d, AccessHasher: pm, Storage: cstorage.NewState(kvd)})
	h = pm.UpdateHook(gaps)
	return &Runtime{Client: client, API: client.API(), Peers: pm, Gaps: gaps, Ready: make(chan struct{})}, nil
}
func (r *Runtime) Run(ctx context.Context) error {
	return r.Client.Run(ctx, func(ctx context.Context) error {
		if err := r.Peers.Init(ctx); err != nil {
			return err
		}
		self, err := r.Client.Self(ctx)
		if err != nil {
			return err
		}
		isBot := self.Bot
		if isBot {
			return fmt.Errorf("archive requires a Telegram user account, not a bot")
		}
		close(r.Ready)
		return r.Gaps.Run(ctx, r.API, self.ID, updates.AuthOptions{IsBot: false})
	})
}

// Resolve turns user-supplied input into a peer. Supported forms:
// @username, bare username, numeric id (including -100xxx), t.me/<username>,
// t.me/c/<internal-id> and private invite links t.me/+<hash> / t.me/joinchat/<hash>
// (resolvable when the account has already joined that chat).
func Resolve(ctx context.Context, rt *Runtime, target string) (peers.Peer, error) {
	pm := rt.Peers
	rest, inviteHash, channelID := parseTarget(target)
	if inviteHash != "" {
		return resolveInvite(ctx, rt, inviteHash)
	}
	if channelID != 0 {
		return pm.ResolveChannelID(ctx, channelID)
	}
	target = strings.TrimSpace(strings.TrimPrefix(rest, "@"))
	if target == "" {
		return nil, fmt.Errorf("empty target")
	}
	if p, err := pm.Resolve(ctx, target); err == nil {
		return p, nil
	}
	var id int64
	if _, err := fmt.Sscan(target, &id); err != nil {
		return nil, fmt.Errorf("cannot resolve target %q", target)
	}
	if id < 0 {
		id = -id
		if id >= 1000000000000 {
			id -= 1000000000000
			return pm.ResolveChannelID(ctx, id)
		}
		return pm.ResolveChatID(ctx, id)
	}
	if p, err := pm.ResolveChannelID(ctx, id); err == nil {
		return p, nil
	}
	p, err := pm.ResolveUserID(ctx, id)
	if err != nil {
		// 纯数字 ID 解析用户时，缓存里没有 access_hash 就会发
		// InputUser{AccessHash:0}，服务器回一个空结果。原始错误
		// （got empty result for InputUser{...}）对用户毫无意义，
		// 换成能照着做的提示，同时用 %w 保留原因给日志。
		return nil, fmt.Errorf("无法按 ID 解析聊天 %d：请改用 @用户名，或从「从我的聊天列表选择」重新选一次（%w）", id, err)
	}
	return p, nil
}

// ResolveChatID resolves a stored chat id (user/basic group/channel) through
// the peers cache, which persists access hashes in the archive KV store.
func ResolveChatID(ctx context.Context, rt *Runtime, id int64) (peers.Peer, error) {
	if p, err := rt.Peers.ResolveChannelID(ctx, id); err == nil {
		return p, nil
	}
	if p, err := rt.Peers.ResolveChatID(ctx, id); err == nil {
		return p, nil
	}
	return rt.Peers.ResolveUserID(ctx, id)
}

// ResolveChatWithUsername resolves a chat by id and, when that fails, by its
// public username.
//
// Bots are the reason this exists. A bot chat is a *tg.PeerUser, and Telegram
// does not repeat the bot's User object on every update, so the archive never
// learns its access_hash. peers.ResolveUserID then sends
// InputUser{AccessHash:0} and the server answers 400 CONNECTION_LAYER_INVALID —
// which is what made every media job of a monitored bot fail with "get users".
// contacts.resolveUsername needs no hash, and gotd writes the resolved peer
// into its own storage, so one successful call here also unblocks the plain
// resolve-by-id path for the rest of the process lifetime.
func ResolveChatWithUsername(ctx context.Context, rt *Runtime, id int64, username string) (peers.Peer, error) {
	p, err := ResolveChatID(ctx, rt, id)
	if err == nil {
		return p, nil
	}
	if strings.TrimSpace(username) == "" {
		return nil, err
	}
	q, uerr := rt.Peers.ResolveDomain(ctx, username)
	if uerr != nil {
		slog.WarnContext(ctx, "archive resolve: username fallback failed",
			"chat", id, "username", username, "error", uerr)
		// Report the id-based failure: it names the peer the caller asked for.
		return nil, err
	}
	return q, nil
}

// parseTarget extracts a resolvable token from raw input, handling t.me link
// shapes: it returns the remaining username/id token, a chat-invite hash when
// the link is a private invite, or a private channel internal id for t.me/c/.
func parseTarget(s string) (rest, inviteHash string, channelID int64) {
	s = strings.TrimSpace(s)
	for _, prefix := range []string{"https://t.me/", "http://t.me/", "https://telegram.me/", "http://telegram.me/", "t.me/", "telegram.me/"} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	s = strings.Trim(s, "/? ")
	switch {
	case strings.HasPrefix(s, "+"):
		return "", strings.TrimPrefix(s, "+"), 0
	case strings.HasPrefix(s, "joinchat/"):
		return "", strings.Trim(strings.TrimPrefix(s, "joinchat/"), "/"), 0
	case strings.HasPrefix(s, "c/"):
		parts := strings.Split(strings.TrimPrefix(s, "c/"), "/")
		id, _ := strconv.ParseInt(parts[0], 10, 64)
		return "", "", id
	}
	return s, "", 0
}

// resolveInvite resolves a private chat by its invite hash. The server only
// reveals the chat to accounts that already joined it; others get a clear
// pointer to the dialog list instead of joining silently.
func resolveInvite(ctx context.Context, rt *Runtime, hash string) (peers.Peer, error) {
	inv, err := rt.API.MessagesCheckChatInvite(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("check invite link: %w", err)
	}
	var chats []tg.ChatClass
	switch x := inv.(type) {
	case *tg.ChatInviteAlready:
		chats = append(chats, x.Chat)
	case *tg.ChatInvitePeek:
		chats = append(chats, x.Chat)
	case *tg.ChatInvite:
		return nil, fmt.Errorf("该邀请链接对应的聊天你尚未加入：请先在 Telegram 中加入，再从「从我的聊天列表选择」中选取")
	default:
		return nil, fmt.Errorf("unexpected invite response %T", inv)
	}
	if err := rt.Peers.Apply(ctx, nil, chats); err != nil {
		return nil, err
	}
	for _, c := range chats {
		switch ch := c.(type) {
		case *tg.Channel:
			return rt.Peers.ResolveChannelID(ctx, ch.ID)
		case *tg.Chat:
			return rt.Peers.ResolveChatID(ctx, ch.ID)
		}
	}
	return nil, fmt.Errorf("invite did not contain a resolvable chat")
}
