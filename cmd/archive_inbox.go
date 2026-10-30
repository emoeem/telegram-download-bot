package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-faster/errors"
	"github.com/iyear/tdl/archive"
	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/spf13/cobra"
)

// archiveInboxRoot is where serial files and the by-serial view live. It matches
// the media worker's default root so both layers resolve to the same tree.
func archiveInboxRoot() string { return consts.DataDir + "/archive" }

// newArchiveInbox prints the receive ledger: what this account got, in the order
// it got it, by number.
func newArchiveInbox() *cobra.Command {
	var (
		limit  int
		kind   string
		status string
		all    bool
		since  string
		asJSON bool
		maxSer int64
	)
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "List received media by receive number (the serial ledger)",
		Long: `List what the monitored account received, numbered.

The number is assigned when the message lands, not when the download finishes, so
queued and failed items still appear here — that is the point: the ledger tells
you what arrived, the status tells you what is ready.

  tdl archive inbox                 # newest 50 inbound items
  tdl archive inbox --all           # include what this account sent
  tdl archive inbox --kind video    # only videos
  tdl archive inbox --json          # machine-readable, for scripts`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()
			ctx := cmd.Context()
			var sinceT *time.Time
			if strings.TrimSpace(since) != "" {
				t, err := parseSince(since)
				if err != nil {
					return err
				}
				if t != nil {
					sinceT = t
				}
			}
			items, err := s.Serials(ctx, store.SerialFilter{
				Limit:       limit,
				Kind:        kind,
				Status:      status,
				InboundOnly: !all,
				MaxSerial:   maxSer,
				Since:       sinceT,
			})
			if err != nil {
				return err
			}
			sum, err := s.SerialSummary(ctx)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"summary": sum,
					"items":   items,
				})
			}
			fmt.Printf("收件箱 · 共 %d 条 · 最高编号 %s · 待下载 %d · 已完成 %d\n",
				sum.Total, archive.SerialRef(sum.Max), sum.Pending, sum.Completed)
			fmt.Printf("视图目录 %s\n\n", archive.SerialViewRoot(archiveInboxRoot()))
			if len(items) == 0 {
				fmt.Println("暂无编号记录。先运行: tdl archive watch @<bot>")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "编号\t时间\t向\t类型\t大小\t状态\t文件\t来源")
			for _, it := range items {
				dir := "←"
				if it.Out {
					dir = "→"
				}
				file := it.FileName
				if file == "" {
					file = "—"
				}
				size := humanSize(it.Size)
				if size == "" {
					size = "—"
				}
				when := "—"
				if !it.Date.IsZero() {
					when = it.Date.Local().Format("2006-01-02 15:04:05")
				}
				label := it.ChatLabel
				if label == "" {
					label = fmt.Sprintf("%d", it.ChatID)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					archive.SerialRef(it.Serial), when, dir, it.Kind, size, it.Status, file, label)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum rows to show")
	cmd.Flags().StringVar(&kind, "kind", "", "filter by media kind (photo, video, document, ...)")
	cmd.Flags().StringVar(&status, "status", "", "filter by download status (pending, completed, failed, skipped)")
	cmd.Flags().BoolVar(&all, "all", false, "include items this account sent (default: inbound only)")
	cmd.Flags().StringVar(&since, "since", "", "only items newer than RFC3339 or YYYY-MM-DD")
	cmd.Flags().Int64Var(&maxSer, "max-serial", 0, "only numbers at or below this one")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

// newArchiveSerialFile resolves one receive number to a path on disk.
func newArchiveSerialFile() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "file <serial>",
		Short: "Print the local path of one receive number",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var serial int64
			if _, err := fmt.Sscanf(strings.TrimPrefix(args[0], "#"), "%d", &serial); err != nil || serial <= 0 {
				return fmt.Errorf("invalid serial %q", args[0])
			}
			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()
			p, ok, err := archive.LoadSerialPost(cmd.Context(), s, archiveInboxRoot(), serial)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s not found in the receive ledger", archive.SerialRef(serial))
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(p)
			}
			if !p.HasFile {
				fmt.Printf("%s %s · status not ready (no local bytes)\n", archive.SerialRef(serial), p.Kind)
				return errors.New("this number has no file on disk yet; run tdl archive media pending")
			}
			fmt.Println(p.Path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the resolved record as JSON")
	return cmd
}

