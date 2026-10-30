package archive

import (
	"fmt"
	"strconv"
	"strings"
)

// Deep links are how a bot conversation gets started on purpose instead of by
// hand: https://t.me/<bot>?start=<payload>. The payload survives the whole
// round trip — the user taps a link carrying a receive number, the bot answers,
// the answer is archived and numbered. Encoding the number into the payload is
// what lets the archive correlate "I asked for #1337" with "this is what came
// back", instead of treating the reply as an anonymous new item.
//
// Telegram restricts ?start= to A-Za-z0-9_- with a 64 character cap, so the
// number is carried as base36 with a one-letter prefix that marks it as ours.

// startPayloadPrefix marks a payload as an archive receive number. A different
// prefix (or none) is a foreign payload and is left alone.
const startPayloadPrefix = "r"

// StartPayload renders a receive number as a Telegram-safe start payload.
func StartPayload(serial int64) string {
	if serial <= 0 {
		return ""
	}
	return startPayloadPrefix + strconv.FormatInt(serial, 36)
}

// ParseStartPayload recovers the receive number from a start payload. ok=false
// means the payload is not one of ours (or is malformed), not that it is invalid
// for Telegram.
func ParseStartPayload(payload string) (int64, bool) {
	payload = strings.TrimSpace(payload)
	if !strings.HasPrefix(payload, startPayloadPrefix) || len(payload) <= len(startPayloadPrefix) {
		return 0, false
	}
	n, err := strconv.ParseInt(payload[len(startPayloadPrefix):], 36, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// BotUsername normalizes a bot reference to a bare username: accepts "@name",
// "name", "t.me/name" and "https://t.me/name?start=...".
func BotUsername(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "t.me/")
	s = strings.TrimPrefix(s, "telegram.me/")
	if i := strings.IndexAny(s, "?/"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "@")
}

// BuildStartLink renders the tappable deep link for a bot + payload.
func BuildStartLink(bot, payload string) string {
	bot = BotUsername(bot)
	if bot == "" {
		return ""
	}
	if payload == "" {
		return "https://t.me/" + bot
	}
	return "https://t.me/" + bot + "?start=" + payload
}

// BuildSerialLink renders the deep link that starts a bot conversation asking
// for one receive number.
func BuildSerialLink(bot string, serial int64) string {
	return BuildStartLink(bot, StartPayload(serial))
}

// ParseStartLink splits a t.me deep link into its bot and payload. It returns
// ok=false for anything that is not a t.me bot link.
func ParseStartLink(link string) (bot, payload string, ok bool) {
	link = strings.TrimSpace(link)
	if !strings.Contains(link, "t.me/") && !strings.Contains(link, "telegram.me/") {
		return "", "", false
	}
	bot = BotUsername(link)
	if bot == "" {
		return "", "", false
	}
	if i := strings.Index(link, "start="); i >= 0 {
		payload = link[i+len("start="):]
		if j := strings.IndexAny(payload, "&#"); j >= 0 {
			payload = payload[:j]
		}
	}
	return bot, payload, true
}

// SerialRef formats a receive number the way a person writes it.
func SerialRef(serial int64) string {
	if serial <= 0 {
		return "#—"
	}
	return fmt.Sprintf("#%06d", serial)
}
