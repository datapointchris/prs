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
// the board gives a meaning has one entry, which a row's cell and the description's
// detail both read.
type meaning struct {
	// word is the row's, after "checks" or "review". The description writes the state itself.
	word string
	code string
	// passed is a check that needs nothing from anyone, which the description leaves out.
	passed bool
}

// checkMeanings covers a rollup on a row and one check run or commit status in the
// description.
var checkMeanings = map[string]meaning{
	"SUCCESS":   {word: "passing", code: gotermboard.Green, passed: true},
	"NEUTRAL":   {word: "neutral", code: gotermboard.Green, passed: true},
	"SKIPPED":   {word: "skipped", code: dim, passed: true},
	"FAILURE":   {word: "failing", code: gotermboard.Red},
	"ERROR":     {word: "failing", code: gotermboard.Red},
	"TIMED_OUT": {word: "timed out", code: gotermboard.Red},
	"":          {word: "not run", code: dim},
}

// reviewMeanings covers a decision on a row and one submitted review in the description.
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
		WrapLast:    titleLine(pr, parentOf(pr, parents), b.settled[pr.Key()]),
		Under:       gotermboard.C(identity(pr), dim),
		Document:    b.description(pr, false),
		ReaderTitle: pr.Title,
		StartsOpen:  b.isOpenedOn(pr),
		Panel:       b.description(pr, true),
		Text:        strings.Join([]string{pr.Body, pr.Author, pr.Branch, pr.Slug}, " "),
	}
}

// settledState is the state a key's merge or close left a pull request in, which is also the
// word its row reads.
type settledState string

const (
	merged settledState = "merged"
	closed settledState = "closed"
)

// settledCodes color each settled state as GitHub does.
var settledCodes = map[settledState]string{merged: gotermboard.Magenta, closed: gotermboard.Red}

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

// titleLine is the state a merge or close left it in, the title, the open pull request it is
// stacked on, and the app that opened it. A pull request a person opened carries no mark, so
// the mark sets an app's row apart. A settled one dims its title, because it waits on nobody.
func titleLine(pr pullrequests.PullRequest, parent int, settled settledState) gotermboard.Line {
	line := gotermboard.Line{gotermboard.C(pr.Title, "")}
	switch {
	case settled != "":
		line = gotermboard.Line{gotermboard.C(string(settled), settledCodes[settled]), gotermboard.C("  "+pr.Title, dim)}
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

// description is what identifies the pull request, then its body as markdown. Enter opens it
// under the row or on a reader, each of which already shows the title, so only the side
// panel's copy carries the title too.
func (b *Board) description(pr pullrequests.PullRequest, titled bool) gotermboard.Document {
	return func(width int) []gotermboard.Line {
		var lines []gotermboard.Line
		if titled {
			lines = append(lines, gotermboard.Line{gotermboard.C(pr.Title, gotermboard.Bold)})
		}
		lines = append(lines, []gotermboard.Line{
			{gotermboard.C(fmt.Sprintf("%s #%d   opened by %s", pr.Slug, pr.Number, orDash(pr.Author)), dim)},
			{
				gotermboard.C(branchGlyph+" "+orDash(pr.Branch), gotermboard.Blue), gotermboard.C("  into  ", dim),
				gotermboard.C(orDash(pr.Base), gotermboard.Blue),
			},
			{},
		}...)
		if strings.TrimSpace(pr.Body) == "" {
			lines = append(lines, gotermboard.Line{gotermboard.C("No description.", dim)})
		} else {
			lines = append(lines, b.renderer.Lines(pr.Body, min(width, gotermboard.ReadingWidth))...)
		}
		return append(lines, b.detailLines(pr)...)
	}
}
