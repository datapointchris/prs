package board

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	stateGlyph   = "" // oct-git-merge
	checksGlyph  = "" // fa-check-circle
	reviewGlyph  = "" // fa-eye
	claudeGlyph  = "" // oct-comment-discussion
	ageGlyph     = "" // fa-clock-o
	filesGlyph   = "" // fa-file
	addedGlyph   = "" // oct-diff-added
	removedGlyph = "" // oct-diff-removed
	branchGlyph  = "" // dev-git-branch
	stackGlyph   = "" // fa-arrow-up, toward the pull request this one is stacked on
	pathGlyph    = "" // fa-folder
	numberGlyph  = "" // oct-git-pull-request
	sinceGlyph   = "" // oct-git-commit
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
	// ready is a merge state that merges on its own, which space marks, and computing one
	// GitHub has not worked out yet, which a batch reads again.
	ready, computing bool
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

// mergeMeanings covers GitHub's merge state on an open row: what its merge waits on. A
// mergeable branch whose checks are not passing reads mergeable, and its checks cell says
// why.
var mergeMeanings = map[string]meaning{
	"CLEAN":     {word: "ready", code: gotermboard.Green, ready: true},
	"HAS_HOOKS": {word: "ready", code: gotermboard.Green, ready: true},
	"UNSTABLE":  {word: "mergeable", code: gotermboard.Yellow},
	"BEHIND":    {word: "behind", code: gotermboard.Cyan},
	"BLOCKED":   {word: "blocked"},
	"DIRTY":     {word: "conflicts", code: gotermboard.Yellow},
	"DRAFT":     {word: "draft", code: dim},
	"UNKNOWN":   {word: "unknown", code: dim, computing: true},
}

// standingMeanings cover a row that waits on nobody. Merged and closed are GitHub's colors,
// and gone is a change to notice.
var standingMeanings = map[rowState]meaning{
	merged: {word: "merged", code: gotermboard.Magenta},
	closed: {word: "closed", code: gotermboard.Red},
	gone:   {word: "gone", code: gotermboard.Yellow},
}

// stateMeaning is the one word for where the pull request stands: its row state once it
// waits on nobody, and otherwise draft, conflicts, or what its merge state says. A merge state
// not there reads as its own words, because GitHub adds states. A host that carries no merge
// state reads open, dim, because it says no more than the group does.
func stateMeaning(pr pullrequests.PullRequest, state rowState) meaning {
	switch {
	case state != "":
		return standingMeanings[state]
	case pr.Draft:
		return mergeMeanings["DRAFT"]
	case pr.Mergeable != nil && *pr.Mergeable == "CONFLICTING":
		return mergeMeanings["DIRTY"]
	case pr.MergeStateStatus == nil:
		return meaning{word: "open", code: dim}
	}
	if m, known := mergeMeanings[*pr.MergeStateStatus]; known {
		return m
	}
	return meaning{word: words(*pr.MergeStateStatus)}
}

// words is an enum as words, CHANGES_REQUESTED as "changes requested".
func words(enum string) string { return strings.ToLower(strings.ReplaceAll(enum, "_", " ")) }

// rowColumn is one column of a table of pull requests: its header, and the cell it draws on a
// pull request's line. A column with no cell is the line's last, which the table fills itself.
// An optional column draws only while a row fills it.
type rowColumn struct {
	gotermboard.Column
	cell     func(b *Board, at rowAt) gotermboard.Cell
	optional bool
}

// rowAt is what a row's cells read: its pull request, its row state and the time.
type rowAt struct {
	pr    pullrequests.PullRequest
	state rowState
	now   time.Time
}

