package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/iyear/tdl/archive"
	"github.com/spf13/cobra"
)

// newArchiveMaterial builds the normalized material layer from raw messages.
//
// This is the step that turns "a table of archived messages" into "a table of
// assets": an album becomes one row, a reposted post is marked as a duplicate
// of the earliest copy, and each row carries a content hash stable enough to
// compare across channels. It is derived data by construction, so --rebuild is
// always safe: nothing here is the source of truth.
func newArchiveMaterial() *cobra.Command {
	var (
		rebuild bool
		dense   bool
		chatID  int64
		batch   int
		stats   bool
		asJSON  bool
	)

	cmd := &cobra.Command{
		Use:   "material",
		Short: "Build or refresh the normalized material layer",
		Long: `Fold raw archived messages into materials.

One message without grouped_id becomes one material; every message sharing a
grouped_id becomes a single album material whose body joins the members' text
and captions. Repeated posts are marked status=duplicate with duplicate_of
pointing at the earliest copy — they are never deleted, because "which channel
posted this first" is the question the layer exists to answer.

Examples:
  tdl archive material                # incremental refresh of every chat
  tdl archive material --rebuild      # wipe and rebuild (safe: derived data)
  tdl archive material --chat 1234    # one chat only
  tdl archive material --stats --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()

			ctx := context.Background()
			if stats {
				st, err := s.MaterialStats(ctx)
				if err != nil {
					return err
				}
				if asJSON {
					b, err := json.MarshalIndent(st, "", "  ")
					if err != nil {
						return err
					}
					fmt.Fprintln(cmd.OutOrStdout(), string(b))
					return nil
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "materials: %d (active %d, duplicate %d, with media %d)\n",
					st.Total, st.Active, st.Duplicates, st.WithMedia)
				kinds := make([]string, 0, len(st.ByKind))
				for k := range st.ByKind {
					kinds = append(kinds, k)
				}
				sort.Strings(kinds)
				for _, k := range kinds {
					fmt.Fprintf(out, "  %-12s %d\n", k, st.ByKind[k])
				}
				return nil
			}

			mz := archive.NewMaterializer(s)
			rep, err := mz.Build(ctx, archive.BuildOptions{
				ChatID:    chatID,
				Rebuild:   rebuild,
				BatchSize: batch,
				Dense:     dense,
			})
			if err != nil {
				return err
			}
			if asJSON {
				b, err := json.MarshalIndent(rep, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "chats=%d materials=%d albums=%d duplicates=%d skipped=%d deleted=%d took=%s\n",
				rep.Chats, rep.Materials, rep.Albums, rep.Duplicates, rep.Skipped, rep.Deleted, rep.Duration.Round(1e6))
			return nil
		},
	}

	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "delete and rebuild the material layer (derived data, always safe)")
	cmd.Flags().BoolVar(&dense, "dense", false, "re-run the global duplicate pass even when nothing changed")
	cmd.Flags().Int64Var(&chatID, "chat", 0, "restrict to one chat id")
	cmd.Flags().IntVar(&batch, "batch", 500, "raw messages read per page")
	cmd.Flags().BoolVar(&stats, "stats", false, "print material counters instead of building")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
