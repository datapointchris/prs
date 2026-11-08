package board

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/datapointchris/gotermboard"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// documentInput is what each section of a pull request's document is written from. detail
// is how its detail read stands, and nil where its host reads no detail. diff is how its
// diff read stands, and nil where the reader has the diff in front of them already.
type documentInput struct {
	pr     pullrequests.PullRequest
	detail *pullrequests.DetailState
	diff   *pullrequests.DiffState
	now    time.Time
}

// documentSection is one level-1 heading of the document and what goes under it. A section
// whose write returns nothing is left out, heading and all. fromDetail says the pull
// request's own detail read fills it, rather than its listing.
type documentSection struct {
	heading    string
	fromDetail bool
	write      func(documentInput) string
}

// documentSections are the document's headings in the order they draw. A section added here
// reaches every place MarkdownDocument is drawn, and every sentence that names the sections.
var documentSections = []documentSection{
	{heading: "Description", write: describe},
	{heading: "Checks", fromDetail: true, write: checkSection},
	{heading: "Files", fromDetail: true, write: fileSection},
	{heading: "Diff", write: diffSection},
	{heading: "Comments", fromDetail: true, write: commentSection},
}

// DocumentSectionNames is every section's heading, lowercased and joined as prose joins a list.
func DocumentSectionNames() string { return sectionNames(func(documentSection) bool { return true }) }

// detailSectionNames is the heading of each section the detail read fills, joined the same way.
func detailSectionNames() string {
	return sectionNames(func(section documentSection) bool { return section.fromDetail })
}

