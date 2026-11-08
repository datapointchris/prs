package board

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
)

// commandTimeout bounds a program a key runs while the board waits, such as tmux opening a
// window.
const commandTimeout = 30 * time.Second

// Actions is gotermboard.Keyed's. The board's keys sit ahead of the table's, so the footer
// and the ? overlay list them first.
func (b *Board) Actions() []gotermboard.Action { return b.actions }

// Refused says why a window action cannot have its key, and is nil where the key is free.
// A board's key sits ahead of the table's, so a window action on a bound key would take it.
// esc backs out of whatever is open, which no table action names.
func Refused(window config.WindowAction) error {
	for _, action := range new(Board).keyTable(nil) {
		if slices.Contains(action.Keys, window.Key) {
			return fmt.Errorf("key %q is prs's own, for %s", window.Key, action.Label)
		}
	}
	if slices.Contains(append(gotermboard.TableKeys(), "esc"), window.Key) {
		return fmt.Errorf("key %q is one the board's table binds", window.Key)
	}
	return nil
}

// keyTable is every key prs adds, in the order ? lists them: reading first, then the window
// actions the config adds, then what changes a pull request, so merge is listed last.
//
// The letters are the ones common terminal tools use for the same thing: o opens outside,
// y copies, m merges and d reads, never deletes. x closes, because c is the comment.
func (b *Board) keyTable(windows []config.WindowAction) []gotermboard.Action {
	actions := []gotermboard.Action{
		{Keys: []string{"d"}, Label: "view diff: fetch its head and open it in nvim here, until :qa", Footer: "d diff", Run: b.onRow(b.diff)},
	}
	for _, window := range windows {
		if len(window.Missing()) > 0 {
			continue
		}
		label := window.Label
		if window.Detail != "" {
			label += ": " + window.Detail
		}
		actions = append(actions, gotermboard.Action{Keys: []string{window.Key}, Label: label, Run: b.onCursor(b.windowAction(window))})
	}
	return append(actions,
		gotermboard.Action{Keys: []string{"o"}, Label: "open in browser", Footer: "o browser", Run: b.onRow(b.browse)},
		gotermboard.Action{Keys: []string{"y"}, Label: "copy branch: its name, to the system clipboard", Footer: "y copy", Run: b.onCursor(b.copyBranch)},
		gotermboard.Action{Keys: []string{"c"}, Label: "comment: write it in your editor, and it posts when the editor exits", Run: b.onRow(b.comment)},
		gotermboard.Action{Keys: []string{"x"}, Label: "close without merging, after asking, and enter alone keeps it open", Run: b.onRow(b.closePR)},
		gotermboard.Action{Keys: []string{"m"}, Label: "merge, after asking, and enter alone merges", Run: b.onRow(b.merge)},
	)
}

// onRow runs act on the pull request under the cursor.
func (b *Board) onRow(act func(*gotermboard.Model, pullrequests.PullRequest) tea.Cmd) func(*gotermboard.Model) tea.Cmd {
	return func(m *gotermboard.Model) tea.Cmd {
		pr, ok := b.shown[m.Cursor()]
		if !ok {
			m.Say("put the cursor on a pull request first")
			return nil
		}
		return act(m, pr)
	}
}

// onCursor runs act on the pull request under the cursor, and says what it did or why it
// could not.
func (b *Board) onCursor(act func(pullrequests.PullRequest) (string, error)) func(*gotermboard.Model) tea.Cmd {
	return b.onRow(func(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
		done, err := act(pr)
		if err != nil {
			m.Say(err.Error())
			return nil
		}
		m.Say(done)
		return nil
	})
}

