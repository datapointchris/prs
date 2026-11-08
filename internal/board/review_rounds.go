package board

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/reviewrounds"
)

// roundPostsWithin is how long after a round's newest post, with no round comment yet, the
// round still reads as running. A reviewer posts once it ends, and the slowest ends well
// inside it, so a round quiet for longer stopped before its round comment posted.
const roundPostsWithin = time.Hour

// roundState is where the newest review round stands.
type roundState int

const (
	// noRound is a pull request no round posted on and no window runs one on.
	noRound roundState = iota
	// roundRunning is a round a window runs, or one still posting.
	roundRunning
	// roundUnfinished is a round whose posts stopped before its round comment posted.
	roundUnfinished
	// roundUnconfirmed is a round with no round comment among the comments the detail read
	// holds, which leave out the oldest, so it may have finished.
	roundUnconfirmed
	roundFinished
)

// roundStateOf is where the newest round stands, given the window running one, if any.
func roundStateOf(rounds reviewrounds.Rounds, window string, now time.Time) roundState {
	newest := rounds.Newest
	switch {
	case window != "":
		return roundRunning
	case newest == nil:
		return noRound
	case newest.Finish != nil:
		return roundFinished
	case now.Sub(newest.PostedAt()) < roundPostsWithin:
		return roundRunning
	case rounds.CommentsCut:
		return roundUnconfirmed
	}
	return roundUnfinished
}

// reviewStanding is where a pull request's review rounds stand on the board.
type reviewStanding struct {
	rounds reviewrounds.Rounds
	// window is the open tmux window running a round on it, where one is.
	window string
	state  roundState
}

// reviewOf is where the pull request's review rounds stand now.
func (b *Board) reviewOf(pr pullrequests.PullRequest, now time.Time) reviewStanding {
	rounds, window := b.roundsOf(pr), b.roundWindowOf(pr)
	return reviewStanding{rounds: rounds, window: window, state: roundStateOf(rounds, window, now)}
}

// roundWindowOf is the open tmux window running a review round on the pull request, and empty
// where none is. A window this board opened runs one until a round finishes after it opened.
// One it did not open runs one only where no round has finished, because a session that ran a
// round keeps its window after the round comment posts.
func (b *Board) roundWindowOf(pr pullrequests.PullRequest) string {
	name := b.roundWindowName(pr)
	if name == "" || !readsDetail(pr) {
		return ""
	}
	open, openedAt := b.windows.open(name)
	finished := b.roundsOf(pr).FinishedAt()
	if open && (finished == nil || !openedAt.IsZero() && finished.Before(openedAt)) {
		return name
	}
	return ""
}

// roundsMemo is a pull request's review rounds and the detail read they came from. Every
// frame draws the cell, and reading a round decodes each review's JSON block.
type roundsMemo struct {
	readAt time.Time
	rounds reviewrounds.Rounds
}

// roundsOf is the review rounds the pull request's held detail carries, read again only when
// another detail read lands.
func (b *Board) roundsOf(pr pullrequests.PullRequest) reviewrounds.Rounds {
	detail := b.detailOf(pr)
	if detail == nil || !detail.Held {
		return reviewrounds.Rounds{}
	}
	if memo, ok := b.rounds[pr.Key()]; ok && memo.readAt.Equal(detail.Detail.ReadAt) {
		return memo.rounds
	}
	rounds := reviewrounds.Read(detail.Detail)
	b.rounds[pr.Key()] = roundsMemo{readAt: detail.Detail.ReadAt, rounds: rounds}
	return rounds
}

