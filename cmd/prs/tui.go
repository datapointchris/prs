package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/datapointchris/prs/internal/board"
	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/registry"
)

// The size --once draws at where neither the environment nor a terminal gives one.
const (
	onceFallbackWidth  = 160
	onceFallbackHeight = 50
)

func tuiCommand() *cobra.Command {
	var (
		view         string
		once, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the board of every open pull request",
		Long: `One program with a tab per view. The pull-requests tab is every open pull request on
the repos the registry names, oldest first, each read again every minute while the board
is open. Each row says its checks and its review in words, its age, its diff and its
title, then its branch, the pull request it is stacked on, its checkout, its number and
the app that opened it.

enter opens a pull request's description, full screen where it runs past the screen.
A letter acts on the pull request under the cursor, and ? lists every key. d opens its
diff in nvim and p its page and comments, each in a tmux window. The config can add
keys of its own, each a command in a tmux window. esc backs out one level, and q quits.

--once prints one frame at COLUMNS by LINES, and --json the snapshot it is drawn from,
so an agent reads what a person reads. With no terminal and neither flag it refuses.`,
		Example: `  prs tui
  COLUMNS=160 LINES=50 prs tui --once > frame.txt
  prs tui --json | jq '.pull_requests[] | {slug, number, checks}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if view != "" && view != board.View {
				return goclikit.UsageError(fmt.Errorf("--view takes %s, not %q", board.View, view))
			}
			if once && asJSON {
				return goclikit.UsageError(errors.New("--once prints a frame and --json a snapshot, so give one of them"))
			}
			printing := once || asJSON
			if !printing && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
				return goclikit.UsageError(errors.New("prs tui needs a terminal; --once prints one frame and --json the snapshot"))
			}
			b, err := loadBoard()
			if err != nil {
				return err
			}
			pal := gotermstyle.PaletteFor(os.Stdout)
			if !printing {
				tabs, err := gotermboard.NewTabs([]gotermboard.Tab{{Name: board.View, Board: b}}, pal, view)
				if err != nil {
					return err
				}
				_, err = tea.NewProgram(tabs).Run()
				return err
			}
			gotermboard.RefreshAll(cmd.Context(), b.Sources()...)
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				encoder.SetEscapeHTML(false)
				return encoder.Encode(b.Snapshot(time.Now()))
			}
			width, height := gotermboard.OnceSize(onceFallbackWidth, onceFallbackHeight)
			_, err = fmt.Fprintln(cmd.OutOrStdout(), gotermboard.Once(b, pal, width, height))
			return err
		},
	}
	cmd.Flags().StringVar(&view, "view", "", "Open on this tab: "+board.View)
	cmd.Flags().BoolVar(&once, "once", false, "Print one frame of the tab at COLUMNS by LINES, then exit")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the tab's snapshot as JSON to stdout")
	return cmd
}

// loadBoard reads the config and the registry it names.
func loadBoard() (*board.Board, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Registry == "" {
		return nil, errors.New("prs reads its repos from a registry, and the config names none: " +
			"set registry in the file prs config path prints")
	}
	repos, err := registry.Load(cfg.Registry)
	if err != nil {
		return nil, err
	}
	return board.New(cfg.Registry, repos, cfg.WindowActions), nil
}
