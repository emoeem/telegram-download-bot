package store

import (
	"encoding/json"
	"fmt"
	"github.com/go-ego/gse"
	"github.com/gotd/td/tg"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var segmenter = new(gse.Segmenter)

// urlRE matches a URL as it appears in channel prose. The second alternative
// exists because a t.me post link is very often written bare ("看这个 t.me/x/1"),
// and only a scheme-bearing URL would otherwise be extracted — the link a
// channel quotes is precisely the shape that arrives without "https://".
var urlRE = regexp.MustCompile(`(?:https?://|\b(?:t|telegram)\.me/)[A-Za-z0-9\-._~:/?#\[\]@!$&'*+,;=%]+`)

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

// InlineButton is one display-only inline keyboard button extracted from the
// raw message at ingest time, so views can show what was clickable.
type InlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"`
	Kind string `json:"kind"` // url | callback
}

type Message struct {
	ChatID                      int64
	MessageID                   int
	SenderID                    int64
	Date                        time.Time
	EditDate                    *time.Time
	Text, Caption               string
	GroupedID                   int64
	ReplyTo                     int
	ReplyToTopID                int
	ReplyToPeerID               int64
	ThreadID                    int
	ForwardFromID               int64
	ForwardFromChatID           int64
	ForwardFromMessageID        int
	ForwardFromName             string
	ForwardPostAuthor           string
	Views, Forwards, ReplyCount int
	HasMedia, Pinned, Deleted   bool
	// Out distinguishes what this account sent from what it received. The
	// receive ledger numbers inbound items first, and an operator reading the
	// archive needs to see the direction without cross-referencing sender ids.
	Out                                    bool
	Raw                                    []byte
	Entities                               []Entity
	Reactions                              []Reaction
	Buttons                                []InlineButton
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
	x := Message{ChatID: peerID(m.PeerID), MessageID: m.ID, Date: time.Unix(int64(m.Date), 0), Text: m.Message, Pinned: m.Pinned, Out: m.Out}
	// MTProto puts the words of a media message in Message, exactly like a plain
	// post: the field name is the only thing that changes. Splitting it into
	// Caption at ingest time is what lets a material treat "body" and "image
	// caption" as the distinct things they are — and it must happen here,
	// because the upsert overwrites text/caption on every edit, so a later
	// pass cannot recover the distinction from a merged value.
	if _, ok := m.GetMedia(); ok {
		x.Caption, x.Text = m.Message, ""
	}
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
	// Direct field access: GetEntities() keys off the wire flags, which are
	// absent on values rebuilt in tests, and the field itself is authoritative.
	if entities := m.Entities; len(entities) > 0 {
		for _, e := range entities {
			ent := Entity{Type: fmt.Sprintf("%T", e), Offset: e.GetOffset(), Length: e.GetLength()}
			// A text_link pill — "👉一键查询验证视频👈" and its relatives — keeps
			// its destination in the entity, not in the body. Storing only the
			// type throws the target away and the button becomes undisplayable
			// and unclickable forever.
			if u, ok := e.(*tg.MessageEntityTextURL); ok {
				ent.Value = u.URL
			}
			x.Entities = append(x.Entities, ent)
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
	x.Buttons = ButtonsFromTG(m)
	return x
}

// ButtonsFromTG extracts the display form of an inline keyboard: URL buttons
// keep their link, everything else (callback, switch-inline, buy) renders as a
// plain button label.
func ButtonsFromTG(m *tg.Message) []InlineButton {
	inline, ok := m.ReplyMarkup.(*tg.ReplyInlineMarkup)
	if !ok || inline == nil {
		return nil
	}
	var out []InlineButton
	for _, row := range inline.Rows {
		for _, b := range row.Buttons {
			btn := InlineButton{Kind: "callback"}
			switch v := b.(type) {
			case *tg.KeyboardButtonURL:
				btn = InlineButton{Kind: "url", URL: v.URL}
			case *tg.KeyboardButtonURLAuth:
				btn = InlineButton{Kind: "url", URL: v.URL}
			}
			btn.Text = b.GetText()
			if btn.Text == "" {
				continue
			}
			out = append(out, btn)
		}
	}
	return out
}

// MediaMeta is everything knowable about a message's media *before* the file is
// downloaded: the archive queues on this, so it drives both the size cap and the
// browse filters.
type MediaMeta struct {
	Kind, Name, MIME, FileUniqueID string
	// URL is the external source of a Kind=="link" entry: a post whose own
	// media is only a preview of a video hosted somewhere else. Telegram gives
	// us the page, not the bytes, so this URL is what the downloader is handed.
	URL                     string
	Size                    int64
	Width, Height, Duration int
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
			// A photo payload says nothing about its dimensions directly: they
			// live on the renditions Telegram generated. Without this the whole
			// photo branch archives width/height NULL, and the browse grid has
			// no aspect ratio to reserve space with, so every thumbnail that
			// arrives reflows the page.
			meta.Width, meta.Height = LargestPhotoSize(p.Sizes)
		}
		return meta, true
	case *tg.MessageMediaDocument:
		d, ok := v.Document.(*tg.Document)
		if !ok {
			return MediaMeta{Kind: "document"}, true
		}
		return docMeta(d)
	case *tg.MessageMediaWebPage:
		wp, ok := v.Webpage.(*tg.WebPage)
		if !ok {
			return MediaMeta{}, false
		}
		// A page that embeds its *own* document is an ordinary Telegram file
		// wearing a preview: take the bytes we already know how to fetch
		// instead of handing the URL to an external extractor.
		if dc, ok := wp.GetDocument(); ok {
			if d, isDoc := dc.(*tg.Document); isDoc {
				if dm, ok := docMeta(d); ok {
					return dm, true
				}
			}
		}
		rawURL := wp.GetURL()
		pageType, _ := wp.GetType()
		if !IsVideoLink(rawURL, pageType) {
			return MediaMeta{}, false
		}
		meta := MediaMeta{Kind: "link", URL: rawURL, Name: rawURL}
		if title, ok := wp.GetTitle(); ok && title != "" {
			meta.Name = title
		}
		if d, ok := wp.GetDuration(); ok {
			meta.Duration = d
		}
		return meta, true
	}
	return MediaMeta{}, false
}

