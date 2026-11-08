// Package reviewrounds reads the Claude review rounds a pull request carries. A round posts
// one review per reviewer, one synthesis review and one round comment, each opening with a
// marker that names its grammar's version and its round. Each review closes with a collapsed
// block holding its findings as JSON, which is what this package reads, never the markdown.
// A post whose marker names a version it does not read is kept whole rather than read wrong.
package reviewrounds

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// Version is the one grammar this package reads.
const Version = "v6"

// The two markers a round's posts open with: a review's, and the round comment's.
const (
	reviewMarker = "pr-review"
	roundMarker  = "pr-review-round"
)

// synthesisLabel is the reviewer label of the review that names the causes findings share.
const synthesisLabel = "synthesis"

// Section is one finding section: the name a finding gives it, the heading a review writes
// over it, and whether a finding in it is a defect the pull request should not merge with.
type Section struct {
	Name     string
	Heading  string
	Blocking bool
}

// Sections are the finding sections in the order a review writes them.
var Sections = []Section{
	{Name: "correctness", Heading: "Correctness", Blocking: true},
	{Name: "design", Heading: "Design and architecture"},
	{Name: "prose", Heading: "Prose"},
}

// SectionOf is the section named name and its place in Sections. A name Sections does not
// hold is headed by itself and placed after them all.
func SectionOf(name string) (Section, int) {
	if at := slices.IndexFunc(Sections, func(s Section) bool { return s.Name == name }); at >= 0 {
		return Sections[at], at
	}
	heading := strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
	if heading == "" {
		heading = "No section"
	}
	return Section{Name: name, Heading: strings.ToUpper(heading[:1]) + heading[1:]}, len(Sections)
}

// The round record's statuses this package tells apart. A reviewer whose status is neither
// did not run, or ran and could not post.
const (
	ran     = "ran"
	skipped = "skipped"
)

// Rounds is every review round a pull request carries.
type Rounds struct {
	// Newest is the round with the highest number, and nil where none posted.
	Newest *Round
	// Earlier are the other rounds, newest first.
	Earlier []Round
	// Whole are the posts no round could take, oldest first, each shown as it was written.
	Whole []Post
	// CommentsCut says the detail read holds only the newest of the pull request's comments,
	// so a round comment older than those is not read, and a round lacking one may have
	// finished.
	CommentsCut bool
}

// Round is one round's posts.
type Round struct {
	Number int
	// Head is the commit the round comment names, or else the one its newest review read.
	Head string
	// Reviews are each reviewer's newest review, in the order they posted, the synthesis apart.
	Reviews   []Review
	Synthesis *Synthesis
	// Finish is the round comment, which posts once every reviewer has ended, so the round
	// runs while it is nil.
	Finish *Finish
}

// Review is one reviewer's review.
type Review struct {
	Reviewer string
	Model    string
	Effort   string
	Head     string
	At       *time.Time
	URL      string
	Findings []Finding
	// Unread is why its findings block could not be read, where it could not, and Body is then
	// what shows.
	Unread string
	Body   string
}

// Finding is one finding as the review's JSON block holds it. Reach is empty on a prose one.
type Finding struct {
	Number  string   `json:"number"`
	Section string   `json:"section"`
	Sites   []string `json:"sites"`
	Reach   string   `json:"reach"`
	Body    string   `json:"body"`
}

// Synthesis is the review naming the causes the round's findings share.
type Synthesis struct {
	At         *time.Time
	URL        string
	Shapes     []Shape
	Duplicates []Duplicate
	Unread     string
	Body       string
}

// Shape is one cause several findings share, and the findings it explains.
type Shape struct {
	Number   string   `json:"number"`
	Section  string   `json:"section"`
	Explains []string `json:"explains"`
	Body     string   `json:"body"`
}

// Duplicate is a group of findings that describe one defect at one site.
type Duplicate struct {
	Defect   string   `json:"defect"`
	Findings []string `json:"findings"`
}

// Finish is the round comment.
type Finish struct {
	At  time.Time
	URL string
	// Reviewers is what became of each reviewer its round record names, in the record's order.
	Reviewers []Outcome
	// Unread is why its round record could not be read, where it could not.
	Unread string
	Body   string
}

// Outcome is what became of one reviewer in a round, in the round record's words. Status is
// ran, skipped, not run or not posted, and Why says why where it did not run, or the status
// where the record gives no reason.
type Outcome struct {
	Label  string
	Status string
	Why    string
}

