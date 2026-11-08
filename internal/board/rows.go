package board

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// One Nerd Font glyph per field, never an emoji, because an emoji is two cells wide in some
// terminals and one in others. A field's glyph never changes with its state, only its
// color, because two glyphs are not guaranteed the same width either. Each is a
// private-use codepoint, which draws as nothing without the font, so its name is here.
const (
	checksGlyph  = "" // fa-check-circle
	reviewGlyph  = "" // fa-eye
	ageGlyph     = "" // fa-clock-o
	filesGlyph   = "" // fa-file
	addedGlyph   = "" // oct-diff-added
	removedGlyph = "" // oct-diff-removed
	branchGlyph  = "" // dev-git-branch
	stackGlyph   = "" // fa-arrow-up, toward the pull request this one is stacked on
	pathGlyph    = "" // fa-folder
	numberGlyph  = "" // oct-git-pull-request
	botGlyph     = "" // fa-robot, in every Nerd Font, where U+F544 is in none
)

// dim draws text that only marks an absence or sits behind what is read first.
const dim = "\033[2m"

// Days before a pull request's age reads as getting old, then as stale. A pull request is
// meant to be read the day it lands, so both are short.
const (
	aging = 3
	stale = 7
)

// meaning is what a GitHub state says on the board. A state is a word, because a legend
// made the reader carry a color from the foot of the screen back up to a glyph. Each state
// the board gives a meaning has one entry, which a row's cell and the document both read.
type meaning struct {
	// word is the row's, after "checks" or "review", and the document's for one check.
	word string
	code string
	// passed is a check that needs nothing from anyone, which the document lists after
	// those that do.
	passed bool
}

// checkMeanings covers a rollup on a row and one check run or commit status in the
// document.
var checkMeanings = map[string]meaning{
	"SUCCESS":   {word: "passing", code: gotermboard.Green, passed: true},
	"NEUTRAL":   {word: "neutral", code: gotermboard.Green, passed: true},
	"SKIPPED":   {word: "skipped", code: dim, passed: true},
	"FAILURE":   {word: "failing", code: gotermboard.Red},
	"ERROR":     {word: "failing", code: gotermboard.Red},
	"TIMED_OUT": {word: "timed out", code: gotermboard.Red},
	"":          {word: "not run", code: dim},
}

// reviewMeanings covers a decision on a row.
var reviewMeanings = map[string]meaning{
	"APPROVED":          {word: "approved", code: gotermboard.Green},
	"CHANGES_REQUESTED": {word: "changes", code: gotermboard.Red},
	"REVIEW_REQUIRED":   {word: "required", code: gotermboard.Cyan},
	"":                  {word: "none", code: dim},
}

// checkMeaning is a check state's entry. One not there is yellow, because GitHub adds
// states and a new one is in flight rather than absent.
func checkMeaning(state string) meaning {
	if m, known := checkMeanings[state]; known {
		return m
	}
	return meaning{word: words(state), code: gotermboard.Yellow}
}

// reviewMeaning is a review state's entry. One not there, such as a review that only
// commented, is cyan.
func reviewMeaning(state string) meaning {
	if m, known := reviewMeanings[state]; known {
		return m
	}
	return meaning{word: words(state), code: gotermboard.Cyan}
}

// words is an enum as words, CHANGES_REQUESTED as "changes requested".
func words(enum string) string { return strings.ToLower(strings.ReplaceAll(enum, "_", " ")) }

// columns carry the state, the age and the sizes, then the repo and the number, then the
// title wrapping under itself. The three diff counts each right-align in a column of their
// own, so the signs stack. The branch and the checkout are read when acting rather than when
// scanning, so they sit dim under the row, where f shows them.
var columns = []gotermboard.Column{
	{Title: "CHECKS"},
	{Title: "REVIEW"},
	{Title: "AGE", Right: true},
	{Title: "DIFF", Right: true},
	{Right: true},
	{Right: true},
	{Title: "REPO", Max: repoWidth},
	{Title: "#", Right: true},
	{Title: "PULL REQUEST"},
}

// repoWidth caps the repo column, so one long name never takes the title's room.
const repoWidth = 20

// stackParents maps a head branch to its pull request, so one whose base is another's head
// can name its parent. The key is the repo and the branch together, because branch names
// recur across repos.
func stackParents(prs []pullrequests.PullRequest) map[[2]string]int {
	parents := map[[2]string]int{}
	for _, pr := range prs {
		if pr.Branch != "" {
			parents[[2]string{pr.Slug, pr.Branch}] = pr.Number
		}
	}
	return parents
}

