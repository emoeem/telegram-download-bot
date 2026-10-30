package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/iyear/tdl/archive/store"
)

// Materializer folds raw archived messages into the normalized material layer.
//
// The rules it enforces are deliberately simple, because a wrong rule silently
// loses archived content:
//
//   - one message with no grouped_id  -> one material (kind = its media kind)
//   - every message of one grouped_id -> one material (kind = "album")
//   - the material body is the join of the members' text/caption, so an album
//     whose caption sat on any of its ten photos still keeps it
//   - content_hash is text + sorted media identity: identical posts collapse,
//     and a post whose media differs stays a distinct asset
//
// Nothing is ever deleted by dedupe: a repeated post becomes status=duplicate
// with duplicate_of pointing at the earliest copy, which is what "which channel
// posted this first" needs to answer.
type Materializer struct {
	Store *store.Store
}

func NewMaterializer(s *store.Store) *Materializer { return &Materializer{Store: s} }

// BuildOptions controls a (re)build pass.
type BuildOptions struct {
	// ChatID restricts the pass to one chat; 0 means every chat.
	ChatID int64
	// Rebuild wipes the existing materials of the scope first. Without it the
	// pass is an idempotent refresh: existing rows are updated in place.
	Rebuild bool
	// BatchSize is how many raw messages one page reads.
	BatchSize int
	// Dense re-runs the global duplicate pass even when no material changed.
	// A rebuild always runs it.
	Dense bool
}

// BuildReport summarizes one materializer pass.
type BuildReport struct {
	Chats      int           `json:"chats"`
	Materials  int           `json:"materials"`
	Albums     int           `json:"albums"`
	Skipped    int           `json:"skipped"`
	Duplicates int           `json:"duplicates"`
	Deleted    int64         `json:"deleted"`
	Duration   time.Duration `json:"duration"`
}

// Build walks the raw message table and (re)writes the material layer.
func (mz *Materializer) Build(ctx context.Context, opt BuildOptions) (BuildReport, error) {
	started := time.Now()
	rep := BuildReport{}
	limit := opt.BatchSize
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	if opt.Rebuild {
		n, err := mz.Store.DeleteMaterials(ctx, opt.ChatID)
		if err != nil {
			return rep, err
		}
		rep.Deleted = n
	}

	// Carry is the tail group of the previous page. A page boundary can fall in
	// the middle of an album, so the last group is only flushed once the next
	// message proves the group ended.
	var carry []store.MaterialSource
	chats := map[int64]bool{}
	flush := func(group []store.MaterialSource) error {
		if len(group) == 0 {
			return nil
		}
		mat, rowIDs, err := mz.materialOf(ctx, group)
		if err != nil {
			return err
		}
		if mat == nil {
			rep.Skipped += len(group)
			return nil
		}
		if _, err := mz.Store.ReplaceMaterial(ctx, *mat, rowIDs); err != nil {
			return err
		}
		rep.Materials++
		if mat.GroupedID != 0 {
			rep.Albums++
		}
		chats[mat.ChatID] = true
		return nil
	}

	var afterRowID int64
	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		batch, err := mz.Store.MaterialSources(ctx, opt.ChatID, afterRowID, limit)
		if err != nil {
			return rep, err
		}
		if len(batch) == 0 {
			break
		}
		afterRowID = batch[len(batch)-1].RowID
		all := make([]store.MaterialSource, 0, len(carry)+len(batch))
		all = append(all, carry...)
		all = append(all, batch...)
		carry = nil

		start := 0
		// Walk the merged slice and flush every group that is known to be closed:
		// a group is closed when the *next* message belongs to a different one.
		for i := 1; i < len(all); i++ {
			if all[i].GroupedID != all[i-1].GroupedID || all[i].GroupedID == 0 {
				if err := flush(all[start:i]); err != nil {
					return rep, err
				}
				start = i
			}
		}
		// Everything from start is still open. When the page was short the table
		// is exhausted, so the tail is closed too; otherwise keep it as carry.
		if len(batch) < limit {
			if err := flush(all[start:]); err != nil {
				return rep, err
			}
		} else {
			carry = append(carry, all[start:]...)
		}
		if len(batch) < limit {
			break
		}
	}
	if err := flush(carry); err != nil {
		return rep, err
	}
	rep.Chats = len(chats)

	if opt.Rebuild || opt.Dense || rep.Materials > 0 {
		n, err := mz.Store.MarkDuplicateMaterials(ctx)
		if err != nil {
			return rep, err
		}
		rep.Duplicates = n
	}
	rep.Duration = time.Since(started)
	return rep, nil
}

