package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datapointchris/tuidrive"

	"github.com/datapointchris/prs/internal/board"
)

// standIn is prs run against a stand-in gh answering testdata/search.json, a stand-in bbkt
// answering testdata/bbkt-list.json, and a stand-in tmux, claude, git, nvim and editor. Each
// records its arguments, and a client records the body it was handed to post, so no test
// reads a real account, changes a real pull request or starts a real Claude.
//
// The stand-in gh takes a second to merge, so the work shows under way, and refuses to merge
// #13 in gh's own words. The stand-in git names fetchedHead as any ref's commit, and the
// stand-in tmux names prs's window prs.
type standIn struct {
	env      map[string]string
	checkout string
	ledger   string
	tmp      string
	recorded string
}

// fetchedHead is the commit the stand-in git names for any ref.
const fetchedHead = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"

func newStandIn(t *testing.T) standIn {
	t.Helper()
	dir := t.TempDir()
	answer, err := filepath.Abs(filepath.Join("testdata", "search.json"))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := filepath.Abs(filepath.Join("testdata", "bbkt-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := standIn{checkout: filepath.Join(dir, "widget"), ledger: filepath.Join(dir, "ledger"), tmp: filepath.Join(dir, "tmp"), recorded: filepath.Join(dir, "recorded")}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, s.checkout, s.ledger, s.tmp, filepath.Join(dir, "config", "prs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	record := fmt.Sprintf("printf '%%s\\n' \"$(basename \"$0\")\" \"cwd=$(pwd)\" \"$@\" >> %q\n", s.recorded)
	posted := fmt.Sprintf("for a; do [ \"$prev\" = --body-file ] && cat \"$a\" >> %q; prev=$a; done\n", s.recorded)
	scripts := map[string]string{
		"gh": "#!/bin/sh\n" + record + `case "$1 $2" in
'pr merge')
  if [ "$3" = 13 ]; then
    echo 'X Pull request acme/widget#13 is not mergeable: the base branch policy prohibits the merge.' >&2
    echo 'To have the pull request merged after all the requirements have been met, add the ` + "`--auto`" + ` flag.' >&2
    exit 1
  fi
  sleep 1; exit 0 ;;
'pr close') exit 0 ;;
'pr comment') ` + posted + ` exit 0 ;;
esac
` + fmt.Sprintf("cat %q\n", answer),
		"bbkt": "#!/bin/sh\n" + record + `case "$1 $2" in
'pr merge') exit 0 ;;
'pr comment') ` + posted + ` exit 0 ;;
esac
` + fmt.Sprintf("cat %q\n", listed),
		"tmux":   "#!/bin/sh\n" + record + "case \"$*\" in *automatic-rename}*) printf 'prs\\t0\\n' ;; esac\n",
		"claude": "#!/bin/sh\n" + record,
		"git":    "#!/bin/sh\n" + record + "[ \"$1\" = rev-parse ] && echo " + fetchedHead + "\nexit 0\n",
		"nvim":   "#!/bin/sh\n" + record,
		"editor": "#!/bin/sh\n" + record + "printf 'Looks good to me.\\n' > \"$1\"\n",
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry := fmt.Sprintf(`{"repos": [
		{"name": "widget", "owner": "acme", "path": %q, "status": "active"},
		{"name": "gadget", "owner": "acme", "path": %q, "status": "active"},
		{"name": "ledger", "owner": "FIN", "path": %q, "status": "active", "provider": "bitbucket"}
	]}`, s.checkout, filepath.Join(dir, "gadget"), filepath.Join(dir, "ledger"))
	config := fmt.Sprintf(`repos_registry: %s
window_actions:
  - key: r
    label: claude review
    needs: [claude]
    providers: [github]
    window: "review {repo} #{number}"
    command: claude '/review-pr {number}'
`, filepath.Join(dir, "repos.json"))
	files := map[string]string{filepath.Join(dir, "repos.json"): registry, filepath.Join(dir, "config", "prs", "config.yml"): config}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s.env = map[string]string{
		"PATH":            bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
		"TMUX":            filepath.Join(dir, "tmux") + ",1,0",
		"TMUX_PANE":       "%1",
		"EDITOR":          filepath.Join(bin, "editor"),
		"TMPDIR":          s.tmp,
	}
	return s
}

// ran says whether the stand-ins recorded lines in this order, one argument a line.
func (s standIn) ran(t *testing.T, lines ...string) bool {
	t.Helper()
	recorded, err := os.ReadFile(s.recorded)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(recorded), strings.Join(lines, "\n")+"\n")
}

func TestTheSnapshotReadsBothProvidersAndFindsAParentByRepoAndBranch(t *testing.T) {
	s := newStandIn(t)
	cmd := exec.Command(tuidrive.Build(t, "."), "tui", "--json")
	for name, value := range s.env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.Env = append(cmd.Env, "HOME="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("prs tui --json: %v", err)
	}
	var snap board.Snapshot
	if err := json.Unmarshal(out, &snap); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, row := range snap.PullRequests {
		stacked := 0
		if row.StackedOn != nil {
			stacked = *row.StackedOn
		}
		bot := "unknown"
		if row.Bot != nil {
			bot = fmt.Sprint(*row.Bot)
		}
		got = append(got, fmt.Sprintf("%s#%d %s into %s bot=%s stacked_on=%d", row.Slug, row.Number, row.Branch, row.Base, bot, stacked))
	}
	// gadget#5 targets a branch only widget has. bbkt's listing types no bot.
	want := []string{
		"FIN/ledger#4 feature/ledger-export into develop bot=unknown stacked_on=0",
		"acme/widget#12 add-cache into main bot=false stacked_on=0",
		"acme/widget#13 cache-metrics into add-cache bot=true stacked_on=12",
		"acme/gadget#5 tidy into add-cache bot=false stacked_on=0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheBoardSaysEachStateInWordsOpensADescriptionAndRunsAKeyTheConfigAdds(t *testing.T) {
	s := newStandIn(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui"}, Env: s.env, Cols: 140, Rows: 30})
	term.WaitFor("open · 4")
	for _, words := range []string{"checks passing", "checks failing", "checks not run", "review required", "review 2 posted", "bot acme-automation", "Tidy the gadget (draft)", " #12"} {
		term.WaitFor(words)
	}

	// The cursor starts on the first row of whichever provider answered first.
	term.Press("g")
	term.Press("j")
	term.Press("enter")
	term.WaitFor("Caches the widget lookups for a minute.")

	term.Press("r")
	term.WaitFor("opened review widget #12 in a tmux window")
	// Bubble Tea runs tmux info itself to learn the terminal's colors, so the window is one
	// call among those recorded.
	if !s.ran(t, "new-window", "-n", "review widget #12", "-c", s.checkout, "claude '/review-pr 12'") {
		t.Error("r did not open the review's tmux window in the checkout")
	}

	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
}

func TestEveryKeyThatActsLeavesPrsOnTheListAndAMergedRowReadsMergedAtOnce(t *testing.T) {
	s := newStandIn(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui"}, Env: s.env, Cols: 140, Rows: 30})
	term.WaitFor("open · 4")
	term.WaitFor(" #12")

	// The rows run ledger #4, widget #12, widget #13 and gadget #5, oldest first.
	term.Press("g", "c")
	term.WaitFor("commented on FIN/ledger #4")
	if !s.ran(t, "bbkt", "cwd="+s.ledger, "pr", "comment", "create", "4", "--body-file") || !s.ran(t, "Looks good to me.") {
		t.Error("c on a Bitbucket row should post what the editor wrote through bbkt, in its checkout")
	}

	term.Press("j", "m")
	term.WaitFor("merge acme/widget #12 — Cache the widget lookups? [Y/n]")
	term.Press("enter")
	term.WaitFor("merging acme/widget #12")
	term.Press("m")
	term.WaitFor("merging acme/widget #12 is still under way; merge it once that ends")
	term.WaitFor("merged acme/widget #12")
	term.WaitFor("merged  Cache the widget lookups")
	if recorded, _ := os.ReadFile(s.recorded); strings.Count(string(recorded), "pr\nmerge\n12\n") != 1 {
		t.Error("m pressed while a merge is under way should not send a second merge")
	}

	term.Press("j", "m", "enter")
	term.WaitFor("gh could not merge #13: X Pull request acme/widget#13 is not mergeable")
	term.Press("x")
	term.WaitFor("close acme/widget #13 — Count cache hits? [y/N]")
	term.Press("enter")
	term.WaitFor("did not close acme/widget #13")
	if s.ran(t, "pr", "close") {
		t.Error("enter alone should leave the pull request open")
	}

	term.Press("k", "m")
	term.WaitFor("acme/widget #12 is merged already")

	term.Press("j", "d")
	term.WaitFor("closed the diff of widget #13")
	if !s.ran(t, "git", "cwd="+s.checkout, "fetch", "--quiet", "--force", "origin", "pull/13/head:refs/remotes/origin/pr/13") {
		t.Error("d should fetch the head into its own ref in the checkout")
	}
	socket := filepath.Join(s.tmp, "prs-diff-widget-13.sock")
	open := "if exists(':DiffviewOpen') == 2 | execute 'DiffviewOpen origin/add-cache...refs/remotes/origin/pr/13' | else | cquit 3 | endif"
	if !s.ran(t, "nvim", "cwd="+s.checkout, "--listen", socket, "-c", open) {
		t.Error("d should diff three-dot against the base the listing reports, in the checkout")
	}
	// A program split into the window reads the pull request off its name.
	if !s.ran(t, "rename-window", "-t", "%1", "diff widget #13") || !s.ran(t, "rename-window", "-t", "%1", "prs") {
		t.Error("d should name prs's tmux window for the diff while nvim has it, then give its name back")
	}
	term.Press("m")
	term.WaitFor("merge acme/widget #13 — Count cache hits? [Y/n]")
	term.Press("enter")
	term.WaitFor("gh could not merge #13")
	if !s.ran(t, "pr", "merge", "13", "--repo", "acme/widget", "--merge", "--match-head-commit", fetchedHead) {
		t.Error("a merge after d should pin the head d fetched")
	}
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
}
