package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datapointchris/tuidrive"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/datapointchris/prs/internal/board"
)

// standInToken is the token the stand-in GitHub takes, so a request carrying none is refused
// as GitHub refuses it.
const standInToken = "stand-in-token"

// standIn is prs run against a stand-in GitHub answering the listing with
// testdata/search.json and one pull request with testdata/pull-request-<number>.json where
// there is one, else testdata/pull-request.json, a stand-in
// bbkt answering testdata/bbkt-list.json, and a stand-in gh, tmux, claude, git, nvim and
// editor. Each records its arguments, and a client records the body it was handed to post,
// so no test reads a real account, changes a real pull request or starts a real Claude.
//
// The stand-in gh holds no login, takes a second to merge, so the work shows under way, and
// refuses to merge #13 in gh's own words. The stand-in git names fetchedHead as any ref's
// commit, and the stand-in tmux names prs's window prs.
type standIn struct {
	env      map[string]string
	checkout string
	ledger   string
	tmp      string
	recorded string
}

// fetchedHead is the commit the stand-in git names for any ref.
const fetchedHead = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"

// gitHubStandIn serves GitHub's GraphQL endpoint on a unix socket and returns a gh config
// directory whose http_unix_socket names it, which go-gh then sends every request through.
// The socket sits under a short temporary directory, because a socket's path has a length
// limit that a test's own temporary directory can pass.
func gitHubStandIn(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "prs-gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "gh.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+standInToken {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
			return
		}
		var request struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		answer := "search.json"
		if strings.Contains(request.Query, "pullRequest(number:") {
			answer = "pull-request.json"
			// A pull request outside the listing has a file of its own.
			if own := fmt.Sprintf("pull-request-%v.json", request.Variables["number"]); fileExists(filepath.Join("testdata", own)) {
				answer = own
			}
		}
		w.Header().Set("Content-Type", "application/json")
		http.ServeFile(w, r, filepath.Join("testdata", answer))
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	config := filepath.Join(dir, "gh")
	if err := os.MkdirAll(config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.yml"), []byte("http_unix_socket: "+socket+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return config
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func newStandIn(t *testing.T) standIn {
	t.Helper()
	dir := t.TempDir()
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
echo 'no oauth token found for github.com' >&2
exit 1
`,
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
		"GH_CONFIG_DIR":   gitHubStandIn(t),
		"GH_TOKEN":        standInToken,
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

// run runs the built prs with args in the stand-in's environment, and returns its stdout,
// its stderr and its exit status.
func (s standIn) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(tuidrive.Build(t, "."), args...)
	cmd.Env = []string{"HOME=" + t.TempDir()}
	for name, value := range s.env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), cmd.ProcessState.ExitCode()
}

func TestTheSnapshotReadsBothProvidersAndFindsAParentByRepoAndBranch(t *testing.T) {
	s := newStandIn(t)
	out, said, code := s.run(t, "tui", "--json")
	if code != 0 {
		t.Fatalf("prs tui --json exited %d: %s", code, said)
	}
	var snap board.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	var rows struct {
		PullRequests json.RawMessage `json:"pull_requests"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	if err := listSchema(t).Validate(decoded(t, string(rows.PullRequests))); err != nil {
		t.Errorf("prs tui --json's pull_requests break list.schema.json: %v", err)
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

// listSchema is cmd/prs/list.schema.json, compiled to check dates as dates.
func listSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.Open("list.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource("list.schema.json", decodedFrom(t, raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("list.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func decoded(t *testing.T, document string) any {
	t.Helper()
	return decodedFrom(t, strings.NewReader(document))
}

func decodedFrom(t *testing.T, r io.Reader) any {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// The schema is what a program reading --json is pointed at, so the rows must fit it. A
// provider that cannot be read must say so on the table's first line without taking the
// other provider's rows with it.
func TestListPrintsRowsTheSchemaHoldsAndNamesAProviderItCouldNotRead(t *testing.T) {
	s := newStandIn(t)
	out, said, code := s.run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("prs list --json exited %d: %s", code, said)
	}
	if err := listSchema(t).Validate(decoded(t, out)); err != nil {
		t.Errorf("prs list --json breaks list.schema.json: %v\n%s", err, out)
	}
	if !strings.Contains(out, `"merge_state_status": "BEHIND"`) || !strings.Contains(out, `"merge_state_status": null`) {
		t.Errorf("want GitHub's merge state on its rows and null on bbkt's:\n%s", out)
	}

	s.env["GH_TOKEN"] = ""
	out, _, code = s.run(t, "list")
	first, rest, _ := strings.Cut(out, "\n")
	if code != 1 || !strings.Contains(first, "github unread: gh is not logged in to github.com") ||
		!strings.Contains(rest, "ledger") || strings.Contains(rest, "widget") {
		t.Errorf("logged out, prs list exited %d and printed:\n%s", code, out)
	}
}

// A registry whose only repo is a Bitbucket one not checked out here reads nothing and
// leaves that repo's pull requests out. A script reads an empty array, which jq iterates,
// and an exit status saying the array is not the whole.
func TestListPrintsAnEmptyArrayAndExits1WhenARepoIsLeftOut(t *testing.T) {
	s := newStandIn(t)
	registry := filepath.Join(t.TempDir(), "repos.json")
	if err := os.WriteFile(registry, []byte(`{"repos": [{"name": "ledger", "owner": "FIN", "status": "active", "provider": "bitbucket"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.env["PRS_REPOS_REGISTRY"] = registry
	out, said, code := s.run(t, "list", "--json")
	if out != "[]\n" || code != 1 || !strings.Contains(said, "bitbucket: not checked out here, so not read: FIN/ledger") {
		t.Errorf("prs list --json exited %d and printed %q, saying %q", code, out, said)
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
	// The board read the pull request's own detail after its listing landed.
	term.WaitFor("approved   mona  at 9e8d7c6")

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

// Another program jumps to a pull request with --pr and gets its screen back on q. A listed
// pull request opens where the list puts it, and a merged one is read on its own and opens
// above the list. Each opens with its description showing and the cursor on it, which a
// key's question naming it proves.
func TestTuiOpensOnTheNamedPullRequestWithItsDescriptionAndQExits0(t *testing.T) {
	s := newStandIn(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--pr", "acme/widget#12"}, Env: s.env, Cols: 140, Rows: 30})
	term.WaitFor("open · 4")
	term.WaitFor("Caches the widget lookups for a minute.")
	term.WaitFor("approved   mona  at 9e8d7c6")
	term.Press("m")
	term.WaitFor("merge acme/widget #12 — Cache the widget lookups? [Y/n]")
	term.Press("n", "enter")
	term.WaitFor("did not merge acme/widget #12")
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}

	term = tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--pr", "https://github.com/acme/widget/pull/7"}, Env: s.env, Cols: 140, Rows: 30})
	term.WaitFor("merged · 1")
	term.WaitFor("Pins the schema the widget reads, so a newer one fails loudly.")
	term.WaitFor("open · 4")
	term.Press("m")
	term.WaitFor("acme/widget #7 is merged already, so there is nothing to merge")
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
}

// Bitbucket's client reads no single pull request, so --pr finds one in its repo's open
// pull requests and refuses one they do not hold, before anything draws.
func TestABitbucketPullRequestOpensOnlyFromItsReposOpenPullRequests(t *testing.T) {
	s := newStandIn(t)
	out, said, code := s.run(t, "tui", "--pr", "https://bitbucket.example.com/projects/FIN/repos/ledger/pull-requests/4/overview", "--json")
	var snap board.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil || code != 0 {
		t.Fatalf("prs tui --pr <bitbucket URL> --json exited %d, saying %q: %v", code, said, err)
	}
	if on := snap.OpenedOn; on == nil || on.Provider != "bitbucket" || on.Slug != "FIN/ledger" || on.Number != 4 {
		t.Errorf("opened_on is %+v, want FIN/ledger #4 on bitbucket", on)
	}

	out, said, code = s.run(t, "tui", "--pr", "FIN/ledger#9", "--json")
	if code != 1 || out != "" || !strings.Contains(said, "FIN/ledger#9 is not among FIN/ledger's open pull requests on bitbucket") {
		t.Errorf("prs tui --pr FIN/ledger#9 exited %d and printed %q, saying %q", code, out, said)
	}
}