// claudeCell is the newest round: running, stopped short of its round comment, run by none of
// its reviewers, or its finding counts, with how many reviewers did not run and how many
// reviews could not be read. It reads reviewed where the newest review is one prs cannot
// count, and is empty where no round reviewed the pull request.
func claudeCell(review reviewStanding) gotermboard.Cell {
	newest := review.rounds.Newest
	switch {
	case review.state == roundRunning:
		return gotermboard.C(claudeGlyph+"  claude running", gotermboard.Yellow)
	case review.rounds.WholeIsNewest():
		return gotermboard.C(claudeGlyph+"  claude reviewed", gotermboard.Cyan)
	case newest == nil:
		return gotermboard.C("", "")
	case review.state == roundUnfinished:
		return gotermboard.C(claudeGlyph+"  claude unfinished", gotermboard.Yellow)
	case newest.NoneRan():
		return gotermboard.C(claudeGlyph+"  claude did not run", gotermboard.Yellow)
	}
	counts, gaps := newest.Counts(), roundGaps(*newest)
	code := gotermboard.Green
	switch {
	case len(counts) > 0 && counts[0].Section.Blocking:
		code = gotermboard.Red
	case len(gaps) > 0:
		code = gotermboard.Yellow
	case len(counts) > 0:
		code = gotermboard.Cyan
	}
	return gotermboard.C(claudeGlyph+"  claude "+roundText(*newest), code)
}

// roundText is the round's finding counts, or no findings, then its gaps.
func roundText(round reviewrounds.Round) string {
	parts := []string{"no findings"}
	if counts := round.Counts(); len(counts) > 0 {
		parts = []string{countsText(counts)}
	}
	return strings.Join(append(parts, roundGaps(round)...), " · ")
}

// roundGaps is how many of the round's reviewers did not run and how many of its reviews could
// not be read, where any, so a count missing their findings says so.
func roundGaps(round reviewrounds.Round) []string {
	var gaps []string
	if missed := len(round.Missed()); missed > 0 {
		gaps = append(gaps, fmt.Sprintf("%d not run", missed))
	}
	if unread := len(round.Unread()); unread > 0 {
		gaps = append(gaps, fmt.Sprintf("%d unread", unread))
	}
	return gaps
}

// countsText is each section's count as the reviews' own Findings line writes it.
func countsText(counts []reviewrounds.Count) string {
	parts := make([]string, len(counts))
	for i, count := range counts {
		parts[i] = fmt.Sprintf("%d %s", count.Findings, count.Section.Name)
	}
	return strings.Join(parts, " · ")
}

// roundWindowName is the name of the tmux window a review round on the pull request runs in,
// from the first window action that runs one on its provider, and empty where none does.
func (b *Board) roundWindowName(pr pullrequests.PullRequest) string {
	for _, action := range b.reviewActions {
		if name, _, err := windowOn(action, pr); err == nil {
			return name
		}
	}
	return ""
}

// reviewShows is every post the Review section shows, which Comments leaves out so no post
// reads twice: the newest round's, and each post no round took.
type reviewShows struct {
	newest *reviewrounds.Round
	whole  []reviewrounds.Post
}

func reviewShowsOf(rounds reviewrounds.Rounds) reviewShows {
	return reviewShows{newest: rounds.Newest, whole: rounds.Whole}
}

// bodies is the body of each post it holds.
func (s reviewShows) bodies() map[string]bool {
	shown := map[string]bool{}
	if s.newest != nil {
		for _, body := range s.newest.Bodies() {
			shown[body] = true
		}
	}
	for _, post := range s.whole {
		shown[post.Body] = true
	}
	return shown
}

// reviewSection is where the newest review round stands, its synthesis, and its reviewers'
// findings a section to a heading, read from each review's JSON block. A line follows for each
// earlier round, then each post no round took, whole. A pull request no round reviewed has no
// Review section unless a window running a round is open, which shows one before the detail
// read lands.
func reviewSection(in documentInput) string {
	d := in.detail
	if d == nil || !d.Held && in.roundWindow == "" {
		return ""
	}
	rounds, shows := in.rounds, reviewShowsOf(in.rounds)
	if shows.newest == nil && len(shows.whole) == 0 && in.roundWindow == "" {
		return ""
	}
	var parts []string
	if standing := roundStanding(rounds, in.roundWindow, in.now); standing != "" {
		parts = append(parts, standing)
	}
	if newest := shows.newest; newest != nil {
		parts = append(parts, synthesisPart(newest.Synthesis)...)
		parts = append(parts, findingsPart(newest.Reviews)...)
		for _, review := range newest.Unread() {
			parts = append(parts, fmt.Sprintf("## %s, whole\n\n_Its findings could not be read, because %s._", review.Reviewer, review.Unread),
				nestedAt(strings.TrimSpace(review.Body), 3))
		}
	}
	if len(rounds.Earlier) > 0 {
		lines := []string{"## Earlier rounds"}
		for _, round := range rounds.Earlier {
			lines = append(lines, "- "+earlierLine(round, rounds.CommentsCut, in.now))
		}
		parts = append(parts, strings.Join(lines, "\n\n"))
	}
	for _, post := range shows.whole {
		heading := "## " + post.Marker
		if post.Reviewer != "" {
			heading = "## " + post.Reviewer + " · " + post.Marker
		}
		if post.Author != "" {
			heading += " · " + post.Author
		}
		if post.At != nil {
			heading += " · " + gotermboard.Ago(in.now.Sub(*post.At)) + " ago"
		}
		parts = append(parts, heading, fmt.Sprintf("_%s, so it shows whole._", post.Why), nestedAt(strings.TrimSpace(post.Body), 3))
	}
	return strings.Join(parts, "\n\n")
}

