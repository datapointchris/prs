package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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

// tuiTab is one tab prs tui draws: its name, which --view takes, the flags beside --view,
// --once and --json that its printed frame reads, where to go for the rest, why the config
// leaves it out, and how it opens on the board. A nil absent is a tab every config draws.
type tuiTab struct {
	name      string
	prints    []string
	elsewhere string
	absent    func(cfg config.Config) string
	open      func(b *board.Board, cfg config.Config) openedTab
}

// openedTab is a tab opened on the board: what gotermboard draws, what --json prints, why
// --once or --json exits 1, and what it holds open, nil where it holds nothing.
type openedTab struct {
	board    gotermboard.Board
	snapshot func(now time.Time) any
	missed   func() error
	close    func() error
}

// tuiTabs are prs tui's tabs, in the order tab steps through them, the first opening where
// --view is not given.
var tuiTabs = []tuiTab{
	{name: board.View, prints: []string{"since", "pr"}, open: boardTab},
	{name: board.SummaryView, elsewhere: "prs summary --since and --until set the window it counts", open: summaryTab},
	{name: board.RecapsView, absent: recapsAbsent, open: recapsTab},
}

// recapsAbsent says why the config draws no Recaps tab: it names no list command, or a
// program its commands need is not on PATH, as on a machine that has none of them.
func recapsAbsent(cfg config.Config) string {
	if !cfg.Recaps.Named() {
		return "the config names no recaps commands"
	}
	if missing := cfg.Recaps.Missing(); len(missing) > 0 {
		return "the recaps commands need " + strings.Join(missing, ", ") + ", which PATH does not hold"
	}
	return ""
}

func recapsTab(b *board.Board, cfg config.Config) openedTab {
	// With no home and no XDG_CACHE_HOME a write's output goes nowhere, and a failed one says
	// only its error.
	logs, _ := config.RecapWritesDir()
	r := b.OpenRecaps(cfg.Recaps, logs)
	return openedTab{
		board:    r,
		snapshot: func(time.Time) any { return r.Snapshot() },
		missed: func() error {
			if faults := r.Faults(); len(faults) > 0 {
				return errors.New(strings.Join(faults, "; "))
			}
			return nil
		},
	}
}

// drawnTabs are the tabs the config draws, in tuiTabs' order.
func drawnTabs(cfg config.Config) []tuiTab {
	return slices.DeleteFunc(slices.Clone(tuiTabs), func(tab tuiTab) bool { return tab.absent != nil && tab.absent(cfg) != "" })
}

func tuiTabNames() []string {
	names := make([]string, len(tuiTabs))
	for i, tab := range tuiTabs {
		names[i] = tab.name
	}
	return names
}

func boardTab(b *board.Board, _ config.Config) openedTab {
	return openedTab{
		board:    b,
		snapshot: func(now time.Time) any { return b.Snapshot(now) },
		// A failed read and a repo with no open pull requests print the same rows, so the
		// exit status is what tells a script the two apart.
		missed: func() error {
			var missed []string
			if gaps := b.Gaps(); len(gaps) > 0 {
				missed = append(missed, "could not read every open pull request: "+strings.Join(gaps, "; "))
			}
			if faults := b.ClosedFaults(); len(faults) > 0 {
				missed = append(missed, "could not read the recently closed pull requests: "+strings.Join(faults, "; "))
			}
			if len(missed) > 0 {
				return errors.New(strings.Join(missed, "; "))
			}
			return nil
		},
	}
}

func summaryTab(b *board.Board, cfg config.Config) openedTab {
	window, _ := pullrequests.ParseSince(board.DefaultSummaryWindow, time.Local)
	s := openSummary(b, cfg, window)
	return openedTab{
		board:    s,
		snapshot: func(now time.Time) any { return s.Snapshot(now) },
		missed:   func() error { return summaryFaults(s) },
		close:    s.Close,
	}
}

