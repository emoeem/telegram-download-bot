package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/AlecAivazis/survey/v2"
	"github.com/fatih/color"
	"github.com/go-faster/errors"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/app/login"
	"github.com/iyear/tdl/archive"
	"github.com/iyear/tdl/archive/store"
	"github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/iyear/tdl/pkg/kv"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.etcd.io/bbolt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const archiveNamespace = "archive"

func NewArchive() *cobra.Command {
	root := &cobra.Command{Use: "archive", Short: "Monitor and archive Telegram messages", GroupID: groupTools.ID}
	root.PersistentFlags().String("session-passphrase", "", "passphrase for encrypting the archive session at rest (env TGA_SESSION_PASSPHRASE)")
	root.PersistentFlags().String("log-level", "", "archive log level: debug, info, warn, error (env TDL_LOG_LEVEL)")
	root.PersistentFlags().String("log-format", "", "archive log format: text or json (env TDL_LOG_FORMAT; ENV=production defaults to json)")
	_ = viper.BindPFlag("archive.session-passphrase", root.PersistentFlags().Lookup("session-passphrase"))
	root.AddCommand(newArchiveLogin(), newMonitor(), newArchiveDestination(), newArchiveSync(), newArchiveRun(), newArchiveSearch(), newArchiveMessage(), newArchiveMedia(), newArchiveStats(), newArchiveDoctor(), newArchiveServe(), newArchiveGC(), newArchiveNotify())
	return root
}
func configureArchiveLogging(cmd *cobra.Command) {
	format := strings.TrimSpace(os.Getenv("TDL_LOG_FORMAT"))
	level := strings.TrimSpace(os.Getenv("TDL_LOG_LEVEL"))
	if flag := cmd.Flags().Lookup("log-format"); flag != nil && flag.Value.String() != "" {
		format = flag.Value.String()
	}
	if flag := cmd.Flags().Lookup("log-level"); flag != nil && flag.Value.String() != "" {
		level = flag.Value.String()
	}
	production := strings.EqualFold(strings.TrimSpace(os.Getenv("ENV")), "production")
	if format == "" {
		if production {
			format = "json"
		} else {
			format = "text"
		}
	}
	if level == "" {
		if production {
			level = "info"
		} else {
			level = "debug"
		}
	}
	logFormat := archive.LogFormatText
	if strings.EqualFold(format, "json") {
		logFormat = archive.LogFormatJSON
	}
	archive.ConfigureLogging(logFormat, archive.ParseLogLevel(level))
}

func archivePath() string { return filepath.Join(consts.DataDir, "archive", "tgarchive.db") }
func openArchiveStore() (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(archivePath()), 0755); err != nil {
		return nil, err
	}
	return store.Open(archivePath())
}
func archiveKV(ctx context.Context) (storage.Storage, error) {
	kvd, err := kv.From(ctx).Open(archiveNamespace)
	if err != nil {
		return nil, err
	}
	pass := archivePassphrase()
	if pass == "" {
		enc, e := archive.IsEncrypted(ctx, kvd)
		if e != nil {
			return nil, e
		}
		if enc {
			return nil, archive.ErrPassphraseRequired
		}
		return kvd, nil
	}
	return archive.SecureNamespace(ctx, kvd, pass)
}

func archivePassphrase() string {
	if v := strings.TrimSpace(viper.GetString("archive.session-passphrase")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("TGA_SESSION_PASSPHRASE"))
}

func promptArchivePassphrase(create bool) (string, error) {
	prompt := &survey.Password{Message: "Enter archive session passphrase:"}
	if create {
		prompt.Message = "Set a passphrase to encrypt the archive session at rest (empty = keep plaintext):"
	}
	pwd := ""
	if err := survey.AskOne(prompt, &pwd); err != nil {
		return "", err
	}
	if create && pwd == "" {
		return "", nil // user opted out of encryption
	}
	if create {
		confirm := ""
		if err := survey.AskOne(&survey.Password{Message: "Confirm passphrase:"}, &confirm); err != nil {
			return "", err
		}
		if confirm != pwd {
			return "", errors.New("passphrases do not match")
		}
	}
	return strings.TrimSpace(pwd), nil
}