// newArchiveSerialPost renders one number as a post body, which is the
// "number → material → publish" chain in a single command.
func newArchiveSerialPost() *cobra.Command {
	var tmplPath string
	var tmplText string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "post <serial>",
		Short: "Render one receive number as a publishable post",
		Long: `Resolve a receive number to its material and render the post text.

This is a rendering command: nothing is sent. Pipe it into your own sender, or
use "tdl archive publish" with the same template for a batch.

  tdl archive post 1337
  tdl archive post 1337 --template post.tmpl`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var serial int64
			if _, err := fmt.Sscanf(strings.TrimPrefix(args[0], "#"), "%d", &serial); err != nil || serial <= 0 {
				return fmt.Errorf("invalid serial %q", args[0])
			}
			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()
			p, ok, err := archive.LoadSerialPost(cmd.Context(), s, archiveInboxRoot(), serial)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%s not found in the receive ledger", archive.SerialRef(serial))
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(p)
			}
			tmpl := tmplText
			if tmplPath != "" {
				b, err := os.ReadFile(tmplPath)
				if err != nil {
					return err
				}
				tmpl = string(b)
			}
			out, err := archive.RenderSerialPost(p, tmpl)
			if err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
	cmd.Flags().StringVar(&tmplPath, "template", "", "template file (default: built-in serial template)")
	cmd.Flags().StringVar(&tmplText, "tmpl", "", "template text, overrides --template when both are set")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the resolved record as JSON (no rendering)")
	return cmd
}

// newArchiveRelink rebuilds the by-serial view for media archived before the
// view existed (or for a view that was deleted to reclaim inodes).
func newArchiveRelink() *cobra.Command {
	var pageSize int
	cmd := &cobra.Command{
		Use:   "relink",
		Short: "Rebuild the by-serial file view for already-downloaded media",
		Long: `Hard-link every completed media object under objects/by-serial/<serial>.

The view is derived data: safe to delete, safe to rebuild. Run this after
upgrading an old archive, or after clearing the view to free inodes.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()
			examined, created, err := archive.RelinkAll(cmd.Context(), s, archiveInboxRoot(), pageSize)
			if err != nil {
				return err
			}
			fmt.Printf("检查 %d 条已完成媒体，新建 %d 个编号视图\n视图目录 %s\n",
				examined, created, archive.SerialViewRoot(archiveInboxRoot()))
			return nil
		},
	}
	cmd.Flags().IntVar(&pageSize, "page-size", 500, "rows per paging batch")
	return cmd
}

// newArchiveDeeplink renders the ?start= deep link for a bot, optionally bound
// to a receive number.
func newArchiveDeeplink() *cobra.Command {
	var serial int64
	var payload string
	var count int
	var open bool
	cmd := &cobra.Command{
		Use:   "deeplink <@bot>",
		Short: "Build a t.me ?start= deep link for a bot",
		Long: `Build the deep link that opens a bot conversation with a start payload.

With --serial the payload carries the receive number, so the answer that comes
back can be traced to the request that produced it:

  tdl archive deeplink @some_bot --serial 1337
  → https://t.me/some_bot?start=r16

--count N prints N links for a serial range (serial..serial+N-1), which is how a
batch of numbered requests is handed out.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			bot := archive.BotUsername(cmd.Flags().Arg(0))
			if bot == "" {
				return errors.New("a bot username is required")
			}
			if payload != "" {
				fmt.Println(archive.BuildStartLink(bot, payload))
				return nil
			}
			if serial <= 0 {
				fmt.Println(archive.BuildStartLink(bot, ""))
				return nil
			}
			if count < 1 {
				count = 1
			}
			for i := 0; i < count; i++ {
				link := archive.BuildSerialLink(bot, serial+int64(i))
				fmt.Println(link)
				if open && i == 0 {
					_ = openURL(link)
				}
			}
			return nil
		},
	}
	cmd.Flags().Int64Var(&serial, "serial", 0, "bind the payload to this receive number")
	cmd.Flags().StringVar(&payload, "payload", "", "explicit start payload (overrides --serial)")
	cmd.Flags().IntVar(&count, "count", 1, "print this many consecutive serials")
	cmd.Flags().BoolVar(&open, "open", false, "open the first link in the desktop browser")
	return cmd
}

