package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/iyear/tdl/archive"
	"github.com/spf13/cobra"
)

// newArchiveExport exposes the local archive as a file.
//
// The point of the CLI path is that an export can be reshaped for a specific
// destination — pick columns, order them, rename them — without touching the
// database or the web UI. The same column syntax as the web export is used, so
// a scripted pipeline and a manual download agree on what "body:content" means.
func newArchiveExport() *cobra.Command {
	var (
		kind      string
		format    string
		fields    string
		mapPath   string
		outPath   string
		query     string
		mediaKind string
		domain    string
		chatID    int64
		topicID   int64
		limit     int
		withMedia bool
		split     bool
	)

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export archived messages or materials as CSV, JSON or JSONL",
		Long: `Export the local archive as a file.

--fields selects and orders columns; a colon renames one:
  --fields id,title:name,body:content,date

--map applies a {"source":"output"} JSON file, which is the artefact to keep
when the target is a site importer with its own schema. The file wins over an
inline rename.

Kinds: messages, media, links, deleted, edited, materials, serials.
Formats: csv (default), json, jsonl, tgjson.

--format tgjson writes Telegram Desktop's own export layout instead of a flat
table, so the result opens in tools that already read a client export:

  tdl archive export --format tgjson --with-media -o ./export/

That produces ./export/result.json plus photos/, video_files/, voice_messages/
and friends. With --split each chat gets its own directory and result.json,
which is the single-chat layout some importers expect.

For tgjson, -o is a DIRECTORY (created if missing) and --fields/--map do not
apply: the field names are fixed by the format.

Example — build an import file for a site backend without touching the DB:
  tdl archive export --kind materials --format jsonl \
    --fields id,title:name,body:content,caption,source_url,media_sha256 \
    -o materials.jsonl`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if kind == "" {
				kind = "messages"
			}
			format = strings.ToLower(strings.TrimSpace(format))
			if format == "" {
				format = "csv"
			}
			switch format {
			case "csv", "json", "jsonl", "ndjson", "tgjson":
			default:
				return errors.New("--format must be csv, json, jsonl or tgjson")
			}
			if format == "tgjson" {
				if fields != "" || mapPath != "" {
					return errors.New("--fields/--map do not apply to --format tgjson: the format fixes the field names")
				}
				if outPath == "" {
					return errors.New("--format tgjson writes a directory tree; pass -o DIR")
				}
			}

			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := context.Background()

			if format == "tgjson" {
				var ids []int64
				if chatID != 0 {
					ids = []int64{chatID}
				}
				stats, err := archive.WriteTelegramExport(ctx, s, archive.TGExportOptions{
					OutDir:    outPath,
					WithMedia: withMedia,
					ChatIDs:   ids,
					Limit:     limit,
					Split:     split,
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "tgjson: %d chats, %d messages, %d media files (%s), %d skipped\n",
					stats.Chats, stats.Messages, stats.MediaCopied,
					humanBytes(stats.MediaBytes), stats.MediaMissed)
				return nil
			}

			spec, err := archive.ParseFieldSpec(fields)
			if err != nil {
				return err
			}
			if mapPath != "" {
				m, err := archive.LoadFieldMapping(mapPath)
				if err != nil {
					return err
				}
				spec = archive.MergeRenames(spec, m)
			}

			src, err := archive.BuildExportSource(ctx, s, kind, query, mediaKind, domain, chatID, topicID)
			if err != nil {
				return err
			}
			src, err = archive.ReshapeSource(src, spec)
			if err != nil {
				return err
			}
			src = archive.LimitSource(src, limit)

			var out io.Writer = cmd.OutOrStdout()
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return err
				}
				defer f.Close()
				out = f
			}
			switch format {
			case "csv":
				return archive.WriteCSV(out, src)
			case "json":
				return archive.WriteJSON(out, src)
			default:
				return archive.WriteJSONL(out, src)
			}
		},
	}

	cmd.Flags().StringVarP(&kind, "kind", "k", "messages", "view to export: messages, media, links, deleted, edited, materials")
	cmd.Flags().StringVarP(&format, "format", "f", "csv", "output format: csv, json or jsonl")
	cmd.Flags().StringVar(&fields, "fields", "", "ordered column list, colon renames (e.g. id,title:name,body:content)")
	cmd.Flags().StringVar(&mapPath, "map", "", `path to a {"source":"output"} JSON rename map`)
	cmd.Flags().StringVarP(&outPath, "out", "o", "", "write to this file instead of stdout")
	cmd.Flags().StringVarP(&query, "query", "q", "", "restrict to rows matching this text")
	cmd.Flags().StringVar(&mediaKind, "media-kind", "", "for --kind media: image, video, audio, document, ...")
	cmd.Flags().StringVar(&domain, "domain", "", "for --kind links: filter by domain")
	cmd.Flags().Int64Var(&chatID, "chat", 0, "restrict to one chat id")
	cmd.Flags().Int64Var(&topicID, "topic", 0, "restrict to one forum topic id")
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after this many rows/messages (0 = no cap)")
	cmd.Flags().BoolVar(&withMedia, "with-media", false, "tgjson: copy media files next to result.json")
	cmd.Flags().BoolVar(&split, "split", false, "tgjson: one directory with its own result.json per chat")
	return cmd
}