// Post is a review or comment carrying a round's marker that no round could take.
type Post struct {
	// Marker is its marker's name and version, such as pr-review v7, and Reviewer the label it
	// names, where it names one.
	Marker   string
	Reviewer string
	Author   string
	At       *time.Time
	URL      string
	Body     string
	// Why is why no round took it.
	Why string
	// Outsider says its author is neither an app on the repo nor someone who can write to it,
	// so the post says nothing about the pull request's rounds.
	Outsider bool
}

// Count is how many findings a round's reviewers raised in one section.
type Count struct {
	Section  Section
	Findings int
}

// Read is every round the pull request's reviews and comments carry. A post with no marker
// is not a round's, and is left out. A marked post by an outsider is kept whole, because
// anyone who can comment can open a post with a round's marker.
func Read(detail pullrequests.Detail) Rounds {
	numbered := map[int]*Round{}
	var whole []Post
	place := func(number int) *Round {
		if numbered[number] == nil {
			numbered[number] = &Round{Number: number}
		}
		return numbered[number]
	}
	for _, review := range detail.Reviews {
		m, marked := markerOf(review.Body)
		if !marked {
			continue
		}
		post := Post{Marker: m.name + " " + m.version, Reviewer: m.pairs["reviewer"], Author: review.Author, At: review.SubmittedAt, URL: review.URL, Body: review.Body}
		number, why := m.placed(reviewMarker)
		if outsider := outsiderWhy(review.Author, review.AuthorType, review.AuthorAssociation); outsider != "" {
			post.Outsider, why = true, outsider
		}
		if why != "" {
			post.Why = why
			whole = append(whole, post)
			continue
		}
		round := place(number)
		if post.Reviewer == synthesisLabel {
			if round.Synthesis == nil || later(review.SubmittedAt, round.Synthesis.At) {
				round.Synthesis = synthesisOf(review)
			}
			continue
		}
		round.Reviews = keptNewest(round.Reviews, reviewOf(review, m))
	}
	for _, comment := range detail.Comments {
		m, marked := markerOf(comment.Body)
		if !marked {
			continue
		}
		at := comment.CreatedAt
		post := Post{Marker: m.name + " " + m.version, Reviewer: m.pairs["reviewer"], Author: comment.Author, At: &at, URL: comment.URL, Body: comment.Body}
		number, why := m.placed(roundMarker)
		if outsider := outsiderWhy(comment.Author, comment.AuthorType, comment.AuthorAssociation); outsider != "" {
			post.Outsider, why = true, outsider
		}
		if why != "" {
			post.Why = why
			whole = append(whole, post)
			continue
		}
		round := place(number)
		if round.Finish == nil || at.After(round.Finish.At) {
			round.Finish = finishOf(comment)
			round.Head = m.pairs["head"]
		}
	}
	rounds := Rounds{CommentsCut: detail.CommentCount > len(detail.Comments)}
	for _, number := range slices.Backward(slices.Sorted(maps.Keys(numbered))) {
		round := numbered[number]
		if round.Head == "" {
			round.Head = newestHead(round.Reviews)
		}
		if rounds.Newest == nil {
			rounds.Newest = round
			continue
		}
		rounds.Earlier = append(rounds.Earlier, *round)
	}
	slices.SortStableFunc(whole, func(a, b Post) int { return compareAt(a.At, b.At) })
	rounds.Whole = whole
	return rounds
}

// Counts is how many findings the round's reviewers raised in each section, in Sections'
// order, then any other section a review names, by name. A section none raised is left out.
func (r Round) Counts() []Count {
	by := map[string]int{}
	for _, review := range r.Reviews {
		for _, finding := range review.Findings {
			by[finding.Section]++
		}
	}
	counts := make([]Count, 0, len(by))
	for _, name := range slices.Sorted(maps.Keys(by)) {
		section, _ := SectionOf(name)
		counts = append(counts, Count{Section: section, Findings: by[name]})
	}
	slices.SortStableFunc(counts, func(a, b Count) int {
		_, at := SectionOf(a.Section.Name)
		_, bt := SectionOf(b.Section.Name)
		return at - bt
	})
	return counts
}

// Skipped is each reviewer the round comment names as skipped on purpose, and Missed each it
// names as not having run or not having posted. Both are empty before the round comment posts.
func (r Round) Skipped() []Outcome {
	return r.outcomes(func(status string) bool { return status == skipped })
}

func (r Round) Missed() []Outcome {
	return r.outcomes(func(status string) bool { return status != ran && status != skipped })
}

