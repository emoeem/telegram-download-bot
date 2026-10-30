package archive

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// The serial → material → publish chain in one place.
//
// Each step already existed, but disconnected: a media row knew its number, a
// material knew its text, a publish plan knew a template. Nothing could answer
// the question a person actually asks — "give me #001337, as a post I can
// send". SerialPost is that join, and it is deliberately read-only: it resolves
// the number to bytes and text, and rendering is a pure function of the result.

// DefaultSerialTemplate is the post body used when no template is supplied. It
// leads with the number so the published item can be traced back to the ledger
// by eye.
const DefaultSerialTemplate = `{{.Ref}}{{with .KindLabel}} · {{.}}{{end}}

{{.Text}}{{if .HasFile}}

📎 {{.FileName}}{{with .SizeHuman}} · {{.}}{{end}}{{end}}{{with .SourceURL}}

{{.}}{{end}}`

// SerialPost is one receive number resolved to everything a post needs.
type SerialPost struct {
	Serial     int64
	Kind       string
	KindLabel  string
	MIME       string
	FileName   string
	Path       string
	Size       int64
	SizeHuman  string
	Date       time.Time
	ChatID     int64
	ChatLabel  string
	MessageID  int
	Text       string
	MaterialID int64
	// HasFile reports that bytes exist locally. A numbered item can legitimately
	// have none (download skipped, queued, or failed) — the post still renders.
	HasFile   bool
	SourceURL string
}

// Ref is the display number, exposed to templates as {{.Ref}}.
func (p SerialPost) Ref() string { return SerialRef(p.Serial) }

// LoadSerialPost resolves one receive number. found=false means no such number;
// err is only for a real storage failure.
func LoadSerialPost(ctx context.Context, s *store.Store, root string, serial int64) (SerialPost, bool, error) {
	it, ok, err := s.SerialByNumber(ctx, serial)
	if err != nil || !ok {
		return SerialPost{}, ok, err
	}
	p := SerialPost{
		Serial:    it.Serial,
		Kind:      it.Kind,
		KindLabel: kindLabel(it.Kind),
		MIME:      it.MIME,
		FileName:  it.FileName,
		Size:      it.Size,
		SizeHuman: humanBytes(it.Size),
		Date:      it.Date,
		ChatID:    it.ChatID,
		ChatLabel: it.ChatLabel,
		MessageID: it.MessageID,
		Text:      strings.TrimSpace(it.Caption),
	}
	// Prefer the receive-number view (that is the path the user navigates), and
	// fall back to the content-addressed object when the view has not been
	// linked yet (older rows, or a link that failed on a foreign filesystem).
	if view := SerialViewPath(root, p.Serial, p.Kind, p.MIME); view != "" {
		if st, e := os.Stat(view); e == nil && st.Size() > 0 {
			p.Path, p.HasFile = view, true
		}
	}
	if !p.HasFile && it.Path != "" {
		if st, e := os.Stat(it.Path); e == nil && st.Size() > 0 {
			p.Path, p.HasFile = it.Path, true
		}
	}
	if p.FileName == "" && p.Path != "" {
		p.FileName = filepathBase(p.Path)
	}
	if p.MessageID > 0 {
		p.SourceURL = fmt.Sprintf("tg://openmessage?chat_id=%d&message_id=%d", p.ChatID, p.MessageID)
	}
	// Attach the material so a caller can see the normalized identity (dedup
	// hash, tags) without a second lookup.
	if rowID, e := s.RowID(ctx, it.ChatID, it.MessageID); e == nil {
		if mv, e := s.MaterialByRowID(ctx, rowID); e == nil {
			p.MaterialID = mv.ID
			if p.Text == "" {
				p.Text = strings.TrimSpace(firstNonEmpty(mv.Body, mv.Caption, mv.Title))
			}
		}
	}
	return p, true, nil
}

// RenderSerialPost renders one post with tmpl (empty means DefaultSerialTemplate).
func RenderSerialPost(p SerialPost, tmpl string) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = DefaultSerialTemplate
	}
	t, err := ParsePublishTemplate("serial-post", tmpl)
	if err != nil {
		return "", err
	}
	return renderSerialTemplate(t, p)
}

func renderSerialTemplate(t *template.Template, p SerialPost) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, p); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()), nil
}

func kindLabel(kind string) string {
	switch strings.ToLower(kind) {
	case "photo":
		return "图片"
	case "video":
		return "视频"
	case "document":
		return "文件"
	case "audio":
		return "音频"
	case "voice":
		return "语音"
	case "animation":
		return "动图"
	case "video_note":
		return "圆视频"
	case "sticker":
		return "贴纸"
	}
	return kind
}

func humanBytes(n int64) string {
	if n <= 0 {
		return ""
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}