func sectionNames(keep func(documentSection) bool) string {
	var names []string
	for _, section := range documentSections {
		if keep(section) {
			names = append(names, strings.ToLower(section.heading))
		}
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// MarkdownDocument is the pull request as one markdown document: a line naming it, then a
// level-1 heading per section. Description is the listing's body, so it is whole at once.
// The sections the detail read fills say they are reading until it lands, and so does Diff
// until its own read does. A nil detail is a host that reads none, whose document has no
// detail section, and a nil diff leaves Diff out, as beside d's diff in nvim. now is what
// each age counts from. The title is not in it, since every place it shows names the title
// already.
func MarkdownDocument(pr pullrequests.PullRequest, detail *pullrequests.DetailState, diff *pullrequests.DiffState, now time.Time) string {
	in := documentInput{pr: pr, detail: detail, diff: diff, now: now}
	parts := []string{preamble(in)}
	for _, section := range documentSections {
		if body := section.write(in); body != "" {
			parts = append(parts, "# "+section.heading+"\n\n"+body)
		}
	}
	return strings.Join(parts, "\n\n") + "\n"
}

// preamble names the repo, the number, who opened it and the branches, and says where a
// newer detail read failed while an older one still shows.
func preamble(in documentInput) string {
	pr := in.pr
	line := fmt.Sprintf("%#q #%d · opened by %s · %#q into %#q", pr.Slug, pr.Number, orDash(pr.Author), orDash(pr.Branch), orDash(pr.Base))
	if pr.Draft {
		line += " · draft"
	}
	if d := in.detail; d != nil && d.Held && d.Fault != "" {
		line += fmt.Sprintf("\n\n_Its %s were read %s ago, and the newest read failed: %s_",
			detailSectionNames(), gotermboard.Ago(in.now.Sub(d.Detail.ReadAt)), d.Fault)
	}
	return line
}

func describe(in documentInput) string {
	if strings.TrimSpace(in.pr.Body) == "" {
		return "_No description._"
	}
	return nestedAt(strings.TrimSpace(in.pr.Body), 2)
}

// unread is what a detail section says before its read lands, and empty once it has.
func unread(d *pullrequests.DetailState) string {
	switch {
	case d.Held:
		return ""
	case d.Fault != "":
		return "_Could not read them: " + d.Fault + "_"
	case d.Reading:
		return "_Reading._"
	}
	return "_Not read._"
}

// checkSection is each check on the head commit and its state, those not passing first,
// and the listing's overall state until they are read.
func checkSection(in documentInput) string {
	d := in.detail
	if d == nil {
		return ""
	}
	if !d.Held {
		return "Overall " + checkMeaning(in.pr.Checks).word + ".\n\n" + unread(d)
	}
	checks := slices.Clone(d.Detail.Checks)
	if len(checks) == 0 {
		return "No check ran on its head commit."
	}
	slices.SortStableFunc(checks, func(a, b pullrequests.Check) int {
		return cmp.Compare(boolRank(checkMeaning(a.State).passed), boolRank(checkMeaning(b.State).passed))
	})
	failing := 0
	rows := []string{"| State | Check |", "| --- | --- |"}
	for _, check := range checks {
		state := checkMeaning(check.State)
		if !state.passed {
			failing++
		}
		rows = append(rows, fmt.Sprintf("| %s | %s |", state.word, cell(check.Name)))
	}
	summary := fmt.Sprintf("%d of %d not passing.", failing, len(checks))
	if failing == 0 {
		summary = fmt.Sprintf("All %d passing.", len(checks))
	}
	return summary + "\n\n" + strings.Join(rows, "\n")
}

// fileSection is each changed path with its counts, under the totals the listing read.
func fileSection(in documentInput) string {
	d := in.detail
	if d == nil {
		return ""
	}
	pr := in.pr
	total := ""
	if pr.ChangedFiles != nil && pr.Additions != nil && pr.Deletions != nil {
		total = fmt.Sprintf("%s, %s added and %d removed.", counted(*pr.ChangedFiles, "file"), counted(*pr.Additions, "line"), *pr.Deletions)
	}
	if !d.Held {
		return strings.TrimSpace(total + "\n\n" + unread(d))
	}
	files := d.Detail.Files
	if len(files) == 0 {
		return "No file changed."
	}
	rows := []string{"| File | Change | Added | Removed |", "| --- | --- | ---: | ---: |"}
	for _, file := range files {
		rows = append(rows, fmt.Sprintf("| %#q | %s | %d | %d |", cell(file.Path), words(file.Change), file.Additions, file.Deletions))
	}
	parts := []string{total, strings.Join(rows, "\n")}
	if more := d.Detail.FileCount - len(files); more > 0 {
		var where string
		switch {
		case in.diff == nil:
			where = "the diff beside this shows every one."
		case in.diff.Held && in.diff.Diff.Whole:
			where = "the Diff section below holds every one."
		default:
			where = opener(*in.diff) + "."
		}
		parts = append(parts, fmt.Sprintf("%d more files are left out here, and %s", more, where))
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// diffSection is each changed file under a level-2 heading of its own, its hunks in a diff
// fence, so the Reader's section keys move between files. A diff past the limit is its size
// alone, and the key that opens it whole.
func diffSection(in documentInput) string {
	if in.diff == nil {
		return ""
	}
	state := *in.diff
	switch {
	case state.Cannot != "":
		return "_" + state.Cannot + "_"
	case !state.Held && state.Fault != "":
		return "_Could not read it: " + state.Fault + "_"
	case !state.Held && state.Reading:
		return "_Reading._"
	case !state.Held:
		return "_Not read._"
	}
	diff := state.Diff
	var parts []string
	if state.Fault != "" {
		parts = append(parts, fmt.Sprintf("_Read %s ago, and the newest read failed: %s_", gotermboard.Ago(in.now.Sub(diff.ReadAt)), state.Fault))
	}
	if !diff.Whole {
		return strings.Join(append(parts, fmt.Sprintf("%s changed, more than the %d this section shows whole. %s.",
			counted(diff.Lines, "line"), state.Limit, opener(state))), "\n\n")
	}
	if len(diff.Files) == 0 {
		return strings.Join(append(parts, "No line changed."), "\n\n")
	}
	source := "read from " + in.pr.Provider
	if state.InCheckout {
		source = "read in the checkout here"
	}
	at := ""
	if diff.Head != "" {
		at = fmt.Sprintf(" at %#q", short(diff.Head))
	}
	parts = append(parts, fmt.Sprintf("%s changed in %s%s, %s.", counted(diff.Lines, "line"), counted(len(diff.Files), "file"), at, source))
	if diff.BaseFault != "" {
		parts = append(parts, fmt.Sprintf("_It runs against the `origin/%s` fetched earlier, because fetching it again failed: %s_", in.pr.Base, diff.BaseFault))
	}
	for _, file := range diff.Files {
		parts = append(parts, "## "+fileHeading(file))
		switch {
		case file.Binary:
			parts = append(parts, "_A binary file, which a diff does not show._")
		case strings.TrimSpace(file.Hunks) == "":
			parts = append(parts, "_Its lines are unchanged._")
		default:
			parts = append(parts, fenced("diff", file.Hunks))
		}
	}
	return strings.Join(parts, "\n\n")
}

// opener is the key that opens the whole diff: d needs the checkout, and o does not.
func opener(state pullrequests.DiffState) string {
	if state.InCheckout {
		return "d opens the whole diff in nvim"
	}
	return "o opens it in the browser"
}

// fileHeading is the file's path, where it came from when it moved, and how it changed
// where it was more than modified.
func fileHeading(file pullrequests.FileDiff) string {
	heading := fmt.Sprintf("%#q", file.Path)
	if file.OldPath != "" && file.OldPath != file.Path {
		heading = fmt.Sprintf("%#q → %#q", file.OldPath, file.Path)
	}
	if file.Change != "modified" {
		heading += " · " + file.Change
	}
	return heading
}

// fenced is body in a fence of language one backtick longer than the longest run in body,
// so no line of it closes the fence early.
func fenced(language, body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r != '`' {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + language + "\n" + strings.TrimRight(body, "\n") + "\n" + fence
}

// counted is n and the noun, plural where n is not 1.
func counted(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// commentSection is who is still asked to review, the conversation oldest first with each
// submitted review in it, then each review thread, unresolved first.
func commentSection(in documentInput) string {
	d := in.detail
	if d == nil {
		return ""
	}
	if !d.Held {
		return unread(d)
	}
	detail := d.Detail
	var parts []string
	if len(detail.ReviewRequests) > 0 {
		parts = append(parts, "Asked to review: "+strings.Join(detail.ReviewRequests, ", ")+".")
	}
	if conversation := conversationOf(detail, in.now); conversation != "" {
		parts = append(parts, "## Conversation\n\n"+conversation)
	}
	if threads := threadsOf(detail, in.now); threads != "" {
		parts = append(parts, "## Review threads\n\n"+threads)
	}
	if len(parts) == 0 {
		return "No comments yet."
	}
	return strings.Join(parts, "\n\n")
}

// said is one comment or review in the conversation.
type said struct {
	at      *time.Time
	heading string
	body    string
}

// conversationOf is each timeline comment and each submitted review, oldest first, each
// under a level-3 heading naming who said it and when. A review that only commented and
// says nothing itself is left out, since its comments are in the threads.
func conversationOf(detail pullrequests.Detail, now time.Time) string {
	var entries []said
	for _, comment := range detail.Comments {
		at := comment.CreatedAt
		entries = append(entries, said{at: &at, heading: orDash(comment.Author) + " commented", body: comment.Body})
	}
	for _, review := range detail.Reviews {
		if review.State == "COMMENTED" && strings.TrimSpace(review.Body) == "" {
			continue
		}
		heading := fmt.Sprintf("%s %s at %s", orDash(review.Author), words(review.State), short(review.CommitOID))
		entries = append(entries, said{at: review.SubmittedAt, heading: heading, body: review.Body})
	}
	// A review still pending has no time, and goes last.
	slices.SortStableFunc(entries, func(a, b said) int {
		if a.at == nil || b.at == nil {
			return cmp.Compare(boolRank(a.at == nil), boolRank(b.at == nil))
		}
		return a.at.Compare(*b.at)
	})
	var parts []string
	for _, entry := range entries {
		heading := "### " + entry.heading
		if entry.at != nil {
			heading += " · " + gotermboard.Ago(now.Sub(*entry.at)) + " ago"
		}
		parts = append(parts, heading)
		if body := strings.TrimSpace(entry.body); body != "" {
			parts = append(parts, nestedAt(body, 4))
		}
	}
	if more := detail.CommentCount - len(detail.Comments); more > 0 {
		parts = append(parts, fmt.Sprintf("_%d older comments are left out here, and o opens them in the browser._", more))
	}
	return strings.Join(parts, "\n\n")
}

// threadsOf is each review thread under a level-3 heading naming its file and line, with
// its comments oldest first.
func threadsOf(detail pullrequests.Detail, now time.Time) string {
	if len(detail.Threads) == 0 {
		return ""
	}
	threads := slices.Clone(detail.Threads)
	slices.SortStableFunc(threads, func(a, b pullrequests.Thread) int {
		return cmp.Compare(boolRank(a.Resolved), boolRank(b.Resolved))
	})
	unresolved := 0
	for _, thread := range threads {
		if !thread.Resolved {
			unresolved++
		}
	}
	parts := []string{fmt.Sprintf("%d of %d unresolved.", unresolved, len(threads))}
	for _, thread := range threads {
		where := "`" + thread.Path + "`, on a line gone from the diff"
		if thread.Line != nil {
			where = "`" + thread.Path + ":" + strconv.Itoa(*thread.Line) + "`"
		}
		state := "unresolved"
		if thread.Resolved {
			state = "resolved"
		}
		if thread.Outdated {
			state += ", outdated"
		}
		parts = append(parts, "### "+where+" · "+state)
		for _, comment := range thread.Comments {
			parts = append(parts, fmt.Sprintf("**%s** · %s ago", orDash(comment.Author), gotermboard.Ago(now.Sub(comment.CreatedAt))))
			if body := strings.TrimSpace(comment.Body); body != "" {
				parts = append(parts, nestedAt(body, 4))
			}
		}
		if more := thread.CommentCount - len(thread.Comments); more > 0 {
			parts = append(parts, fmt.Sprintf("_%d later replies are left out here._", more))
		}
	}
	if more := detail.ThreadCount - len(detail.Threads); more > 0 {
		parts = append(parts, fmt.Sprintf("_%d older threads are left out here, and o opens them in the browser._", more))
	}
	return strings.Join(parts, "\n\n")
}

// boolRank sorts false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// cell is text safe in a table cell, whose pipe would otherwise end it.
func cell(text string) string { return strings.ReplaceAll(text, "|", `\|`) }

// short is a commit's first seven characters, as git abbreviates one.
func short(oid string) string {
	if oid == "" {
		return "a commit gone from the branch"
	}
	return oid[:min(len(oid), 7)]
}

var headingParser = goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser()

// nestedAt is source with every heading moved deeper by one amount, so its shallowest sits
// at depth. Text written under a section keeps its own headings inside that section rather
// than closing it, which is what the Reader's path and its section jumps read. A heading
// goes no deeper than 6, markdown's deepest.
func nestedAt(source string, depth int) string {
	src := []byte(source)
	var headings []*ast.Heading
	_ = ast.Walk(headingParser.Parse(text.NewReader(src)), func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if h, ok := n.(*ast.Heading); ok && entering && h.Pos() >= 0 {
			headings = append(headings, h)
		}
		return ast.WalkContinue, nil
	})
	shallowest := 6
	for _, h := range headings {
		shallowest = min(shallowest, h.Level)
	}
	shift := depth - shallowest
	if len(headings) == 0 || shift <= 0 {
		return source
	}
	// Each edit is made from the end back, so the offsets of those before it still hold.
	for _, h := range slices.Backward(headings) {
		src = deepened(src, h, min(h.Level+shift, 6))
	}
	return string(src)
}

// deepened writes heading h at level. goldmark puts an ATX heading's position on its run of
// #, which grows, empty heading or not, and a setext heading's on its first word. A setext
// heading becomes one ATX line, its lines joined by a space as they render, and loses its
// underline.
func deepened(src []byte, h *ast.Heading, level int) []byte {
	at := h.Pos()
	hashes := at
	for hashes < len(src) && src[hashes] == '#' {
		hashes++
	}
	if hashes > at && (hashes == len(src) || strings.ContainsRune(" \t\r\n", rune(src[hashes]))) {
		return slices.Concat(src[:at], []byte(strings.Repeat("#", level)), src[hashes:])
	}
	lines := h.Lines()
	if lines.Len() == 0 {
		return src
	}
	words := make([]string, 0, lines.Len())
	for i := range lines.Len() {
		line := lines.At(i)
		words = append(words, strings.TrimSpace(string(line.Value(src))))
	}
	lastLine := lines.At(lines.Len() - 1).Start
	lineEnd := len(src)
	if i := slices.Index(src[lastLine:], '\n'); i >= 0 {
		lineEnd = lastLine + i
	}
	underlineEnd := len(src)
	if lineEnd < len(src) {
		if i := slices.Index(src[lineEnd+1:], '\n'); i >= 0 {
			underlineEnd = lineEnd + 1 + i
		}
	}
	return slices.Concat(src[:at], []byte(strings.Repeat("#", level)+" "+strings.Join(words, " ")), src[underlineEnd:])
}
