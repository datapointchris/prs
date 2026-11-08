package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermstyle"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func listCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Print every open pull request once, as a table or as JSON",
		Long: `list reads each provider once and prints every open pull request on the repos the
registry names, oldest first: a line saying how each read went, then a table of the
board's columns.

--json prints the rows as a JSON array instead, one object per pull request.
cmd/prs/list.schema.json in prs's repository is that shape. additions, deletions and
changed_files are null, never 0, from a provider whose listing does not count them, and
so are the merge fields and head_oid where it does not carry them.

A provider that could not be read is named on the first line, or on stderr with --json,
and the other provider's rows still print. Either form then exits 1, because a failed
read and a repo with no open pull requests otherwise print the same rows.`,
		Example: `  prs list
  prs list --json | jq -r '.[].url'
  prs list --json | jq '.[] | select(.merge_state_status == "BEHIND") | .slug'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := loadBoard()
			if err != nil {
				return err
			}
			gotermboard.RefreshAll(cmd.Context(), b.Sources()...)
			now := time.Now()
			out := cmd.OutOrStdout()
			faults := b.Faults()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(b.PullRequests(now)); err != nil {
					return err
				}
				if len(faults) > 0 {
					return fmt.Errorf("could not read %s", strings.Join(faults, "; "))
				}
				return nil
			}
			pal := gotermstyle.PaletteFor(out)
			width := 0
			if term.IsTerminal(int(os.Stdout.Fd())) {
				width, _, _ = term.GetSize(int(os.Stdout.Fd()))
			}
			for _, line := range b.Table(now) {
				painted := line.Paint(pal)
				if width > 0 {
					painted = ansi.Truncate(painted, width, "…")
				}
				if _, err := fmt.Fprintln(out, painted); err != nil {
					return err
				}
			}
			if len(faults) > 0 {
				return fmt.Errorf("%w: the first line names what could not be read", goclikit.ErrReported)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the rows as a JSON array, in the shape cmd/prs/list.schema.json gives")
	return cmd
}