// parentOf is the number of the open pull request this one is stacked on, or 0.
func parentOf(pr pullrequests.PullRequest, parents map[[2]string]int) int {
	parent := parents[[2]string{pr.Slug, pr.Base}]
	if parent == pr.Number {
		return 0
	}
	return parent
}

func (b *Board) row(pr pullrequests.PullRequest, parents map[[2]string]int, now time.Time) gotermboard.Row {
	cells := append([]gotermboard.Cell{checksCell(pr), reviewCell(pr), ageCell(pr, now)}, diffCells(pr)...)
	cells = append(cells, repoCell(pr), gotermboard.C(fmt.Sprintf("%s #%d", numberGlyph, pr.Number), gotermboard.Yellow))
	return gotermboard.Row{
		Key:         pr.Key(),
		Cells:       cells,
		WrapLast:    titleLine(pr, parentOf(pr, parents), b.stateOf(pr)),
		Under:       gotermboard.C(identity(pr), dim),
		Document:    b.document(pr),
		ReaderTitle: pr.Title,
		StartsOpen:  b.isOpenedOn(pr),
		Text:        strings.Join([]string{pr.Body, pr.Author, pr.Branch, pr.Slug}, " "),
	}
}

// rowState is what a row reads before its title where its pull request waits on nobody: merged
// or closed by a key here, or gone from the listing while the cursor sat on it. It is empty for
// an open pull request.
type rowState string

const (
	merged rowState = "merged"
	closed rowState = "closed"
	gone   rowState = "gone"
)

// rowStateCodes color merged and closed as GitHub does, and gone as a change to notice.
var rowStateCodes = map[rowState]string{merged: gotermboard.Magenta, closed: gotermboard.Red, gone: gotermboard.Yellow}

// stateOf is the pull request's row state.
func (b *Board) stateOf(pr pullrequests.PullRequest) rowState {
	if state := b.settled[pr.Key()]; state != "" {
		return state
	}
	if b.dropped != nil && b.dropped.Key() == pr.Key() {
		return gone
	}
	return ""
}

// checksCell is the CI state.
func checksCell(pr pullrequests.PullRequest) gotermboard.Cell {
	s := checkMeaning(pr.Checks)
	return gotermboard.C(checksGlyph+"  checks "+s.word, s.code)
}

// reviewCell is the provider's decision, which gates the merge, and otherwise how many
// reviews were posted without one.
func reviewCell(pr pullrequests.PullRequest) gotermboard.Cell {
	if pr.Review == "" && pr.Reviews > 0 {
		return gotermboard.C(fmt.Sprintf("%s  review %d posted", reviewGlyph, pr.Reviews), reviewMeaning("REVIEW_REQUIRED").code)
	}
	s := reviewMeaning(pr.Review)
	return gotermboard.C(reviewGlyph+"  review "+s.word, s.code)
}

func ageCell(pr pullrequests.PullRequest, now time.Time) gotermboard.Cell {
	code := dim
	switch days := now.Sub(pr.CreatedAt).Hours() / 24; {
	case pr.CreatedAt.IsZero():
		return gotermboard.C(ageGlyph+"  ?", dim)
	case days >= stale:
		code = gotermboard.Red
	case days >= aging:
		code = gotermboard.Yellow
	}
	return gotermboard.Ticking(ageGlyph+"  "+gotermboard.Ago(now.Sub(pr.CreatedAt)), code)
}

// diffCells are files changed, lines added and lines removed. A provider that counts
// nothing gets a dash rather than zeros, because a pull request nobody counted is not one
// that changed nothing.
func diffCells(pr pullrequests.PullRequest) []gotermboard.Cell {
	if pr.Additions == nil || pr.Deletions == nil || pr.ChangedFiles == nil {
		return []gotermboard.Cell{gotermboard.C("—", dim), gotermboard.C("", ""), gotermboard.C("", "")}
	}
	return []gotermboard.Cell{
		gotermboard.C(filesGlyph+" "+strconv.Itoa(*pr.ChangedFiles), dim),
		gotermboard.C(addedGlyph+" "+strconv.Itoa(*pr.Additions), gotermboard.Green),
		gotermboard.C(removedGlyph+" "+strconv.Itoa(*pr.Deletions), gotermboard.Red),
	}
}

// repoCell is the repo's bare name, dimmed where it is not cloned here, which is the repo d
// and the window keys refuse.
func repoCell(pr pullrequests.PullRequest) gotermboard.Cell {
	if pr.Path == "" {
		return gotermboard.C(pathGlyph+" "+pr.Repo, dim)
	}
	return gotermboard.C(pathGlyph+" "+pr.Repo, gotermboard.Cyan)
}

