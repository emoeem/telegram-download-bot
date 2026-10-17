package store

import (
	"encoding/json"
	"fmt"
	"github.com/go-ego/gse"
	"github.com/gotd/td/tg"
	"regexp"
	"strings"
	"time"
)

var segmenter = new(gse.Segmenter)
var urlRE = regexp.MustCompile("https?://[^\\s<>()]+")

func init() {
	// LoadDictEmbed("zh") uses the dictionary embedded in the gse package
	// itself: the binary stays self-contained (Docker / CGO_ENABLED=0 safe),
	// while LoadDict() reads from the Go module cache at runtime.
	_ = segmenter.LoadDictEmbed("zh")
}

type Entity struct {
	Type           string
	Offset, Length int
	Value          string
}

// Reaction is one aggregated reaction counter of a message. Custom emoji
// reactions (premium stickers) are keyed as "custom:<document-id>".
type Reaction struct {
	Emoji string
	Count int
}
type Chat struct {
	ID                                int64
	Type, Username, Title, AvatarPath string
	PhotoID                           int64
}
type User struct {
	ID                                        int64
	Username, FirstName, LastName, AvatarPath string
	PhotoID                                   int64
	IsBot                                     bool
}
type Message struct {
	ChatID                                 int64
	MessageID                              int
	SenderID                               int64
	Date                                   time.Time
	EditDate                               *time.Time
	Text, Caption                          string
	GroupedID                              int64
	ReplyTo                                int
	ReplyToTopID                           int
	ReplyToPeerID                          int64
	ThreadID                               int
	ForwardFromID                          int64
	ForwardFromChatID                      int64
	ForwardFromMessageID                   int
	ForwardFromName                        string
	ForwardPostAuthor                      string
	Views, Forwards, ReplyCount            int
	HasMedia, Pinned, Deleted              bool
	Raw                                    []byte
	Entities                               []Entity
	Reactions                              []Reaction
	MediaSHA256, MediaPath, MediaMime      string
	MediaWidth, MediaHeight, MediaDuration int
}

func (m Message) SearchText() string {
	return strings.Join(segmenter.CutSearch(strings.TrimSpace(m.Text+" "+m.Caption), true), " ")
}
func peerID(p tg.PeerClass) int64 {
	switch x := p.(type) {
	case *tg.PeerUser:
		return x.UserID
	case *tg.PeerChat:
		return x.ChatID
	case *tg.PeerChannel:
		return x.ChannelID
	}
	return 0
}

// PeerID is the exported form of the store's chat-id convention: it maps a
// Telegram peer to the same id space used in chats/messages (raw channel ids,
// positive user/chat ids).
func PeerID(p tg.PeerClass) int64 { return peerID(p) }
func FromTG(m *tg.Message) Message {
	x := Message{ChatID: peerID(m.PeerID), MessageID: m.ID, Date: time.Unix(int64(m.Date), 0), Text: m.Message, Pinned: m.Pinned}
	if id, ok := m.GetFromID(); ok {
		x.SenderID = peerID(id)
	}
	if d, ok := m.GetEditDate(); ok {
		t := time.Unix(int64(d), 0)
		x.EditDate = &t
	}
	if g, ok := m.GetGroupedID(); ok {
		x.GroupedID = g
	}
	if reply, ok := m.GetReplyTo(); ok {
		if h, ok := reply.(*tg.MessageReplyHeader); ok {
			x.ReplyTo = h.ReplyToMsgID
			x.ReplyToTopID = h.ReplyToTopID
			x.ReplyToPeerID = peerID(h.ReplyToPeerID)
			x.ThreadID = h.ReplyToTopID
		}
	}
	if f, ok := m.GetFwdFrom(); ok {
		x.ForwardFromMessageID = f.ChannelPost
		if x.ForwardFromMessageID == 0 {
			x.ForwardFromMessageID = f.SavedFromMsgID
		}
		x.ForwardFromName = f.FromName
		if x.ForwardFromName == "" {
			x.ForwardFromName = f.SavedFromName
		}
		x.ForwardPostAuthor = f.PostAuthor
		if f.SavedFromPeer != nil {
			x.ForwardFromChatID = peerID(f.SavedFromPeer)
		}
		switch p := f.FromID.(type) {
		case *tg.PeerUser:
			x.ForwardFromID = p.UserID
		case *tg.PeerChat, *tg.PeerChannel:
			if x.ForwardFromChatID == 0 {
				x.ForwardFromChatID = peerID(p)
			}
		}
	}
	if v, ok := m.GetViews(); ok {
		x.Views = v
	}
	if v, ok := m.GetForwards(); ok {
		x.Forwards = v
	}
	if r, ok := m.GetReplies(); ok {
		x.ReplyCount = r.GetReplies()
	}
	if md, ok := m.GetMedia(); ok {
		x.HasMedia = md != nil
	}
	if rs, ok := m.GetReactions(); ok {
		x.Reactions = ReactionsFromTG(&rs)
	}
	if entities, ok := m.GetEntities(); ok {
		for _, e := range entities {
			x.Entities = append(x.Entities, Entity{Type: fmt.Sprintf("%T", e), Offset: e.GetOffset(), Length: e.GetLength()})
		}
	}
	if md, ok := m.GetMedia(); ok {
		switch v := md.(type) {
		case *tg.MessageMediaPhoto:
			if p, ok := v.Photo.(*tg.Photo); ok {
				x.MediaSHA256 = fmt.Sprintf("tg-photo-%d", p.ID)
			}
		case *tg.MessageMediaDocument:
			if d, ok := v.Document.(*tg.Document); ok {
				x.MediaSHA256 = fmt.Sprintf("tg-document-%d", d.ID)
				x.MediaMime = d.MimeType
			}
		}
	}
	x.Raw, _ = json.Marshal(m)
	return x
}