// boardColumns are a pull request's first line on the board: where it stands, its title, its
// diff, its age and its branch, spread across the width. The diff's three counts each
// right-align on a column of their own, so the signs stack, and the two after the first carry
// no title, so the spread keeps them beside it. Its second line, under the first column, is
// secondLine.
var boardColumns = []rowColumn{
	{Column: gotermboard.Column{Title: "STATUS"}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return stateCell(at.pr, at.state) }},
	{Column: gotermboard.Column{Title: "TITLE"}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return titleCell(at.pr, at.state) }},
	{Column: gotermboard.Column{Title: "DIFF", Right: true}, cell: diffCell(0)},
	{Column: gotermboard.Column{Right: true}, cell: diffCell(1)},
	{Column: gotermboard.Column{Right: true}, cell: diffCell(2)},
	{Column: gotermboard.Column{Title: "AGE", Right: true}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return ageCell(at.pr, at.state, at.now) }},
	{Column: gotermboard.Column{Title: "BRANCH"}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return branchCell(at.pr) }},
}

// listColumns are what prs list prints of a pull request on one line: the state, the checks,
// the review, the newest Claude review round, the age and the sizes, then the repo and the
// number, then the title. The round's column is optional, so a list of repos no round reviews
// never has it.
var listColumns = []rowColumn{
	{Column: gotermboard.Column{Title: "STATE"}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return stateCell(at.pr, at.state) }},
	{Column: gotermboard.Column{Title: "CHECKS"}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return checksCell(at.pr) }},
	{Column: gotermboard.Column{Title: "REVIEW"}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return reviewCell(at.pr) }},
	{
		Column:   gotermboard.Column{Title: "CLAUDE"},
		cell:     func(b *Board, at rowAt) gotermboard.Cell { return claudeCell(b.reviewOf(at.pr, at.now)) },
		optional: true,
	},
	{Column: gotermboard.Column{Title: "AGE", Right: true}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return ageCell(at.pr, at.state, at.now) }},
	{Column: gotermboard.Column{Title: "DIFF", Right: true}, cell: diffCell(0)},
	{Column: gotermboard.Column{Right: true}, cell: diffCell(1)},
	{Column: gotermboard.Column{Right: true}, cell: diffCell(2)},
	{Column: gotermboard.Column{Title: "REPO", Max: repoWidth}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return repoCell(at.pr) }},
	{Column: gotermboard.Column{Title: "#", Right: true}, cell: func(_ *Board, at rowAt) gotermboard.Cell { return numberCell(at.pr) }},
	{Column: gotermboard.Column{Title: "PULL REQUEST"}},
}

// listedColumns is listColumns less each optional one no row of prs fills.
func (b *Board) listedColumns(prs []pullrequests.PullRequest, now time.Time) []rowColumn {
	return slices.DeleteFunc(slices.Clone(listColumns), func(column rowColumn) bool {
		return column.optional && !slices.ContainsFunc(prs, func(pr pullrequests.PullRequest) bool {
			return column.cell(b, rowAt{pr: pr, state: b.stateOf(pr), now: now}).Text != ""
		})
	})
}

// headers is each column's header.
func headers(columns []rowColumn) []gotermboard.Column {
	out := make([]gotermboard.Column, len(columns))
	for i, column := range columns {
		out[i] = column.Column
	}
	return out
}

// repoWidth caps the repo column, so one long name never takes the title's room.
const repoWidth = 20