// LargestPhotoSize returns the pixel dimensions of a photo's biggest rendition.
//
// A tg.Photo carries every rendition Telegram generated ("m" 320x180 up to "w"
// 2560x1440), not the original alone. The archive records the largest one: the
// browse grid needs it to reserve layout space before the bytes arrive, and
// using a thumbnail's size would make each image jump when the real file loads.
//
// The slice arrives smallest-first in practice, but that ordering is not part of
// the API contract, so this compares areas rather than trusting the last element.
// PhotoStrippedSize carries a decodable placeholder instead of a rendition with
// known dimensions and is skipped; a photo whose only sizes are stripped yields
// (0, 0), which the store writes as NULL rather than as a wrong 0x0.
func LargestPhotoSize(sizes []tg.PhotoSizeClass) (int, int) {
	w, h := 0, 0
	for _, s := range sizes {
		var sw, sh int
		switch x := s.(type) {
		case *tg.PhotoSize:
			sw, sh = x.W, x.H
		case *tg.PhotoSizeProgressive:
			sw, sh = x.W, x.H
		case *tg.PhotoCachedSize:
			sw, sh = x.W, x.H
		default:
			continue
		}
		// Degenerate renditions exist (a sticker sent as a photo reports 0x0);
		// they must not win the comparison nor be reported as a real size.
		if sw <= 0 || sh <= 0 {
			continue
		}
		if sw*sh > w*h {
			w, h = sw, sh
		}
	}
	return w, h
}

// linkVideoHosts are the sites yt-dlp has a maintained extractor for and that
// actually get posted to Telegram channels. The list exists to keep the queue
// clean, not to enumerate yt-dlp's coverage: a miss only costs a video that is
// archived as a link instead of as a file.
var linkVideoHosts = []string{
	"youtube.com", "youtu.be", "youtube-nocookie.com",
	"bilibili.com", "b23.tv", "acfun.cn",
	"twitter.com", "x.com",
	"tiktok.com", "douyin.com", "kuaishou.com", "ixigua.com",
	"vimeo.com", "dailymotion.com", "twitch.tv", "nicovideo.jp",
	"instagram.com", "facebook.com", "fb.watch", "reddit.com", "v.redd.it",
	"weibo.com", "weibo.cn", "xiaohongshu.com",
	"streamable.com", "rumble.com", "odysee.com", "bitchute.com",
}

