package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/archive"
	"github.com/spf13/cobra"
)

// newArchivePublish renders materials back into Telegram messages and sends
// them to a destination.
//
// It is a dry run unless --apply is passed. That default is not caution for its
// own sake: publishing is the one operation in this tool that cannot be undone
// — an edited or deleted destination message has already been seen. Rendering
// the whole batch first is also the only way to catch a template that produces
// empty text for a subset of materials.
func newArchivePublish() *cobra.Command {
	var (
		dest     string
		tmplPath string
		tmplText string
		limit    int
		apply    bool
		chatID   int64
		kind     string
		query    string
		status   string
		asJSON   bool
	)

	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Render materials with a template and post them to a destination",
		Long: `Render materials into messages and (optionally) send them.

Without --apply nothing is sent: the batch is rendered and printed so the exact
text can be reviewed. --template takes a file; the default template is

  {{.Title}}

  {{.Body}}

  来源：{{.SourceName}}

Template data is one material, with the fields exposed by --kind materials
(id, title, body, caption, source_name, source_url, date, tags, media_path, ...)
plus helpers: trim, truncate N, date LAYOUT, hashtags, join.

Examples:
  # review what would be posted
  tdl archive publish --dest @mychannel --limit 5

  # post for real
  tdl archive publish --dest @mychannel --limit 5 --apply

  # custom template, text-only materials of one chat
  tdl archive publish --dest @mychannel --template post.tmpl \
    --kind text --chat 1234 --limit 3 --apply`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()

			// A template file wins over --template, which exists for one-liners
			// in scripts and for a quick override without touching disk.
			tmpl := tmplText
			if tmplPath != "" {
				b, err := os.ReadFile(tmplPath)
				if err != nil {
					return err
				}
				tmpl = string(b)
			}

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			plan, err := archive.BuildPublishPlan(ctx, s, archive.PublishOptions{
				Template: tmpl,
				Limit:    limit,
				ChatID:   chatID,
				Kind:     kind,
				Query:    query,
				Status:   status,
			})
			if err != nil {
				return err
			}
			if len(plan.Items) == 0 {
				return errors.New("no materials matched; nothing to publish")
			}

			out := cmd.OutOrStdout()
			if asJSON {
				b, err := json.MarshalIndent(plan, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(out, string(b))
			} else {
				printPlan(out, plan, apply)
			}
			if !apply {
				fmt.Fprintln(out, "\n(dry run: nothing was sent. re-run with --apply to post)")
				return nil
			}
			if strings.TrimSpace(dest) == "" {
				return errors.New("--dest is required with --apply")
			}

			kvd, err := archiveKV(ctx)
			if err != nil {
				return err
			}
			rt, _, err := newArchiveRuntime(ctx, kvd, s)
			if err != nil {
				return err
			}
			exporter := &archive.ChannelExporter{Runtime: rt, Store: s}
			return rt.Client.Run(ctx, func(ctx context.Context) error {
				st, err := rt.Client.Auth().Status(ctx)
				if err != nil {
					return err
				}
				if !st.Authorized {
					return errors.New("archive account is not logged in; run tdl archive login")
				}
				if err := rt.Peers.Init(ctx); err != nil {
					return err
				}
				peer, err := archive.Resolve(ctx, rt, dest)
				if err != nil {
					return fmt.Errorf("resolve destination %q: %w", dest, err)
				}
				if _, ok := peer.InputPeer().(*tg.InputPeerChannel); !ok {
					return errors.New("--dest must resolve to a channel or supergroup")
				}

				sent, failed := 0, 0
				for _, item := range plan.Items {
					if item.Skip != "" {
						continue
					}
					if item.MediaPath != "" {
						_, err = exporter.SendMedia(ctx, peer.InputPeer(), item.MediaPath, item.MediaMIME, item.Text)
					} else {
						_, err = exporter.SendMessage(ctx, peer.InputPeer(), item.Text)
					}
					if err != nil {
						failed++
						fmt.Fprintf(cmd.ErrOrStderr(), "material %d: send failed: %v\n", item.MaterialID, err)
						// A flood wait or an auth failure will not fix itself on
						// the next item; stop rather than burn the batch.
						if archive.IsFatalSendError(err) {
							return fmt.Errorf("stopping after material %d: %w", item.MaterialID, err)
						}
						continue
					}
					sent++
					fmt.Fprintf(out, "sent material %d\n", item.MaterialID)
				}
				fmt.Fprintf(out, "\nsent=%d failed=%d skipped=%d\n", sent, failed, plan.Skipped)
				if failed > 0 {
					return fmt.Errorf("%d of %d messages failed", failed, sent+failed)
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&dest, "dest", "", "destination channel (@name or id); required with --apply")
	cmd.Flags().StringVar(&tmplPath, "template", "", "path to a text/template file for one material")
	cmd.Flags().StringVar(&tmplText, "template-text", "", "inline template, overridden by --template")
	cmd.Flags().IntVar(&limit, "limit", 20, "how many materials to render (max 500)")
	cmd.Flags().BoolVar(&apply, "apply", false, "actually send; without it this is a dry run")
	cmd.Flags().Int64Var(&chatID, "chat", 0, "restrict to materials from one source chat id")
	cmd.Flags().StringVar(&kind, "kind", "", "restrict to one material kind (text, photo, video, album, ...)")
	cmd.Flags().StringVar(&query, "query", "", "restrict to materials matching this text")
	cmd.Flags().StringVar(&status, "status", "active", "material status: active (default), duplicate or all")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the rendered plan as JSON")
	return cmd
}

// printPlan writes the rendered batch for review. Each item is separated by a
// rule so an operator can tell where one Post ends and the next begins, which a
// bare dump of the concatenated texts does not show.
func printPlan(out io.Writer, plan archive.PublishPlan, applying bool) {
	verb := "would send"
	if applying {
		verb = "sending"
	}
	fmt.Fprintf(out, "%s %d of %d materials (%d skipped)\n", verb, len(plan.Items)-plan.Skipped, len(plan.Items), plan.Skipped)
	for i, item := range plan.Items {
		fmt.Fprintf(out, "\n─── %d/%d material %d", i+1, len(plan.Items), item.MaterialID)
		if item.MediaPath != "" {
			fmt.Fprintf(out, " [media %s, %d item(s)]", item.MediaMIME, item.MediaCount)
		}
		if item.Skip != "" {
			fmt.Fprintf(out, " SKIP: %s\n", item.Skip)
			continue
		}
		fmt.Fprintf(out, " (%d chars) ───\n%s\n", len([]rune(item.Text)), item.Text)
	}
}