// stackParents maps a head branch to its open pull request, so one whose base is another's
// head can name its parent. The key is the repo and the branch together, because branch
// names recur across repos.
func stackParents(prs []pullrequests.PullRequest) map[[2]string]int {
	parents := map[[2]string]int{}
	for _, pr := range prs {
		if pr.Branch != "" && sectionOf(pr) != recentlyClosed {
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

// cells are the cells columns draw on the pull request's line.
func (b *Board) cells(columns []rowColumn, at rowAt) []gotermboard.Cell {
	var cells []gotermboard.Cell
	for _, column := range columns {
		if column.cell != nil {
			cells = append(cells, column.cell(b, at))
		}
	}
	return cells
}

func (b *Board) row(pr pullrequests.PullRequest, parents map[[2]string]int, now time.Time) gotermboard.Row {
	at := rowAt{pr: pr, state: b.stateOf(pr), now: now}
	return gotermboard.Row{
		Key:         pr.Key(),
		Cells:       b.cells(boardColumns, at),
		Second:      b.secondLine(at, parentOf(pr, parents)),
		Document:    b.document(pr),
		ReaderTitle: pr.Title,
		StartsOpen:  b.isOpenedOn(pr),
		Marked:      b.marked[pr.Key()],
		Text:        strings.Join([]string{pr.Body, pr.Author, pr.Branch, pr.Slug}, " "),
	}
}

// rowState is where a row's pull request stands once it waits on nobody: merged or closed by
// its record, or gone from every listing while the cursor sat on it. It is empty for an open
// pull request.
type rowState string

const (
	merged rowState = "merged"
	closed rowState = "closed"
	gone   rowState = "gone"
)

// stateOf is the pull request's row state. A state word its host's table does not name reads
// as open, as the listing that found it does.
func (b *Board) stateOf(pr pullrequests.PullRequest) rowState {
	if b.held != nil && b.held.gone && b.held.pr.Key() == pr.Key() {
		return gone
	}
	switch standing, _ := pr.Standing(); standing {
	case pullrequests.Merged:
		return merged
	case pullrequests.Closed:
		return closed
	}
	return ""
}

// stateCell is the row state where it has one, and otherwise what the pull request waits on.
func stateCell(pr pullrequests.PullRequest, state rowState) gotermboard.Cell {
	s := stateMeaning(pr, state)
	return gotermboard.C(stateGlyph+"  "+s.word, s.code)
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

// ageCell is how long the pull request has been open, colored as it gets old, or on a merged
// or closed row, dim, how long ago it closed.
func ageCell(pr pullrequests.PullRequest, state rowState, now time.Time) gotermboard.Cell {
	if at := closedAt(pr); (state == merged || state == closed) && !at.IsZero() {
		return gotermboard.Ticking(ageGlyph+"  "+gotermboard.Ago(now.Sub(at)), dim)
	}
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

// diffCell is the diffCells cell at i, one to a column.
func diffCell(i int) func(*Board, rowAt) gotermboard.Cell {
	return func(_ *Board, at rowAt) gotermboard.Cell { return diffCells(at.pr)[i] }
}

// repoCell is the repo's bare name, dimmed where it is not cloned here, which is the repo d
// and the window keys refuse.
func repoCell(pr pullrequests.PullRequest) gotermboard.Cell {
	if pr.Path == "" {
		return gotermboard.C(pathGlyph+" "+pr.Repo, dim)
	}
	return gotermboard.C(pathGlyph+" "+pr.Repo, gotermboard.Cyan)
}

// titleCell is the title. A draft's and one with a row state are dim, because neither waits
// on a reviewer.
func titleCell(pr pullrequests.PullRequest, state rowState) gotermboard.Cell {
	if state != "" || pr.Draft {
		return gotermboard.C(pr.Title, dim)
	}
	return gotermboard.C(pr.Title, "")
}

// titleLine is the title, the open pull request it is stacked on, and the app that opened it.
// A pull request a person opened carries no mark, so the mark sets an app's row apart.
func titleLine(pr pullrequests.PullRequest, parent int, state rowState) gotermboard.Line {
	line := gotermboard.Line{titleCell(pr, state)}
	if parent != 0 {
		line = append(line, titleGap, stackCell(parent))
	}
	if isBot(pr) {
		line = append(line, titleGap, botCell(pr))
	}
	return line
}

// titleGap is the space between the title and each mark after it in prs list.
var titleGap = gotermboard.C("  ", "")

// branchCell is the branch the pull request merges from.
func branchCell(pr pullrequests.PullRequest) gotermboard.Cell {
	return gotermboard.C(branchGlyph+"  "+orDash(pr.Branch), gotermboard.Blue)
}

// numberCell is the pull request's number.
func numberCell(pr pullrequests.PullRequest) gotermboard.Cell {
	return gotermboard.C(fmt.Sprintf("%s  #%d", numberGlyph, pr.Number), gotermboard.Yellow)
}

// stackCell is the number of the open pull request this one is stacked on.
func stackCell(parent int) gotermboard.Cell {
	return gotermboard.C(fmt.Sprintf("%s #%d", stackGlyph, parent), gotermboard.Yellow)
}

// botCell names the app that opened the pull request.
func botCell(pr pullrequests.PullRequest) gotermboard.Cell {
	return gotermboard.C(botGlyph+"  bot "+pr.Author, gotermboard.Magenta)
}

func isBot(pr pullrequests.PullRequest) bool { return pr.Bot != nil && *pr.Bot }

// fieldGap is the space between two fields of a second line, wider than a field's own glyph
// and words are apart, so each reads as a field.
var fieldGap = gotermboard.C("   ", "")

// secondLine is a pull request's second line on the board: its checks under where it stands,
// then where it is checked out, its number and the open pull request it is stacked on, its
// review, its newest Claude review round, what was pushed since its newest review, and the
// app that opened it. A field a pull request does not have takes no room. The line is cut at
// the table's edge, so a narrow screen loses its end first.
func (b *Board) secondLine(at rowAt, parent int) gotermboard.Line {
	pr := at.pr
	line := gotermboard.Line{checksCell(pr)}
	line = append(line, whereCells(pr)...)
	line = append(line, fieldGap, numberCell(pr))
	if parent != 0 {
		line = append(line, gotermboard.C(" ", ""), stackCell(parent))
	}
	line = append(line, fieldGap, reviewCell(pr))
	if claude := claudeCell(b.reviewOf(pr, at.now)); claude.Text != "" {
		line = append(line, fieldGap, claude)
	}
	if since := sinceReviewCell(b.detailOf(pr)); since.Text != "" {
		line = append(line, fieldGap, since)
	}
	if isBot(pr) {
		line = append(line, fieldGap, botCell(pr))
	}
	return line
}

// sinceReviewCell is how many commits were pushed after the one the newest review read, or
// that a rebase took that commit off the branch, which i then opens the whole diff for. It is
// empty where the detail is not read, no review was submitted, or nothing was pushed since.
func sinceReviewCell(detail *pullrequests.DetailState) gotermboard.Cell {
	if detail == nil || !detail.Held {
		return gotermboard.C("", "")
	}
	switch since := detail.Detail.SinceReview(); {
	case !since.Known:
		return gotermboard.C("", "")
	case since.Gone:
		return gotermboard.C(sinceGlyph+"  rebased since review", gotermboard.Yellow)
	case since.Commits == 1:
		return gotermboard.C(sinceGlyph+"  1 commit since review", gotermboard.Yellow)
	case since.Commits > 1:
		return gotermboard.C(fmt.Sprintf("%s  %d commits since review", sinceGlyph, since.Commits), gotermboard.Yellow)
	}
	return gotermboard.C("", "")
}

// whereCells are where the pull request is checked out on this disk, or its repo where it is
// not cloned here, which d and the window keys refuse.
func whereCells(pr pullrequests.PullRequest) gotermboard.Line {
	if pr.Path == "" {
		return gotermboard.Line{gotermboard.C(pathGlyph+"  "+pr.Slug, gotermboard.Cyan), gotermboard.C("  not cloned here", dim)}
	}
	return gotermboard.Line{gotermboard.C(pathGlyph+"  "+homeAsTilde(pr.Path), gotermboard.Cyan)}
}

// homeAsTilde is path for a person to read, with the home directory written ~ where path sits
// inside it. The cut falls at a path segment, so a sibling of home keeps its whole path.
func homeAsTilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}

// sideMargin is the columns of margin at each side of the board from marginFrom columns
// wide. A narrower screen gives up a column a side for every two columns it lacks, rounded
// up, and keeps one.
const (
	sideMargin = 12
	marginFrom = 150
)

// Look is gotermboard.LookBoard's: a margin around the screen, the columns spread across the
// width, rules under the titles and above the keys, and the cursor's pull request bold on the
// selection. The margin gives way to the side panel, which gotermboard measures inside it, so
// a screen wide enough for the panel keeps it. Every row opens its document, so no row draws
// a chevron.
func (b *Board) Look(width int) gotermboard.Look {
	sides := min(sideMargin, max(sideMargin-(marginFrom-width+1)/2, 1))
	if width >= gotermboard.PanelFrom {
		sides = min(sides, (width-gotermboard.PanelFrom)/2)
	}
	return gotermboard.Look{Sides: sides, Ends: 1, Spread: true, Rules: true, BoldCursor: true, NoRowChevrons: true}
}

func orDash(text string) string {
	if text == "" {
		return "-"
	}
	return text
}

// document is the pull request's MarkdownDocument laid out at the width it is drawn at. The
// side panel heads it with the row's ReaderTitle, and the Reader and the row each name the
// title already. A row with a state whose detail was never read says why, which is the
// board's to say rather than the pull request's.
func (b *Board) document(pr pullrequests.PullRequest) gotermboard.Document {
	return func(width int) []gotermboard.Line {
		var lines []gotermboard.Line
		state := b.stateOf(pr)
		if detail := b.detailOf(pr); state != "" && detail != nil && !detail.Held {
			why := "It is " + string(state)
			if state == gone {
				why = "No read lists it any more"
			}
			lines = append(lines, gotermboard.Line{gotermboard.C(
				why+", so its "+detailSectionNames()+" are not read; o opens its page.", dim)}, gotermboard.Line{})
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

// documentFrom is every input of a MarkdownDocument. A listing read makes new rows, a review
// round's window opening or closing moves the Review section, and a minute passing moves each
// age the document writes.
type documentFrom struct {
	pr            pullrequests.PullRequest
	readsDetail   bool
	readAt        time.Time
	held, reading bool
	fault         string
	diff          diffFrom
	roundWindow   string
	minute        time.Time
}

// diffFrom is what of a DiffState moves the document: a new read, or a read starting,
// failing or found impossible.
type diffFrom struct {
	readAt        time.Time
	held, reading bool
	fault, cannot string
}

// markdownOf is the pull request's MarkdownDocument at now, built again only where its
// inputs moved since the last frame drew it.
func (b *Board) markdownOf(pr pullrequests.PullRequest, now time.Time) string {
	detail, diff, window := b.detailOf(pr), b.diffOf(pr), b.roundWindowOf(pr)
	from := documentFrom{
		pr: pr, minute: now.Truncate(time.Minute), roundWindow: window,
		diff: diffFrom{readAt: diff.Diff.ReadAt, held: diff.Held, reading: diff.Reading, fault: diff.Fault, cannot: diff.Cannot},
	}
	if detail != nil {
		from.readsDetail, from.readAt, from.held, from.reading, from.fault = true, detail.Detail.ReadAt, detail.Held, detail.Reading, detail.Fault
	}
	if memo, ok := b.documents[pr.Key()]; ok && memo.from == from {
		return memo.markdown
	}
	markdown := MarkdownDocument(pr, detail, &diff, window, now)
	b.documents[pr.Key()] = documentMemo{from: from, markdown: markdown}
	return markdown
}

// diffOf is how the pull request's diff stands for its document. A row drawn before its
// listing claims its read is due one, as in detailOf.
func (b *Board) diffOf(pr pullrequests.PullRequest) pullrequests.DiffState {
	if b.diffs == nil {
		return pullrequests.DiffState{}
	}
	state := b.diffs.Of(pr)
	if !state.Held && state.Fault == "" && state.Cannot == "" && b.stateOf(pr) != gone {
		state.Reading = true
	}
	return state
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
	// A listing claims its rows' reads once it lands, so a row drawn before that is due a read,
	// where readsDetail says its detail is read and it is not gone.
	if !state.Held && state.Fault == "" && b.readDetails && readsDetail(pr) && b.stateOf(pr) != gone {
		state.Reading = true
	}
	return &state
}
