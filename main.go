package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	surveyterm "github.com/AlecAivazis/survey/v2/terminal"
	"github.com/fatih/color"
	"github.com/go-faster/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.etcd.io/bbolt"

	"github.com/iyear/tdl/app/tgbot"
	"github.com/iyear/tdl/cmd"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	loadConfig()

	root := cmd.New()

	if token := strings.TrimSpace(viper.GetString("bot.token")); token != "" && !isLocalOnlyCommand(root, os.Args[1:]) {
		if err := tgbot.Run(ctx, tgbot.Options{
			Token:      token,
			Debug:      viper.GetBool("bot.debug"),
			ConfigPath: viper.ConfigFileUsed(),
		}); err != nil && !errors.Is(err, context.Canceled) {
			color.Red("Error: %+v", err)
			os.Exit(1)
		}
		return
	}

	humanizeErrors := map[error]string{
		bbolt.ErrTimeout:        "Current database is used by another process, please terminate it first",
		surveyterm.InterruptErr: "Interrupted",
	}

	if err := root.ExecuteContext(ctx); err != nil {
		for e, m := range humanizeErrors {
			if errors.Is(err, e) {
				color.Red("%s", m)
				os.Exit(1)
			}
		}

		color.Red("Error: %+v", err)
		os.Exit(1)
	}
}

// isLocalOnlyCommand reports whether the invocation resolves to a command that
// must run in the local terminal (gui, desktop, archive) rather than hand over
// to the Telegram bot.
//
// Cobra resolves the command tree, so flag values are never mistaken for a
// command name: `tdl dl -n archive` targets `dl`, not `archive`, which a plain
// scan of os.Args would get wrong.
func isLocalOnlyCommand(root *cobra.Command, args []string) bool {
	target, _, err := root.Find(args)
	if err != nil {
		return false
	}
	for c := target; c != nil; c = c.Parent() {
		switch c.Name() {
		case "gui", "desktop", "archive":
			return true
		}
	}
	return false
}

func loadConfig() {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.SetEnvPrefix("tdl")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	viper.AutomaticEnv()

	viper.SetDefault("bot.debug", false)

	_ = viper.ReadInConfig()
	if viper.ConfigFileUsed() == "" {
		viper.SetConfigFile("config.yaml")
	}
}