// roundStanding is the newest round's opening lines: where it stands, the head it read, what
// its reviewers found, and who skipped or did not run. It is empty where no round posted and
// no window runs one.
func roundStanding(rounds reviewrounds.Rounds, window string, now time.Time) string {
	newest := rounds.Newest
	state := roundStateOf(rounds, window, now)
	inWindow := ""
	if window != "" {
		inWindow = fmt.Sprintf(" in the tmux window %#q", window)
	}
	var lines []string
	switch {
	case state == noRound:
		return ""
	case newest == nil:
		return fmt.Sprintf("A review round is running%s, and no reviewer has posted yet.", inWindow)
	case state == roundRunning && newest.Finish == nil:
		lines = append(lines, fmt.Sprintf("Round %d of %#q is running%s, and its round comment has not posted.", newest.Number, short(newest.Head), inWindow))
	case state == roundRunning:
		lines = append(lines, fmt.Sprintf("A new round is running%s, and no reviewer has posted yet. Round %d of %#q, below, finished %s ago.",
			inWindow, newest.Number, short(newest.Head), gotermboard.Ago(now.Sub(newest.Finish.At))))
	case state == roundUnfinished:
		lines = append(lines, fmt.Sprintf("Round %d of %#q stopped %s ago, before its round comment posted. Running a review again continues it.",
			newest.Number, short(newest.Head), gotermboard.Ago(now.Sub(newest.PostedAt()))))
	case state == roundUnconfirmed:
		lines = append(lines, fmt.Sprintf("Round %d of %#q may have finished. The detail read holds only the newest comments, and its round comment is not among them.",
			newest.Number, short(newest.Head)))
	default:
		lines = append(lines, fmt.Sprintf("Round %d of %#q finished %s ago.", newest.Number, short(newest.Head), gotermboard.Ago(now.Sub(newest.Finish.At))))
	}
	var found, nothing []string
	total := 0
	for _, review := range newest.Reviews {
		switch {
		case review.Unread != "":
		case len(review.Findings) == 0:
			nothing = append(nothing, review.Reviewer)
		default:
			found = append(found, review.Reviewer)
			total += len(review.Findings)
		}
	}
	if total > 0 {
		sofar := ""
		if state == roundRunning {
			sofar = " so far"
		}
		lines = append(lines, fmt.Sprintf("%s raised %s%s: %s.", joined(found, ""), counted(total, "finding"), sofar, countsText(newest.Counts())))
	}
	if len(nothing) > 0 {
		lines = append(lines, joined(nothing, "")+" found nothing.")
	}
	for _, outcome := range newest.Skipped() {
		lines = append(lines, fmt.Sprintf("%s skipped: %s.", outcome.Label, strings.TrimSuffix(outcome.Why, ".")))
	}
	for _, outcome := range newest.Missed() {
		lines = append(lines, fmt.Sprintf("%s did not run: %s.", outcome.Label, strings.TrimSuffix(outcome.Why, ".")))
	}
	if finish := newest.Finish; finish != nil && finish.Unread != "" {
		lines = append(lines, fmt.Sprintf("Its round record could not be read, because %s.", finish.Unread))
	}
	return strings.Join(lines, " ")
}

