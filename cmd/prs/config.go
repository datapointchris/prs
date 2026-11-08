package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"

	"github.com/datapointchris/prs/internal/board"
	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
)

func configCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show where prs's config lives and what it says",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print the config file's path",
		Long: fmt.Sprintf(`Print the config file's path, $XDG_CONFIG_HOME/prs/config.yml, whether or not it
exists. The file is YAML:

  repos_registry: ~/repos.json
  refresh_every_seconds: %[1]d
  closed_refresh_every_seconds: %[2]d
  diff_section_max_lines: %[4]d
  window_actions:
    - key: r
      label: claude review
      detail: a Claude window reviewing it
      needs: [claude]
      providers: [github]
      window: "review {repo} #{number}"
      command: claude '/review-pr {number}'
      runs_review_round: true
  recaps:
    needs: [recap]
    list: [recap, list]
    show: [recap, show, "--{kind}", "{label}"]
    write: [recap, write, "--{kind}", "{label}"]

repos_registry names the repo registry, a JSON file of repos, each with a name, an owner,
a path, a status and optionally a provider. PRS_REPOS_REGISTRY names it ahead of the
file, and with neither prs reads $XDG_DATA_HOME/prs/repos.json. In place of a registry,
owners lists GitHub users and organizations whose every repo is read:

  owners: [acme, octo]

With no registry anywhere and no owners, prs reads the repos your GitHub login owns.

refresh_every_seconds is how often prs tui reads each provider's open pull requests again
while it is open, a whole number of seconds, 1 or more, and %[1]d where the file does not
say. closed_refresh_every_seconds is the same for the recently closed and for what changed in
the Summary tab's history, and %[2]d where the file does not say. Each GitHub read spends points from the token's hourly GraphQL
allowance, about a point for every %[3]d repos whichever it reads, so a shorter interval spends
them faster. gh api rate_limit shows what is left.

diff_section_max_lines is the most lines a pull request may add and remove for the Diff
section of its document to show the diff whole, a whole number, 1 or more, and %[4]d where
the file does not say. A larger one's section names how many lines it changes and the key
that opens it.

Each window action is a key that runs its command in a new tmux window in the pull
request's checkout. needs are the programs it runs, and a machine missing one is not
offered the key. {number}, {repo} and {slug} are filled in. Quote a value holding " #",
which YAML otherwise reads as a comment. runs_review_round marks a command that runs a
Claude review round, so the row reads claude running while its window is open, until a
round finishes after the key opened it.

recaps names the three commands prs tui's recaps tab runs, each an argv run with no shell,
so one that pipes runs through sh -c. list prints the windows a recap is written for,
newest first, one a line as its kind and its label, such as day 2026-11-06, and prs reads
nothing else it prints. show prints one window's recap as markdown. write writes one, a
model call, and may be left out. %[5]s in show and write are filled in from the window.
needs are the programs the commands run beyond each one's first, and a machine missing one
draws no recaps tab, as a config naming no list does.`, int(config.DefaultRefreshEvery.Seconds()), int(config.DefaultClosedRefreshEvery.Seconds()),
			pullrequests.ReposPerSearch, config.DefaultDiffSectionMaxLines, strings.Join(config.RecapPlaceholders(), " and ")),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), path)
			return err
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print each setting, the value it resolved to, and where that came from",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			pal := gotermstyle.PaletteFor(cmd.OutOrStdout())
			out := cmd.OutOrStdout()
			file := cfg.File
			if !cfg.Exists {
				file += pal.Paint(gotermstyle.CodeYellow, "  not there")
			}
			_, _ = fmt.Fprintf(out, "%s  %s\n", pal.Bold("config  "), file)
			switch cfg.ReposFrom {
			case config.FromOwners:
				_, _ = fmt.Fprintf(out, "%s  %s  from %s\n", pal.Bold("owners  "), strings.Join(cfg.Owners, ", "), cfg.ReposFrom)
			case config.FromViewer:
				where, err := config.DefaultRegistry()
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(out, "%s  your own on GitHub, because no registry is at %s and the config names no owners\n",
					pal.Bold("repos   "), where)
			default:
				registry := cfg.Registry
				if _, err := os.Stat(registry); err != nil {
					registry += pal.Paint(gotermstyle.CodeRed, "  not there")
				}
				_, _ = fmt.Fprintf(out, "%s  %s  from %s\n", pal.Bold("registry"), registry, cfg.ReposFrom)
			}
			_, _ = fmt.Fprintf(out, "%s  open every %d seconds  from %s\n", pal.Bold("refresh "), int(cfg.RefreshEvery.Seconds()), cfg.RefreshFrom)
			_, _ = fmt.Fprintf(out, "%s  closed and the summary's history every %d seconds  from %s\n", pal.Bold("        "), int(cfg.ClosedRefreshEvery.Seconds()), cfg.ClosedRefreshFrom)
			_, _ = fmt.Fprintf(out, "%s  shown whole up to %d changed lines  from %s\n", pal.Bold("diff    "), cfg.DiffSectionMaxLines, cfg.DiffSectionFrom)
			kept, err := config.ListingCacheDir()
			if err != nil {
				return err
			}
			if _, err := os.Stat(kept); err != nil {
				kept += pal.Paint(gotermstyle.CodeYellow, "  not there until prs tui reads")
			}
			_, _ = fmt.Fprintf(out, "%s  %s\n", pal.Bold("cache   "), kept)
			for _, action := range cfg.WindowActions {
				state := pal.Paint(gotermstyle.CodeGreen, "offered")
				if err := board.Refused(action); err != nil {
					state = pal.Paint(gotermstyle.CodeRed, "refused, "+err.Error())
				} else if missing := action.Missing(); len(missing) > 0 {
					state = pal.Paint(gotermstyle.CodeYellow, "not offered, missing "+strings.Join(missing, ", "))
				}
				if action.RunsReviewRound {
					state += ", runs review rounds"
				}
				_, _ = fmt.Fprintf(out, "%s  %s  %s\n", pal.Bold(fmt.Sprintf("key %-4s", action.Key)), action.Label, state)
			}
			if recaps := cfg.Recaps; recaps.Named() {
				state := pal.Paint(gotermstyle.CodeGreen, "drawn")
				if why := recapsAbsent(cfg); why != "" {
					state = pal.Paint(gotermstyle.CodeYellow, "not drawn, because "+why)
				}
				_, _ = fmt.Fprintf(out, "%s  %s\n", pal.Bold("recaps  "), state)
				for _, command := range []struct {
					name string
					argv []string
				}{{"list", recaps.List}, {"show", recaps.Show}, {"write", recaps.Write}} {
					if len(command.argv) > 0 {
						_, _ = fmt.Fprintf(out, "%s  %s\n", pal.Bold(fmt.Sprintf("  %-6s", command.name)), shellQuoted(command.argv))
					}
				}
			}
			return nil
		},
	})
	return cmd
}

// shellQuoted is argv as a shell reads it back, quoting each argument it would split or expand.
func shellQuoted(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		if arg != "" && strings.Trim(arg, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@%+,{}") == "" {
			quoted[i] = arg
			continue
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