func (r Round) outcomes(kept func(status string) bool) []Outcome {
	if r.Finish == nil {
		return nil
	}
	var outcomes []Outcome
	for _, outcome := range r.Finish.Reviewers {
		if kept(outcome.Status) {
			outcomes = append(outcomes, outcome)
		}
	}
	return outcomes
}

// NoneRan says the round comment names the round's reviewers and none of them ran.
func (r Round) NoneRan() bool {
	return r.Finish != nil && len(r.Finish.Reviewers) > 0 &&
		!slices.ContainsFunc(r.Finish.Reviewers, func(outcome Outcome) bool { return outcome.Status == ran })
}

// Unread is each review whose findings block could not be read.
func (r Round) Unread() []Review {
	return slices.DeleteFunc(slices.Clone(r.Reviews), func(review Review) bool { return review.Unread == "" })
}

// Bodies is the body of each of the round's posts: its reviews, its synthesis and its round
// comment.
func (r Round) Bodies() []string {
	var bodies []string
	for _, review := range r.Reviews {
		bodies = append(bodies, review.Body)
	}
	if r.Synthesis != nil {
		bodies = append(bodies, r.Synthesis.Body)
	}
	if r.Finish != nil {
		bodies = append(bodies, r.Finish.Body)
	}
	return bodies
}

// PostedAt is when the round's newest post landed, and zero where none says when.
func (r Round) PostedAt() time.Time {
	var newest time.Time
	var stamps []*time.Time
	for _, review := range r.Reviews {
		stamps = append(stamps, review.At)
	}
	if r.Synthesis != nil {
		stamps = append(stamps, r.Synthesis.At)
	}
	if r.Finish != nil {
		stamps = append(stamps, &r.Finish.At)
	}
	for _, at := range stamps {
		if at != nil && at.After(newest) {
			newest = *at
		}
	}
	return newest
}

// FinishedAt is when the newest round's comment posted, and nil where no round posted or the
// newest still runs.
func (rs Rounds) FinishedAt() *time.Time {
	if rs.Newest == nil || rs.Newest.Finish == nil {
		return nil
	}
	return &rs.Newest.Finish.At
}

// WholeIsNewest says a post no round could take, by someone other than an outsider, landed
// after every post of the newest round, so the newest review on the pull request is one this
// package cannot count.
func (rs Rounds) WholeIsNewest() bool {
	for _, post := range slices.Backward(rs.Whole) {
		if post.Outsider {
			continue
		}
		return rs.Newest == nil || post.At == nil || post.At.After(rs.Newest.PostedAt())
	}
	return false
}

// marker is a post's opening line, `<!-- <name> <version> key=value … -->`, its pairs by key.
type marker struct {
	name, version string
	pairs         map[string]string
}

// markerOf is the marker the body opens with, where it opens with one of a round's.
func markerOf(body string) (marker, bool) {
	line, _, _ := strings.Cut(strings.TrimLeft(body, " \t\r\n"), "\n")
	inner, opened := strings.CutPrefix(strings.TrimSpace(line), "<!--")
	inner, closed := strings.CutSuffix(inner, "-->")
	fields := strings.Fields(inner)
	if !opened || !closed || len(fields) == 0 || (fields[0] != reviewMarker && fields[0] != roundMarker) {
		return marker{}, false
	}
	m := marker{name: fields[0], pairs: map[string]string{}}
	for _, field := range fields[1:] {
		key, value, paired := strings.Cut(field, "=")
		switch {
		case paired:
			m.pairs[key] = value
		case m.version == "":
			m.version = field
		}
	}
	return m, true
}

// placed is the round a post marked m belongs to, where it is a post of kind name in the
// version this package reads, and otherwise why no round takes it.
func (m marker) placed(name string) (int, string) {
	if m.version != Version {
		version := m.version
		if version == "" {
			version = "no version"
		}
		return 0, fmt.Sprintf("prs reads %s %s, and this one's marker names %s", m.name, Version, version)
	}
	if m.name != name {
		return 0, fmt.Sprintf("a %s marker belongs on a %s", m.name, map[string]string{reviewMarker: "review", roundMarker: "comment"}[m.name])
	}
	number, err := strconv.Atoi(m.pairs["round"])
	if err != nil || number < 1 {
		return 0, "its marker names no round"
	}
	if name == reviewMarker && m.pairs["reviewer"] == "" {
		return 0, "its marker names no reviewer"
	}
	return number, ""
}

