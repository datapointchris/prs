package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"

	"github.com/datapointchris/prs/internal/board"
	"github.com/datapointchris/prs/internal/config"
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
  window_actions:
    - key: r
      label: claude review
      detail: a Claude window reviewing it
      needs: [claude]
      providers: [github]
      window: "review {repo} #{number}"
      command: claude '/review-pr {number}'

repos_registry names the repo registry, a JSON file of repos, each with a name, an owner,
a path, a status and optionally a provider. PRS_REPOS_REGISTRY names it ahead of the
file, and with neither prs reads $XDG_DATA_HOME/prs/repos.json. In place of a registry,
owners lists GitHub users and organizations whose every repo is read:

  owners: [acme, octo]

With no registry anywhere and no owners, prs reads the repos your GitHub login owns.

refresh_every_seconds is how often prs tui reads each provider again while it is open, a
whole number of seconds, 1 or more, and %[1]d where the file does not say. Each GitHub read
spends points from the token's hourly GraphQL allowance, so a shorter interval spends them
faster. gh api rate_limit shows what is left.

Each window action is a key that runs its command in a new tmux window in the pull
request's checkout. needs are the programs it runs, and a machine missing one is not
offered the key. {number}, {repo} and {slug} are filled in. Quote a value holding " #",
which YAML otherwise reads as a comment.`, int(config.DefaultRefreshEvery.Seconds())),
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
			_, _ = fmt.Fprintf(out, "%s  every %d seconds  from %s\n", pal.Bold("refresh "), int(cfg.RefreshEvery.Seconds()), cfg.RefreshFrom)
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
				_, _ = fmt.Fprintf(out, "%s  %s  %s\n", pal.Bold(fmt.Sprintf("key %-4s", action.Key)), action.Label, state)
			}
			return nil
		},
	})
	return cmd
}
