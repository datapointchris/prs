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
		gotermboard.Action{
			Keys: []string{"space"}, Label: "mark for m to merge with the others marked, where its checks pass and GitHub reads it clean; on a group's header, fold it",
			Footer: "space mark", Run: b.space,
		},
		gotermboard.Action{Keys: []string{"m"}, Label: "merge, after asking, and enter alone merges; with rows marked, every one of them, one at a time", Run: b.mergeKey},
	)
}

// space folds a group's header and closes a row's text open in place, as the table's space
// does, and otherwise marks the row for m, or takes its mark off. A row whose merge could not
// go through without a person is refused, with why, and the batch's question names it again.
func (b *Board) space(m *gotermboard.Model) tea.Cmd {
	pr, ok := b.shown[m.Cursor()]
	switch {
	case !ok:
		return m.Toggle()
	case m.CloseInPlace():
		return nil
	case b.marked[pr.Key()]:
		delete(b.marked, pr.Key())
		m.Say(fmt.Sprintf("unmarked %s #%d%s", pr.Slug, pr.Number, b.markedCount()))
	default:
		if why := b.whyNotMarkable(pr); why != "" {
			b.refused[pr.Key()] = true
			m.Say(fmt.Sprintf("%s #%d cannot be marked, because %s", pr.Slug, pr.Number, why))
			return nil
		}
		delete(b.refused, pr.Key())
		b.marked[pr.Key()] = true
		m.Say(fmt.Sprintf("marked %s #%d%s", pr.Slug, pr.Number, b.markedCount()))
	}
	m.Rebuild()
	return nil
}

// markedCount is how many are marked, for what space says after it.
func (b *Board) markedCount() string {
	if len(b.marked) == 0 {
		return ", and none is marked"
	}
	return fmt.Sprintf(", and m merges the %d marked", len(b.marked))
}

// whyNotMarkable is why space will not mark the pull request, and empty where it can: open,
// with nothing under way on it, on a host prs can merge on, with its checks passing and its
// merge state CLEAN.
func (b *Board) whyNotMarkable(pr pullrequests.PullRequest) string {
	if why := b.whyNotMergeable(pr); why != "" {
		return why
	}
	return mergeBlocker(pr)
}

// whyNotMergeable is why the board will not merge the pull request whatever its record says,
// and empty where it can.
func (b *Board) whyNotMergeable(pr pullrequests.PullRequest) string {
	switch state := b.stateOf(pr); state {
	case merged, closed:
		return "it is " + string(state) + " already"
	case gone:
		return "no read lists it any more"
	}
	if doing := b.busy(pr); doing != "" {
		return doing + " is still under way"
	}
	if provider, err := pullrequests.ProviderOf(pr.Provider); err != nil || provider.Merge == nil {
		return "prs cannot merge a pull request on " + pr.Provider
	}
	return ""
}

// mergeBlocker is why the pull request's record says it would not merge on its own, and empty
// where its checks pass and its merge state is CLEAN. A host that reports no merge state is
// one such, because nothing says the merge would go through.
func mergeBlocker(pr pullrequests.PullRequest) string {
	var why []string
	if checks := checkMeaning(pr.Checks); !checks.passed {
		why = append(why, "its checks are "+checks.word)
	}
	switch {
	case pr.MergeStateStatus == nil:
		why = append(why, pr.Provider+" reports no merge state for it")
	case *pr.MergeStateStatus != "CLEAN":
		why = append(why, "GitHub reads its merge state as "+words(*pr.MergeStateStatus)+", not clean")
	}
	return strings.Join(why, ", and ")
}

