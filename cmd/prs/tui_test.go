package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/datapointchris/tuidrive"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/datapointchris/prs/internal/board"
)

// standInToken is the token the stand-in GitHub takes, so a request carrying none is refused
// as GitHub refuses it.
const standInToken = "stand-in-token"

// standIn is prs run against a stand-in GitHub answering the listing with
// testdata/search.json, the closed listing with testdata/search-closed.json, and one pull
// request with testdata/pull-request-<number>.json where there is one, else the listing's
// row over testdata/pull-request.json, a stand-in
// bbkt answering testdata/bbkt-list.json, and a stand-in gh, tmux, claude, git, nvim and
// editor. Each records its arguments, and a client records the body it was handed to post,
// so no test reads a real account, changes a real pull request or starts a real Claude.
//
// The stand-in gh holds no login, takes a second to merge, so the work shows under way, and
// refuses to merge #13 in gh's own words. The stand-in git names fetchedHead as any ref's
// commit, and diffs a pull request's fetched head as testdata/pull-request-<number>.diff,
// which the stand-in GitHub sends as its diff too. The stand-in tmux names prs's window prs.
// The stand-in nvim also records the variables naming the pull request's files, and copies
// each to handed.md and handed.threads.json, since prs removes them once nvim exits.
type standIn struct {
	env      map[string]string
	config   string
	checkout string
	ledger   string
	tmp      string
	recorded string
	handed   string
	github   *gitHub
}

// fetchedHead is the commit the stand-in git names for any ref.
const fetchedHead = "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"

// gitHub is the stand-in GitHub's listing, which a test changes while prs runs. It answers
// testdata/search.json until a test hands it another listing, and refuses each listing with
// a 502 while down. One pull request reads as the listing's row over testdata/pull-request.json,
// with the merge state mergeStates names for its number where it names one, and each such read
// is recorded beside the stand-in programs' arguments.
type gitHub struct {
	mu          sync.Mutex
	listing     []byte
	down        bool
	mergeStates map[int]string
	recorded    string
}

// readsAs has one pull request read on its own carry state as its merge state, whatever the
// listing says.
func (g *gitHub) readsAs(number int, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mergeStates[number] = state
}

// serveOne answers a read of one pull request: the listing's row for number laid over
// testdata/pull-request.json, which carries the detail.
func (g *gitHub) serveOne(w http.ResponseWriter, number int) {
	g.mu.Lock()
	listing, state := g.listing, g.mergeStates[number]
	g.mu.Unlock()
	if listing == nil {
		listing, _ = os.ReadFile(filepath.Join("testdata", "search.json"))
	}
	raw, _ := os.ReadFile(filepath.Join("testdata", "pull-request.json"))
	var answer, rows map[string]any
	_ = json.Unmarshal(raw, &answer)
	_ = json.Unmarshal(listing, &rows)
	pr := answer["data"].(map[string]any)["repository"].(map[string]any)["pullRequest"].(map[string]any)
	for _, node := range rows["data"].(map[string]any)["s0"].(map[string]any)["nodes"].([]any) {
		if row := node.(map[string]any); row["number"] == float64(number) {
			maps.Copy(pr, row)
		}
	}
	if state != "" {
		pr["mergeStateStatus"] = state
	}
	file, err := os.OpenFile(g.recorded, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = fmt.Fprintf(file, "github\npullRequest\n%d\n", number)
		_ = file.Close()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func (g *gitHub) answer(listing []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listing, g.down = listing, false
}

func (g *gitHub) fail() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.down = true
}

// serveListing answers a listing request as the test last said. A search for closed pull
// requests reads testdata/search-closed.json, keeping those closed at or after the instant
// its closed:>= qualifier names, as GitHub does, so a query without one finds none.
func (g *gitHub) serveListing(w http.ResponseWriter, r *http.Request, query string) {
	g.mu.Lock()
	listing, down := g.listing, g.down
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case down:
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message": "Server Error"}`))
	case strings.Contains(query, "is:closed"):
		_, _ = w.Write(closedSince(query))
	case listing == nil:
		http.ServeFile(w, r, filepath.Join("testdata", "search.json"))
	default:
		_, _ = w.Write(listing)
	}
}

