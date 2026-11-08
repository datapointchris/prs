// prs shows every open pull request across a registry's repos on one terminal board.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/datapointchris/goclikit"
	"github.com/datapointchris/goselfupdate"
	"github.com/datapointchris/goselfupdate/autoupdate"
	"github.com/spf13/cobra"
)

// version is set through -ldflags by goreleaser.
var version = "dev"

// buildVersion falls back to the module version, because go install applies no
// ldflags but does stamp a tagged build.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && goselfupdate.IsReleaseVersion(info.Main.Version) {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return version
}

// updateConfig is shared by the update command and the update check, so the two
// cannot name different releases.
func updateConfig() goselfupdate.Config {
	return goselfupdate.Config{
		Owner:   "datapointchris",
		Repo:    "prs",
		Binary:  "prs",
		Version: buildVersion(),
	}
}

func main() {
	err := goclikit.Execute(context.Background(), rootCommand(), autoupdate.Config{Update: updateConfig()})
	os.Exit(exitCode(err, os.Stderr))
}

func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if !errors.Is(err, goclikit.ErrReported) {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
	}
	if errors.Is(err, goclikit.ErrUsage) {
		return 2
	}
	return 1
}

func rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "prs",
		Short: "Every open pull request across a registry's repos, on one terminal board",
		Long: `prs reads every open pull request on the repos a registry names, whoever opened
it, and acts on one with a key. prs tui opens the board, and prs list prints it once.
prs config show names the registry, or the owners read in its place.`,
		Example: `  prs tui
  prs list
  prs list --json | jq -r '.[].url'
  prs config show`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildVersion(),
	}
	root.SetVersionTemplate("prs {{.Version}}\n")
	// Registered here because cobra's own version flag takes -v.
	root.Flags().Bool("version", false, "print the version")
	root.AddCommand(tuiCommand(), listCommand(), configCommand(), goclikit.UpdateCommand(updateConfig()))
	return root
}