// newArchivePurge deletes archived content. Dry run is the default.
func newArchivePurge() *cobra.Command {
	var (
		serial   int64
		chatID   int64
		until    string
		kind     string
		keepMsgs bool
		yes      bool
	)
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete archived media and/or messages (dry run by default)",
		Long: `Delete archive rows and the files they own.

Without --yes this is a dry run: it prints what would be deleted and exits. The
row is deleted before the file, so an interrupted purge leaves an orphan file
(which "tdl archive gc" reclaims) instead of a row pointing at missing bytes.

  tdl archive purge --serial 1337                 # preview
  tdl archive purge --serial 1337 --yes           # delete number 1337
  tdl archive purge --chat -100123 --until 2026-01-01 --yes
  tdl archive purge --kind video --until 2026-06-01 --keep-messages --yes

--keep-messages drops the media and its bytes but keeps the message text, which
is the "free space, keep the record" mode.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openArchiveStore()
			if err != nil {
				return err
			}
			defer s.Close()
			sel := store.PurgeSelector{
				Serial:   serial,
				ChatID:   chatID,
				Kind:     kind,
				Messages: !keepMsgs,
				DryRun:   !yes,
			}
			if strings.TrimSpace(until) != "" {
				t, err := parseSince(until)
				if err != nil {
					return err
				}
				if t != nil {
					sel.Until = *t
				}
			}
			rep, err := s.Purge(cmd.Context(), sel)
			if err != nil {
				return err
			}
			if sel.DryRun {
				fmt.Printf("dry run: 将删除 %d 条媒体、%d 条消息、释放 %s、移除 %d 个对象文件\n",
					rep.Media, rep.Messages, humanSize(rep.Bytes), len(rep.Files))
				for _, f := range rep.Files {
					fmt.Println("  " + f)
				}
				if rep.Media > 0 {
					fmt.Println("\n确认后加 --yes 执行。")
				}
				return nil
			}
			removed, freed := archive.ApplyPurge(archiveInboxRoot(), rep)
			fmt.Printf("已删除 %d 条媒体、%d 条消息；移除 %d 个文件（%s）\n",
				rep.Media, rep.Messages, removed, humanSize(freed))
			return nil
		},
	}
	cmd.Flags().Int64Var(&serial, "serial", 0, "delete exactly this receive number")
	cmd.Flags().Int64Var(&chatID, "chat", 0, "restrict to one chat id")
	cmd.Flags().StringVar(&until, "until", "", "only content older than this date (YYYY-MM-DD or RFC3339)")
	cmd.Flags().StringVar(&kind, "kind", "", "only this media kind")
	cmd.Flags().BoolVar(&keepMsgs, "keep-messages", false, "delete media and bytes but keep message text")
	cmd.Flags().BoolVar(&yes, "yes", false, "actually delete (otherwise dry run)")
	return cmd
}

// newArchiveWatch registers a chat as a numbered inbox and then runs the monitor.
// It exists because "listen to my bot DM" is the common case and spelling it as
// monitor add + run is two commands with a flag you have to remember.
func newArchiveWatch(run *cobra.Command) *cobra.Command {
	var download bool
	var start string
	cmd := &cobra.Command{
		Use:   "watch <@bot|chat-id>",
		Short: "Register a chat (e.g. a bot DM) as a numbered inbox and start receiving",
		Long: `Register the chat and keep it monitored, then run the archive.

This is the "my account stays online and collects what the bot sends me" mode:
the target is created with media download on, every inbound media item is
numbered on arrival, and the process keeps running until interrupted.

  tdl archive watch @some_bot
  tdl archive watch @some_bot --start r16   # also print the bound deep link

Put it under systemd for a durable deployment (see deploy/tdl-archive.service).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := registerInbox(cmd.Context(), args[0], download); err != nil {
				return err
			}
			if start != "" {
				fmt.Printf("深链: %s\n", archive.BuildStartLink(archive.BotUsername(args[0]), start))
			}
			// Delegate to the same run path so watch and run cannot drift.
			return run.RunE(cmd, nil)
		},
	}
	cmd.Flags().BoolVar(&download, "download-media", true, "download media bytes (false: number the item, fetch later)")
	cmd.Flags().StringVar(&start, "start", "", "print a deep link carrying this start payload after registering")
	return cmd
}

// registerInbox resolves a chat and makes sure it is a monitored target with the
// requested download switch, without wiping filters a previous run configured.
func registerInbox(ctx context.Context, target string, download bool) error {
	s, err := openArchiveStore()
	if err != nil {
		return err
	}
	defer s.Close()
	kvd, err := archiveKV(ctx)
	if err != nil {
		return err
	}
	rt, _, err := newArchiveRuntime(ctx, kvd, s)
	if err != nil {
		return err
	}
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
		p, err := archive.Resolve(ctx, rt, target)
		if err != nil {
			return err
		}
		u, _ := p.Username()
		if existing, e := s.Target(ctx, p.ID()); e == nil {
			// Keep keywords, size caps and destination wiring; only make sure the
			// inbox is on.
			on, dl := true, download
			if err := s.UpdateTargetRules(ctx, p.ID(), store.TargetRules{Enabled: &on, DownloadMedia: &dl}); err != nil {
				return err
			}
			fmt.Printf("已在监控: %s (%d)，下载媒体=%v\n", p.VisibleName(), p.ID(), download)
			_ = existing
			return nil
		}
		t := store.Target{
			ChatID: p.ID(), Username: u, Enabled: true,
			ArchiveText: true, ArchiveMedia: true, ArchiveFiles: true,
			DownloadMedia: download, ArchiveMode: "local",
		}
		if err := s.AddTarget(ctx, t); err != nil {
			return err
		}
		fmt.Printf("新登记: %s (%d)，入站媒体将按编号归档\n", p.VisibleName(), p.ID())
		return nil
	})
}

// openURL hands a link to the desktop opener. Failure is not an error: the
// caller printed the link already, so a headless host loses nothing.
func openURL(u string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", u)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	return c.Start()
}

func humanSize(n int64) string {
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
