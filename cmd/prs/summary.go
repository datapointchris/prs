package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"

	"github.com/datapointchris/prs/internal/board"
	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
)

func summaryCommand() *cobra.Command {
	var (
		since, until string
		asJSON       bool
	)
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Count the pull requests opened, merged and closed over a window",
		Long: `summary counts the pull requests on the repos prs reads, which prs config show names,
over the window from --since up to --until: those open at its end, those opened, merged and
closed unmerged within it, the median and 90th percentile time from opening to merging, and
the share of merges whose first review was a Claude review round's. Each count is split
into those a person opened and those an app opened. Then it counts each repo's merges, the
most first. It prints the numbers the Summary tab of prs tui shows.

The merged and closed pull requests are GitHub's, kept in a SQLite file for each set of
repos under $XDG_CACHE_HOME/prs/closed-pull-requests. The first run reads every pull request
closed within the window, a few search pages for a month. Each run after reads those
updated since the last, which is a page or so, and a window reaching further back than the
file reads back to its start. Bitbucket's pull requests count among the open only, because
bbkt says no time a pull request closed.

--json prints the numbers as one JSON object, with the counts per hour or per day, the open
pull requests across the window and the times to merge in buckets that the tab charts.
cmd/prs/summary.schema.json in prs's repository is that shape.

A read that failed, or that left open pull requests out, is named on stderr after the
numbers, and either form then exits 1, because a failed read and a quiet window otherwise
print the same counts.`,
		Example: `  prs summary
  prs summary --since 7d
  prs summary --since 2026-10-01 --until 2026-10-31
  prs summary --since 30d --json | jq '.merged'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			window, err := pullrequests.ParseSince(since, time.Local)
			if err != nil {
				return goclikit.UsageError(fmt.Errorf("--since %w", err))
			}
			var end time.Time
			if until != "" {
				if end, err = pullrequests.ParseUntil(until, time.Now(), time.Local); err != nil {
					return goclikit.UsageError(fmt.Errorf("--until %w", err))
				}
				if !end.After(window.From(time.Now())) {
					return goclikit.UsageError(fmt.Errorf("--until %s ends the window at or before --since %s starts it", until, since))
				}
			}
			b, cfg, err := loadBoard()
			if err != nil {
				return err
			}
			s := openSummary(b, cfg, window)
			defer func() { _ = s.Close() }()
			if !end.IsZero() {
				s.CountUntil(end)
			}
			gotermboard.RefreshAll(cmd.Context(), s.Sources()...)
			out := cmd.OutOrStdout()
			if asJSON {
				err = printSummaryJSON(out, s)
			} else {
				pal := gotermstyle.PaletteFor(out)
				for _, line := range s.Table(time.Now()) {
					if _, err = fmt.Fprintln(out, line.Paint(pal)); err != nil {
						break
					}
				}
			}
			if err != nil {
				return err
			}
			return summaryFaults(s)
		},
	}
	cmd.Flags().StringVar(&since, "since", board.DefaultSummaryWindow,
		"Count from then: a span back from now (30m, 12h, 7d, 2w), a day (2026-10-01) or an RFC 3339 instant")
	cmd.Flags().StringVar(&until, "until", "",
		"Count up to then, in --since's forms, a day taking in the whole of it; now where unset")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the numbers as one JSON object, in the shape cmd/prs/summary.schema.json gives")
	return cmd
}

// openSummary is the Summary tab over the board's repos, keeping the merged and closed pull
// requests under prs's cache, or with no home and no XDG_CACHE_HOME, in memory.
func openSummary(b *board.Board, cfg config.Config, window pullrequests.Since) *board.Summary {
	dir, _ := config.ClosedPullRequestsDir()
	return b.OpenSummary(dir, cfg.ClosedRefreshEvery, window)
}

func printSummaryJSON(out io.Writer, s *board.Summary) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(s.Snapshot(time.Now()))
}

// summaryFaults fails where a read the counts rest on failed or left pull requests out.
func summaryFaults(s *board.Summary) error {
	if faults := s.Faults(); len(faults) > 0 {
		return fmt.Errorf("the counts miss what could not be read: %s", strings.Join(faults, "; "))
	}
	return nil
}
