//go:build !cgo

package cmd

import (
	"github.com/go-faster/errors"
	"github.com/spf13/cobra"
)

// NewGUI is the stub used by CGO_ENABLED=0 builds: the Fyne desktop downloader
// needs cgo for OpenGL and is not linked into the static binary.
func NewGUI() *cobra.Command {
	return &cobra.Command{
		Use:     "gui",
		Aliases: []string{"desktop"},
		Short:   "Start the desktop downloader (not available in this static build)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("this binary was built with CGO_ENABLED=0 and has no desktop GUI; rebuild with CGO_ENABLED=1 or use the web/CLI commands")
		},
	}
}
