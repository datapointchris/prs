package board

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/registry"
)

// commandTimeout bounds a client a key runs while the board waits, such as gh opening the
// browser.
const commandTimeout = 30 * time.Second

// Actions is gotermboard.Keyed's. The board's keys sit ahead of the table's, so the footer
// and the ? overlay list them first.
func (b *Board) Actions() []gotermboard.Action { return b.actions }

// keyTable is every key prs adds, in the order ? lists them: reading first, then the window
// actions the config adds, then what changes a pull request, so merge is listed last.
//
// The letters are the ones common terminal tools use for the same thing: o opens outside,
// y copies, m merges and d reads, never deletes. x closes, because c is the comment.
func (b *Board) keyTable(windows []config.WindowAction) []gotermboard.Action {
	actions := []gotermboard.Action{
		{
			Keys: []string{"enter"}, Label: "open the pull request's description, full screen where it runs past the screen",
			Footer: "enter open", Run: b.open,
		},
		{Keys: []string{"d"}, Label: "view diff: fetch its ref and open it in nvim in a tmux window", Footer: "d diff", Run: b.onCursor(b.diff)},
		{Keys: []string{"p"}, Label: "page and comments: GitHub's page and every comment, in a pager in a tmux window", Run: b.onCursor(b.page)},
	}
	for _, window := range windows {
		if missing := missingPrograms(window.Needs); len(missing) > 0 {
			continue
		}
		label := window.Label
		if window.Detail != "" {
			label += ": " + window.Detail
		}
		actions = append(actions, gotermboard.Action{Keys: []string{window.Key}, Label: label, Run: b.onCursor(b.windowAction(window))})
	}
	return append(actions,
		gotermboard.Action{Keys: []string{"o"}, Label: "open in browser", Footer: "o browser", Run: b.onCursor(b.browse)},
		gotermboard.Action{Keys: []string{"y"}, Label: "copy branch: its name, to the system clipboard", Footer: "y copy", Run: b.onCursor(b.copyBranch)},
		notBuilt("c", "comment"),
		notBuilt("x", "close"),
		notBuilt("m", "merge"),
	)
}

// notBuilt is a key whose action prs does not run yet. It keeps its letter and its place in
// ? so the key means one thing from the start.
func notBuilt(key, name string) gotermboard.Action {
	return gotermboard.Action{
		Keys: []string{key}, Label: name + " (not built yet)",
		Run: func(m *gotermboard.Model) tea.Cmd {
			m.Say(name + " is not built into prs yet; o opens the pull request's page, which can " + name + " it")
			return nil
		},
	}
}

// open is enter: a pull request's description full screen where it runs past the screen,
// named by its title, and otherwise what enter does on the table.
func (b *Board) open(m *gotermboard.Model) tea.Cmd {
	pr, ok := b.shown[m.Cursor()]
	if !ok {
		return m.Toggle()
	}
	if document := b.description(pr, false); m.Tall(document) {
		m.Read(pr.Title, document)
		return nil
	}
	return m.Toggle()
}

// onCursor runs act on the pull request under the cursor, and says what it did or why it
// could not.
func (b *Board) onCursor(act func(pullrequests.PullRequest) (string, error)) func(*gotermboard.Model) tea.Cmd {
	return func(m *gotermboard.Model) tea.Cmd {
		pr, ok := b.shown[m.Cursor()]
		if !ok {
			m.Say("put the cursor on a pull request first")
			return nil
		}
		done, err := act(pr)
		if err != nil {
			m.Say(err.Error())
			return nil
		}
		m.Say(done)
		return nil
	}
}

// diff fetches the pull request's head into its own ref and opens nvim's diff against the
// base it targets, in a tmux window in the checkout. It never checks the branch out, which
// would abort on uncommitted work and disturb the tree where there is none.
//
// The base is the one the listing reports, because a pull request stacked on another
// targets its parent, and a diff against main would show both as one change. The diff is
// three-dot, from where the branch left the base, which is what the provider shows.
//
// nvim listens on a socket named for the window, so another program in the window can ask
// it what is under the cursor.
func (b *Board) diff(pr pullrequests.PullRequest) (string, error) {
	checkout, err := checkoutOf(pr)
	if err != nil {
		return "", err
	}
	base := pr.Base
	if base == "" {
		base = defaultBranch(checkout)
	}
	if base == "" {
		return "", fmt.Errorf("could not work out what #%d is based on", pr.Number)
	}
	ref := "refs/remotes/origin/pr/" + strconv.Itoa(pr.Number)
	// Bitbucket serves no pull request refs, so its head is the source branch on origin.
	head := fmt.Sprintf("pull/%d/head", pr.Number)
	if pr.Provider == registry.Bitbucket {
		head = pr.Branch
	}
	if head == "" {
		return "", fmt.Errorf("#%d in %s names no source branch to fetch", pr.Number, pr.Repo)
	}
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("prs-diff-%s-%d.sock", pr.Repo, pr.Number))
	_ = os.Remove(socket)
	script := strings.Join([]string{
		`fail() { printf 'prs: %s\npress enter to close ' "$1"; read -r answer; exit 1; }`,
		fmt.Sprintf("git fetch --quiet --force origin %s || fail %s",
			quote(head+":"+ref), quote(fmt.Sprintf("could not fetch #%d in %s", pr.Number, pr.Repo))),
		"git fetch --quiet origin " + quote(base),
		fmt.Sprintf("git rev-parse --verify --quiet %s >/dev/null || fail %s",
			quote("origin/"+base), quote(fmt.Sprintf("%s has no origin/%s to diff against", pr.Repo, base))),
		fmt.Sprintf("exec nvim --listen %s -c %s", quote(socket), quote(fmt.Sprintf("DiffviewOpen origin/%s...%s", base, ref))),
	}, "; ")
	name := fmt.Sprintf("diff %s #%d", pr.Repo, pr.Number)
	if err := openWindow(name, script, checkout, "git", "nvim"); err != nil {
		return "", err
	}
	return "opened " + name + " in a tmux window", nil
}

