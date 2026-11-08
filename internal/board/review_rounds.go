package board

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/reviewrounds"
)

// claudeColumn is the newest review round's column. It sits after the review's, and draws
// only while a row has a round to show, so a board on repos no round reviews never has it.
var claudeColumn = gotermboard.Column{Title: "CLAUDE"}

// claudeAt is where claudeColumn sits among columns: after the state, the checks and the review.
const claudeAt = 3

// frameColumns is columns with the review round's column where a row fills it.
func frameColumns(withClaude bool) []gotermboard.Column {
	if !withClaude {
		return columns
	}
	return slices.Insert(slices.Clone(columns), claudeAt, claudeColumn)
}

// reviewStanding is where a pull request's review rounds stand on the board.
type reviewStanding struct {
	rounds reviewrounds.Rounds
	// window is the open tmux window running a round on it, where one is, and running says a
	// round runs, by that window or by a newest round whose round comment has not posted.
	window  string
	running bool
}

// reviewOf is where the pull request's review rounds stand now. A window that runs a round
// and is open says one is running until a round finishes after this board opened it. A
// window this board did not open says so only where no round has finished, because a
// session that ran a round keeps its window after the round comment posts.
func (b *Board) reviewOf(pr pullrequests.PullRequest) reviewStanding {
	standing := reviewStanding{rounds: b.roundsOf(pr)}
	if newest := standing.rounds.Newest; newest != nil && newest.Finish == nil {
		standing.running = true
	}
	name := b.roundWindowName(pr)
	if name == "" || !readsDetail(pr) {
		return standing
	}
	open, openedAt := b.roundWindows.open(name)
	finished := standing.rounds.FinishedAt()
	if open && (finished == nil || !openedAt.IsZero() && finished.Before(openedAt)) {
		standing.window, standing.running = name, true
	}
	return standing
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

// claudeCell is the newest round: its finding counts once its round comment posts, running
// while it posts or while a window runs one, or reviewed where the newest review is one prs
// cannot count. It is empty where no round reviewed the pull request.
func claudeCell(review reviewStanding) gotermboard.Cell {
	newest := review.rounds.Newest
	switch {
	case review.running:
		return gotermboard.C(claudeGlyph+"  claude running", gotermboard.Yellow)
	case review.rounds.WholeIsNewest():
		return gotermboard.C(claudeGlyph+"  claude reviewed", gotermboard.Cyan)
	case newest == nil:
		return gotermboard.C("", "")
	}
	counts := newest.Counts()
	if len(counts) == 0 {
		return gotermboard.C(claudeGlyph+"  claude no findings", gotermboard.Green)
	}
	code := gotermboard.Cyan
	if counts[0].Section == "correctness" {
		code = gotermboard.Red
	}
	return gotermboard.C(claudeGlyph+"  claude "+countsText(counts), code)
}

// countsText is each section's count as the reviews' own Findings line writes it.
func countsText(counts []reviewrounds.Count) string {
	parts := make([]string, len(counts))
	for i, count := range counts {
		parts[i] = fmt.Sprintf("%d %s", count.Findings, count.Section)
	}
	return strings.Join(parts, " · ")
}

// withClaude says a row of prs has a review round to show, so the frame draws its column.
func (b *Board) withClaude(prs []pullrequests.PullRequest) bool {
	return slices.ContainsFunc(prs, func(pr pullrequests.PullRequest) bool {
		return claudeCell(b.reviewOf(pr)).Text != ""
	})
}

// roundWindowName is the name of the tmux window a review round on the pull request runs in,
// from the first window action that runs one on its provider, and empty where none does.
func (b *Board) roundWindowName(pr pullrequests.PullRequest) string {
	for _, action := range b.reviewActions {
		if len(action.Providers) == 0 || slices.Contains(action.Providers, pr.Provider) {
			return config.Fill(action.Window, config.Fields{Number: pr.Number, Repo: pr.Repo, Slug: pr.Slug})
		}
	}
	return ""
}

// roundWindowsEvery is how often tmux is asked which windows are open. tmux answers from
// memory, so a short interval costs nothing a provider would charge for.
const roundWindowsEvery = 5 * time.Second

// roundWindows is which tmux windows are open, read every roundWindowsEvery, and when this
// board opened each one it opened that runs a review round.
type roundWindows struct {
	source *gotermboard.Source[map[string]bool]
	mu     sync.Mutex
	opened map[string]time.Time
}

// watchRoundWindows reads tmux's windows where a window action runs a review round and prs
// runs inside tmux, and is nil elsewhere.
func watchRoundWindows(actions []config.WindowAction) *roundWindows {
	if len(actions) == 0 || os.Getenv("TMUX") == "" || len(config.MissingPrograms([]string{"tmux"})) > 0 {
		return nil
	}
	w := &roundWindows{opened: map[string]time.Time{}}
	w.source = gotermboard.NewSource("tmux windows", roundWindowsEvery, w.read)
	return w
}

// read is every window name tmux holds, across its sessions. A window this board opened is
// closed once a read that began after it omits it, and its opening time goes with it.
func (w *roundWindows) read(ctx context.Context) (map[string]bool, string, error) {
	started := time.Now()
	out, err := pullrequests.Run(ctx, "", "tmux", "list-windows", "-a", "-F", "#{window_name}")
	if err != nil {
		return nil, "", fmt.Errorf("tmux could not list its windows: %w", err)
	}
	open := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		if name := strings.TrimRight(line, "\n"); name != "" {
			open[name] = true
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for name, at := range w.opened {
		if !open[name] && at.Before(started) {
			delete(w.opened, name)
		}
	}
	return open, "", nil
}

// openedHere records a window this board opened, which is open from now, and reads tmux
// again at once.
func (w *roundWindows) openedHere(name string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.opened[name] = time.Now()
	w.mu.Unlock()
	w.source.Force()
}

// open says the window is open, and when this board opened it, zero where it did not. A
// window opened after the last read began is open though that read could not list it.
func (w *roundWindows) open(name string) (bool, time.Time) {
	if w == nil {
		return false, time.Time{}
	}
	w.mu.Lock()
	at, here := w.opened[name]
	w.mu.Unlock()
	switch {
	case w.source.Value()[name]:
		return true, at
	case here && at.After(w.source.Status().TriedAt):
		return true, at
	}
	return false, time.Time{}
}

// reviewSection is where the newest review round stands, its synthesis, and its reviewers'
// findings a section to a heading, read from each review's JSON block. A line follows for each
// earlier round, then each round's post prs could not read, whole. A pull request no round
// reviewed has no Review section.
func reviewSection(in documentInput) string {
	d := in.detail
	if d == nil || !d.Held && in.roundWindow == "" {
		return ""
	}
	rounds := in.rounds
	newest := rounds.Newest
	if newest == nil && len(rounds.Whole) == 0 && in.roundWindow == "" {
		return ""
	}
	var parts []string
	if standing := roundStanding(rounds, in.roundWindow, in.now); standing != "" {
		parts = append(parts, standing)
	}
	if newest != nil {
		parts = append(parts, synthesisPart(newest.Synthesis)...)
		parts = append(parts, findingsPart(newest.Reviews)...)
		for _, review := range newest.Reviews {
			if review.Unread != "" {
				parts = append(parts, fmt.Sprintf("## %s, whole\n\n_Its findings could not be read, because %s._", review.Reviewer, review.Unread),
					nestedAt(strings.TrimSpace(review.Body), 3))
			}
		}
	}
	if len(rounds.Earlier) > 0 {
		lines := []string{"## Earlier rounds"}
		for _, round := range rounds.Earlier {
			lines = append(lines, "- "+earlierLine(round, in.now))
		}
		parts = append(parts, strings.Join(lines, "\n\n"))
	}
	for _, post := range rounds.Whole {
		heading := "## " + post.Marker
		if post.Reviewer != "" {
			heading = "## " + post.Reviewer + " · " + post.Marker
		}
		if post.At != nil {
			heading += " · " + gotermboard.Ago(in.now.Sub(*post.At)) + " ago"
		}
		parts = append(parts, heading, fmt.Sprintf("_%s, so it shows whole._", post.Why), nestedAt(strings.TrimSpace(post.Body), 3))
	}
	return strings.Join(parts, "\n\n")
}

// roundStanding is the newest round's opening lines: whether it runs or when it finished, the
// head it read, what its reviewers found, and who did not run. It is empty where no round
// posted and no window runs one.
func roundStanding(rounds reviewrounds.Rounds, window string, now time.Time) string {
	newest := rounds.Newest
	inWindow := ""
	if window != "" {
		inWindow = fmt.Sprintf(" in the tmux window %#q", window)
	}
	var lines []string
	switch {
	case newest == nil && window == "":
		return ""
	case newest == nil:
		lines = append(lines, fmt.Sprintf("A review round is running%s, and no reviewer has posted yet.", inWindow))
	case newest.Finish == nil:
		lines = append(lines, fmt.Sprintf("Round %d of %#q is running%s, and its round comment has not posted.", newest.Number, short(newest.Head), inWindow))
	case window != "":
		lines = append(lines, fmt.Sprintf("A new round is running%s, and no reviewer has posted yet. Round %d of %#q, below, finished %s ago.",
			inWindow, newest.Number, short(newest.Head), gotermboard.Ago(now.Sub(newest.Finish.At))))
	default:
		lines = append(lines, fmt.Sprintf("Round %d of %#q finished %s ago.", newest.Number, short(newest.Head), gotermboard.Ago(now.Sub(newest.Finish.At))))
	}
	if newest == nil {
		return strings.Join(lines, " ")
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
		if newest.Finish == nil {
			sofar = " so far"
		}
		lines = append(lines, fmt.Sprintf("%s raised %s%s: %s.", joined(found, ""), counted(total, "finding"), sofar, countsText(newest.Counts())))
	}
	if len(nothing) > 0 {
		lines = append(lines, joined(nothing, "")+" found nothing.")
	}
	if finish := newest.Finish; finish != nil {
		for _, absent := range finish.Absent {
			lines = append(lines, fmt.Sprintf("%s did not run: %s.", absent.Label, strings.TrimSuffix(absent.Why, ".")))
		}
		if finish.Unread != "" {
			lines = append(lines, fmt.Sprintf("Its round record could not be read, because %s.", finish.Unread))
		}
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
		parts = append(parts, fmt.Sprintf("### %s · %s", shape.Number, sectionHeading(shape.Section)))
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
	slices.SortStableFunc(order, func(a, b string) int { return sectionRank(a) - sectionRank(b) })
	var parts []string
	for _, section := range order {
		parts = append(parts, "## "+sectionHeading(section))
		for _, finding := range by[section] {
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
func earlierLine(round reviewrounds.Round, now time.Time) string {
	line := fmt.Sprintf("Round %d of %#q", round.Number, short(round.Head))
	if round.Finish == nil {
		line += " never posted its round comment"
	} else {
		line += " finished " + gotermboard.Ago(now.Sub(round.Finish.At)) + " ago"
	}
	if counts := round.Counts(); len(counts) > 0 {
		return line + ": " + countsText(counts) + "."
	}
	return line + ", with no findings."
}

// sectionHeading is a finding section as a review heads it.
func sectionHeading(section string) string {
	switch section {
	case "correctness":
		return "Correctness"
	case "design":
		return "Design and architecture"
	case "prose":
		return "Prose"
	}
	heading := words(section)
	if heading == "" {
		return "No section"
	}
	return strings.ToUpper(heading[:1]) + heading[1:]
}

// sectionRank orders a section as reviewrounds.Sections does, any other after them.
func sectionRank(section string) int {
	if at := slices.Index(reviewrounds.Sections, section); at >= 0 {
		return at
	}
	return len(reviewrounds.Sections)
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

// shownInReview is the body of every post the Review section shows, which Comments leaves
// out so no post reads twice: the newest round's, and each one prs could not read.
func shownInReview(rounds reviewrounds.Rounds) map[string]bool {
	shown := map[string]bool{}
	if newest := rounds.Newest; newest != nil {
		for _, review := range newest.Reviews {
			shown[review.Body] = true
		}
		if newest.Synthesis != nil {
			shown[newest.Synthesis.Body] = true
		}
		if newest.Finish != nil {
			shown[newest.Finish.Body] = true
		}
	}
	for _, post := range rounds.Whole {
		shown[post.Body] = true
	}
	return shown
}