// Touch re-materializes the posts that the given raw messages belong to. It is
// the incremental hook the collector calls after writing a message, so a live
// archive does not need a full rebuild to stay current.
func (mz *Materializer) Touch(ctx context.Context, chatID int64, messageIDs []int) error {
	if len(messageIDs) == 0 {
		return nil
	}
	seen := make(map[int64]bool, len(messageIDs))
	for _, msgID := range messageIDs {
		rowID, err := mz.Store.RowID(ctx, chatID, msgID)
		if err != nil {
			// The message is gone (deleted before it was ever archived): drop the
			// stale material instead of leaving a row pointing at nothing.
			_ = mz.Store.DeleteMaterialByAnchor(ctx, chatID, msgID)
			continue
		}
		src, err := mz.Store.MaterialSourceByRowID(ctx, rowID)
		if err != nil {
			return err
		}
		groupID := src.GroupedID
		if groupID != 0 {
			if seen[groupID] {
				continue
			}
			seen[groupID] = true
			sources, err := mz.Store.MaterialSourcesByGrouped(ctx, chatID, groupID)
			if err != nil {
				return err
			}
			mat, rowIDs, err := mz.materialOf(ctx, sources)
			if err != nil {
				return err
			}
			if mat == nil {
				continue
			}
			if _, err := mz.Store.ReplaceMaterial(ctx, *mat, rowIDs); err != nil {
				return err
			}
			continue
		}
		mat, rowIDs, err := mz.materialOf(ctx, []store.MaterialSource{src})
		if err != nil {
			return err
		}
		if mat == nil {
			continue
		}
		if _, err := mz.Store.ReplaceMaterial(ctx, *mat, rowIDs); err != nil {
			return err
		}
	}
	_, err := mz.Store.MarkDuplicateMaterials(ctx)
	return err
}

// materialOf converts one closed group of raw messages into a material row.
// It returns nil when the group carries nothing worth exporting (no text, no
// media): an empty shell would pollute the asset list and every export.
func (mz *Materializer) materialOf(ctx context.Context, group []store.MaterialSource) (*store.Material, []int64, error) {
	if len(group) == 0 {
		return nil, nil, nil
	}
	chatID := group[0].ChatID
	var bodies []string
	var captions []string
	var mediaKeys []string
	var kinds []string
	mediaCount := 0
	anchor := group[0]
	anchorSet := false
	rowIDs := make([]int64, 0, len(group))
	sourceURL := ""
	date := group[0].Date
	groupedID := group[0].GroupedID
	for _, s := range group {
		rowIDs = append(rowIDs, s.RowID)
		if s.Text != "" {
			bodies = append(bodies, s.Text)
		}
		if s.Caption != "" {
			captions = append(captions, s.Caption)
		}
		if s.Date < date && date != 0 {
			date = s.Date
		}
		if s.HasMedia {
			mediaCount++
			key := s.MediaFileID
			if key == "" {
				key = fmt.Sprintf("%s:%d", s.MediaType, s.RowID)
			}
			mediaKeys = append(mediaKeys, key)
			if s.MediaType != "" {
				kinds = append(kinds, s.MediaType)
			}
			if !anchorSet {
				anchor = s
				anchorSet = true
			}
		}
	}
	if !anchorSet {
		anchor = group[0]
	}
	body := strings.Join(append(append([]string{}, bodies...), captions...), "\n\n")
	body = strings.TrimSpace(body)
	if body == "" && mediaCount == 0 {
		return nil, nil, nil
	}

	title, username := mz.Store.ChatIdentity(ctx, chatID)
	name := username
	if name == "" {
		name = title
	}
	if username != "" {
		sourceURL = fmt.Sprintf("https://t.me/%s/%d", username, anchor.MessageID)
	} else {
		sourceURL = fmt.Sprintf("https://t.me/c/%d/%d", chatID, anchor.MessageID)
	}

	kind := materialKind(groupedID, kinds, mediaCount)
	mat := &store.Material{
		ChatID:          chatID,
		AnchorRowID:     anchor.RowID,
		AnchorMessageID: anchor.MessageID,
		GroupedID:       groupedID,
		Kind:            kind,
		Title:           materialTitle(body, kind, anchor.MessageID),
		Body:            body,
		Caption:         strings.Join(captions, "\n\n"),
		ContentHash:     materialContentHash(body, mediaKeys),
		TextHash:        materialTextHash(body),
		MediaCount:      mediaCount,
		MediaKind:       mediaKind(kinds, mediaCount),
		CharCount:       len([]rune(body)),
		SourceURL:       sourceURL,
		SourceName:      name,
		Date:            date,
		Status:          "active",
	}
	return mat, rowIDs, nil
}

