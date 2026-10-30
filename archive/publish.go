package archive

import (
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tgerr"
	"github.com/iyear/tdl/archive/store"
)

// Publishing turns a material back into a Telegram message.
//
// The archive stores assets in a shape that suits querying, not posting: fields
// are separate, the media is a content-addressed file, and the caption is its
// own column. A destination channel wants one composed message. That gap is
// bridged with text/template rather than a bespoke formatter — template gives
// operators conditional sections and trimming for free, and a broken template
// fails at parse time instead of mid-batch.

// DefaultPublishTemplate is used when no template file is given. It mirrors the
// way most curated channels repost: title, body, source credit.
const DefaultPublishTemplate = `{{- with .Title}}{{.}}

{{end}}{{- .Body}}{{- if .SourceName}}

来源：{{.SourceName}}{{end}}`

// PublishItem is one material rendered into a ready-to-send message.
type PublishItem struct {
	MaterialID int64
	ChatID     int64
	MessageIDs string
	Text       string
	// MediaPath is the absolute path of the primary media file, empty when the
	// material is text-only. A destination can post for text alone.
	MediaPath string
	MediaMIME string
	// MediaCount > 1 means the material is an album, which this publisher posts
	// as its first image: sending a media group needs one upload per member and
	// belongs in a follow-up, not in a silent partial post.
	MediaCount int
	// Skip records why an item cannot be posted. A skipped item is reported, not
	// silently dropped: an operator needs to know five of ten posts vanished.
	Skip string
}

// PublishPlan is a rendered batch plus the reasons anything was dropped.
type PublishPlan struct {
	Items    []PublishItem
	Skipped  int
	Template string
}

// templateFuncs are the small helpers a channel operator reaches for. They are
// deliberately few: a template language with logic invites templates that only
// their author can read.
var templateFuncs = template.FuncMap{
	"trim": strings.TrimSpace,
	// truncate cuts by rune, not byte: a Chinese body cut mid-codepoint is a
	// replacement character in the destination channel.
	"truncate": func(n int, s string) string {
		if n <= 0 || utf8.RuneCountInString(s) <= n {
			return s
		}
		r := []rune(s)
		return strings.TrimSpace(string(r[:n])) + "…"
	},
	"date": func(layout string, unix int64) string {
		if unix == 0 {
			return ""
		}
		return time.Unix(unix, 0).Format(layout)
	},
	"hashtags": func(tags string) string {
		out := make([]string, 0, 4)
		for _, t := range strings.Split(tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, "#"+strings.ReplaceAll(t, " ", "_"))
			}
		}
		return strings.Join(out, " ")
	},
	"join": strings.Join,
}

// ParsePublishTemplate compiles a template with the helper set attached.
func ParsePublishTemplate(name, text string) (*template.Template, error) {
	t, err := template.New(name).Funcs(templateFuncs).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("parse template %s: %w", name, err)
	}
	return t, nil
}

// BuildPublishPlan renders one page worth of materials into sendable items.
//
// Rendering is separated from sending so a batch can be reviewed in full before
// anything leaves the account. That is the whole reason the command defaults to
// a dry run.
func BuildPublishPlan(ctx context.Context, st *store.Store, opt PublishOptions) (PublishPlan, error) {
	tmplText := opt.Template
	if strings.TrimSpace(tmplText) == "" {
		tmplText = DefaultPublishTemplate
	}
	tmpl, err := ParsePublishTemplate("publish", tmplText)
	if err != nil {
		return PublishPlan{}, err
	}

	limit := opt.Limit
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	items, err := st.ListMaterials(ctx, store.MaterialFilter{
		ChatID:   opt.ChatID,
		Kind:     opt.Kind,
		Query:    opt.Query,
		Status:   opt.Status,
		Page:     1,
		PageSize: limit,
	})
	if err != nil {
		return PublishPlan{}, err
	}

	plan := PublishPlan{Template: tmplText}
	for _, m := range items {
		text, err := renderMaterial(tmpl, m)
		if err != nil {
			return PublishPlan{}, fmt.Errorf("render material %d: %w", m.ID, err)
		}
		item := PublishItem{
			MaterialID: m.ID,
			ChatID:     m.ChatID,
			MessageIDs: m.MessageIDs,
			Text:       text,
			MediaPath:  m.MediaPath,
			MediaMIME:  m.MediaMIME,
			MediaCount: m.MediaCount,
		}
		switch {
		case strings.TrimSpace(text) == "" && item.MediaPath == "":
			// Telegram rejects an empty message, and a material that renders to
			// nothing means the template does not fit this data.
			item.Skip = "rendered text is empty and the material has no media"
		case utf8.RuneCountInString(text) > 4096:
			// The Bot API and raw MTProto both cap a message at 4096 characters;
			// truncating silently would publish half a post.
			item.Skip = fmt.Sprintf("rendered text is %d characters, over the 4096 limit", utf8.RuneCountInString(text))
		}
		if item.Skip != "" {
			plan.Skipped++
		}
		plan.Items = append(plan.Items, item)
	}
	return plan, nil
}

// renderMaterial executes the template against one material.
func renderMaterial(tmpl *template.Template, m store.MaterialView) (string, error) {
	var b strings.Builder
	if err := tmpl.Execute(&b, m); err != nil {
		return "", err
	}
	// Templates routinely leave a trailing blank line; Telegram preserves it.
	return strings.TrimRight(b.String(), "\n \t"), nil
}

// PublishOptions selects and shapes one publish batch.
type PublishOptions struct {
	Template string
	Limit    int
	ChatID   int64
	Kind     string
	Query    string
	Status   string
}

// IsFatalSendError reports whether a send failure should stop the whole batch.
//
// The distinction is the difference between "this item is bad" and "this
// account cannot post right now". Continuing past a flood wait or a lost
// authorization turns one failure into a banned account and a batch of silent
// misses; continuing past a per-item rejection (a chat that forbids media, a
// message too long) is exactly what a batch should do.
func IsFatalSendError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := tgerr.AsFloodWait(err); ok {
		return true
	}
	return tgerr.Is(err,
		"AUTH_KEY_UNREGISTERED",
		"AUTH_KEY_DUPLICATED",
		"USER_DEACTIVATED_BAN",
		"USER_DEACTIVATED",
		"SESSION_REVOKED",
		"CHANNEL_PRIVATE",
		"CHAT_WRITE_FORBIDDEN",
		"CHANNEL_MONOFORUM_UNSUPPORTED",
		"PEER_ID_INVALID",
	)
}
