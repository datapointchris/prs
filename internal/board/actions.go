package board

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
// y copies, m merges and d reads, never deletes. x closes, because c is the comment. i is
// the interdiff, the diff since review. D is left unbound, because D deletes in other
// terminal tools.
func (b *Board) keyTable(windows []config.WindowAction) []gotermboard.Action {
	actions := []gotermboard.Action{
		{Keys: []string{"d"}, Label: "view diff: fetch its head and open it in nvim here, until :qa", Footer: "d diff", Run: b.onRow(b.diff)},
		{
			Keys:  []string{"i"},
			Label: "view diff since review: open in nvim what was pushed after the commit its newest review read, or the whole diff where no review read one still on the branch",
			Run:   b.onRow(b.diffSinceReview),
		},
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
		gotermboard.Action{
			Keys:  []string{"f"},
			Label: "fix what keeps GitHub from merging it: a menu of fixes for conflicts, a branch behind, failing checks or a draft",
			Run:   b.onRow(b.fix),
		},
		gotermboard.Action{Keys: []string{"x"}, Label: "close without merging, after asking, and enter alone keeps it open", Run: b.onRow(b.closePR)},
		gotermboard.Action{
			Keys: []string{"space"}, Label: "mark for m to merge with the others marked, where its checks pass and GitHub reads it ready; on a group's header, fold it",
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
		if why := b.notMarkable(pr); why != "" {
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

// notMarkable is why space will not mark the pull request, and empty where it can: a key may
// merge it, and its checks pass and its merge state is ready.
func (b *Board) notMarkable(pr pullrequests.PullRequest) string {
	if _, why := b.mergeable(pr); why != "" {
		return why
	}
	return mergeBlocker(pr)
}

// mergeBlocker is why the pull request's record says it would not merge on its own, and empty
// where its checks pass and its merge state is ready. A host that reports no merge state is
// one such, because nothing says the merge would go through.
func mergeBlocker(pr pullrequests.PullRequest) string {
	var why []string
	if checks := checkMeaning(pr.Checks); !checks.passed {
		why = append(why, "its checks are "+checks.word)
	}
	switch {
	case pr.MergeStateStatus == nil:
		why = append(why, pr.Provider+" reports no merge state for it")
	case !mergeMeanings[*pr.MergeStateStatus].ready:
		why = append(why, "GitHub reads its merge state as "+words(*pr.MergeStateStatus)+", not ready")
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

// diff opens the pull request's whole diff: three-dot from where its branch left origin/<base>.
func (b *Board) diff(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	return b.openDiff(m, pr, func(_ context.Context, target pullrequests.LocalDiff, _ string) (diffOpened, error) {
		return wholeDiff(pr, target, ""), nil
	})
}

// diffSinceReview opens what was pushed after the commit the pull request's newest review read,
// which is all a reviewer has not seen. Where no review was submitted, the fetched head does
// not hold that commit, as after a rebase, or the base was merged into the branch since, it
// opens the whole diff, and the footer says why once nvim exits. Where git cannot say whether
// the head holds it, nvim does not start and the footer names git's failure. A pull request
// whose detail prs does not read has no reviewed commit to start from, so the key refuses
// there, as it does before the detail's first read lands.
func (b *Board) diffSinceReview(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	detail := b.detailOf(pr)
	switch {
	case detail == nil:
		m.Say(fmt.Sprintf("prs reads no reviews from %s, so #%d has no reviewed commit to diff from; d opens the whole diff", pr.Provider, pr.Number))
		return nil
	case detail.Held:
	case detail.Fault != "":
		m.Say(fmt.Sprintf("#%d's reviews could not be read, so it has no reviewed commit to diff from: %s", pr.Number, detail.Fault))
		return nil
	case detail.Reading:
		m.Say(fmt.Sprintf("#%d's reviews are still being read; i diffs from the commit the newest one read once they land", pr.Number))
		return nil
	default:
		m.Say(unreadReviews(pr, b.stateOf(pr)) + ", so it has no reviewed commit to diff from; d opens the whole diff")
		return nil
	}
	review, reviewed := detail.Detail.NewestReview()
	return b.openDiff(m, pr, func(ctx context.Context, target pullrequests.LocalDiff, head string) (diffOpened, error) {
		at := short(review.CommitOID)
		switch {
		case !reviewed:
			return wholeDiff(pr, target, "no review was submitted on it"), nil
		case review.CommitOID == "":
			return wholeDiff(pr, target, "GitHub names no commit for its newest review, as after a rebase"), nil
		case review.CommitOID == head:
			return diffOpened{nothing: fmt.Sprintf("nothing was pushed to %s #%d since its newest review, at %s", pr.Repo, pr.Number, at)}, nil
		}
		if holds, err := pullrequests.HeadHolds(ctx, target, review.CommitOID); err != nil || !holds {
			return wholeDiff(pr, target, fmt.Sprintf("the commit its newest review read, %s, is gone from the branch, as after a rebase", at)), err
		}
		if merged, err := pullrequests.BaseMergedSince(ctx, target, review.CommitOID); err != nil || merged {
			return wholeDiff(pr, target, fmt.Sprintf("origin/%s was merged into the branch since its newest review, at %s, and a diff from there would show the base's changes as the branch's", target.Base, at)), err
		}
		return diffOpened{
			revisions: target.RangeFrom(review.CommitOID), base: review.CommitOID, kind: sinceReview,
			closed: fmt.Sprintf("closed the diff of %s #%d since its newest review, at %s", pr.Repo, pr.Number, at),
		}, nil
	})
}

// unreadReviews is why prs holds none of the pull request's reviews on a host whose detail it
// reads: a recently closed one's detail is never read, and neither is one no read lists.
func unreadReviews(pr pullrequests.PullRequest, state rowState) string {
	switch state {
	case "":
		return fmt.Sprintf("prs has not read #%d's reviews", pr.Number)
	case gone:
		return fmt.Sprintf("no read lists #%d any more, so prs reads none of its reviews", pr.Number)
	}
	return fmt.Sprintf("#%d is %s, so prs reads none of its reviews", pr.Number, state)
}

// diffKind is which diff a key opened, as the threads file names it.
type diffKind string

const (
	// whole runs three-dot from where the branch left origin/<base>, so its left side is the
	// base branch's copy, where a LEFT thread's line counts.
	whole diffKind = "whole"
	// sinceReview runs from the commit the newest review read, which is its left side.
	sinceReview diffKind = "since_review"
)

// diffOpened is what a diff key opens once the head is fetched: the revisions Diffview
// compares, which diff they are, the base the threads file names, and what the footer says
// once nvim exits.
type diffOpened struct {
	revisions, base, closed string
	kind                    diffKind
	// why is the sentence after closed, saying why the diff is what it is.
	why string
	// nothing is what the footer says where revisions is empty, and nvim does not start.
	nothing string
}

// wholeDiff is the pull request's whole diff, three-dot from where its branch left
// origin/<base>, with why it is whole where the key asked for less.
func wholeDiff(pr pullrequests.PullRequest, target pullrequests.LocalDiff, why string) diffOpened {
	opened := diffOpened{
		revisions: target.Range(), base: "origin/" + target.Base, kind: whole,
		closed: fmt.Sprintf("closed the diff of %s #%d", pr.Repo, pr.Number),
	}
	if why != "" {
		opened.closed = fmt.Sprintf("closed the whole diff of %s #%d", pr.Repo, pr.Number)
		opened.why = "It was whole, because " + why
	}
	return opened
}

// openDiff fetches the pull request's head and base in the background, then hands prs's
// terminal to nvim's diff of what choose picks from the fetched head, run in the checkout.
// Quitting nvim gives the terminal back to the board, with the cursor where it was, and
// Receive reads the providers again. The head it fetched is the one m then pins.
//
// nvim listens on a socket named for the pull request, so another program can ask it what
// is under the cursor. Inside tmux the window is named diff <repo> #<n> while nvim has it,
// so a program split into the window finds that socket from the name. Beside the socket it
// writes the pull request's document and its review threads, and starts nvim with
// PRS_DIFF_DOCUMENT and PRS_DIFF_THREADS naming them, so an nvim config can show them
// beside the diff.
func (b *Board) openDiff(m *gotermboard.Model, pr pullrequests.PullRequest,
	choose func(ctx context.Context, target pullrequests.LocalDiff, head string) (diffOpened, error),
) tea.Cmd {
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
	// run writes these and done reads them, after the message that ends the work. A base that
	// failed to fetch still diffs against the origin/<base> fetched earlier, and the footer
	// says so once nvim exits.
	var got pullrequests.Fetched
	var opened diffOpened
	return b.start(work{
		pr:    pr,
		doing: fmt.Sprintf("fetching %s #%d", pr.Repo, pr.Number),
		run: func(ctx context.Context) error {
			var err error
			if got, err = pullrequests.Fetch(ctx, target); err != nil {
				return err
			}
			opened, err = choose(ctx, target, got.Head)
			return err
		},
		done: func(m *gotermboard.Model) tea.Cmd {
			b.diffed[pr.Key()] = got.Head
			if opened.revisions == "" {
				m.Say(opened.nothing)
				return nil
			}
			stem := filepath.Join(os.TempDir(), fmt.Sprintf("prs-diff-%s-%d", pr.Repo, pr.Number))
			socket := stem + ".sock"
			_ = os.Remove(socket)
			nvim := exec.Command("nvim", "--listen", socket, "-c", openDiffview(opened.revisions))
			nvim.Dir = target.Checkout
			files := diffFilesAt(stem)
			unwritten := files.write(pr, b.detailOf(pr), b.roundWindowOf(pr), opened.base, got.Head, opened.kind, time.Now())
			if unwritten == nil {
				nvim.Env = append(os.Environ(), files.env()...)
			}
			restore := nameWindow(fmt.Sprintf("diff %s #%d", pr.Repo, pr.Number))
			return m.HandOver(nvim, func(m *gotermboard.Model, err error, stderr []byte) tea.Cmd {
				restore()
				files.remove()
				var exit *exec.ExitError
				said := opened.closed
				switch {
				case errors.As(err, &exit) && exit.ExitCode() == noDiffview:
					said = "nvim has no :DiffviewOpen, and the diff needs the diffview.nvim plugin"
				case err != nil:
					said = fmt.Sprintf("nvim ended the diff of #%d: %s", pr.Number, gotermboard.LastLine(stderr, err))
				default:
					if got.BaseFault != "" && opened.kind == whole {
						said += fmt.Sprintf(", which ran against the origin/%s fetched earlier, because fetching it again failed: %s", target.Base, got.BaseFault)
					}
					if opened.why != "" {
						said += ". " + opened.why
					}
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
// but y or yes leaves it, so a stray key still stops it. A pull request whose record says
// GitHub would refuse the merge is read again first, and opens the fix menu where the new read
// still says so.
func (b *Board) merge(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	if _, why := b.mergeable(pr); why != "" {
		m.Say(refused(pr, "merge", why).Error())
		return nil
	}
	if blockerOf(pr) != "" {
		return b.readThenFix(m, pr, fixFrom{merge: true})
	}
	return b.askMerge(m, pr)
}

// askMerge is merge's question.
func (b *Board) askMerge(m *gotermboard.Model, pr pullrequests.PullRequest) tea.Cmd {
	merge, why := b.mergeOf(pr)
	if why != "" {
		m.Say(refused(pr, "merge", why).Error())
		return nil
	}
	b.ask(m, pr, "merge", true, merge.work)
	return nil
}

// pinnedMerge is a merge of a pull request ready to start: its work, through provider, pinned
// to head, which came from where from says.
type pinnedMerge struct {
	work       work
	provider   pullrequests.Provider
	head, from string
}

// mergeOf is the merge every key starts, pinned through pinFor, or why no key may merge the
// pull request now.
func (b *Board) mergeOf(pr pullrequests.PullRequest) (pinnedMerge, string) {
	provider, why := b.mergeable(pr)
	if why != "" {
		return pinnedMerge{}, why
	}
	head, from := b.pinFor(pr)
	return pinnedMerge{work: b.mergeWork(pr, provider, head), provider: provider, head: head, from: from}, ""
}

// mergeable is the provider that merges the pull request, or why no key may merge it now.
func (b *Board) mergeable(pr pullrequests.PullRequest) (pullrequests.Provider, string) {
	if why := b.refusal(pr); why != "" {
		return pullrequests.Provider{}, why
	}
	provider, err := pullrequests.ProviderOf(pr.Provider)
	switch {
	case err != nil:
		return pullrequests.Provider{}, err.Error()
	case provider.Merge == nil:
		return pullrequests.Provider{}, "its client on " + pr.Provider + " has no verb to merge"
	}
	return provider, ""
}

// pinFor is the head a merge of the pull request pins, and where it came from: the one a diff
// key last fetched, else the one its Diff section was read at, else the one the board's
// record holds, so a commit pushed since the screen showed it is not merged unseen. A host
// whose client cannot pin a head ignores it.
func (b *Board) pinFor(pr pullrequests.PullRequest) (head, from string) {
	if head := b.diffed[pr.Key()]; head != "" {
		return head, "its diff in nvim fetched it"
	}
	if b.diffs != nil {
		if state := b.diffs.Of(pr); state.Held && state.Diff.Head != "" {
			return state.Diff.Head, "its Diff section showed it"
		}
	}
	if pr.HeadOID != nil {
		return *pr.HeadOID, "the list showed it"
	}
	return "", ""
}

// mergeWork merges the pull request through its host's client, pinned to head where head is
// set, and moves its row to recently closed once it has. A merge the host refuses opens the fix
// menu, with the host's words, on a host that reports a merge state. One that got no answer
// may have merged, so it says so instead.
func (b *Board) mergeWork(pr pullrequests.PullRequest, provider pullrequests.Provider, head string) work {
	argv, inCheckout := provider.Merge(pr, head)
	return work{
		pr:    pr,
		doing: fmt.Sprintf("merging %s #%d", pr.Slug, pr.Number),
		run:   runClient(pr, "merge", argv, inCheckout),
		done: func(m *gotermboard.Model) tea.Cmd {
			b.closeHere(m, pr, pullrequests.Merged)
			m.Say(fmt.Sprintf("merged %s #%d", pr.Slug, pr.Number))
			return nil
		},
		failed: func(m *gotermboard.Model, err error) tea.Cmd {
			if errors.Is(err, pullrequests.ErrNoAnswer) || pr.MergeStateStatus == nil {
				m.Say(err.Error())
				return nil
			}
			return b.readThenFix(m, pr, fixFrom{refusal: err.Error()})
		},
	}
}

// mergeMarked asks once, listing every marked pull request it will merge and each marked or
// refused one it leaves out with why, then merges them one at a time in the order they draw.
// Enter alone merges, as it does for one. A stacked one names the branch it merges into. The
// marks clear once the question is answered yes, because it named every one of them, and stay
// on a no so the batch can be asked again.
func (b *Board) mergeMarked(m *gotermboard.Model) tea.Cmd {
	if b.batching {
		m.Say("a batch is still merging; m merges the marked ones once it ends")
		return nil
	}
	var open []pullrequests.PullRequest
	for _, key := range b.drawnOrder {
		if b.listedOpen(key) {
			open = append(open, b.shown[key])
		}
	}
	parents := stackParents(open)
	var batch []pullrequests.PullRequest
	var merging, leftOut []gotermboard.Line
	for _, key := range b.drawnOrder {
		pr := b.shown[key]
		if !b.marked[key] && !b.refused[key] {
			continue
		}
		switch why := b.notMarkable(pr); {
		case why != "":
			leftOut = append(leftOut, prLine("    ", pr, why))
		case b.marked[key]:
			batch = append(batch, pr)
			line := prLine("  "+gotermboard.MarkGlyph+" ", pr, "")
			if parentOf(pr, parents) != 0 {
				line = append(line, gotermboard.C("  into "+pr.Base, dim))
			}
			merging = append(merging, line)
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
			return b.mergeInTurn(m, batch, &batchTally{of: len(batch), heads: map[[2]string]bool{}})
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

// batchTally is how far a batch has got: how many it holds, which it merged, each it left with
// why, and the head branches it merged, by repo and branch as stackParents keys them.
type batchTally struct {
	of     int
	merged []string
	left   []string
	heads  map[[2]string]bool
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

// mergeInTurn merges the first of queue, then the rest, each as m merges one. Each is read
// again just before, because merging one can leave the next on the same repo behind or
// conflicting, or based on a branch this batch merged, where it would land on that branch.
// One that reads as anything but ready with passing checks is left and named while the rest
// go on, and so is one whose head moved from the one its merge pins. The open listings are
// read again once it ends, because the merges move the merge state of the rows it leaves on
// the same repos.
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
	pinned, why := b.mergeOf(pr)
	if why != "" {
		return next(m, why)
	}
	provider, head, from := pinned.provider, pinned.head, pinned.from
	ref := pullrequests.Reference{Provider: pr.Provider, Slug: pr.Slug, Number: pr.Number}
	mergedHeads := maps.Clone(tally.heads)
	w := pinned.work
	merge, merged := w.run, w.done
	w.doing = fmt.Sprintf("merging %s, %d of %d", name, tally.of-len(rest), tally.of)
	w.run = func(ctx context.Context) error {
		fresh, _, err := readMergeState(ctx, provider, ref)
		if err != nil {
			return err
		}
		if why := mergeBlocker(fresh); why != "" {
			return errors.New(why)
		}
		if mergedHeads[[2]string{pr.Slug, fresh.Base}] {
			return fmt.Errorf("its base %s merged earlier in this batch, so it would land on %s rather than where that went", fresh.Base, fresh.Base)
		}
		if head != "" && fresh.HeadOID != nil && *fresh.HeadOID != head {
			return fmt.Errorf("a commit was pushed to it since %s", from)
		}
		return merge(ctx)
	}
	w.done = func(m *gotermboard.Model) tea.Cmd {
		merged(m)
		tally.merged = append(tally.merged, name)
		tally.heads[[2]string{pr.Slug, pr.Branch}] = true
		return b.mergeInTurn(m, rest, tally)
	}
	w.failed = func(m *gotermboard.Model, err error) tea.Cmd { return next(m, err.Error()) }
	return b.start(w)
}

// readMergeState reads the pull request again for its merge state, with its detail, and again
// after a pause while GitHub is still computing it, up to mergeStateReads times. A draft is not
// waited on, because a draft does not merge whatever GitHub works out.
func readMergeState(ctx context.Context, provider pullrequests.Provider, ref pullrequests.Reference) (pullrequests.PullRequest, *pullrequests.Detail, error) {
	for read := 1; ; read++ {
		fresh, detail, err := provider.ReadOne(ctx, ref)
		if err != nil {
			return pullrequests.PullRequest{}, nil, fmt.Errorf("reading it again failed: %w", err)
		}
		if fresh.MergeStateStatus == nil || fresh.Draft || !mergeMeanings[*fresh.MergeStateStatus].computing || read == mergeStateReads {
			return fresh, detail, nil
		}
		select {
		case <-ctx.Done():
			return fresh, detail, ctx.Err()
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

// refusal is why no key may change the pull request now, and empty where one may. A merge or a
// close already settled it, no read lists it any more, or a merge, close or fetch is under way
// on it, so a second press never sends a second change beside the first.
func (b *Board) refusal(pr pullrequests.PullRequest) string {
	switch state := b.stateOf(pr); state {
	case merged, closed:
		return "it is " + string(state) + " already"
	case gone:
		return "no read lists it any more; o opens its page"
	}
	if doing := b.busy(pr); doing != "" {
		return doing + " is still under way"
	}
	return ""
}

// writable is the provider of a pull request a key may verb, or the refusal saying why not.
func (b *Board) writable(pr pullrequests.PullRequest, verb string) (pullrequests.Provider, error) {
	if why := b.refusal(pr); why != "" {
		return pullrequests.Provider{}, refused(pr, verb, why)
	}
	return pullrequests.ProviderOf(pr.Provider)
}

// refused is what the footer says where a key will not verb the pull request, because of why.
func refused(pr pullrequests.PullRequest, verb, why string) error {
	return fmt.Errorf("prs will not %s %s #%d, because %s", verb, pr.Slug, pr.Number, why)
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
	return m.HandOver(gotermboard.EditorCommand(path), func(m *gotermboard.Model, err error, stderr []byte) tea.Cmd {
		body, readErr := os.ReadFile(path)
		switch {
		case err != nil:
			m.Say(fmt.Sprintf("the editor ended with %s, so nothing was posted; what it wrote is in %s", gotermboard.LastLine(stderr, err), path))
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

// windowOn is the name of the window the action opens on the pull request and the fields its
// command fills in, or why the action does not run on the pull request's provider. The board
// finds a round's window by this name, so the window opened and the window looked for agree.
func windowOn(action config.WindowAction, pr pullrequests.PullRequest) (string, config.Fields, error) {
	if len(action.Providers) > 0 && !slices.Contains(action.Providers, pr.Provider) {
		return "", config.Fields{}, fmt.Errorf("%s runs on %s pull requests, and #%d is on %s",
			action.Label, strings.Join(action.Providers, " and "), pr.Number, pr.Provider)
	}
	fields := config.Fields{Number: pr.Number, Repo: pr.Repo, Slug: pr.Slug}
	return config.Fill(action.Window, fields), fields, nil
}

// windowAction runs a key the config declares in a tmux window in the checkout.
func (b *Board) windowAction(window config.WindowAction) func(pullrequests.PullRequest) (string, error) {
	return func(pr pullrequests.PullRequest) (string, error) {
		name, fields, err := windowOn(window, pr)
		if err != nil {
			return "", err
		}
		checkout, err := pullrequests.CheckoutOf(pr)
		if err != nil {
			return "", err
		}
		// The command is a shell command, which tmux runs whole through its shell, so needs
		// names the programs it runs.
		if err := gotermboard.OpenWindow(name, checkout, []string{config.FillCommand(window.Command, fields)}, window.Needs...); err != nil {
			return "", err
		}
		if window.RunsReviewRound {
			b.windows.openedHere(name)
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
		return "", fmt.Errorf("%s could not take the branch: %s", argv[0], gotermboard.LastLine(out, err))
	}
	return "copied " + pr.Branch, nil
}