func isInteractive() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
func newArchiveRuntime(ctx context.Context, kvd storage.Storage, s *store.Store) (*archive.Runtime, *archive.Collector, error) {
	c := archive.NewCollector(s)
	appID := viper.GetInt("telegram.app-id")
	appHash := strings.TrimSpace(viper.GetString("telegram.app-hash"))
	if appID <= 0 || appHash == "" {
		return nil, nil, errors.New("telegram.app-id and telegram.app-hash are required")
	}
	rt, err := archive.NewRuntime(ctx, kvd, appID, appHash, c.SaveUpdate, c.SaveUpdateGroup, c.SaveService, c.SaveEdit, c.SaveDelete, c.SaveReactions)
	if err != nil {
		return nil, nil, err
	}
	c.Exporter = archive.NewExporter(rt, s)
	c.Notifier = archive.NewNotifier(s)
	return rt, c, nil
}
func newArchiveLogin() *cobra.Command {
	var qr bool
	cmd := &cobra.Command{Use: "login", Short: "Login the archive Telegram user account", RunE: func(cmd *cobra.Command, _ []string) error {
		viper.Set(consts.FlagNamespace, archiveNamespace)
		kvd, err := kv.From(cmd.Context()).Open(archiveNamespace)
		if err != nil {
			return err
		}
		enc, err := archive.IsEncrypted(cmd.Context(), kvd)
		if err != nil {
			return err
		}
		pass := archivePassphrase()
		switch {
		case enc && pass == "":
			if !isInteractive() {
				return archive.ErrPassphraseRequired
			}
			if pass, err = promptArchivePassphrase(false); err != nil {
				return err
			}
		case !enc && pass == "" && isInteractive():
			if pass, err = promptArchivePassphrase(true); err != nil {
				return err
			}
		}
		if enc && pass != "" {
			// Fail fast on a wrong passphrase instead of mid-login: decrypting
			// the stored session only succeeds with the right key.
			st, secErr := archive.SecureNamespace(cmd.Context(), kvd, pass)
			if secErr != nil {
				return secErr
			}
			if _, gErr := st.Get(cmd.Context(), "session"); errors.Is(gErr, archive.ErrPassphraseRequired) {
				return errors.New("archive session passphrase is incorrect")
			}
		}
		var loginErr error
		if qr {
			loginErr = login.QR(cmd.Context())
		} else {
			loginErr = login.Code(cmd.Context())
		}
		if loginErr != nil {
			return loginErr
		}
		// login.Code/QR write the session in plaintext through their own raw KV
		// handle — seal the namespace afterwards whenever encryption is on.
		if pass != "" {
			st, secErr := archive.SecureNamespace(cmd.Context(), kvd, pass)
			if secErr != nil {
				return secErr
			}
			if err = st.Reseal(cmd.Context()); err != nil {
				return err
			}
			color.Green("Archive session encrypted at rest.")
		}
		return nil
	}}
	cmd.Flags().BoolVar(&qr, "qr", false, "login by scanning a QR code with the Telegram app")
	return cmd
}
func newMonitor() *cobra.Command {
	root := &cobra.Command{Use: "monitor", Short: "Manage monitored chats"}
	var limit int
	var since string
	var download bool
	var mode string
	var telegramChannel string
	var keywords, exclude string
	var maxSize string
	add := &cobra.Command{Use: "add <@username|chat-id>", Short: "Add a chat to monitor and backfill its history", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := openArchiveStore()
		if err != nil {
			return err
		}
		defer s.Close()
		kvd, err := archiveKV(cmd.Context())
		if err != nil {
			return err
		}
		rt, c, err := newArchiveRuntime(cmd.Context(), kvd, s)
		if err != nil {
			return err
		}
		return rt.Client.Run(cmd.Context(), func(ctx context.Context) error {
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
			p, err := archive.Resolve(ctx, rt, args[0])
			if err != nil {
				return err
			}
			u, _ := p.Username()
			sval, err := parseSince(since)
			if err != nil {
				return err
			}
			if mode != "local" && mode != "telegram" && mode != "both" {
				return fmt.Errorf("invalid --mode %q; use local, telegram, or both", mode)
			}
			var destinationID sql.NullInt64
			if mode == "telegram" || mode == "both" {
				if strings.TrimSpace(telegramChannel) == "" {
					return errors.New("--telegram-channel is required for telegram/both mode")
				}
				dp, e := archive.Resolve(ctx, rt, telegramChannel)
				if e != nil {
					return fmt.Errorf("resolve archive channel: %w", e)
				}
				if _, ok := dp.InputPeer().(*tg.InputPeerChannel); !ok {
					return errors.New("--telegram-channel must resolve to a Telegram channel")
				}
				did, e := s.EnsureDestination(ctx, dp.VisibleName(), "telegram", dp.ID())
				if e != nil {
					return e
				}
				destinationID = sql.NullInt64{Int64: did, Valid: true}
			}
			if mode == "telegram" {
				download = false
			}
			maxBytes, sizeErr := parseSize(maxSize)
			if sizeErr != nil {
				return sizeErr
			}
			t := store.Target{ChatID: p.ID(), Username: u, Enabled: true, ArchiveText: true, ArchiveMedia: true, ArchiveFiles: true, DownloadMedia: download, ArchiveMode: mode, DestinationID: destinationID, Since: sval, Keywords: splitCSV(keywords), ExcludeKeywords: splitCSV(exclude), MediaMaxSize: maxBytes}
			if err := s.AddTarget(ctx, t); err != nil {
				return err
			}
			n, err := c.Backfill(ctx, rt, t, archive.BackfillOptions{Limit: limit, Resume: true, Since: sval})
			if err != nil {
				return err
			}
			_ = archive.NewMediaWorker(s, rt, "").Run(ctx)
			_ = c.Exporter.ExportPending(ctx, t.ChatID, limit)
			_ = archive.NewAvatarWorker(s, rt, "").Run(ctx)
			fmt.Printf("monitoring %s (%d), archived %d messages\n", p.VisibleName(), p.ID(), n)
			return nil
		})
	}}
	add.Flags().IntVar(&limit, "limit", 1000, "maximum messages to archive during initial backfill")
	add.Flags().StringVar(&since, "since", "", "archive messages newer than RFC3339 or YYYY-MM-DD")
	add.Flags().BoolVar(&download, "download-media", true, "queue media for local archive")
	add.Flags().StringVar(&mode, "mode", "local", "archive destination: local, telegram, or both")
	add.Flags().StringVar(&telegramChannel, "telegram-channel", "", "Telegram archive channel for telegram/both mode")
	add.Flags().StringVar(&keywords, "keywords", "", "only archive messages containing any of these comma-separated keywords")
	add.Flags().StringVar(&exclude, "exclude", "", "skip messages containing any of these comma-separated keywords")
	add.Flags().StringVar(&maxSize, "max-size", "", "skip media larger than this, e.g. 200MB or 1GB")
	var setKeywords, setExclude, setMaxSize string
	var setDownload, setEnable, setDisable, setJSON bool
	set := &cobra.Command{
		Use:   "set <chat-id>",
		Args:  cobra.ExactArgs(1),
		Short: "Change filters, size cap or download switch of a monitored chat",
		Long: "Update an existing monitor target in place (no need to remove and re-add it).\n" +
			"Only the flags you pass are changed; passing --keywords \"\" clears the list.",
		Example: "  tdl archive monitor set -1001234567890 --keywords 开源,Go --exclude 招聘\n" +
			"  tdl archive monitor set -1001234567890 --max-size 50MB --download-media=false\n" +
			"  tdl archive monitor set -1001234567890 --disable",
		RunE: func(cmd *cobra.Command, args []string) error {
			chatID, e := strconv.ParseInt(args[0], 10, 64)
			if e != nil {
				return fmt.Errorf("invalid chat id %q: %w", args[0], e)
			}
			if setEnable && setDisable {
				return errors.New("--enable and --disable are mutually exclusive")
			}
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			var rules store.TargetRules
			changed := false
			if cmd.Flags().Changed("keywords") {
				rules.Keywords = splitCSV(setKeywords)
				changed = true
			}
			if cmd.Flags().Changed("exclude") {
				rules.ExcludeKeywords = splitCSV(setExclude)
				changed = true
			}
			if cmd.Flags().Changed("max-size") {
				n, e := parseSize(setMaxSize)
				if e != nil {
					return e
				}
				rules.MediaMaxSize = &n
				changed = true
			}
			if cmd.Flags().Changed("download-media") {
				rules.DownloadMedia = &setDownload
				changed = true
			}
			if setEnable || setDisable {
				on := setEnable
				rules.Enabled = &on
				changed = true
			}
			if !changed {
				return errors.New("nothing to change: pass at least one of --keywords/--exclude/--max-size/--download-media/--enable/--disable")
			}
			if e := s.UpdateTargetRules(cmd.Context(), chatID, rules); e != nil {
				return e
			}
			if setJSON {
				t, e := s.Target(cmd.Context(), chatID)
				if e != nil {
					return e
				}
				return printJSON(t)
			}
			fmt.Printf("updated monitor %d\n", chatID)
			return nil
		},
	}
	set.Flags().StringVar(&setKeywords, "keywords", "", "replace the keyword whitelist (comma separated; empty clears it)")
	set.Flags().StringVar(&setExclude, "exclude", "", "replace the keyword blacklist (comma separated; empty clears it)")
	set.Flags().StringVar(&setMaxSize, "max-size", "", "replace the per-file size cap, e.g. 200MB (0 clears it)")
	set.Flags().BoolVar(&setDownload, "download-media", true, "set whether media is downloaded for this chat")
	set.Flags().BoolVar(&setEnable, "enable", false, "enable monitoring")
	set.Flags().BoolVar(&setDisable, "disable", false, "pause monitoring without deleting the target")
	set.Flags().BoolVar(&setJSON, "json", false, "print the updated target as JSON")
	root.AddCommand(set)

	var listJSON bool
	list := &cobra.Command{Use: "list", Short: "List monitored chats", RunE: func(cmd *cobra.Command, _ []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		ts, e := s.Targets(cmd.Context())
		if e != nil {
			return e
		}
		if listJSON {
			return printJSON(ts)
		}
		for _, t := range ts {
			fmt.Printf("%d\t%s\tenabled=%t\tmedia=%t\tkeywords=%d\texclude=%d\n", t.ChatID, t.Username, t.Enabled, t.DownloadMedia, len(t.Keywords), len(t.ExcludeKeywords))
		}
		return nil
	}}
	list.Flags().BoolVar(&listJSON, "json", false, "emit the target list as JSON")
	root.AddCommand(add, list,
		&cobra.Command{Use: "remove <chat-id>", Short: "Stop monitoring a chat (archived data is kept)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			id, e := strconv.ParseInt(args[0], 10, 64)
			if e != nil {
				return e
			}
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			return s.RemoveTarget(cmd.Context(), id)
		}})
	return root
}
func parseSince(v string) (*time.Time, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, e := time.Parse(layout, v); e == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("invalid --since: %s", v)
}
func newArchiveSync() *cobra.Command {
	var limit int
	var resume bool
	cmd := &cobra.Command{Use: "sync <target>", Short: "Rescan a chat and close archive gaps", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		k, e := archiveKV(cmd.Context())
		if e != nil {
			return e
		}
		rt, c, e := newArchiveRuntime(cmd.Context(), k, s)
		if e != nil {
			return e
		}
		return rt.Client.Run(cmd.Context(), func(ctx context.Context) error {
			if st, e := rt.Client.Auth().Status(ctx); e != nil || !st.Authorized {
				if e != nil {
					return e
				}
				return errors.New("archive account is not logged in")
			}
			if e := rt.Peers.Init(ctx); e != nil {
				return e
			}
			p, e := archive.Resolve(ctx, rt, args[0])
			if e != nil {
				return e
			}
			t, e := s.Target(ctx, p.ID())
			if e != nil {
				return e
			}
			n, e := c.Backfill(ctx, rt, t, archive.BackfillOptions{Limit: limit, Resume: resume})
			if e != nil {
				return e
			}
			_ = archive.NewMediaWorker(s, rt, "").Run(ctx)
			_ = c.Exporter.ExportPending(ctx, t.ChatID, limit)
			_ = archive.NewAvatarWorker(s, rt, "").Run(ctx)
			fmt.Printf("synced %d messages\n", n)
			return nil
		})
	}}
	cmd.Flags().IntVar(&limit, "limit", 500, "maximum messages to rescan")
	cmd.Flags().BoolVar(&resume, "resume", false, "continue backfilling from the oldest archived message instead of the newest")
	return cmd
}
func newArchiveRun() *cobra.Command {
	return &cobra.Command{Use: "run", Short: "Run realtime monitor and periodic rescan", RunE: func(cmd *cobra.Command, _ []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		k, e := archiveKV(cmd.Context())
		if e != nil {
			return e
		}
		rt, c, e := newArchiveRuntime(cmd.Context(), k, s)
		if e != nil {
			return e
		}
		worker := archive.NewMediaWorker(s, rt, "")
		avatars := archive.NewAvatarWorker(s, rt, "")
		rescan := func(ctx context.Context) {
			// Gap-aware rescan, shared with the managed serve session.
			archive.RescanTargets(ctx, s, rt, c)
			// View/forward counters only refresh here or via the sweep; keep
			// both modes on the same cadence.
			archive.RefreshStatsTargets(ctx, s, rt)
			ts, _ := s.Targets(ctx)
			for _, t := range ts {
				if t.Enabled {
					_ = c.Exporter.ExportPending(ctx, t.ChatID, 200)
				}
			}
			_ = avatars.Run(ctx)
			if n, e := archive.CleanStaging(filepath.Join(consts.DataDir, "archive"), archive.DefaultStagingMaxAge); e == nil && n > 0 {
				fmt.Fprintf(os.Stderr, "archive gc: removed %d stale staging file(s)\n", n)
			}
			if b, e := archive.MaybeBackup(ctx, s); e != nil {
				fmt.Fprintf(os.Stderr, "archive backup: %v\n", e)
			} else if b != "" {
				fmt.Printf("archive backup: %s\n", b)
			}
		}
		// The media queue is drained on its own faster ticker (same cadence and
		// pause gate as the managed serve session): a realtime enqueue waits at
		// most one tick instead of up to a full sweep interval.
		go func() {
			select {
			case <-rt.Ready:
			case <-cmd.Context().Done():
				return
			}
			media := time.NewTicker(archive.MediaTick)
			defer media.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return
				case <-media.C:
					if _, e := archive.DrainMediaQueue(cmd.Context(), s, 5, func(ctx context.Context) error { return worker.Run(ctx) }); e != nil {
						fmt.Fprintf(os.Stderr, "archive media pass: %v\n", e)
					}
				}
			}
		}()
		go func() {
			select {
			case <-rt.Ready:
			case <-cmd.Context().Done():
				return
			}
			rescan(cmd.Context())
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-cmd.Context().Done():
					return
				case <-ticker.C:
					rescan(cmd.Context())
				}
			}
		}()
		fmt.Printf("archive monitor running; database=%s\n", archivePath())
		return rt.Run(cmd.Context())
	}}
}
func newArchiveSearch() *cobra.Command {
	var chat int64
	var limit int
	var after, before, sender, media string
	cmd := &cobra.Command{Use: "search <query>", Short: "Full-text search the local archive", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		opt := store.SearchOptions{ChatID: chat, Limit: limit}
		if sender != "" {
			id, e := resolveSenderID(cmd.Context(), s, sender)
			if e != nil {
				return e
			}
			opt.SenderID = id
		}
		for _, v := range []struct {
			val string
			dst **time.Time
		}{{after, &opt.After}, {before, &opt.Before}} {
			if strings.TrimSpace(v.val) == "" {
				continue
			}
			t, e := parseDayParam(v.val)
			if e != nil {
				return e
			}
			*v.dst = &t
		}
		if media != "" {
			m := strings.ToLower(strings.TrimSpace(media))
			if m != "any" && m != "image" && m != "video" && m != "file" && m != "audio" {
				return fmt.Errorf("invalid --media %q; use any, image, video, file, or audio", media)
			}
			if m == "any" {
				opt.HasMedia = true
			} else {
				opt.MediaType = m
			}
		}
		rs, e := s.SearchAdvanced(cmd.Context(), args[0], opt)
		if e != nil {
			return e
		}
		for _, r := range rs {
			fmt.Printf("%d/%d\t%s\t%s\n", r.ChatID, r.MessageID, time.Unix(r.Date, 0).Format("2006-01-02 15:04"), strings.ReplaceAll(strings.TrimSpace(r.Text), "\n", " "))
		}
		return nil
	}}
	cmd.Flags().Int64Var(&chat, "chat", 0, "filter by chat id")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum results")
	cmd.Flags().StringVar(&after, "after", "", "only messages on/after this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&before, "before", "", "only messages before this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&sender, "sender", "", "filter by sender (@username or user id)")
	cmd.Flags().StringVar(&media, "media", "", "filter by media: any, image, video, file, audio")
	return cmd
}
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseSize accepts "500MB", "1g", "1024" (bytes) and returns bytes.
func parseSize(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	if v == "" {
		return 0, nil
	}
	mult := int64(1)
	for i, suffix := range []string{"KB", "MB", "GB", "TB"} {
		if strings.HasSuffix(v, suffix) || strings.HasSuffix(v, suffix[:1]) {
			mult = int64(1024) << (10 * (i + 1))
			v = strings.TrimSuffix(strings.TrimSuffix(v, suffix), suffix[:1])
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	return int64(n * float64(mult)), nil
}

func parseDayParam(v string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, e := time.Parse(layout, v); e == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q: use YYYY-MM-DD", v)
}
func resolveSenderID(ctx context.Context, s *store.Store, sender string) (int64, error) {
	name := strings.TrimPrefix(strings.TrimSpace(sender), "@")
	if name == "" {
		return 0, fmt.Errorf("empty --sender")
	}
	var id int64
	if e := s.DB().QueryRowContext(ctx, "SELECT id FROM users WHERE lower(username)=lower(?)", name).Scan(&id); e == nil {
		return id, nil
	}
	if _, e := fmt.Sscan(name, &id); e != nil {
		return 0, fmt.Errorf("unknown sender %q", sender)
	}
	return id, nil
}
func newArchiveMessage() *cobra.Command {
	return &cobra.Command{Use: "message <chat-id> <message-id>", Short: "Show one archived message with its raw payload", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		chat, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil {
			return e
		}
		id, e := strconv.Atoi(args[1])
		if e != nil {
			return e
		}
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		v, e := s.Message(cmd.Context(), chat, id)
		if e != nil {
			return e
		}
		fmt.Println(v)
		return nil
	}}
}

// newArchiveGC reclaims unreferenced content-addressed objects. It defaults to a
// dry run so the destructive form is always an explicit choice.
func newArchiveGC() *cobra.Command {
	var apply, repair, jsonOut bool
	var stagingAge time.Duration
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim unreferenced media objects and stale staging files",
		Long: "Reclaim unreferenced content-addressed objects (files with no completed media row),\n" +
			"remove stale staging partials and optionally re-queue media whose object vanished.\n" +
			"Runs as a dry run unless --apply is given.",
		Example: "  tdl archive gc                 # show what would be reclaimed\n" +
			"  tdl archive gc --apply         # actually delete\n" +
			"  tdl archive gc --repair --apply",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			root := filepath.Join(consts.DataDir, "archive")
			rep, e := archive.GarbageCollect(cmd.Context(), s, root, archive.GCOptions{
				Apply: apply, Repair: repair, StagingMaxAge: stagingAge,
			})
			if e != nil {
				return e
			}
			if jsonOut {
				return printJSON(rep)
			}
			mode := "dry run"
			if rep.Applied {
				mode = "applied"
			}
			fmt.Printf("gc (%s): %d orphan object(s), %s reclaimable, %d files row(s), %d staging file(s)\n",
				mode, len(rep.OrphanObjects), humanBytes(rep.OrphanBytes), rep.FilesRowsPruned, rep.StagingRemoved)
			for _, sha := range rep.OrphanObjects {
				fmt.Printf("  orphan %s\n", sha)
			}
			if len(rep.RequeuedMedia) > 0 {
				fmt.Printf("  %d completed media object(s) missing from disk were re-queued\n", len(rep.RequeuedMedia))
			}
			if !apply && (len(rep.OrphanObjects) > 0 || rep.StagingRemoved > 0) {
				fmt.Println("re-run with --apply to delete")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "delete instead of only reporting")
	cmd.Flags().BoolVar(&repair, "repair", false, "also re-queue completed media whose object file is missing")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the report as JSON")
	cmd.Flags().DurationVar(&stagingAge, "staging-age", archive.DefaultStagingMaxAge, "remove staging partials older than this")
	return cmd
}

// botTokenFlag resolves the Telegram Bot API token for notification rules from
// the flag or the TGA_NOTIFY_BOT_TOKEN environment variable (tokens on command
// lines leak through shell history and process listings).
func botTokenFlag(flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		return strings.TrimSpace(flagValue)
	}
	return strings.TrimSpace(os.Getenv("TGA_NOTIFY_BOT_TOKEN"))
}

