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

// standIn is prs run against a stand-in gh answering testdata/search.json, and a stand-in
// tmux and claude that each record their arguments, so no test reads a real account or
// starts a real Claude.
type standIn struct {
	env      map[string]string
	checkout string
	recorded string
}

func newStandIn(t *testing.T) standIn {
	t.Helper()
	dir := t.TempDir()
	answer, err := filepath.Abs(filepath.Join("testdata", "search.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := standIn{checkout: filepath.Join(dir, "widget"), recorded: filepath.Join(dir, "recorded")}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, s.checkout, filepath.Join(dir, "config", "prs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scripts := map[string]string{
		"gh":     fmt.Sprintf("#!/bin/sh\ncat %q\n", answer),
		"tmux":   fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' tmux \"$@\" >> %q\n", s.recorded),
		"claude": fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' claude \"$@\" >> %q\n", s.recorded),
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry := fmt.Sprintf(`{"repos": [
		{"name": "widget", "owner": "acme", "path": %q, "status": "active"},
		{"name": "gadget", "owner": "acme", "path": %q, "status": "active"},
		{"name": "typos", "owner": "acme", "path": %q, "status": "active"}
	]}`, s.checkout, filepath.Join(dir, "gadget"), filepath.Join(dir, "typos"))
	config := fmt.Sprintf(`registry: %s
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
	}
	return s
}

func TestTheSnapshotKeepsEachRegistryPullRequestOnceAndFindsItsParentByRepoAndBranch(t *testing.T) {
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
		got = append(got, fmt.Sprintf("%s#%d bot=%v stacked_on=%d", row.Slug, row.Number, *row.Bot, stacked))
	}
	// crate-ci/typos shares a registry repo's bare name and is not it; #12 is in both searches;
	// gadget#5 targets a branch only widget has.
	want := []string{
		"acme/widget#12 bot=false stacked_on=0",
		"acme/widget#13 bot=true stacked_on=12",
		"acme/gadget#5 bot=false stacked_on=0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTheBoardSaysEachStateInWordsOpensADescriptionAndRunsAKeyTheConfigAdds(t *testing.T) {
	s := newStandIn(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui"}, Env: s.env, Cols: 140, Rows: 30})
	term.WaitFor("open · 3")
	for _, words := range []string{"checks passing", "checks failing", "checks not run", "review required", "review 2 posted", "bot acme-automation", "Tidy the gadget (draft)", " #12"} {
		term.WaitFor(words)
	}

	term.Press("?")
	for _, label := range []string{"view diff", "page and comments", "claude review", "open in browser", "copy branch", "merge (not built yet)"} {
		term.WaitFor(label)
	}
	term.Press("esc")
	term.WaitGone("Keys, any key closes this")

	term.Press("enter")
	term.WaitFor("Caches the widget lookups for a minute.")

	term.Press("r")
	term.WaitFor("opened review widget #12 in a tmux window")
	recorded, err := os.ReadFile(s.recorded)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"tmux", "new-window", "-n", "review widget #12", "-c", s.checkout, "claude '/review-pr 12'"}, "\n")
	// Bubble Tea runs tmux info itself to learn the terminal's colors, so the window is one
	// call among those recorded.
	if !strings.Contains(string(recorded), want) {
		t.Errorf("the stand-ins recorded:\n%s\nwant:\n%s", recorded, want)
	}

	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
}