// IsVideoLink reports whether an external page is worth handing to the video
// downloader. Telegram's own page classification ("video", "gif") is trusted
// first because it is computed server-side; the host list is the fallback for
// pages Telegram classified as a plain article.
func IsVideoLink(rawURL, pageType string) bool {
	switch pageType {
	case "video", "gif":
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host == "" {
		return false
	}
	for _, known := range linkVideoHosts {
		if host == known || strings.HasSuffix(host, "."+known) {
			return true
		}
	}
	return false
}

// linkTrailingPunct is what a URL picks up from the prose around it. The
// extractor stops at whitespace, so "看这个 https://youtu.be/x。" arrives with
// the sentence's full stop attached and would 404.
const linkTrailingPunct = ".,;:!?)]}>'\"…、，。；：！？）】》”’"

// clipURL cuts a URL at the first byte that cannot appear in one.
//
// The extractor's pattern stops only at whitespace, which is wrong for Chinese
// prose: "https://youtu.be/abc，还有这个" has no space, so the sentence rides
// along inside the URL. Only RFC 3986 characters survive here, and because
// every CJK codepoint is multi-byte UTF-8 (first byte >= 0x80), any Chinese
// text — comma, word, anything — terminates the URL at the right place.
func clipURL(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("-._~:/?#[]@!$&'()*+,;=%", c) >= 0:
		default:
			return s[:i]
		}
	}
	return s
}

// canonicalLinkURL reduces the spellings of one page to a single queue key.
//
// A channel pastes the same video as "bilibili.com/x", "www.bilibili.com/x" and
// "https://www.bilibili.com/x/" — three strings, one page. Matching on the raw
// text queues it three times and stores three rows for one file. Only the parts
// that cannot change identity are normalised: scheme, "www." and a trailing
// slash. The path, query and case of everything after the host are left alone.
func canonicalLinkURL(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return rawURL
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host == "" {
		return rawURL
	}
	port := ""
	if p := u.Port(); p != "" && p != "443" && p != "80" {
		port = ":" + p
	}
	path := u.Path
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	if path == "" {
		path = "/"
	}
	out := "https://" + host + port + path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// LinkMedias reports the external videos a message's own words point at.
//
// MediaInfo only sees the Telegram media payload, and Telegram only builds a
// MessageMediaWebPage when it decided to render a preview. A channel with
// previews off — or one whose preview fetch failed server-side — leaves the
// URL nowhere but Message.text, so following MediaInfo alone archives the
// sentence and silently drops the video it advertises. This reads the text.
//
// One entry per distinct video URL, in posting order. Hosts yt-dlp has no
// extractor for are skipped rather than queued: a job that can only ever fail
// is worse than a link that stays a link, and message_links still records it.
func LinkMedias(m *tg.Message) []MediaMeta {
	if !strings.Contains(m.Message, "://") &&
		!strings.Contains(m.Message, "t.me/") &&
		!strings.Contains(m.Message, "telegram.me/") {
		return nil
	}
	seen := make(map[string]bool, 2)
	var out []MediaMeta
	for _, raw := range ExtractLinks(m.Message) {
		u := strings.TrimRight(clipURL(raw), linkTrailingPunct)
		// A t.me post link quotes another chat's message rather than pointing
		// at an external site: the bytes live behind the Telegram API, not
		// behind yt-dlp, so it gets its own job kind.
		if ref, ok := ParseTMeMessage(u); ok {
			key := ref.TMeURL()
			if seen[key] {
				continue
			}
			seen[key] = true
			// The link is the identity: the same quoted post re-posted in ten
			// channels is one file, and FindReusableMedia keys on this column.
			out = append(out, MediaMeta{Kind: KindTMe, URL: key, Name: key, FileUniqueID: key})
			continue
		}
		canon := canonicalLinkURL(u)
		if seen[canon] || !IsVideoLink(canon, "") {
			continue
		}
		seen[canon] = true
		out = append(out, MediaMeta{Kind: "link", URL: canon, Name: canon})
	}
	return out
}

// docMeta classifies a document payload. Kept separate from MediaInfo so the
// webpage branch can reuse it: a page that embeds its own file is a document
// that happens to be wrapped in a preview, and must take the same path.
func docMeta(d *tg.Document) (MediaMeta, bool) {
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