func newArchiveNotify() *cobra.Command {
	root := &cobra.Command{Use: "notify", Short: "Keyword-triggered notification outlets (webhook / Telegram Bot API)"}

	var addName, addKeywords, addExclude, addChats, addWebhook, addBotToken, addBotChat string
	var addDisabled bool
	add := &cobra.Command{Use: "add --name <name> --keywords <k1,k2> (--webhook <url> | --bot-token <token> --bot-chat-id <id>)", Short: "Create a notification rule", RunE: func(cmd *cobra.Command, _ []string) error {
		if strings.TrimSpace(addName) == "" {
			return errors.New("--name is required")
		}
		if strings.TrimSpace(addWebhook) == "" && (botTokenFlag(addBotToken) == "" || strings.TrimSpace(addBotChat) == "") {
			return errors.New("either --webhook or both --bot-token and --bot-chat-id are required")
		}
		var chatIDs []int64
		for _, p := range splitCSV(addChats) {
			id, e := strconv.ParseInt(p, 10, 64)
			if e != nil {
				return fmt.Errorf("invalid chat id %q", p)
			}
			chatIDs = append(chatIDs, id)
		}
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		id, e := s.AddNotifyRule(cmd.Context(), store.NotifyRule{
			Name:            strings.TrimSpace(addName),
			Keywords:        splitCSV(addKeywords),
			ExcludeKeywords: splitCSV(addExclude),
			ChatIDs:         chatIDs,
			WebhookURL:      strings.TrimSpace(addWebhook),
			BotToken:        botTokenFlag(addBotToken),
			BotChatID:       strings.TrimSpace(addBotChat),
			Enabled:         !addDisabled,
		})
		if e != nil {
			return e
		}
		fmt.Printf("notify rule %d created (%s)\n", id, addName)
		return nil
	}}
	add.Flags().StringVar(&addName, "name", "", "rule name")
	add.Flags().StringVar(&addKeywords, "keywords", "", "comma-separated keywords, any match triggers (empty = every archived message)")
	add.Flags().StringVar(&addExclude, "exclude", "", "comma-separated exclude keywords, a match suppresses")
	add.Flags().StringVar(&addChats, "chats", "", "comma-separated monitored chat ids (empty = all monitored chats)")
	add.Flags().StringVar(&addWebhook, "webhook", "", "HTTP(S) webhook URL receiving a JSON POST per match")
	add.Flags().StringVar(&addBotToken, "bot-token", "", "Telegram Bot API token (env TGA_NOTIFY_BOT_TOKEN)")
	add.Flags().StringVar(&addBotChat, "bot-chat-id", "", "Telegram chat id the bot sends to")
	add.Flags().BoolVar(&addDisabled, "disabled", false, "create the rule disabled")

	var listJSON bool
	list := &cobra.Command{Use: "list", Short: "List notification rules", RunE: func(cmd *cobra.Command, _ []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		rs, e := s.NotifyRules(cmd.Context())
		if e != nil {
			return e
		}
		if listJSON {
			return printJSON(rs)
		}
		for _, r := range rs {
			outlets := ""
			if r.WebhookURL != "" {
				outlets += " webhook"
			}
			if r.BotToken != "" && r.BotChatID != "" {
				outlets += " telegram:" + r.BotChatID
			}
			fmt.Printf("%d\t%s\tenabled=%t\tkeywords=%d\texclude=%d\tchats=%d%s\n", r.ID, r.Name, r.Enabled, len(r.Keywords), len(r.ExcludeKeywords), len(r.ChatIDs), outlets)
		}
		return nil
	}}
	list.Flags().BoolVar(&listJSON, "json", false, "emit the rule list as JSON")

	var enable, disable bool
	set := &cobra.Command{Use: "set <rule-id> [--keywords k1,k2] [--exclude e1,e2] [--enable|--disable]", Short: "Update a notification rule (credentials unchanged)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil {
			return e
		}
		r := store.NotifyRule{Enabled: enable || !disable}
		if cmd.Flags().Changed("keywords") {
			r.Keywords = splitCSV(addKeywords)
		}
		if cmd.Flags().Changed("exclude") {
			r.ExcludeKeywords = splitCSV(addExclude)
		}
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		return s.UpdateNotifyRule(cmd.Context(), id, r)
	}}
	set.Flags().StringVar(&addKeywords, "keywords", "", "comma-separated keywords (replaces the list)")
	set.Flags().StringVar(&addExclude, "exclude", "", "comma-separated exclude keywords (replaces the list)")
	set.Flags().BoolVar(&enable, "enable", false, "enable the rule")
	set.Flags().BoolVar(&disable, "disable", false, "disable the rule")

	remove := &cobra.Command{Use: "remove <rule-id>", Short: "Delete a notification rule", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil {
			return e
		}
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		return s.RemoveNotifyRule(cmd.Context(), id)
	}}

	var testText string
	test := &cobra.Command{Use: "test <rule-id>", Short: "Send a synthetic message through a rule's outlets", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, e := strconv.ParseInt(args[0], 10, 64)
		if e != nil {
			return e
		}
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		r, e := s.NotifyRule(cmd.Context(), id)
		if e != nil {
			return e
		}
		if testText == "" {
			testText = "tdl archive notify test: 如果你在目标聊天收到这条消息，说明规则 " + r.Name + " 的通知链路是通的。"
		}
		m := store.Message{ChatID: 0, MessageID: 0, Date: time.Now(), Text: testText}
		n := archive.NewNotifier(s)
		d := n.DispatchTo(cmd.Context(), r, m)
		if d.Status == "sent" {
			fmt.Printf("test notification sent through: %s\n", d.Channel)
			return nil
		}
		return fmt.Errorf("test notification %s: %s", d.Status, d.Error)
	}}
	test.Flags().StringVar(&testText, "text", "", "custom test message text")

	var logLimit int
	logCmd := &cobra.Command{Use: "log", Short: "Show recent notification delivery attempts", RunE: func(cmd *cobra.Command, _ []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		ds, e := s.RecentNotifyDeliveries(cmd.Context(), logLimit)
		if e != nil {
			return e
		}
		for _, d := range ds {
			fmt.Printf("%s\trule=%d\tchat=%d\tmsg=%d\t%s\t%s\t%s\n",
				time.Unix(d.CreatedAt, 0).Format("2006-01-02 15:04"), d.RuleID, d.ChatID, d.MessageID, d.Channel, d.Status, d.Error)
		}
		return nil
	}}
	logCmd.Flags().IntVar(&logLimit, "limit", 20, "number of recent attempts")

	root.AddCommand(add, list, set, remove, test, logCmd)
	return root
}