func reviewOf(review pullrequests.Review, m marker) Review {
	r := Review{
		Reviewer: m.pairs["reviewer"], Model: m.pairs["model"], Effort: m.pairs["effort"], Head: m.pairs["head"],
		At: review.SubmittedAt, URL: review.URL, Body: review.Body,
	}
	var block struct {
		Findings []Finding `json:"findings"`
	}
	if err := jsonBlock(review.Body, &block); err != nil {
		r.Unread = err.Error()
		return r
	}
	r.Findings = block.Findings
	return r
}

func synthesisOf(review pullrequests.Review) *Synthesis {
	s := &Synthesis{At: review.SubmittedAt, URL: review.URL, Body: review.Body}
	var block struct {
		Shapes     []Shape     `json:"shapes"`
		Duplicates []Duplicate `json:"duplicates"`
	}
	if err := jsonBlock(review.Body, &block); err != nil {
		s.Unread = err.Error()
		return s
	}
	s.Shapes, s.Duplicates = block.Shapes, block.Duplicates
	return s
}

func finishOf(comment pullrequests.Comment) *Finish {
	f := &Finish{At: comment.CreatedAt, URL: comment.URL, Body: comment.Body}
	var record struct {
		Reviewers []struct {
			Label  string `json:"label"`
			Status string `json:"status"`
			Why    string `json:"why"`
		} `json:"reviewers"`
	}
	if err := jsonBlock(comment.Body, &record); err != nil {
		f.Unread = err.Error()
		return f
	}
	for _, reviewer := range record.Reviewers {
		why := reviewer.Why
		if why == "" {
			why = reviewer.Status
		}
		f.Reviewers = append(f.Reviewers, Outcome{Label: reviewer.Label, Status: reviewer.Status, Why: why})
	}
	return f
}

// outsiderWhy is why a marked post's author cannot be one a round's posts come from, and
// empty where it can. A round posts as an app installed on the repo, which GitHub names a
// Bot, or as someone who can write to the repo.
func outsiderWhy(author, authorType, association string) string {
	if authorType == "Bot" || slices.Contains([]string{"OWNER", "MEMBER", "COLLABORATOR"}, association) {
		return ""
	}
	if author == "" {
		author = "a deleted account"
	}
	return author + " is neither an app on the repo nor someone who can write to it"
}

// jsonBlock decodes the last json fence in the post, which sits in the collapsed block every
// post of a round closes with. A fence opens on a line of three or more backticks and json,
// and closes on a line of the same backticks. JSON escapes a newline inside a string, so no
// line of the block's own text can open a fence, whatever a finding quotes.
func jsonBlock(body string, into any) error {
	lines := slices.Collect(strings.Lines(body))
	opener, fence := -1, ""
	for i, line := range lines {
		bare := strings.TrimRight(line, " \t\r\n")
		ticks := len(bare) - len(strings.TrimLeft(bare, "`"))
		if ticks >= 3 && strings.TrimSpace(bare[ticks:]) == "json" {
			opener, fence = i, bare[:ticks]
		}
	}
	if opener < 0 {
		return errors.New("it holds no json fence")
	}
	for i := opener + 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r\n") == fence {
			if err := json.Unmarshal([]byte(strings.Join(lines[opener+1:i], "")), into); err != nil {
				return fmt.Errorf("its JSON block does not parse: %w", err)
			}
			return nil
		}
	}
	return errors.New("its json fence never closes")
}

// keptNewest is reviews with r in place of an earlier review by the same reviewer, or added
// after them, so a reviewer posting twice in a round counts once.
func keptNewest(reviews []Review, r Review) []Review {
	at := slices.IndexFunc(reviews, func(had Review) bool { return had.Reviewer == r.Reviewer })
	switch {
	case at < 0:
		return append(reviews, r)
	case later(r.At, reviews[at].At):
		return append(slices.Delete(reviews, at, at+1), r)
	}
	return reviews
}

// newestHead is the head the newest of the reviews read.
func newestHead(reviews []Review) string {
	head := ""
	var newest *time.Time
	for _, r := range reviews {
		if r.Head != "" && (head == "" || later(r.At, newest)) {
			head, newest = r.Head, r.At
		}
	}
	return head
}

// later says a landed after b. A post with no time, a review still pending, counts as newest.
func later(a, b *time.Time) bool { return compareAt(a, b) > 0 }

func compareAt(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	}
	return a.Compare(*b)
}
