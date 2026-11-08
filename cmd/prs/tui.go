package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/datapointchris/prs/internal/board"
	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
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
		Long: `The board lists every open pull request on the repos prs reads, which prs config
show names, oldest first, and reads each provider again while it is open. Each row gives its checks and
review in words, its age, its diff, its repo and number, and its title. f shows its
branch and checkout under it.

enter opens a pull request's description, full screen where it runs past the screen.
A letter acts on the pull request under the cursor, and ? lists every key. d hands the
terminal to its diff in nvim, and :qa comes back to the list. m merges and x closes,
each after asking, and c posts what your editor wrote. Each runs while the list stays
usable, named on the footer until it ends. p opens its page and comments in a tmux
window, and the config can add keys of its own, each a command in a tmux window. esc
backs out one level, and q quits.

--once prints one frame at COLUMNS by LINES, and --json the snapshot it is drawn from,
so an agent reads what a person reads. Either exits 1 after printing when a provider
could not be read or left pull requests out. With no terminal and neither flag it
refuses.`,
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
				b.ReadDetails()
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
				err = encoder.Encode(b.Snapshot(time.Now()))
			} else {
				width, height := gotermboard.OnceSize(onceFallbackWidth, onceFallbackHeight)
				_, err = fmt.Fprintln(cmd.OutOrStdout(), gotermboard.Once(b, pal, width, height))
			}
			if err != nil {
				return err
			}
			// A failed read and a repo with no open pull requests print the same rows, so
			// the exit status is what tells a script the two apart.
			if gaps := b.Gaps(); len(gaps) > 0 {
				return fmt.Errorf("could not read every open pull request: %s", strings.Join(gaps, "; "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&view, "view", "", "Open on this tab: "+board.View)
	cmd.Flags().BoolVar(&once, "once", false, "Print one frame of the tab at COLUMNS by LINES, then exit")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the tab's snapshot as JSON to stdout")
	return cmd
}

// loadBoard reads the config and the repos it chooses: the registry's, the owners', or
// with neither, the token's own account's.
func loadBoard() (*board.Board, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	repos := board.Repos{From: cfg.ReposFrom, Registry: cfg.Registry}
	switch cfg.ReposFrom {
	case config.FromOwners:
		repos.Owners = cfg.Owners
	case config.FromViewer:
		repos.Owners = []string{pullrequests.ViewerOwner}
	default:
		if repos.Repos, err = registry.Load(cfg.Registry); err != nil {
			return nil, err
		}
	}
	b, err := board.New(repos, cfg.WindowActions)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.File, err)
	}
	return b, nil
}