// printJSON writes a value as indented JSON (shared by the --json flags).
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// humanBytes renders a byte count for humans.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func newArchiveStats() *cobra.Command {
	var statsJSON bool
	stats := &cobra.Command{Use: "stats", Short: "Show archive counters", RunE: func(cmd *cobra.Command, _ []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		m, media, t, e := s.Stats(cmd.Context())
		if e != nil {
			return e
		}
		if statsJSON {
			return printJSON(map[string]any{"messages": m, "media": media, "monitors": t, "database": archivePath()})
		}
		fmt.Printf("messages: %d\nmedia: %d\nmonitors: %d\ndatabase: %s\n", m, media, t, archivePath())
		return nil
	}}
	stats.Flags().BoolVar(&statsJSON, "json", false, "emit counters as JSON")
	return stats
}
func newArchiveDoctor() *cobra.Command {
	return &cobra.Command{Use: "doctor", RunE: func(cmd *cobra.Command, _ []string) error {
		p := archivePath()
		if _, e := os.Stat(p); e != nil {
			fmt.Printf("database: missing (%s)\n", p)
			return nil
		}
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		var quick string
		if e := s.DB().QueryRowContext(cmd.Context(), "PRAGMA quick_check").Scan(&quick); e != nil || quick != "ok" {
			return fmt.Errorf("database integrity: %s (%v)", quick, e)
		}
		fmt.Println("database integrity: ok (quick_check)")
		m, media, t, e := s.Stats(cmd.Context())
		if e != nil {
			return e
		}
		fmt.Printf("messages: %d\nmedia: %d\nmonitors: %d\napp-id: %d\nsession namespace: %s\n", m, media, t, viper.GetInt("telegram.app-id"), archiveNamespace)

		// session readability: an encrypted store needs the passphrase; a
		// running serve holds the bolt KV write lock, which is expected.
		kvd, kvErr := archiveKV(cmd.Context())
		if kvErr != nil {
			if errors.Is(kvErr, bbolt.ErrTimeout) {
				fmt.Println("session: locked by another tdl process (serve running?) — skipped")
			} else {
				return fmt.Errorf("session: unreadable: %w", kvErr)
			}
		} else {
			enc, _ := archive.IsEncrypted(cmd.Context(), kvd)
			if enc {
				fmt.Println("session: ok (encrypted at rest)")
			} else {
				fmt.Println("session: ok (plaintext; consider --session-passphrase)")
			}
		}

		// disk watermark
		if free, e := archive.FreeDiskBytes(filepath.Dir(archivePath())); e == nil {
			status := "ok"
			if free < uint64(archive.DefaultMinFreeSpace) {
				status = "LOW — media downloads are paused"
			}
			fmt.Printf("disk free: %d bytes (%s)\n", free, status)
		}

		// latest backup
		if latest := archive.LatestBackup(archivePath()); latest != "" {
			if fi, e := os.Stat(latest); e == nil {
				fmt.Printf("last backup: %s (%s ago)\n", fi.Name(), time.Since(fi.ModTime()).Round(time.Hour))
			}
		} else {
			fmt.Println("last backup: none yet (automatic every 24h while run/serve is active)")
		}

		if viper.GetInt("telegram.app-id") <= 0 || strings.TrimSpace(viper.GetString("telegram.app-hash")) == "" {
			return errors.New("telegram app credentials are not configured")
		}
		return nil
	}}
}
func newArchiveMedia() *cobra.Command {
	root := &cobra.Command{Use: "media", Short: "Inspect and retry media archive jobs"}
	var pendingJSON bool
	pending := &cobra.Command{Use: "pending", Short: "List queued or failed media downloads", RunE: func(cmd *cobra.Command, _ []string) error {
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()
		jobs, e := s.PendingMedia(cmd.Context(), 500)
		if e != nil {
			return e
		}
		if pendingJSON {
			return printJSON(jobs)
		}
		for _, j := range jobs {
			fmt.Printf("%d\t%d/%d\t%s\tattempts=%d\n", j.ID, j.ChatID, j.MessageID, j.Status, j.Attempts)
		}
		return nil
	}}
	pending.Flags().BoolVar(&pendingJSON, "json", false, "emit media jobs as JSON")
	root.AddCommand(pending,
		&cobra.Command{Use: "retry <job-id>", Short: "Re-queue one failed media job", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			id, e := strconv.ParseInt(args[0], 10, 64)
			if e != nil {
				return e
			}
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			return s.RetryMedia(cmd.Context(), id)
		}})
	return root
}
func validateArchiveServeSecurity(addr, token, basic string, insecure bool) error {
	if token != "" && basic != "" {
		return errors.New("--auth-token and --auth-basic are mutually exclusive")
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "" {
		host = "0.0.0.0" // ":8080" binds every interface
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsUnspecified() || (ip.To4() != nil && ip.To4().Equal(net.IPv4zero))) && token == "" && basic == "" && !insecure {
		return errors.New("refusing to bind archive web without authentication on an unspecified address; configure --auth-token/--auth-basic or use --insecure")
	}
	return nil
}

func newArchiveServe() *cobra.Command {
	var addr, authToken, authBasic string
	var insecure bool
	cmd := &cobra.Command{Use: "serve", Short: "Serve the local archive viewer", RunE: func(cmd *cobra.Command, _ []string) error {
		configureArchiveLogging(cmd)
		s, e := openArchiveStore()
		if e != nil {
			return e
		}
		defer s.Close()

		if strings.TrimSpace(authToken) == "" {
			authToken = strings.TrimSpace(os.Getenv("TDL_ARCHIVE_AUTH_TOKEN"))
		}
		if strings.TrimSpace(authBasic) == "" {
			authBasic = strings.TrimSpace(os.Getenv("TDL_ARCHIVE_AUTH_BASIC"))
		}
		if e := validateArchiveServeSecurity(addr, authToken, authBasic, insecure); e != nil {
			return e
		}

		var auth archive.WebAuthConfig
		if authToken != "" {
			auth.Token = authToken
		}
		if authBasic != "" {
			parts := strings.SplitN(authBasic, ":", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
				return errors.New("--auth-basic must be in user:pass format")
			}
			auth.BasicUser, auth.BasicPassword = parts[0], parts[1]
		}
		if auth.Token == "" && auth.BasicUser == "" {
			slog.Warn("archive web has no authentication; defaulting to loopback",
				slog.String("bind_policy", "127.0.0.1"))
			if insecure {
				slog.Warn("archive web insecure mode enabled",
					slog.Bool("insecure", true))
			}
		}
		var ctl *archive.Controller
		if appID := viper.GetInt("telegram.app-id"); appID > 0 && strings.TrimSpace(viper.GetString("telegram.app-hash")) != "" {
			kvd, kvErr := archiveKV(cmd.Context())
			if kvErr != nil {
				return kvErr
			}
			ctl = archive.NewController(cmd.Context(), kvd, s, appID, viper.GetString("telegram.app-hash"))
			defer ctl.Close()
			slog.Info("archive web management enabled",
				slog.Bool("management_enabled", true))
		} else {
			slog.Warn("archive web is read-only; Telegram app credentials are not configured",
				slog.Bool("management_enabled", false))
		}
		srv := &http.Server{Addr: addr, Handler: archive.NewManagedWebServer(s, ctl, auth).Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
		go func() { <-cmd.Context().Done(); _ = srv.Close() }()
		slog.Info("archive web listening", slog.String("addr", addr))
		e = srv.ListenAndServe()
		if errors.Is(e, http.ErrServerClosed) {
			return nil
		}
		return e
	}}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "listen address")
	cmd.Flags().StringVar(&authToken, "auth-token", "", "require Bearer token authentication")
	cmd.Flags().StringVar(&authBasic, "auth-basic", "", "require HTTP Basic authentication, format user:pass")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "allow unauthenticated binding to an unspecified address")
	return cmd
}