// page is GitHub's own rendering of the pull request and its comments, which the listing
// does not carry. GH_FORCE_TTY makes gh render the page into a pager rather than print
// key: value lines.
func (b *Board) page(pr pullrequests.PullRequest) (string, error) {
	if pr.Provider != registry.GitHub {
		return "", fmt.Errorf("the page is GitHub's, and #%d is on %s", pr.Number, pr.Provider)
	}
	name := fmt.Sprintf("page %s #%d", pr.Repo, pr.Number)
	command := fmt.Sprintf("GH_FORCE_TTY=$(tput cols) gh pr view %d --repo %s --comments | less -R", pr.Number, quote(pr.Slug))
	if err := openWindow(name, command, "", "gh", "less"); err != nil {
		return "", err
	}
	return "opened " + name + " in a tmux window", nil
}

// windowAction runs a key the config declares in a tmux window in the checkout.
func (b *Board) windowAction(window config.WindowAction) func(pullrequests.PullRequest) (string, error) {
	return func(pr pullrequests.PullRequest) (string, error) {
		if len(window.Providers) > 0 && !slices.Contains(window.Providers, pr.Provider) {
			return "", fmt.Errorf("%s runs on %s pull requests, and #%d is on %s",
				window.Label, strings.Join(window.Providers, " and "), pr.Number, pr.Provider)
		}
		checkout, err := checkoutOf(pr)
		if err != nil {
			return "", err
		}
		name := config.Fill(window.Window, pr.Number, pr.Repo, pr.Slug)
		if err := openWindow(name, config.Fill(window.Command, pr.Number, pr.Repo, pr.Slug), checkout, window.Needs...); err != nil {
			return "", err
		}
		return "opened " + name + " in a tmux window", nil
	}
}

// browse hands the pull request's page to the browser. bbkt finds the repo from the
// directory it runs in, so a Bitbucket pull request needs its checkout.
func (b *Board) browse(pr pullrequests.PullRequest) (string, error) {
	var cmd *exec.Cmd
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	switch pr.Provider {
	case registry.GitHub:
		cmd = exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(pr.Number), "--repo", pr.Slug, "--web")
	default:
		checkout, err := checkoutOf(pr)
		if err != nil {
			return "", err
		}
		cmd = exec.CommandContext(ctx, "bbkt", "pr", "open", strconv.Itoa(pr.Number))
		cmd.Dir = checkout
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s could not open #%d: %s", cmd.Args[0], pr.Number, lastLine(out, err))
	}
	return fmt.Sprintf("opened #%d in the browser", pr.Number), nil
}

// copyBranch copies the branch rather than the number, because a number is a per-repo
// counter and both providers take a branch wherever they take one.
func (b *Board) copyBranch(pr pullrequests.PullRequest) (string, error) {
	var argv []string
	for _, tool := range [][]string{{"wl-copy"}, {"pbcopy"}, {"xclip", "-selection", "clipboard"}} {
		if _, err := exec.LookPath(tool[0]); err == nil {
			argv = tool
			break
		}
	}
	if argv == nil {
		return "", errors.New("no clipboard tool found: install wl-clipboard or xclip")
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(pr.Branch)
	// On PATH is not the same as able to run: wl-copy exits 1 with no compositor to talk
	// to, as over ssh.
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%s could not take the branch: %s", argv[0], lastLine(out, err))
	}
	return "copied " + pr.Branch, nil
}

// openWindow runs command in a new tmux window, once the programs it needs are on PATH.
// tmux reports whether it opened the window and never what the command did, so a missing
// program would open a window that dies at once.
func openWindow(name, command, dir string, needs ...string) error {
	if os.Getenv("TMUX") == "" {
		return errors.New("this opens a tmux window, and prs is not running inside tmux")
	}
	if missing := missingPrograms(append([]string{"tmux"}, needs...)); len(missing) > 0 {
		return fmt.Errorf("not installed here: %s, and the window would open and die without it", strings.Join(missing, ", "))
	}
	args := []string{"new-window", "-n", name}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "tmux", append(args, command)...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux could not open %s: %s", name, lastLine(out, err))
	}
	return nil
}

func missingPrograms(programs []string) []string {
	var missing []string
	for _, program := range programs {
		if _, err := exec.LookPath(program); err != nil {
			missing = append(missing, program)
		}
	}
	return missing
}

// checkoutOf is the pull request's checkout on this disk, which the diff, a window action
// and bbkt each run in.
func checkoutOf(pr pullrequests.PullRequest) (string, error) {
	if pr.Path == "" {
		return "", fmt.Errorf("%s is not checked out on this machine", pr.Repo)
	}
	if info, err := os.Stat(pr.Path); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is registered at %s, which does not exist", pr.Repo, pr.Path)
	}
	return pr.Path, nil
}

// defaultBranch is the branch origin's HEAD names, for a provider whose listing reports no
// base.
func defaultBranch(checkout string) string {
	out, err := exec.Command("git", "-C", checkout, "symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/")
}

// quote makes text one shell word.
func quote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// lastLine is the last thing a failed command said, or its exit status where it said nothing.
func lastLine(out []byte, err error) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}