// synthesisPart is the synthesis's shapes, each under a heading of its own, then the findings
// that describe one defect. One whose block could not be read shows whole.
func synthesisPart(synthesis *reviewrounds.Synthesis) []string {
	if synthesis == nil {
		return nil
	}
	parts := []string{"## Synthesis"}
	if synthesis.Unread != "" {
		return append(parts, fmt.Sprintf("_Its shapes could not be read, because %s, so it shows whole._", synthesis.Unread),
			nestedAt(strings.TrimSpace(synthesis.Body), 3))
	}
	if len(synthesis.Shapes) == 0 && len(synthesis.Duplicates) == 0 {
		return append(parts, "No cause is shared by more than one finding.")
	}
	for _, shape := range synthesis.Shapes {
		section, _ := reviewrounds.SectionOf(shape.Section)
		parts = append(parts, fmt.Sprintf("### %s · %s", shape.Number, section.Heading))
		if len(shape.Explains) > 0 {
			parts = append(parts, "Explains "+codeList(shape.Explains)+".")
		}
		parts = append(parts, nestedAt(strings.TrimSpace(shape.Body), 4))
	}
	if len(synthesis.Duplicates) > 0 {
		lines := []string{"### One defect, raised more than once"}
		for _, duplicate := range synthesis.Duplicates {
			lines = append(lines, fmt.Sprintf("- %s: %s", codeList(duplicate.Findings), duplicate.Defect))
		}
		parts = append(parts, strings.Join(lines, "\n\n"))
	}
	return parts
}

// findingsPart is every reviewer's findings under a heading for their section, in the order
// a review writes its sections, each finding under a heading naming its number and first site.
func findingsPart(reviews []reviewrounds.Review) []string {
	by := map[string][]reviewrounds.Finding{}
	var order []string
	for _, review := range reviews {
		for _, finding := range review.Findings {
			if _, seen := by[finding.Section]; !seen {
				order = append(order, finding.Section)
			}
			by[finding.Section] = append(by[finding.Section], finding)
		}
	}
	slices.SortStableFunc(order, func(a, b string) int {
		_, at := reviewrounds.SectionOf(a)
		_, bt := reviewrounds.SectionOf(b)
		return at - bt
	})
	var parts []string
	for _, name := range order {
		section, _ := reviewrounds.SectionOf(name)
		parts = append(parts, "## "+section.Heading)
		for _, finding := range by[name] {
			heading := "### " + finding.Number
			if len(finding.Sites) > 0 {
				heading += fmt.Sprintf(" · %#q", finding.Sites[0])
			}
			parts = append(parts, heading)
			if len(finding.Sites) > 1 {
				parts = append(parts, "Also "+codeList(finding.Sites[1:])+".")
			}
			if reach := strings.TrimSpace(finding.Reach); reach != "" {
				parts = append(parts, "Reach: "+reach)
			}
			parts = append(parts, nestedAt(strings.TrimSpace(finding.Body), 4))
		}
	}
	return parts
}

// earlierLine is one earlier round: the head it read, when it finished, and what it found.
// Where the detail read holds only the newest comments, a round with no round comment among
// them is not said never to have posted one.
func earlierLine(round reviewrounds.Round, commentsCut bool, now time.Time) string {
	line := fmt.Sprintf("Round %d of %#q", round.Number, short(round.Head))
	switch {
	case round.Finish != nil:
		line += " finished " + gotermboard.Ago(now.Sub(round.Finish.At)) + " ago"
	case commentsCut:
		line += " has no round comment among the newest comments the detail read holds"
	default:
		line += " never posted its round comment"
	}
	switch {
	case round.NoneRan():
		return line + ", and none of its reviewers ran."
	case len(round.Counts()) > 0 || len(roundGaps(round)) > 0:
		return line + ": " + roundText(round) + "."
	}
	return line + ", with no findings."
}

// codeList is each name as code, joined by a middle dot.
func codeList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%#q", name)
	}
	return strings.Join(quoted, " · ")
}

// joined is names as prose joins a list, or none where there are none.
func joined(names []string, none string) string {
	switch len(names) {
	case 0:
		return none
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