func newArchiveDestination() *cobra.Command {
	root := &cobra.Command{Use: "destination", Short: "Manage Telegram archive destinations"}
	var destJSON bool
	root.AddCommand(
		&cobra.Command{Use: "create-channel <title>", Short: "Create and enable a Telegram archive channel", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			k, e := archiveKV(cmd.Context())
			if e != nil {
				return e
			}
			rt, _, e := newArchiveRuntime(cmd.Context(), k, s)
			if e != nil {
				return e
			}
			return rt.Client.Run(cmd.Context(), func(ctx context.Context) error {
				st, e := rt.Client.Auth().Status(ctx)
				if e != nil {
					return e
				}
				if !st.Authorized {
					return errors.New("archive account is not logged in; run tdl archive login")
				}
				if e := rt.Peers.Init(ctx); e != nil {
					return e
				}
				id, e := (&archive.ChannelExporter{Runtime: rt, Store: s}).CreateChannel(ctx, args[0], "Telegram Archive destination created by tdl")
				if e != nil {
					return e
				}
				if _, e = s.AddDestination(ctx, args[0], "telegram", &id); e != nil {
					return e
				}
				fmt.Printf("created and enabled Telegram archive channel: %d\n", id)
				return nil
			})
		}},
		&cobra.Command{Use: "add <@channel|chat-id>", Short: "Enable an existing Telegram channel as destination", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			k, e := archiveKV(cmd.Context())
			if e != nil {
				return e
			}
			rt, _, e := newArchiveRuntime(cmd.Context(), k, s)
			if e != nil {
				return e
			}
			return rt.Client.Run(cmd.Context(), func(ctx context.Context) error {
				if e := rt.Peers.Init(ctx); e != nil {
					return e
				}
				p, e := archive.Resolve(ctx, rt, args[0])
				if e != nil {
					return e
				}
				if _, ok := p.InputPeer().(*tg.InputPeerChannel); !ok {
					return errors.New("destination must be a Telegram channel")
				}
				id := p.ID()
				name := p.VisibleName()
				_, e = s.AddDestination(ctx, name, "telegram", &id)
				if e == nil {
					fmt.Printf("enabled Telegram destination %s (%d)\n", name, id)
				}
				return e
			})
		}},
		&cobra.Command{Use: "list", Short: "List archive destinations", RunE: func(cmd *cobra.Command, _ []string) error {
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			ds, e := s.Destinations(cmd.Context())
			if e != nil {
				return e
			}
			if destJSON {
				return printJSON(ds)
			}
			for _, d := range ds {
				fmt.Printf("%d\t%s\t%s\tchat=%d\tenabled=%t\n", d.ID, d.Name, d.Kind, d.ChatID.Int64, d.Enabled)
			}
			return nil
		}},
		&cobra.Command{Use: "enable <id>", Short: "Resume mirroring to a destination", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			id, e := strconv.ParseInt(args[0], 10, 64)
			if e != nil {
				return e
			}
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			return s.SetDestinationEnabled(cmd.Context(), id, true)
		}},
		&cobra.Command{Use: "attach <source-chat-id> <destination-id>", Short: "Mirror one monitored chat into a destination", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			source, e := strconv.ParseInt(args[0], 10, 64)
			if e != nil {
				return e
			}
			dest, e := strconv.ParseInt(args[1], 10, 64)
			if e != nil {
				return e
			}
			if _, e = s.Destination(cmd.Context(), dest); e != nil {
				return e
			}
			return s.AttachDestination(cmd.Context(), source, dest)
		}},
		&cobra.Command{Use: "disable <id>", Short: "Pause mirroring to a destination", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			id, e := strconv.ParseInt(args[0], 10, 64)
			if e != nil {
				return e
			}
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			return s.SetDestinationEnabled(cmd.Context(), id, false)
		}},
		&cobra.Command{Use: "status", RunE: func(cmd *cobra.Command, _ []string) error {
			s, e := openArchiveStore()
			if e != nil {
				return e
			}
			defer s.Close()
			ds, e := s.Destinations(cmd.Context())
			if e != nil {
				return e
			}
			enabled := 0
			for _, d := range ds {
				if d.Enabled {
					enabled++
				}
			}
			fmt.Printf("local database: enabled\nTelegram destinations: %d\n", enabled)
			return nil
		}},
	)
	for _, sub := range root.Commands() {
		if sub.Name() == "list" {
			sub.Flags().BoolVar(&destJSON, "json", false, "emit destinations as JSON")
		}
	}
	return root
}