// titleLine is the row's state where it has one, then the title, the open pull request it is
// stacked on, and the app that opened it. A pull request a person opened carries no mark, so
// the mark sets an app's row apart. A row with a state dims its title, because it waits on
// nobody.
func titleLine(pr pullrequests.PullRequest, parent int, state rowState) gotermboard.Line {
	line := gotermboard.Line{gotermboard.C(pr.Title, "")}
	switch {
	case state != "":
		line = gotermboard.Line{gotermboard.C(string(state), rowStateCodes[state]), gotermboard.C("  "+pr.Title, dim)}
	case pr.Draft:
		line = gotermboard.Line{gotermboard.C(pr.Title+" (draft)", dim)}
	}
	if parent != 0 {
		line = append(line, gotermboard.C(fmt.Sprintf("  %s #%d", stackGlyph, parent), gotermboard.Yellow))
	}
	if pr.Bot != nil && *pr.Bot {
		line = append(line, gotermboard.C("  "+botGlyph+" bot "+pr.Author, gotermboard.Magenta))
	}
	return line
}

// identity is the branch, the base it targets, and the checkout on this disk, written
// ~-rooted as it would be typed.
func identity(pr pullrequests.PullRequest) string {
	where := "not cloned here"
	if pr.Path != "" {
		where = pr.Path
		if home, err := os.UserHomeDir(); err == nil {
			if rest, ok := strings.CutPrefix(where, home); ok {
				where = "~" + rest
			}
		}
	}
	return fmt.Sprintf("%s %s into %s   %s %s", branchGlyph, orDash(pr.Branch), orDash(pr.Base), pathGlyph, where)
}

func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

// document is the pull request's MarkdownDocument laid out at the width it is drawn at. The
// side panel heads it with the row's ReaderTitle, and the Reader and the row each name the
// title already. A row gone from the listing says why its detail is not read, which is the
// board's to say rather than the pull request's.
func (b *Board) document(pr pullrequests.PullRequest) gotermboard.Document {
	return func(width int) []gotermboard.Line {
		var lines []gotermboard.Line
		if b.stateOf(pr) == gone {
			lines = append(lines, gotermboard.Line{gotermboard.C(
				"It is gone from the open pull requests, so its "+detailSectionNames()+" are not read.", dim)}, gotermboard.Line{})
		}
		markdown := b.markdownOf(pr, time.Now())
		return append(lines, b.renderer.Lines(markdown, min(width, gotermboard.ReadingWidth))...)
	}
}

// documentMemo is a pull request's MarkdownDocument and what it was built from. The panel
// draws it on every frame, and building it parses every body and comment, so it is built
// again only when one of those moves. The renderer keeps what it laid out the same way.
type documentMemo struct {
	from     documentFrom
	markdown string
}

// documentFrom is every input of a MarkdownDocument. A listing read makes new rows, and a
// minute passing moves each age the document writes.
type documentFrom struct {
	pr            pullrequests.PullRequest
	readsDetail   bool
	readAt        time.Time
	held, reading bool
	fault         string
	minute        time.Time
}

// markdownOf is the pull request's MarkdownDocument at now, built again only where its
// inputs moved since the last frame drew it.
func (b *Board) markdownOf(pr pullrequests.PullRequest, now time.Time) string {
	detail := b.detailOf(pr)
	from := documentFrom{pr: pr, minute: now.Truncate(time.Minute)}
	if detail != nil {
		from.readsDetail, from.readAt, from.held, from.reading, from.fault = true, detail.Detail.ReadAt, detail.Held, detail.Reading, detail.Fault
	}
	if memo, ok := b.documents[pr.Key()]; ok && memo.from == from {
		return memo.markdown
	}
	markdown := MarkdownDocument(pr, detail, now)
	b.documents[pr.Key()] = documentMemo{from: from, markdown: markdown}
	return markdown
}

// detailOf is how the pull request's detail stands for its document, and nil where its host
// reads no detail. The pull request the board opened on shows its own read's detail until
// the cache holds one.
func (b *Board) detailOf(pr pullrequests.PullRequest) *pullrequests.DetailState {
	provider, err := pullrequests.ProviderOf(pr.Provider)
	if err != nil || provider.Detail == nil {
		return nil
	}
	state := b.details.Of(pr)
	if detail, own := b.openedOnDetail(pr); own && !state.Held {
		state.Detail, state.Held = detail, true
	}
	// A listing claims its rows' reads once it lands, so a row drawn before that is due a read.
	// A row gone from the listing is due none.
	if !state.Held && state.Fault == "" && b.readDetails && b.stateOf(pr) != gone {
		state.Reading = true
	}
	return &state
}