// materialKind classifies the asset. An album is "album" regardless of what it
// contains; a single post is named after its dominant media type.
func materialKind(groupedID int64, kinds []string, mediaCount int) string {
	if groupedID != 0 {
		return "album"
	}
	if mediaCount == 0 || len(kinds) == 0 {
		return "text"
	}
	return kinds[0]
}

// mediaKind collapses the media types of a group: one type survives, several
// become "mixed".
func mediaKind(kinds []string, mediaCount int) string {
	if mediaCount == 0 || len(kinds) == 0 {
		return ""
	}
	seen := ""
	for _, k := range kinds {
		if seen == "" {
			seen = k
			continue
		}
		if k != seen {
			return "mixed"
		}
	}
	return seen
}

// materialTitle is the human label of an asset: the first line of the body,
// clipped on rune boundaries so a multi-byte truncation cannot produce mojibake.
func materialTitle(body, kind string, messageID int) string {
	text := strings.TrimSpace(body)
	if text == "" {
		return fmt.Sprintf("%s #%d", kind, messageID)
	}
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > 80 {
		return strings.TrimSpace(string(runes[:80])) + "…"
	}
	return text
}

// normalizeBody is the canonical form used for hashing only. Whitespace runs
// (including newlines) collapse to one space so that a re-posted paragraph with
// different line wrapping still hashes identically.
func normalizeBody(s string) string { return strings.Join(strings.Fields(s), " ") }

// materialTextHash fingerprints the text alone. Two posts with the same words
// but different images have the same text hash but a different content hash:
// that is a near-duplicate, reported but never auto-collapsed.
func materialTextHash(body string) string {
	n := normalizeBody(body)
	if n == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(n))
	return hex.EncodeToString(sum[:])
}

// materialContentHash fingerprints text plus the exact media set, which is the
// identity of a post as a repostable asset.
func materialContentHash(body string, mediaKeys []string) string {
	n := normalizeBody(body)
	if n == "" && len(mediaKeys) == 0 {
		return ""
	}
	keys := append([]string{}, mediaKeys...)
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(n))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(keys, ",")))
	return hex.EncodeToString(h.Sum(nil))
}

// RefreshMaterialLayer incrementally folds newly archived messages into the
// material layer.
//
// It exists because the layer is derived data with a schedule of its own: the
// collector writes raw messages continuously, and without this hook an export
// or a publish run would serve a snapshot older than the archive it came from.
// The pass is incremental and idempotent, so calling it on a rescan tick costs
// one pass over what is already there and never duplicates a material.
func RefreshMaterialLayer(ctx context.Context, s *store.Store) (BuildReport, error) {
	return NewMaterializer(s).Build(ctx, BuildOptions{})
}
