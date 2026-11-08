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
		view, openOn string
		once, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the board of every open pull request",
		Long: fmt.Sprintf(`The board lists every open pull request on the repos prs reads, which prs config
show names, oldest first. Each row gives its checks and review in words, its age, its
diff, its repo and number, and its title. f shows its branch and checkout under it.

It opens on the rows the last run on the same repos read, kept under $XDG_CACHE_HOME/prs,
with their age on the first line, and reads each provider behind them. It reads them again
every refresh_every_seconds in the config, %d by default, whenever its terminal gains
focus, and whenever a program a key handed the terminal to exits. A spinner follows the age
while a read runs, and a read that fails keeps the rows and names why on that line. A
refresh leaves the cursor on its pull request, and one the read no longer lists stays under
the cursor, marked gone, until the cursor moves off it.`, int(config.DefaultRefreshEvery.Seconds())) + `

A pull request's document holds its ` + board.DocumentSectionNames() + `, each under a heading.
On a wide terminal a side panel shows the document of the row under the cursor, and enter
opens it full screen, where [ and ] jump between headings and / searches it. Narrower,
enter opens it in place where it fits on the screen.

A letter acts on the pull request under the cursor, and ? lists every key. d hands the
terminal to its diff in nvim, and :qa comes back to the list. m merges and x closes,
each after asking, and c posts what your editor wrote. Each runs while the list stays
usable, named on the footer until it ends. The config can add keys of its own, each a
command in a tmux window. esc backs out one level, and q quits.

--pr opens on one pull request, named as owner/name#number or by its URL, with the cursor
on it and its document showing. q then exits 0, back to whatever ran prs. An open one sits
where the list puts it, and a merged or closed one sits above the list. One no listing
holds is read again on the same timer. A pull request prs cannot read is refused before
the board draws.

--once prints one frame at COLUMNS by LINES, and --json the snapshot it is drawn from,
so an agent reads what a person reads. Either exits 1 after printing when a provider
could not be read or left pull requests out. With no terminal and neither flag it
refuses.`,
		Example: `  prs tui
  prs tui --pr 'acme/widget#12'
  prs tui --pr https://github.com/acme/widget/pull/12
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
			var ref *pullrequests.Reference
			if openOn != "" {
				parsed, err := pullrequests.ParseReference(openOn)
				if err != nil {
					return goclikit.UsageError(fmt.Errorf("--pr: %w", err))
				}
				ref = &parsed
			}
			printing := once || asJSON
			if !printing && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
				return goclikit.UsageError(errors.New("prs tui needs a terminal; --once prints one frame and --json the snapshot"))
			}
			b, err := loadBoard()
			if err != nil {
				return err
			}
			if ref != nil {
				if err := b.OpenOn(cmd.Context(), *ref); err != nil {
					return err
				}
			}
			pal := gotermstyle.PaletteFor(os.Stdout)
			if !printing {
				b.ReadDetails()
				// With no home and no XDG_CACHE_HOME the board reads first, as it would with
				// nothing kept.
				if kept, err := config.ListingCacheDir(); err == nil {
					b.KeepListings(kept)
				}
				tabs, err := gotermboard.NewTabs([]gotermboard.Tab{{Name: board.View, Board: b}}, pal, view)
				if err != nil {
					return err
				}
				_, err = tea.NewProgram(reportsFocus{tabs}).Run()
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
	cmd.Flags().StringVar(&openOn, "pr", "", "Open on this pull request, as owner/name#number or its URL")
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
	b, err := board.New(repos, cfg.RefreshEvery, cfg.WindowActions)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.File, err)
	}
	return b, nil
}

// reportsFocus is the board's program with the terminal's focus reported, so the board reads
// the providers again when its window or pane is returned to, such as from a tmux window a
// key opened. tmux passes focus on only with its focus-events option on.
type reportsFocus struct{ tea.Model }

func (r reportsFocus) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	model, cmd := r.Model.Update(msg)
	return reportsFocus{model}, cmd
}

func (r reportsFocus) View() tea.View {
	view := r.Model.View()
	view.ReportFocus = true
	return view
}