// diff fetches the pull request's head into its own ref in the background, then hands prs's
// terminal to nvim's diff against the base it targets, run in the checkout. Quitting nvim
// gives the terminal back to the board, with the cursor where it was, and Receive reads the
// providers again. It never checks the branch out, which would abort on uncommitted work and
// disturb the tree where there is none. The head it fetched is the one m then pins.
//
// The base is the one the listing reports, because a pull request stacked on another
// targets its parent, and a diff against main would show both as one change. The diff is
// three-dot, from where the branch left the base, which is what the provider shows.
//
// nvim listens on a socket named for the pull request, so another program can ask it what
// is under the cursor. Inside tmux the window is named diff <repo> #<n> while nvim has it,
// so a program split into the window finds that socket from the name. Beside the socket it
// writes the pull request's document and its review threads, and starts nvim with
// PRS_DIFF_DOCUMENT and PRS_DIFF_THREADS naming them, so an nvim config can show them
// beside the diff.
func (b *Board) diff(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	refuse := func(err error) tea.Cmd {
		m.Say(err.Error())
		return nil
	}
	provider, err := pullrequests.ProviderOf(pr.Provider)
	if err != nil {
		return refuse(err)
	}
	checkout, err := checkoutOf(pr)
	if err != nil {
		return refuse(err)
	}
	if missing := config.MissingPrograms([]string{"git", "nvim"}); len(missing) > 0 {
		return refuse(fmt.Errorf("the diff needs %s, which is not installed here", strings.Join(missing, " and ")))
	}
	base := pr.Base
	if base == "" {
		base = defaultBranch(checkout)
	}
	if base == "" {
		return refuse(fmt.Errorf("could not work out what #%d is based on", pr.Number))
	}
	head := provider.Head(pr)
	if head == "" {
		return refuse(fmt.Errorf("#%d in %s names no source branch to fetch", pr.Number, pr.Repo))
	}
	// run writes this and done reads it, after the message that ends the work. A base that
	// failed to fetch still diffs against the origin/<base> fetched earlier, and the footer
	// says so once nvim exits.
	var got pullrequests.Fetched
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("fetching %s #%d", pr.Repo, pr.Number),
		run: func(ctx context.Context) error {
			var err error
			got, err = pullrequests.Fetch(ctx, pr, checkout, head, base)
			return err
		},
		done: func(*gotermboard.Model) tea.Cmd {
			b.diffed[pr.Key()] = got.Head
			stem := filepath.Join(os.TempDir(), fmt.Sprintf("prs-diff-%s-%d", pr.Repo, pr.Number))
			socket := stem + ".sock"
			_ = os.Remove(socket)
			nvim := exec.Command("nvim", "--listen", socket, "-c", openDiffview("origin/"+base+"..."+got.Ref))
			nvim.Dir = checkout
			files := diffFilesAt(stem)
			unwritten := files.write(pr, b.detailOf(pr), "origin/"+base, got.Head, time.Now())
			if unwritten == nil {
				nvim.Env = append(os.Environ(), files.env()...)
			}
			restore := nameWindow(fmt.Sprintf("diff %s #%d", pr.Repo, pr.Number))
			return handOver(nvim, func(m *gotermboard.Model, err error) tea.Cmd {
				restore()
				files.remove()
				var exit *exec.ExitError
				var said string
				switch {
				case errors.As(err, &exit) && exit.ExitCode() == noDiffview:
					said = "nvim has no :DiffviewOpen, and the diff needs the diffview.nvim plugin"
				case err != nil:
					said = fmt.Sprintf("nvim ended the diff of #%d with %v", pr.Number, err)
				case got.BaseFault != "":
					said = fmt.Sprintf("closed the diff of %s #%d, which ran against the origin/%s fetched earlier, because fetching it again failed: %s",
						pr.Repo, pr.Number, base, got.BaseFault)
				default:
					said = fmt.Sprintf("closed the diff of %s #%d", pr.Repo, pr.Number)
				}
				if unwritten != nil {
					said += fmt.Sprintf(". nvim had no pull request to show beside it, because writing it failed: %v", unwritten)
				}
				m.Say(said)
				return nil
			})
		},
	})
}

// noDiffview is the status nvim exits with where Diffview is not installed.
const noDiffview = 3