// mergeKey merges every marked pull request, where any is marked, and otherwise the one under
// the cursor.
func (b *Board) mergeKey(m *gotermboard.Model) tea.Cmd {
	if len(b.marked) > 0 {
		return b.mergeMarked(m)
	}
	return b.onRow(b.merge)(m)
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

// diff fetches the pull request's head and base in the background, then hands prs's terminal
// to nvim's diff of its LocalDiff, run in the checkout. Quitting nvim gives the terminal back
// to the board, with the cursor where it was, and Receive reads the providers again. The head
// it fetched is the one m then pins.
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
	if missing := config.MissingPrograms([]string{"git", "nvim"}); len(missing) > 0 {
		return refuse(fmt.Errorf("the diff needs %s, which is not installed here", strings.Join(missing, " and ")))
	}
	target, err := pullrequests.LocalDiffOf(context.Background(), pr)
	if err != nil {
		return refuse(err)
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
			got, err = pullrequests.Fetch(ctx, target)
			return err
		},
		done: func(*gotermboard.Model) tea.Cmd {
			b.diffed[pr.Key()] = got.Head
			stem := filepath.Join(os.TempDir(), fmt.Sprintf("prs-diff-%s-%d", pr.Repo, pr.Number))
			socket := stem + ".sock"
			_ = os.Remove(socket)
			nvim := exec.Command("nvim", "--listen", socket, "-c", openDiffview(target.Range()))
			nvim.Dir = target.Checkout
			files := diffFilesAt(stem)
			unwritten := files.write(pr, b.detailOf(pr), "origin/"+target.Base, got.Head, time.Now())
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
						pr.Repo, pr.Number, target.Base, got.BaseFault)
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

// shownHead is the commit the reader last saw the pull request at: the head d fetched, or else
// the head its Diff section was read at. A merge pinned to it fails once someone pushes past it.
func (b *Board) shownHead(pr pullrequests.PullRequest) string {
	if head := b.diffed[pr.Key()]; head != "" {
		return head
	}
	if b.diffs == nil {
		return ""
	}
	if state := b.diffs.Of(pr); state.Held {
		return state.Diff.Head
	}
	return ""
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
	argv, inCheckout := provider.Merge(pr, b.shownHead(pr))
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

// mergeMarked asks once, listing every marked pull request it will merge and each marked or
// refused one it leaves out with why, then merges them one at a time in the order they draw.
// Enter alone merges, as it does for one. The marks go once it is asked, because the question
// named every one of them.
func (b *Board) mergeMarked(m *gotermboard.Model) tea.Cmd {
	if b.batching {
		m.Say("a batch is still merging; m merges the marked ones once it ends")
		return nil
	}
	var batch []pullrequests.PullRequest
	var merging, leftOut []gotermboard.Line
	for _, key := range b.drawnOrder {
		pr := b.shown[key]
		if !b.marked[key] && !b.refused[key] {
			continue
		}
		switch why := b.whyNotMarkable(pr); {
		case why != "":
			leftOut = append(leftOut, prLine("    ", pr, why))
		case b.marked[key]:
			batch = append(batch, pr)
			merging = append(merging, prLine("  "+gotermboard.MarkGlyph+" ", pr, ""))
		}
	}
	if len(batch) == 0 {
		m.Say("no marked pull request can merge now, so m merged nothing; space takes a mark off")
		return nil
	}
	listing := append([]gotermboard.Line{{gotermboard.C(fmt.Sprintf("merging %d, one at a time, each read again just before it:", len(batch)), gotermboard.Bold)}}, merging...)
	if len(leftOut) > 0 {
		listing = append(listing, gotermboard.Line{gotermboard.C("leaving out:", gotermboard.Bold)})
		listing = append(listing, leftOut...)
	}
	left := func() tea.Cmd {
		m.Say(fmt.Sprintf("merged none of the %d marked; they stay marked", len(batch)))
		return nil
	}
	m.Ask(gotermboard.Question{
		Listing: listing,
		Text:    fmt.Sprintf("merge these %d pull requests? [Y/n]", len(batch)),
		Typed:   true,
		Answer: func(typed string) tea.Cmd {
			if !slices.Contains([]string{"", "y", "yes"}, strings.ToLower(typed)) {
				return left()
			}
			clear(b.marked)
			clear(b.refused)
			b.batching = true
			m.Rebuild()
			return b.mergeInTurn(m, batch, &batchTally{of: len(batch)})
		},
		Cancel: left,
	})
	return nil
}

// prLine is a pull request as a question lists it, after lead, with why it is left out where
// it is.
func prLine(lead string, pr pullrequests.PullRequest, why string) gotermboard.Line {
	line := gotermboard.Line{
		gotermboard.C(fmt.Sprintf("%s%s #%d  ", lead, pr.Slug, pr.Number), gotermboard.Yellow),
		gotermboard.C(pr.Title, ""),
	}
	if why != "" {
		line = append(line, gotermboard.C(" — "+why, gotermboard.Red))
	}
	return line
}

// batchTally is how far a batch has got: how many it holds, which it merged, and each it left
// with why.
type batchTally struct {
	of     int
	merged []string
	left   []string
}

// said is the footer's line once the batch ends.
func (t *batchTally) said() string {
	said := fmt.Sprintf("merged %d of %d", len(t.merged), t.of)
	if len(t.merged) > 0 {
		said += ": " + strings.Join(t.merged, ", ")
	}
	if len(t.left) > 0 {
		said += ". Left " + strings.Join(t.left, "; ")
	}
	return said
}

// Reads of one pull request while GitHub works out its merge state, which a merge into its
// base sets it doing, and the pause between them.
const (
	mergeStateReads = 5
	mergeStatePause = 2 * time.Second
)

// mergeInTurn merges the first of queue, then the rest. Each is read again just before, because
// merging one can leave the next on the same repo behind or conflicting, and one that reads as
// anything but CLEAN with passing checks is left and named while the rest go on. Each merge pins
// the head d fetched, or else the one the question listed, so a commit pushed since is not
// merged unseen. The open listings are read again once it ends, because the merges move the
// merge state of the rows it leaves on the same repos.
func (b *Board) mergeInTurn(m *gotermboard.Model, queue []pullrequests.PullRequest, tally *batchTally) tea.Cmd {
	if len(queue) == 0 {
		b.batching = false
		b.readAgain()
		m.Say(tally.said())
		return nil
	}
	pr, rest := queue[0], queue[1:]
	name := fmt.Sprintf("%s #%d", pr.Slug, pr.Number)
	next := func(m *gotermboard.Model, why string) tea.Cmd {
		tally.left = append(tally.left, name+": "+why)
		return b.mergeInTurn(m, rest, tally)
	}
	if why := b.whyNotMergeable(pr); why != "" {
		return next(m, why)
	}
	provider, _ := pullrequests.ProviderOf(pr.Provider)
	head, pinned := b.diffed[pr.Key()], "d fetched it"
	if head == "" && pr.HeadOID != nil {
		head, pinned = *pr.HeadOID, "the question listed it"
	}
	ref := pullrequests.Reference{Provider: pr.Provider, Slug: pr.Slug, Number: pr.Number}
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("merging %s, %d of %d", name, tally.of-len(rest), tally.of),
		run: func(ctx context.Context) error {
			fresh, err := readMergeState(ctx, provider, ref)
			if err != nil {
				return err
			}
			if why := mergeBlocker(fresh); why != "" {
				return errors.New(why)
			}
			if head != "" && fresh.HeadOID != nil && *fresh.HeadOID != head {
				return fmt.Errorf("a commit was pushed to it since %s", pinned)
			}
			argv, inCheckout := provider.Merge(pr, head)
			return runClient(pr, "merge", argv, inCheckout)(ctx)
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			b.closeHere(m, pr, pullrequests.Merged)
			tally.merged = append(tally.merged, name)
			return b.mergeInTurn(m, rest, tally)
		},
		failed: func(m *gotermboard.Model, err error) tea.Cmd { return next(m, err.Error()) },
	})
}

// readMergeState reads the pull request again for its merge state, and again after a pause
// while GitHub reads it as UNKNOWN, up to mergeStateReads times.
func readMergeState(ctx context.Context, provider pullrequests.Provider, ref pullrequests.Reference) (pullrequests.PullRequest, error) {
	for read := 1; ; read++ {
		fresh, _, err := provider.ReadOne(ctx, ref)
		if err != nil {
			return pullrequests.PullRequest{}, fmt.Errorf("reading it again failed: %w", err)
		}
		if fresh.MergeStateStatus == nil || *fresh.MergeStateStatus != "UNKNOWN" || read == mergeStateReads {
			return fresh, nil
		}
		select {
		case <-ctx.Done():
			return fresh, ctx.Err()
		case <-time.After(mergeStatePause):
		}
	}
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
		checkout, err := pullrequests.CheckoutOf(pr)
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

// lastLine is the last thing a failed command said, or its exit status where it said nothing.
func lastLine(out []byte, err error) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return err.Error()
}