// closedSince is testdata/search-closed.json holding only the pull requests closed at or
// after the closed:>= instant in query.
func closedSince(query string) []byte {
	var since time.Time
	if at := regexp.MustCompile(`closed:>=(\S+)`).FindStringSubmatch(query); at != nil {
		since, _ = time.Parse(time.RFC3339, at[1])
	}
	raw, _ := os.ReadFile(filepath.Join("testdata", "search-closed.json"))
	var doc struct {
		Data map[string]struct {
			Nodes []map[string]any `json:"nodes"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &doc)
	var nodes []map[string]any
	for _, node := range doc.Data["s0"].Nodes {
		closed, _ := time.Parse(time.RFC3339, fmt.Sprint(node["closedAt"]))
		if !since.IsZero() && !closed.Before(since) {
			nodes = append(nodes, node)
		}
	}
	answer, _ := json.Marshal(map[string]any{"data": map[string]any{"s0": map[string]any{"issueCount": len(nodes), "nodes": nodes}}})
	return answer
}

// gitHubStandIn serves GitHub's GraphQL endpoint on a unix socket and returns a gh config
// directory whose http_unix_socket names it, which go-gh then sends every request through.
// The socket sits under a short temporary directory, because a socket's path has a length
// limit that a test's own temporary directory can pass.
func gitHubStandIn(t *testing.T, listing *gitHub) string {
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
		// REST sends a pull request's diff only under the diff media type.
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/") {
			if r.Header.Get("Accept") != "application/vnd.github.diff" {
				http.Error(w, "asked for no diff", http.StatusUnsupportedMediaType)
				return
			}
			http.ServeFile(w, r, filepath.Join("testdata", "pull-request-"+path.Base(r.URL.Path)+".diff"))
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
		if !strings.Contains(request.Query, "pullRequest(number:") {
			listing.serveListing(w, r, fmt.Sprint(request.Variables["s0"]))
			return
		}
		// A pull request outside the listing has a file of its own.
		if own := fmt.Sprintf("pull-request-%v.json", request.Variables["number"]); fileExists(filepath.Join("testdata", own)) {
			w.Header().Set("Content-Type", "application/json")
			http.ServeFile(w, r, filepath.Join("testdata", own))
			return
		}
		number, _ := request.Variables["number"].(float64)
		listing.serveOne(w, int(number))
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
	s := standIn{
		config: filepath.Join(dir, "config", "prs", "config.yml"), checkout: filepath.Join(dir, "widget"), ledger: filepath.Join(dir, "ledger"),
		tmp: filepath.Join(dir, "tmp"), recorded: filepath.Join(dir, "recorded"), handed: filepath.Join(dir, "handed"),
		github: &gitHub{mergeStates: map[int]string{}, recorded: filepath.Join(dir, "recorded")},
	}
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
'pr view')
  [ -f "` + filepath.Dir(listed) + `/bbkt-view-$3.json" ] && exec cat "` + filepath.Dir(listed) + `/bbkt-view-$3.json"
  echo "error: bitbucket 404 on /projects/FIN/repos/ledger/pull-requests/$3: Pull request $3 does not exist in FIN/ledger." >&2
  exit 1 ;;
esac
` + fmt.Sprintf("cat %q\n", listed),
		"tmux":   "#!/bin/sh\n" + record + "case \"$*\" in *automatic-rename}*) printf 'prs\\t0\\n' ;; esac\n",
		"claude": "#!/bin/sh\n" + record,
		"git": "#!/bin/sh\n" + record + "[ \"$1\" = rev-parse ] && echo " + fetchedHead + "\n" +
			`case " $* " in *' diff '*) for a; do last=$a; done; cat "` + filepath.Dir(listed) + `/pull-request-${last##*/pr/}.diff" 2>/dev/null ;; esac` + "\nexit 0\n",
		"nvim": "#!/bin/sh\n" + record + fmt.Sprintf(`printf '%%s\n' "PRS_DIFF_DOCUMENT=$PRS_DIFF_DOCUMENT" "PRS_DIFF_THREADS=$PRS_DIFF_THREADS" >> %q
cp "$PRS_DIFF_DOCUMENT" %q; cp "$PRS_DIFF_THREADS" %q; exit 0
`, s.recorded, s.handed+".md", s.handed+".threads.json"),
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
	files := map[string]string{filepath.Join(dir, "repos.json"): registry, s.config: config}
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
		"XDG_CACHE_HOME":  filepath.Join(dir, "cache"),
		"GH_CONFIG_DIR":   gitHubStandIn(t, s.github),
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

// The snapshot holds what the board draws: the people's open pull requests, then the bots',
// under pull_requests, and those closed within --since, newest first, under recently_closed.
func TestTheSnapshotReadsBothProvidersSortsPeopleFirstAndFindsAParentByRepoAndBranch(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	out, said, code := s.run(t, "tui", "--json", "--since", "2026-11-01")
	if code != 0 {
		t.Fatalf("prs tui --json exited %d: %s", code, said)
	}
	var snap board.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	var rows map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"pull_requests", "recently_closed"} {
		if err := schemaIn(t, "list.schema.json").Validate(decoded(t, string(rows[key]))); err != nil {
			t.Errorf("prs tui --json's %s break list.schema.json: %v", key, err)
		}
	}
	var got []string
	for _, row := range slices.Concat(snap.PullRequests, []board.Row{{}}, snap.RecentlyClosed) {
		if row.Number == 0 {
			got = append(got, "recently_closed:")
			continue
		}
		stacked := 0
		if row.StackedOn != nil {
			stacked = *row.StackedOn
		}
		bot := "unknown"
		if row.Bot != nil {
			bot = fmt.Sprint(*row.Bot)
		}
		got = append(got, fmt.Sprintf("%s#%d %s %s into %s bot=%s stacked_on=%d", row.Slug, row.Number, row.State, row.Branch, row.Base, bot, stacked))
	}
	// gadget#5 targets a branch only widget has. bbkt's listing types no bot, so its row sits
	// with the people's.
	want := []string{
		"FIN/ledger#4 OPEN feature/ledger-export into develop bot=unknown stacked_on=0",
		"acme/widget#12 OPEN add-cache into main bot=false stacked_on=0",
		"acme/gadget#5 OPEN tidy into add-cache bot=false stacked_on=0",
		"acme/widget#13 OPEN cache-metrics into add-cache bot=true stacked_on=12",
		"recently_closed:",
		"acme/widget#9 MERGED log-misses into main bot=false stacked_on=0",
		"acme/gadget#8 CLOSED bump-linter into main bot=true stacked_on=0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// schemaIn is the schema in the named file under cmd/prs, compiled to check dates as dates.
func schemaIn(t *testing.T, file string) *jsonschema.Schema {
	t.Helper()
	raw, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(file, decodedFrom(t, raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(file)
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
	t.Parallel()
	s := newStandIn(t)
	out, said, code := s.run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("prs list --json exited %d: %s", code, said)
	}
	if err := schemaIn(t, "list.schema.json").Validate(decoded(t, out)); err != nil {
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
	t.Parallel()
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

// A person's open pull request sorts above an app's, and those merged or closed within
// --since sit below both, each state a word that reads with color off. On a wide screen the
// cursor's document sits beside the list and follows the cursor, and enter opens it full
// screen, or on a group's header folds the group.
func TestTheBoardSortsPeopleAboveBotsAndClosedBelowInWordsShowsTheDocumentBesideTheListAndRunsAKeyTheConfigAdds(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	s.env["NO_COLOR"] = "1"
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--since", "2026-11-01"}, Env: s.env, Cols: 378, Rows: 91})
	for _, words := range []string{
		"people · 3", "bots · 1", "recently closed · 2", "checks passing", "checks failing", "checks not run",
		"review required", "review 2 posted", "bot acme-automation",
	} {
		term.WaitFor(words)
	}
	for _, pattern := range []string{
		"behind .*Cache the widget lookups", "draft .*Tidy the gadget", "conflicts .*Count cache hits",
		"merged .*Log the widget misses", "closed .*Bump the gadget's linter",
	} {
		term.WaitForPattern(pattern)
	}
	text := term.Frame().Text()
	order := []string{"#12  Cache the widget lookups", "#13  Count cache hits", "#9  Log the widget misses", "#8  Bump the gadget's linter"}
	for i := 1; i < len(order); i++ {
		if strings.Index(text, order[i-1]) > strings.Index(text, order[i]) {
			t.Errorf("%q should sit above %q:\n%s", order[i-1], order[i], text)
		}
	}

	// The cursor starts on the first row of whichever provider answered first, and g goes to
	// the people's header.
	term.Press("g", "j", "j")
	// The board read the pull request's own detail after its listing landed.
	term.WaitFor("mona approved at 9e8d7c6")
	beside := false
	for line := range strings.SplitSeq(term.Frame().Text(), "\n") {
		beside = beside || strings.Contains(line, "review ") && strings.Contains(line, "acme/widget #12 · opened by octo")
	}
	if !beside {
		t.Errorf("#12's document should share a line of the screen with a row of the table:\n%s", term.Frame().Text())
	}
	term.Press("j", "j", "j")
	term.WaitFor("Counts the hits the cache in #12 takes.")
	term.WaitGone("Caches the widget lookups for a minute.")
	term.Press("G")
	term.WaitFor("It is closed, so its checks, files and comments are not read")
	term.Press("[", "enter")
	term.WaitGone("Log the widget misses")
	term.WaitFor("recently closed · 2")
	term.Press("g", "j", "j", "enter")
	term.WaitFor("[/] section")
	term.Press("esc")

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

// A small pull request's diff reads whole in its document, a file to a heading the Reader's
// ] lands on, sent by GitHub, or diffed in its checkout for Bitbucket, whose client has no
// diff. A large one's section names its size and d. Nothing here starts nvim.
func TestTheDiffSectionShowsASmallDiffWholeAndNamesALargeOnesSize(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui"}, Env: s.env, Cols: 340, Rows: 40})
	term.WaitFor("people · 3")
	term.WaitFor(" #5")

	// The people's rows run ledger #4, widget #12 and gadget #5, oldest first, under their
	// header, and widget #13 sits under the bots'.
	term.Press("g", "j")
	term.WaitFor("5 lines changed in 1 file at 1a2b3c4, read in the checkout here.")
	term.Press("j")
	term.WaitFor("1017 lines changed, more than the 300 this section shows whole. d opens the whole diff in nvim.")
	term.Press("j")
	term.WaitFor("8 lines changed in 1 file at 0f1e2d3, read from github.")
	term.Press("j", "j")
	term.WaitFor("27 lines changed in 3 files at a1b2c3d, read from github.")

	// Full screen, ] lands on each heading: Description, Checks, Files and Diff, then a file each.
	term.Press("enter")
	term.WaitFor("[/] section")
	term.Press("]", "]", "]", "]", "]")
	term.WaitGone("27 lines changed in 3 files")
	term.WaitFor("c.stats.hits.Add(1)")
	term.Press("]")
	term.WaitGone("c.stats.hits.Add(1)")
	term.WaitFor("HitRate is the share of reads the cache answered")
	term.Press("esc", "m")
	term.WaitFor("merge acme/widget #13 — Count cache hits? [Y/n]")
	term.Press("enter")
	term.WaitFor("gh could not merge #13")
	if !s.ran(t, "pr", "merge", "13", "--repo", "acme/widget", "--merge", "--match-head-commit", "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678") {
		t.Error("a merge after reading the Diff section should pin the head that section showed")
	}
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
	if s.ran(t, "nvim") {
		t.Error("reading a diff in its document should start no nvim")
	}
	// Opened on checked-out repos with no key pressed, the board fetches nothing into a GitHub
	// checkout and only the pull request's own ref into a Bitbucket one, so origin/<base> stays put.
	if s.ran(t, "git", "cwd="+s.checkout, "fetch") {
		t.Error("a GitHub pull request's diff should come from GitHub, with nothing fetched into its checkout")
	}
	if !s.ran(t, "git", "cwd="+s.ledger, "fetch", "--quiet", "--force", "--no-write-fetch-head", "--no-auto-maintenance",
		"origin", "feature/ledger-export:refs/remotes/origin/pr/4") || s.ran(t, "origin", "develop") {
		t.Error("a Bitbucket pull request's diff should fetch its head into its own ref and leave origin/develop alone")
	}
}

// A key that acts leaves prs on the list, and a row merged here moves to recently closed at
// once, though the stand-in's search still lists it open, as GitHub's does for a while.
func TestEveryKeyThatActsLeavesPrsOnTheListAndAMergedRowMovesToRecentlyClosedAtOnce(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--since", "2026-11-01"}, Env: s.env, Cols: 140, Rows: 30})
	term.WaitFor("people · 3")
	term.WaitFor("recently closed · 2")

	// The people's rows run ledger #4, widget #12 and gadget #5, oldest first, under their
	// header, and widget #13 sits under the bots'.
	term.Press("g", "j", "c")
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
	term.WaitFor("recently closed · 3")
	term.WaitForPattern("merged .*Cache the widget lookups")
	if recorded, _ := os.ReadFile(s.recorded); strings.Count(string(recorded), "pr\nmerge\n12\n") != 1 {
		t.Error("m pressed while a merge is under way should not send a second merge")
	}
	// The cursor moved on to the next open row in the people's group.
	term.Press("m")
	term.WaitFor("merge acme/gadget #5 — Tidy the gadget? [Y/n]")
	term.Press("esc")

	term.Press("g", "j", "j", "j", "j", "m", "enter")
	term.WaitFor("gh could not merge #13: X Pull request acme/widget#13 is not mergeable")
	term.Press("x")
	term.WaitFor("close acme/widget #13 — Count cache hits? [y/N]")
	term.Press("enter")
	term.WaitFor("did not close acme/widget #13")
	if s.ran(t, "pr", "close") {
		t.Error("enter alone should leave the pull request open")
	}

	term.Press("d")
	term.WaitFor("closed the diff of widget #13")
	if !s.ran(t, "git", "cwd="+s.checkout, "fetch", "--quiet", "--force", "origin", "pull/13/head:refs/remotes/origin/pr/13") {
		t.Error("d should fetch the head into its own ref in the checkout")
	}
	stem := filepath.Join(s.tmp, "prs-diff-widget-13")
	open := "if exists(':DiffviewOpen') == 2 | execute 'DiffviewOpen origin/add-cache...refs/remotes/origin/pr/13' | else | cquit 3 | endif"
	if !s.ran(t, "nvim", "cwd="+s.checkout, "--listen", stem+".sock", "-c", open) {
		t.Error("d should diff three-dot against the base the listing reports, in the checkout")
	}
	// An nvim config shows the pull request beside the diff from the files these name.
	if !s.ran(t, "PRS_DIFF_DOCUMENT="+stem+".md", "PRS_DIFF_THREADS="+stem+".threads.json") {
		t.Error("d should name the pull request's document and threads, beside the socket, in nvim's environment")
	}
	document, _ := os.ReadFile(s.handed + ".md")
	if !strings.HasPrefix(string(document), "# Count cache hits\n\n") || !strings.Contains(string(document), "Counts the hits the cache in #12 takes.") {
		t.Errorf("nvim should be handed the title over the pull request's document:\n%s", document)
	}
	threads, _ := os.ReadFile(s.handed + ".threads.json")
	if err := schemaIn(t, "diff-threads.schema.json").Validate(decoded(t, string(threads))); err != nil {
		t.Errorf("the threads file breaks diff-threads.schema.json: %v\n%s", err, threads)
	}
	if !strings.Contains(string(threads), `"base": "origin/add-cache"`) || !strings.Contains(string(threads), `"line": 14,
      "side": "RIGHT"`) {
		t.Errorf("the threads file should name the diff's base and each thread's line and side:\n%s", threads)
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

// botQueue is testdata/search.json with widget #13 replaced by five pull requests the bot
// opened, oldest first: #24's checks fail, and the rest are green and clean.
func botQueue(t *testing.T) []byte {
	t.Helper()
	titles := map[int]string{21: "Pin the cache's clock", 22: "Drop the unused widget flag", 23: "Bump the linter", 24: "Retry the flaky fetch", 25: "Sort the widget imports"}
	return editedSearch(t, func(pr map[string]any) []any {
		if pr["number"] != 13.0 {
			return []any{pr}
		}
		var queue []any
		for number := 21; number <= 25; number++ {
			raw, _ := json.Marshal(pr)
			var bot map[string]any
			_ = json.Unmarshal(raw, &bot)
			checks, state := "SUCCESS", "CLEAN"
			if number == 24 {
				checks, state = "FAILURE", "UNSTABLE"
			}
			bot["number"], bot["title"], bot["mergeStateStatus"], bot["mergeable"] = number, titles[number], state, "MERGEABLE"
			bot["headRefOid"], bot["createdAt"] = fmt.Sprintf("%040d", number), fmt.Sprintf("2026-11-0%dT09:00:00Z", number-20)
			bot["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": checks}}}}}
			queue = append(queue, bot)
		}
		return queue
	})
}

// space marks only a green, clean row, and says why it refuses another. m on the marks asks
// once, naming each one it merges and the one it leaves out with why, then merges them in the
// order they draw, each after its own read. #22 reads behind by then, so it is left, named,
// and the rest go on.
func TestOneQuestionMergesEveryMarkedGreenPullRequestInTurnEachAfterItsOwnRead(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	s.github.answer(botQueue(t))
	s.github.readsAs(22, "BEHIND")
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--since", "2026-11-01"}, Env: s.env, Cols: 160, Rows: 40})
	term.WaitFor("bots · 5")
	term.WaitFor("people · 3")

	// ] jumps from the people's header to the bots'.
	term.Press("g", "]", "j", "space")
	term.WaitFor("marked acme/widget #21, and m merges the 1 marked")
	term.Press("j", "space", "j", "space", "j", "space")
	term.WaitFor("acme/widget #24 cannot be marked, because its checks are failing, and GitHub reads its merge state as unstable, not clean")
	term.Press("j", "space")
	term.WaitFor("marked acme/widget #25, and m merges the 4 marked")
	if n := strings.Count(term.Frame().Text(), "◆"); n != 4 {
		t.Errorf("4 rows should draw the mark, not %d:\n%s", n, term.Frame().Text())
	}

	term.Press("m")
	term.WaitFor("merge these 4 pull requests? [Y/n]")
	for _, pattern := range []string{
		`merging 4, one at a time`, `◆ acme/widget #21  Pin the cache's clock`, `◆ acme/widget #22  Drop the unused widget flag`,
		`◆ acme/widget #23  Bump the linter`, `◆ acme/widget #25  Sort the widget imports`,
		`leaving out:\s+acme/widget #24  Retry the flaky fetch — its checks are failing`,
	} {
		term.WaitForPattern(pattern)
	}
	before, err := os.ReadFile(s.recorded)
	if err != nil {
		t.Fatal(err)
	}
	term.Press("enter")
	term.WaitFor("merged 3 of 4: acme/widget #21, acme/widget #23, acme/widget #25. Left acme/widget #22: GitHub reads its merge state as behind, not clean")

	recorded, _ := os.ReadFile(s.recorded)
	var events []string
	lines := strings.Split(string(recorded[len(before):]), "\n")
	for i, line := range lines {
		switch {
		case line == "pullRequest":
			events = append(events, "read "+lines[i+1])
		case line == "merge" && lines[i-1] == "pr":
			events = append(events, fmt.Sprintf("merge %s pinned to %s", lines[i+1], lines[i+6]))
		}
	}
	want := []string{
		"read 21", "merge 21 pinned to " + fmt.Sprintf("%040d", 21), "read 22",
		"read 23", "merge 23 pinned to " + fmt.Sprintf("%040d", 23), "read 25", "merge 25 pinned to " + fmt.Sprintf("%040d", 25),
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Errorf("the stand-ins saw:\n%s\nwant each merge after its own read, in the order the rows draw:\n%s", strings.Join(events, "\n"), strings.Join(want, "\n"))
	}
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
}

// Another program jumps to a pull request with --pr and gets its screen back on q. A listed
// pull request opens where the list puts it, and a merged one is read on its own and opens
// above the list. Each opens with its document showing and the cursor on it, which a key's
// question naming it proves. On a wide screen the side panel shows it, and the row stays shut.
func TestTuiOpensOnTheNamedPullRequestWithItsDocumentAndQExits0(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	term := tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--pr", "acme/widget#12"}, Env: s.env, Cols: 378, Rows: 91})
	term.WaitFor("mona approved at 9e8d7c6")
	if n := strings.Count(term.Frame().Text(), "acme/widget #12 · opened by octo"); n != 1 {
		t.Errorf("#12's document should show once, in the side panel, not %d times:\n%s", n, term.Frame().Text())
	}
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}

	term = tuidrive.Start(t, tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui", "--pr", "acme/widget#12"}, Env: s.env, Cols: 140, Rows: 40})
	term.WaitFor("people · 3")
	term.WaitFor("Caches the widget lookups for a minute.")
	// With no side panel, a document running past the screen reads full screen, and ] brings
	// each heading to the top.
	term.Press("enter")
	term.WaitFor("[/] section")
	term.Press("]", "]", "]", "]")
	term.WaitGone("Caches the widget lookups for a minute.")
	term.WaitFor("cache/cache.go:14 · unresolved")
	term.Press("esc", "m")
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
	term.Press("space")
	term.WaitFor("people · 3")
	term.Press("m")
	term.WaitFor("acme/widget #7 is merged already, so there is nothing to merge")
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}
}

// A jump from a merged Bitbucket pull request opens on it by its URL. --json names it as
// opened_on and leaves it out of pull_requests, which holds what the listings read. One bbkt
// cannot read is refused before anything draws.
func TestAMergedBitbucketPullRequestOpensByItsURLAndOneBbktCannotReadIsRefused(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	out, said, code := s.run(t, "tui", "--pr", "https://bitbucket.example.com/projects/FIN/repos/ledger/pull-requests/3/overview", "--json")
	var snap board.Snapshot
	if err := json.Unmarshal([]byte(out), &snap); err != nil || code != 0 {
		t.Fatalf("prs tui --pr <bitbucket URL> --json exited %d, saying %q: %v", code, said, err)
	}
	if on := snap.OpenedOn; on == nil || on.Provider != "bitbucket" || on.Slug != "FIN/ledger" || on.Number != 3 || on.State != "MERGED" {
		t.Errorf("opened_on is %+v, want the merged FIN/ledger #3 on bitbucket", on)
	}
	for _, row := range snap.PullRequests {
		if row.Number == 3 {
			t.Errorf("pull_requests holds #3, which no listing read")
		}
	}

	out, said, code = s.run(t, "tui", "--pr", "FIN/ledger#9", "--json")
	if code != 1 || out != "" || !strings.Contains(said, "bbkt could not read FIN/ledger#9") {
		t.Errorf("prs tui --pr FIN/ledger#9 exited %d and printed %q, saying %q", code, out, said)
	}
}

// refreshEvery sets refresh_every_seconds in the stand-in's config, for the next start.
func (s standIn) refreshEvery(t *testing.T, seconds int) {
	t.Helper()
	raw, err := os.ReadFile(s.config)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.Lines(string(raw)) {
		if !strings.HasPrefix(line, "refresh_every_seconds:") {
			lines = append(lines, line)
		}
	}
	lines = append(lines, fmt.Sprintf("refresh_every_seconds: %d\n", seconds))
	if err := os.WriteFile(s.config, []byte(strings.Join(lines, "")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// widget14Opened is testdata/search.json once widget #13 has left the open pull requests
// and widget #14 has opened.
func widget14Opened(t *testing.T) []byte {
	t.Helper()
	return editedSearch(t, func(pr map[string]any) []any {
		switch pr["number"] {
		case 13.0:
			return nil
		case 12.0:
			opened := maps.Clone(pr)
			opened["number"], opened["title"], opened["createdAt"] = 14, "Expire the widget cache", "2026-11-06T09:00:00Z"
			return []any{opened, pr}
		}
		return []any{pr}
	})
}

// widget12Merged is testdata/search.json once widget #12 has merged, while GitHub's search
// still finds it among the open pull requests, as it does for a while after a merge.
func widget12Merged(t *testing.T) []byte {
	t.Helper()
	return editedSearch(t, func(pr map[string]any) []any {
		if pr["number"] == 12.0 {
			pr["state"], pr["mergedAt"], pr["closedAt"], pr["updatedAt"] = "MERGED", "2026-11-07T09:00:00Z", "2026-11-07T09:00:00Z", "2026-11-07T09:00:00Z"
		}
		return []any{pr}
	})
}

// editedSearch is testdata/search.json with each pull request in it replaced by what edit
// returns for it.
func editedSearch(t *testing.T, edit func(pr map[string]any) []any) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "search.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	search := doc["data"].(map[string]any)["s0"].(map[string]any)
	var nodes []any
	for _, node := range search["nodes"].([]any) {
		nodes = append(nodes, edit(node.(map[string]any))...)
	}
	search["nodes"] = nodes
	raw, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A read lands on the timer and on focus with no key pressed. A pull request a read moves to
// another group, or drops, stays under the cursor in its group until the cursor leaves it,
// named merged or gone. One closed here goes back to its group once a later read lists it
// open. A start whose read fails draws the rows the last run on the same repos kept, and
// names why the read failed.
func TestTheBoardDrawsTheLastListingOfTheSameReposAtOnceThenRereadsOnItsTimerAndOnFocus(t *testing.T) {
	t.Parallel()
	s := newStandIn(t)
	opened := widget14Opened(t)
	s.refreshEvery(t, 1)
	program := tuidrive.Program{Path: tuidrive.Build(t, "."), Args: []string{"tui"}, Env: s.env, Cols: 140, Rows: 30}
	term := tuidrive.Start(t, program)
	term.WaitFor("people · 3")
	term.WaitFor(" #13")
	// The rows run ledger #4, widget #12 and gadget #5 under the people's header, then widget
	// #13 under the bots'.
	term.Press("g", "j", "j")
	// x closes #12, then someone reopens it on its page, so the next read lists it open. That
	// read started after the close, so it stands, and #12 returns to the people's group once
	// the cursor leaves it.
	term.Press("x", "y", "enter")
	term.WaitFor("closed acme/widget #12")
	term.WaitFor("people · 2")
	term.Press("k")
	term.WaitFor("people · 3")
	term.Press("g", "j", "j")
	s.github.answer(widget12Merged(t))
	term.WaitForPattern("merged .*Cache the widget lookups")
	term.WaitFor("people · 3")
	term.Press("j")
	term.WaitFor("people · 2")
	term.Press("j", "j")
	s.github.answer(opened)
	term.WaitFor("Expire the widget cache")
	term.WaitForPattern("gone .*Count cache hits")
	term.Press("m")
	term.WaitFor("acme/widget #13 is in no listing since the last read, so prs will not merge it")
	term.Press("k")
	term.WaitGone("Count cache hits")
	term.Press("q")
	term.WaitExit()

	// GitHub fails from the start, so only the first run's cache holds #14, and only focus
	// reads again within the hour.
	s.refreshEvery(t, 3600)
	s.github.fail()
	term = tuidrive.Start(t, program)
	term.WaitFor("could not read GitHub at github.com")
	term.WaitFor("Expire the widget cache")
	s.github.answer(nil)
	term.Type("\x1b[I")
	term.WaitFor("Count cache hits")
	term.WaitGone("Expire the widget cache")
	term.Press("q")
	if code := term.WaitExit(); code != 0 {
		t.Errorf("q exited %d", code)
	}

	// PRS_REPOS_REGISTRY names other repos, so nothing kept is theirs, and a read that fails
	// is not drawn as a registry with no open pull requests.
	other := filepath.Join(t.TempDir(), "repos.json")
	if err := os.WriteFile(other, []byte(`{"repos": [{"name": "other", "owner": "acme", "status": "active"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s.env["PRS_REPOS_REGISTRY"] = other
	s.github.fail()
	term = tuidrive.Start(t, program)
	term.WaitFor("No provider could be read")
	term.Press("q")
	term.WaitExit()
}