// openDiffview is the command nvim runs to open Diffview on revisions, or to exit at once
// with noDiffview where the plugin is not installed, so the board can say so rather than
// leave an empty buffer. A plugin that loads on its command still defines the command.
func openDiffview(revisions string) string {
	quoted := "'DiffviewOpen " + strings.ReplaceAll(revisions, "'", "''") + "'"
	return fmt.Sprintf("if exists(':DiffviewOpen') == 2 | execute %s | else | cquit %d | endif", quoted, noDiffview)
}

// merge asks first, and enter alone merges. A merge confirmed by reflex lands work that was
// landing anyway, and the question is there to show which pull request lands. Anything typed
// but y or yes leaves it, so a stray key still stops it.
func (b *Board) merge(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	provider, err := b.writable(pr, "merge")
	if err == nil && provider.Merge == nil {
		err = lacking(pr, "merge")
	}
	if err != nil {
		m.Say(err.Error())
		return nil
	}
	argv, inCheckout := provider.Merge(pr, b.diffed[pr.Key()])
	b.ask(m, pr, "merge", true, work{
		pr:    pr,
		doing: fmt.Sprintf("merging %s #%d", pr.Slug, pr.Number),
		run:   runClient(pr, "merge", argv, inCheckout),
		done: func(m *gotermboard.Model) tea.Cmd {
			b.closeHere(m, pr, pullrequests.Merged)
			m.Say(fmt.Sprintf("merged %s #%d", pr.Slug, pr.Number))
			return nil
		},
	})
	return nil
}

// closePR asks first, and enter alone keeps it open. A close confirmed by reflex discards a
// branch nobody read, and the pull request reopens but the moment someone would have read it
// does not.
func (b *Board) closePR(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	provider, err := b.writable(pr, "close")
	if err == nil && provider.Close == nil {
		err = lacking(pr, "close")
	}
	if err != nil {
		m.Say(err.Error())
		return nil
	}
	argv, inCheckout := provider.Close(pr)
	b.ask(m, pr, "close", false, work{
		pr:    pr,
		doing: fmt.Sprintf("closing %s #%d", pr.Slug, pr.Number),
		run:   runClient(pr, "close", argv, inCheckout),
		done: func(m *gotermboard.Model) tea.Cmd {
			b.closeHere(m, pr, pullrequests.Closed)
			m.Say(fmt.Sprintf("closed %s #%d without merging", pr.Slug, pr.Number))
			return nil
		},
	})
	return nil
}

// writable is the provider of a pull request a key may change. It refuses one a merge or a
// close already settled, and one whose merge, close or fetch is still under way, so a second
// press never sends a second change beside the first.
func (b *Board) writable(pr pullrequests.PullRequest, verb string) (pullrequests.Provider, error) {
	switch state := b.stateOf(pr); state {
	case merged, closed:
		return pullrequests.Provider{}, fmt.Errorf("%s #%d is %s already, so there is nothing to %s", pr.Slug, pr.Number, state, verb)
	case gone:
		return pullrequests.Provider{}, fmt.Errorf("%s #%d is in no listing since the last read, so prs will not %s it; o opens its page",
			pr.Slug, pr.Number, verb)
	}
	if doing := b.busy(pr); doing != "" {
		return pullrequests.Provider{}, fmt.Errorf("%s is still under way; %s it once that ends", doing, verb)
	}
	return pullrequests.ProviderOf(pr.Provider)
}

// lacking is the refusal for a verb a host's client cannot do, which its Provider leaves nil.
func lacking(pr pullrequests.PullRequest, verb string) error {
	return fmt.Errorf("prs cannot %s a pull request on %s, because its client has no verb for it", verb, pr.Provider)
}

