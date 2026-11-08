package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"

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
		Long: `Print the config file's path, $XDG_CONFIG_HOME/prs/config.yml, whether or not it
exists. The file is YAML:

  registry: ~/repos.json
  window_actions:
    - key: r
      label: claude review
      detail: a Claude window reviewing it
      needs: [claude]
      providers: [github]
      window: "review {repo} #{number}"
      command: claude '/review-pr {number}'

registry names the repo registry, a JSON file of repos, each with a name, an owner, a
path, a status and optionally a provider. Each window action is a key that runs its
command in a new tmux window in the pull request's checkout. needs are the programs it
runs, and a machine missing one is not offered the key. {number}, {repo} and {slug} are
filled in. Quote a value holding " #", which YAML otherwise reads as a comment.`,
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
			source := "the config file"
			if !cfg.Exists {
				source = "nothing: the file is not there"
			}
			registry := cfg.Registry
			if registry == "" {
				registry = pal.Paint(gotermstyle.CodeRed, "unset")
			}
			_, _ = fmt.Fprintf(out, "%s  %s\n", pal.Bold("config  "), cfg.File)
			_, _ = fmt.Fprintf(out, "%s  %s  from %s\n", pal.Bold("registry"), registry, source)
			for _, action := range cfg.WindowActions {
				state := pal.Paint(gotermstyle.CodeGreen, "offered")
				var missing []string
				for _, program := range action.Needs {
					if _, err := exec.LookPath(program); err != nil {
						missing = append(missing, program)
					}
				}
				if len(missing) > 0 {
					state = pal.Paint(gotermstyle.CodeYellow, "not offered, missing "+strings.Join(missing, ", "))
				}
				_, _ = fmt.Fprintf(out, "%s  %s  %s\n", pal.Bold(fmt.Sprintf("key %-4s", action.Key)), action.Label, state)
			}
			return nil
		},
	})
	return cmd
}