func tuiCommand() *cobra.Command {
	var (
		view, openOn, since string
		once, asJSON        bool
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Open the board of every open pull request, and those recently closed",
		Long: fmt.Sprintf(`The board lists every open pull request on the repos prs reads, which prs config
show names, in three groups. people holds those a person opened, and bots those an app
opened, each oldest first. recently closed holds those merged or closed within --since,
newest first. Each header counts its rows, and enter on it folds them.

Each pull request takes two lines. The first gives where it stands in one word: ready,
behind, conflicts, blocked, mergeable with checks not passing, draft or unknown while open,
and merged or closed after. Then its title, its diff, its age, or how long ago it closed,
and its branch. The second gives its checks in words, its checkout, its number and its
review, then what was pushed since its newest review, as 2 commits since review, or rebased
since review where the branch does not hold the commit that review read, and names the app
that opened it. A Bitbucket row reads open, and only one merged
here reaches recently closed, because prs reads no merge state or close time from
Bitbucket.

A pull request a Claude review round reviewed reads the newest round after its review:
its findings by section once the round comment posts, as in claude 2 correctness · 1
design, with any reviewer that did not run counted beside them. Before the round comment,
it reads claude running while the reviews post and claude unfinished once they have
stopped for an hour. It reads claude reviewed where the newest review is of a version prs
does not read. A window action marked runs_review_round reads running while its window is
open and the round has not finished.

It opens on the rows the last run on the same repos read, kept under $XDG_CACHE_HOME/prs,
with their age on the first line, and reads each provider behind them. It reads the open
pull requests again every refresh_every_seconds in the config, %d by default, whenever its
terminal gains focus, and whenever a program a key handed the terminal to exits. It reads
the recently closed, and the Summary tab what changed in its history, every
closed_refresh_every_seconds, %d by default, because each read costs GitHub as much again. A spinner follows the age while a read runs, and a read that
fails keeps the rows and names why on that line. A refresh leaves the cursor on its pull
request, in the group it was in, and one no read lists any more stays under the cursor,
marked gone, until the cursor moves off it.`,
			int(config.DefaultRefreshEvery.Seconds()), int(config.DefaultClosedRefreshEvery.Seconds())) + `

A pull request's document holds its ` + board.DocumentSectionNames() + `,
each under a heading. On a wide terminal a side panel shows the document of the row under
the cursor, and enter opens it full screen, where [ and ] jump between headings and /
searches it. Narrower, enter opens it in place where it fits on the screen.

The Diff section shows the diff whole, each file under a heading of its own, up to
diff_section_max_lines changed lines in the config, and names the size of a larger one. Each
listed pull request's diff, recently closed ones included, is read once its list lands.
GitHub sends it. Bitbucket's client
has no diff, so prs fetches a Bitbucket pull request's head into
refs/remotes/origin/pr/<number> in its checkout and diffs it there, touching no other ref.
One whose repo is not cloned here gets a section saying so.

A letter acts on the pull request under the cursor, and ? lists every key. d hands the
terminal to its diff in nvim, and :qa comes back to the list. i opens only what was pushed
since its newest review, from the commit that review read to the head. Where no review was
submitted, a rebase took that commit off the branch, or a merge brought the base in since,
it opens the whole diff, and the footer says why once nvim exits. m merges and x closes,
each after asking, and its row moves to recently closed once it has, while the cursor
moves on to the next open row in its group. c posts what your editor wrote. Each runs
while the list stays usable, named on the footer until it ends.

space marks a row whose checks pass and whose merge state GitHub reads as ready, and
names why it refuses any other. With rows marked, m merges the marked rows rather than
the cursor's. It asks once, listing each one and each it leaves out with why, then
merges them one at a time, reading each again just before and leaving one no longer
ready.
The config can add keys of its own, each a command in a tmux window. esc backs out one
level, and q quits.

--pr opens on one pull request, named as owner/name#number or by its URL, with the cursor
on it and its document showing. q then exits 0, back to whatever ran prs. An open one sits
where the list puts it, and a merged or closed one sits above the list. One no listing
holds is read again on the same timer. A pull request prs cannot read is refused before
the board draws.

` + fmt.Sprintf(`tab moves to the Summary tab, and --view summary opens on it. It counts the pull requests
opened, merged and closed over a window, %[1]s at first, which w and W step through
%[2]s. It charts the merges per day, the open pull requests and the times to merge.
prs summary --help says what each count is and where they are kept.

The recaps tab shows where the config names recaps commands and PATH holds what they run.
It lists the windows in the order the list command prints them, and the side panel shows
the recap the show command prints for the one under the cursor, which enter opens full
screen. w writes a recap with the write command, a model call, after asking. y writes the
cursor's window, or type another's kind and label, such as one the list does not hold yet,
and enter alone writes nothing. The write runs while the list stays usable, its output in a
file under $XDG_CACHE_HOME/prs/recap-writes, and quitting leaves it to finish. The list
runs again once it ends, and each run of the list reads every recap again. prs config path
says what the three commands take.

--once prints one frame at COLUMNS by LINES, and --json the snapshot it is drawn from,
so an agent reads what a person reads. Neither reads a pull request's own detail, so both
leave out what the open board fills in from it: the Claude review round, the commits
since review, and the document's review, checks, files and comments. The snapshot holds
the open pull requests under pull_requests and the recently closed under recently_closed.
Either exits 1 after printing when a read failed, or when the open pull requests' read
left some out. A recently closed read past its page keeps the newest, and says so on the
first line. With --view summary, the frame is the Summary tab's and the snapshot is what
prs summary --json prints for %[1]s. With --view recaps, the snapshot holds each window the
list names and the recap of its first, which the frame shows, and either exits 1 where the
list or that recap could not be read, or the list printed a line that is not a window.
--since and --pr are the board's, so the other tabs refuse them with --once or --json. With no terminal and neither flag it refuses.`, board.DefaultSummaryWindow, strings.Join(board.SummaryWindows, ", ")),
		Example: `  prs tui
  prs tui --pr 'acme/widget#12'
  prs tui --pr https://github.com/acme/widget/pull/12
  prs tui --since 1w
  COLUMNS=160 LINES=50 prs tui --once > frame.txt
  prs tui --json | jq '.pull_requests[] | {slug, number, checks}'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			chosen := tuiTabs[0]
			if view != "" {
				at := slices.Index(tuiTabNames(), view)
				if at < 0 {
					return goclikit.UsageError(fmt.Errorf("--view takes %s, not %q", strings.Join(tuiTabNames(), " or "), view))
				}
				chosen = tuiTabs[at]
			}
			if once && asJSON {
				return goclikit.UsageError(errors.New("--once prints a frame and --json a snapshot, so give one of them"))
			}
			printing := once || asJSON
			if printing {
				if err := refuseUnprinted(cmd, chosen); err != nil {
					return goclikit.UsageError(err)
				}
			}
			var ref *pullrequests.Reference
			if openOn != "" {
				parsed, err := pullrequests.ParseReference(openOn)
				if err != nil {
					return goclikit.UsageError(fmt.Errorf("--pr: %w", err))
				}
				ref = &parsed
			}
			window, err := pullrequests.ParseSince(since, time.Local)
			if err != nil {
				return goclikit.UsageError(fmt.Errorf("--since %w", err))
			}
			if !printing && (!term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd()))) {
				return goclikit.UsageError(errors.New("prs tui needs a terminal; --once prints one frame and --json the snapshot"))
			}
			b, cfg, err := loadBoard()
			if err != nil {
				return err
			}
			if chosen.absent != nil {
				if why := chosen.absent(cfg); why != "" {
					return fmt.Errorf("--view %s opens a tab prs does not draw here, because %s", chosen.name, why)
				}
			}
			b.ReadClosed(window, cfg.ClosedRefreshEvery)
			if ref != nil {
				if err := b.OpenOn(cmd.Context(), *ref); err != nil {
					return err
				}
			}
			pal := gotermstyle.PaletteFor(os.Stdout)
			if !printing {
				b.ReadDetails()
				b.ReadDiffs(cfg.DiffSectionMaxLines)
				// With no home and no XDG_CACHE_HOME the board reads first, as it would with
				// nothing kept.
				if kept, err := config.ListingCacheDir(); err == nil {
					b.KeepListings(kept)
				}
				var tabs []gotermboard.Tab
				var closers []func() error
				defer func() {
					for _, close := range closers {
						_ = close()
					}
				}()
				for _, tab := range drawnTabs(cfg) {
					opened := tab.open(b, cfg)
					if opened.close != nil {
						closers = append(closers, opened.close)
					}
					tabs = append(tabs, gotermboard.Tab{Name: tab.name, Board: opened.board})
				}
				model, err := gotermboard.NewTabs(tabs, pal, view)
				if err != nil {
					return err
				}
				_, err = tea.NewProgram(reportsFocus{model}).Run()
				return err
			}
			opened := chosen.open(b, cfg)
			if opened.close != nil {
				defer func() { _ = opened.close() }()
			}
			gotermboard.RefreshAll(cmd.Context(), opened.board.Sources()...)
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				encoder.SetEscapeHTML(false)
				err = encoder.Encode(opened.snapshot(time.Now()))
			} else {
				width, height := gotermboard.OnceSize(onceFallbackWidth, onceFallbackHeight)
				_, err = fmt.Fprintln(cmd.OutOrStdout(), gotermboard.Once(opened.board, pal, width, height))
			}
			if err != nil {
				return err
			}
			return opened.missed()
		},
	}
	cmd.Flags().StringVar(&view, "view", "", "Open on this tab: "+strings.Join(tuiTabNames(), " or "))
	cmd.Flags().StringVar(&openOn, "pr", "", "Open on this pull request, as owner/name#number or its URL")
	cmd.Flags().StringVar(&since, "since", "2d",
		"Hold in recently closed what closed at or after then: a span back from each read (30m, 12h, 2d, 1w), a day (2026-10-01) or an RFC 3339 instant")
	cmd.Flags().BoolVar(&once, "once", false, "Print one frame of the tab at COLUMNS by LINES, then exit")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output the tab's snapshot as JSON to stdout")
	return cmd
}

// refuseUnprinted refuses a flag the tab --once or --json prints does not read, because it
// would be taken and do nothing.
func refuseUnprinted(cmd *cobra.Command, tab tuiTab) error {
	var refused error
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		if refused != nil || slices.Contains([]string{"view", "once", "json"}, flag.Name) || slices.Contains(tab.prints, flag.Name) {
			return
		}
		refused = fmt.Errorf("--view %s prints the %s tab, which reads no --%s", tab.name, tab.name, flag.Name)
		if tab.elsewhere != "" {
			refused = fmt.Errorf("%w: %s", refused, tab.elsewhere)
		}
	})
	return refused
}

// loadBoard reads the config and the repos it chooses: the registry's, the owners', or
// with neither, the token's own account's.
func loadBoard() (*board.Board, config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cfg, err
	}
	repos := board.Repos{From: cfg.ReposFrom, Registry: cfg.Registry}
	switch cfg.ReposFrom {
	case config.FromOwners:
		repos.Owners = cfg.Owners
	case config.FromViewer:
		repos.Owners = []string{pullrequests.ViewerOwner}
	default:
		if repos.Repos, err = registry.Load(cfg.Registry); err != nil {
			return nil, cfg, err
		}
	}
	b, err := board.New(repos, cfg.RefreshEvery, cfg.WindowActions)
	if err != nil {
		return nil, cfg, fmt.Errorf("%s: %w", cfg.File, err)
	}
	return b, cfg, nil
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