// ask puts the question for verb on the footer, naming the pull request it acts on, and
// starts w on yes. Enter alone answers yes where yesByDefault says so.
func (b *Board) ask(m *gotermboard.Model, pr pullrequests.PullRequest, verb string, yesByDefault bool, w work) {
	choices, yes := "[y/N]", []string{"y", "yes"}
	if yesByDefault {
		choices, yes = "[Y/n]", []string{"", "y", "yes"}
	}
	left := func() tea.Cmd {
		m.Say(fmt.Sprintf("did not %s %s #%d", verb, pr.Slug, pr.Number))
		return nil
	}
	m.Ask(gotermboard.Question{
		Text:  fmt.Sprintf("%s %s #%d — %s? %s", verb, pr.Slug, pr.Number, pr.Title, choices),
		Typed: true,
		Answer: func(typed string) tea.Cmd {
			if !slices.Contains(yes, strings.ToLower(typed)) {
				return left()
			}
			return b.start(w)
		},
		Cancel: left,
	})
}

// comment hands the terminal to the editor on an empty file, and posts what was written when
// it exits. An empty file posts nothing. A post that fails keeps the file and names it, so
// the comment is not lost.
func (b *Board) comment(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	provider, err := pullrequests.ProviderOf(pr.Provider)
	if err == nil && provider.Comment == nil {
		err = lacking(pr, "comment on")
	}
	if err != nil {
		m.Say(err.Error())
		return nil
	}
	file, err := os.CreateTemp("", fmt.Sprintf("prs-comment-%s-%d-*.md", pr.Repo, pr.Number))
	if err != nil {
		m.Say("could not make a file to write the comment in: " + err.Error())
		return nil
	}
	path := file.Name()
	_ = file.Close()
	return handOver(gotermboard.EditorCommand(path), func(m *gotermboard.Model, err error) tea.Cmd {
		body, readErr := os.ReadFile(path)
		switch {
		case err != nil:
			m.Say(fmt.Sprintf("the editor ended with %v, so nothing was posted; what it wrote is in %s", err, path))
			return nil
		case readErr != nil || strings.TrimSpace(string(body)) == "":
			_ = os.Remove(path)
			m.Say(fmt.Sprintf("the comment was empty, so nothing was posted to #%d", pr.Number))
			return nil
		}
		argv, inCheckout := provider.Comment(pr, path)
		post := runClient(pr, "comment on", argv, inCheckout)
		return b.start(work{
			pr:    pr,
			doing: fmt.Sprintf("commenting on %s #%d", pr.Slug, pr.Number),
			run: func(ctx context.Context) error {
				if err := post(ctx); err != nil {
					return fmt.Errorf("%w; the comment is kept in %s", err, path)
				}
				_ = os.Remove(path)
				return nil
			},
			done: func(m *gotermboard.Model) tea.Cmd {
				m.Say(fmt.Sprintf("commented on %s #%d", pr.Slug, pr.Number))
				return nil
			},
		})
	})
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
		fields := config.Fields{Number: pr.Number, Repo: pr.Repo, Slug: pr.Slug}
		name := config.Fill(window.Window, fields)
		if err := openWindow(name, config.Fill(window.Command, fields), checkout, window.Needs...); err != nil {
			return "", err
		}
		return "opened " + name + " in a tmux window", nil
	}
}

// browse hands the pull request's page to the browser through the provider's client, in the
// background, because the client asks the provider for the page's address first.
func (b *Board) browse(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	provider, err := pullrequests.ProviderOf(pr.Provider)
	if err == nil && provider.Browse == nil {
		err = lacking(pr, "open")
	}
	if err != nil {
		m.Say(err.Error())
		return nil
	}
	argv, inCheckout := provider.Browse(pr)
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("opening #%d in the browser", pr.Number),
		run:   runClient(pr, "open", argv, inCheckout),
		done: func(m *gotermboard.Model) tea.Cmd {
			m.Say(fmt.Sprintf("opened #%d in the browser", pr.Number))
			return nil
		},
	})
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
	if missing := config.MissingPrograms(append([]string{"tmux"}, needs...)); len(missing) > 0 {
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

// lastLine is the last thing a failed command said, or its exit status where it said nothing.
func lastLine(out []byte, err error) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}