// MediaMeta is everything knowable about a message's media *before* the file is
// downloaded: the archive queues on this, so it drives both the size cap and the
// browse filters.
type MediaMeta struct {
	Kind, Name, MIME, FileUniqueID string
	Size                           int64
	Width, Height, Duration        int
}

// MediaInfo reports the archive metadata of a message's media. ok is false when
// the message carries no downloadable file (geo, poll, contact, webpage, dice,
// service media, ...): queueing those would create jobs that can never succeed
// and would retry forever.
func MediaInfo(m *tg.Message) (MediaMeta, bool) {
	md, yes := m.GetMedia()
	if !yes {
		return MediaMeta{}, false
	}
	switch v := md.(type) {
	case *tg.MessageMediaPhoto:
		meta := MediaMeta{Kind: "photo"}
		if p, ok := v.Photo.(*tg.Photo); ok {
			meta.FileUniqueID = fmt.Sprintf("tg-photo-%d", p.ID)
		}
		return meta, true
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return MediaMeta{Kind: "document"}, true
		}
		meta := MediaMeta{
			Kind:         "document",
			MIME:         d.MimeType,
			Size:         int64(d.Size),
			FileUniqueID: fmt.Sprintf("tg-document-%d", d.ID),
		}
		var sticker, animated, voice, audio, video bool
		for _, a := range d.Attributes {
			switch x := a.(type) {
			case *tg.DocumentAttributeFilename:
				meta.Name = x.FileName
			case *tg.DocumentAttributeVideo:
				video = true
				meta.Width, meta.Height, meta.Duration = x.W, x.H, int(x.Duration)
			case *tg.DocumentAttributeAudio:
				meta.Duration = x.Duration
				if x.Voice {
					voice = true
				} else {
					audio = true
				}
			case *tg.DocumentAttributeSticker:
				sticker = true
			case *tg.DocumentAttributeAnimated:
				animated = true
			case *tg.DocumentAttributeImageSize:
				if meta.Width == 0 {
					meta.Width, meta.Height = x.W, x.H
				}
			}
		}
		switch {
		case sticker:
			meta.Kind = "sticker"
		case animated:
			meta.Kind = "animation"
		case voice:
			meta.Kind = "voice"
		case audio:
			meta.Kind = "audio"
		case video:
			meta.Kind = "video"
		default:
			// No attribute claimed it: fall back to the declared mime type.
			mime := strings.ToLower(d.MimeType)
			switch {
			case strings.HasPrefix(mime, "video/"):
				meta.Kind = "video"
			case strings.HasPrefix(mime, "audio/"):
				meta.Kind = "audio"
			case strings.HasPrefix(mime, "image/"):
				meta.Kind = "photo"
			}
		}
		if meta.Name == "" {
			// Documents without a filename attribute still deserve a label.
			meta.Name = fmt.Sprintf("%s-%d", meta.Kind, d.ID)
		}
		return meta, true
	}
	return MediaMeta{}, false
}

func ExtractLinks(text string) []string { return urlRE.FindAllString(text, -1) }

// ReactionsFromTG flattens a MessageReactions payload into stable (emoji,count)
// pairs. Zero-count entries are dropped: they mean the reaction was just
// removed and would otherwise resurrect an empty row.
func ReactionsFromTG(rs *tg.MessageReactions) []Reaction {
	if rs == nil {
		return nil
	}
	out := make([]Reaction, 0, len(rs.Results))
	for _, r := range rs.Results {
		emoji := reactionKey(r.Reaction)
		count := r.GetCount()
		if emoji == "" || count <= 0 {
			continue
		}
		out = append(out, Reaction{Emoji: emoji, Count: count})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reactionKey maps a ReactionClass to its stable key: an emoji string for the
// standard reactions, "custom:<document-id>" for premium custom emoji.
func reactionKey(r tg.ReactionClass) string {
	switch x := r.(type) {
	case *tg.ReactionEmoji:
		return x.Emoticon
	case *tg.ReactionCustomEmoji:
		return fmt.Sprintf("custom:%d", x.DocumentID)
	}
	return ""
}
