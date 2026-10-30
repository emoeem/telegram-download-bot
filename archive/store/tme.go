package store

import (
	"strconv"
	"strings"
)

// TMeRef is a t.me link that points at one specific message of another chat:
// the shape a channel uses to quote somebody else's post.
//
// Only message links (a trailing message id) parse. A bare channel link is
// rejected on purpose: following one would let a single archived sentence
// trigger a full channel ingest, which is a far larger action than the message
// asked for. Bare channel links stay in message_links as URL text, and adding
// one as a collection target stays an explicit user decision.
type TMeRef struct {
	// Username is the public chat name of t.me/<username>/<id> (no "@").
	Username string
	// ChannelID is the internal id of t.me/c/<id>/<msg>, which is what a
	// private channel's "copy link" produces. It is the positive id; the
	// Bot-API-style -100 prefix is added when the peer is built.
	ChannelID int64
	MessageID int
}

// IsPrivate reports whether the link addresses a private channel by internal id
// rather than by public username.
func (r TMeRef) IsPrivate() bool { return r.ChannelID != 0 }

// TMeURL renders the reference as the canonical link. Two spellings of the same
// post ("t.me/x/1" and "https://t.me/x/1/") must collapse to one queue key, or
// the same video is fetched twice and stored as two rows.
func (r TMeRef) TMeURL() string {
	var b strings.Builder
	b.WriteString("https://t.me/")
	if r.IsPrivate() {
		b.WriteString("c/")
		b.WriteString(strconv.FormatInt(r.ChannelID, 10))
	} else {
		b.WriteString(r.Username)
	}
	b.WriteByte('/')
	b.WriteString(strconv.Itoa(r.MessageID))
	return b.String()
}

// tmeReserved are first path segments that are t.me features rather than chat
// usernames. "c" and "s" are handled separately because they can still lead to
// a message link.
var tmeReserved = map[string]bool{
	"+": true, "joinchat": true, "addstickers": true, "addemoji": true,
	"addtheme": true, "share": true, "iv": true,
	"proxy": true, "socks": true, "setlanguage": true, "bg": true,
	"invoice": true, "login": true, "confirmphone": true, "contact": true,
	"telegram": true, "admin": true, "poll": true, "boost": true,
	"giftcode": true, "nft": true, "service": true, "restore": true,
}

// ParseTMeMessage parses a t.me message link in any of the spellings Telegram
// and channel authors actually use, and reports whether it addresses one post.
//
// Accepted:
//
//	t.me/<user>/<id>            https://t.me/<user>/<id>
//	t.me/s/<user>/<id>          (the web-preview spelling)
//	t.me/c/<channel>/<id>       (a private channel's internal link)
//	telegram.me/<user>/<id>
//
// Rejected: invite links (they would require joining), bot links with a start
// payload (a bot is not content), sticker/theme/article pages, bare channel
// links, and anything whose trailing segment is not a positive message id.
func ParseTMeMessage(raw string) (TMeRef, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return TMeRef{}, false
	}
	// Scheme and host are case-insensitive; the path is not.
	lower := strings.ToLower(s)
	trimmed := false
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(lower, prefix) {
			s, lower = s[len(prefix):], lower[len(prefix):]
			trimmed = true
			break
		}
	}
	_ = trimmed
	var rest string
	for _, host := range []string{"t.me/", "telegram.me/", "telegram.dog/"} {
		if strings.HasPrefix(lower, host) {
			rest = s[len(host):]
			break
		}
	}
	if rest == "" {
		return TMeRef{}, false
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return TMeRef{}, false
	}
	parts := strings.Split(rest, "/")
	for i := range parts {
		if parts[i] == "" {
			return TMeRef{}, false
		}
	}

	switch parts[0] {
	case "c":
		if len(parts) != 3 {
			return TMeRef{}, false
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return TMeRef{}, false
		}
		msg, err := strconv.Atoi(parts[2])
		if err != nil || msg <= 0 {
			return TMeRef{}, false
		}
		return TMeRef{ChannelID: id, MessageID: msg}, true
	case "s":
		// /s/ is the static web preview; the link still names a real post.
		if len(parts) != 3 {
			return TMeRef{}, false
		}
		parts = parts[1:]
	}
	if len(parts) != 2 {
		return TMeRef{}, false
	}
	if tmeReserved[strings.ToLower(parts[0])] {
		return TMeRef{}, false
	}
	msg, err := strconv.Atoi(parts[1])
	if err != nil || msg <= 0 {
		return TMeRef{}, false
	}
	return TMeRef{Username: parts[0], MessageID: msg}, true
}

// KindTMe is the media job type of a link that quotes another Telegram chat's
// post (t.me/<user>/<id>, t.me/c/<id>/<id>).
//
// It is deliberately not "link": a link job is handed to an external extractor
// (yt-dlp) because the bytes live on some other website, while this one is
// answered by the Telegram API itself — resolve the peer, read the post, take
// its attachment. Same destination, completely different machinery.
const KindTMe = "tme"
