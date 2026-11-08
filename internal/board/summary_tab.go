package board

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/registry"
	"github.com/datapointchris/prs/internal/summary"
)

// SummaryView is the Summary tab's name, the name --view takes.
const SummaryView = "summary"

// SummaryWindows are the windows w and W step the Summary tab between, shortest first, and
// DefaultSummaryWindow the one it opens on.
var SummaryWindows = []string{"1d", "7d", "2w", "30d", "90d"}

const DefaultSummaryWindow = "30d"

// Summary is the Summary tab: the pull requests opened, merged and closed within a window,
// in numbers and charts. The open pull requests are the board's own listings, read on the
// board's timer whichever tab shows, and the merged and closed are GitHub's, kept in a store
// and read again every closed refresh.
type Summary struct {
	board *Board
	store *summary.Store
	// scope is the GitHub repos and owners the board reads, and history nil where it reads
	// none, since only GitHub's listing says when a pull request closed.
	scope   pullrequests.Scope
	history *gotermboard.Source[keptHistory]
	// opened is what the store held when the tab opened, which draws until history's first
	// read lands, and openFault why it could not be read. cacheFault is why the store's file
	// could not be opened, so the tab keeps the history in memory.
	opened     keptHistory
	openFault  string
	cacheFault string

	mu     sync.Mutex
	window pullrequests.Since
	until  time.Time
}

// keptHistory is the merged and closed pull requests the store holds, and how far it reaches.
type keptHistory struct {
	closed   []pullrequests.HistoryRecord
	coverage summary.Coverage
	covered  bool
}

// OpenSummary is the Summary tab over the board's repos, counting window, with the merged and
// closed pull requests kept in a file under dir for the repos read, and read again every
// interval. With no dir, or a file that cannot be opened, it keeps them in memory, so each
// start reads the window whole. Close closes the file.
func (b *Board) OpenSummary(dir string, every time.Duration, window pullrequests.Since) *Summary {
	s := &Summary{board: b, window: window}
	scope, ok := b.scopes[registry.GitHub]
	if !ok {
		return s
	}
	s.scope = scope
	var err error
	if dir != "" {
		if s.store, err = summary.Open(summary.FileFor(dir, scope)); err != nil {
			s.cacheFault = err.Error()
		}
	}
	if s.store == nil {
		// An in-memory database opens without touching the disk.
		s.store, _ = summary.Open("")
	}
	s.opened, s.openFault = s.load()
	s.history = gotermboard.NewSource("github history", every, func(ctx context.Context) (keptHistory, string, error) {
		now := time.Now()
		if err := summary.Sync(ctx, s.store, s.scope, s.Window().From(now), now); err != nil {
			return keptHistory{}, "", err
		}
		kept, fault := s.load()
		if fault != "" {
			return keptHistory{}, "", fmt.Errorf("%s", fault)
		}
		return kept, "", nil
	})
	return s
}

// Close closes the store's file.
func (s *Summary) Close() error {
	if s.store == nil {
		return nil
	}
	return s.store.Close()
}

// load is what the store holds now, and why it could not be read.
func (s *Summary) load() (keptHistory, string) {
	coverage, covered, err := s.store.Coverage()
	if err != nil {
		return keptHistory{}, err.Error()
	}
	closed, err := s.store.Closed(time.Time{})
	if err != nil {
		return keptHistory{}, err.Error()
	}
	return keptHistory{closed: closed, coverage: coverage, covered: covered}, ""
}

// Window is the window the tab counts.
func (s *Summary) Window() pullrequests.Since {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.window
}

// CountUntil has the tab count up to until rather than now, for prs summary --until.
func (s *Summary) CountUntil(until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.until = until
}

func (s *Summary) countsTo(now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.until.IsZero() {
		return now
	}
	return s.until
}

// kept is the history the counts come from: its last good read, or before its first, what
// the store held when the tab opened.
func (s *Summary) kept() keptHistory {
	if s.history == nil || s.history.Status().ReadAt.IsZero() {
		return s.opened
	}
	return s.history.Value()
}

// Sources is gotermboard.Board's: the board's open listings, then the history.
func (s *Summary) Sources() []gotermboard.Refresher {
	var refreshers []gotermboard.Refresher
	for _, source := range s.board.sources {
		refreshers = append(refreshers, source)
	}
	if s.history != nil {
		refreshers = append(refreshers, s.history)
	}
	return refreshers
}

// Groupings is gotermboard.Board's. The tab has one.
func (s *Summary) Groupings() []string { return nil }

// Look is gotermboard.LookBoard's, the board's own, so the margin holds still across tabs.
func (s *Summary) Look(width int) gotermboard.Look { return s.board.Look(width) }

// Actions is gotermboard.Keyed's: w and W step the window.
func (s *Summary) Actions() []gotermboard.Action {
	return []gotermboard.Action{
		{Keys: []string{"w"}, Label: "count the next longer window", Footer: "w window", Run: s.step(1)},
		{Keys: []string{"W"}, Label: "count the next shorter window", Run: s.step(-1)},
	}
}

// step moves the window by one along SummaryWindows, round from the last to the first. A
// window the store does not reach back to is read at once.
func (s *Summary) step(by int) func(*gotermboard.Model) tea.Cmd {
	return func(m *gotermboard.Model) tea.Cmd {
		s.mu.Lock()
		at := slices.Index(SummaryWindows, s.window.String())
		if at < 0 {
			at = slices.Index(SummaryWindows, DefaultSummaryWindow)
		}
		name := SummaryWindows[(at+by+len(SummaryWindows))%len(SummaryWindows)]
		s.window, _ = pullrequests.ParseSince(name, time.Local)
		s.mu.Unlock()
		if kept := s.kept(); s.history != nil && (!kept.covered || kept.coverage.From.After(s.Window().From(time.Now()))) {
			s.history.Force()
		}
		m.Rebuild()
		m.Say("counting the last " + name)
		return nil
	}
}

// statuses is how each read stands: the board's open listings, then the history, which
// reads as read when the store last was until its first read here lands.
func (s *Summary) statuses() []gotermboard.Status {
	var statuses []gotermboard.Status
	for _, source := range s.board.sources {
		statuses = append(statuses, s.board.statusOf(source))
	}
	if s.history != nil {
		status := s.history.Status()
		if status.ReadAt.IsZero() && s.opened.covered {
			status.ReadAt = s.opened.coverage.Synced
		}
		if status.Fault == "" && s.openFault != "" && status.ReadAt.IsZero() {
			status.Fault = s.openFault
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func (s *Summary) reading() bool {
	return slices.ContainsFunc(s.Sources(), func(source gotermboard.Refresher) bool { return source.Status().Reading })
}

// open is every open pull request the board's listings hold.
func (s *Summary) open() []pullrequests.PullRequest {
	var open []pullrequests.PullRequest
	for _, source := range s.board.sources {
		open = append(open, s.board.listing(source).PullRequests...)
	}
	return open
}

// Numbers is the counts for the tab's window at now.
func (s *Summary) Numbers(now time.Time) summary.Numbers {
	until := s.countsTo(now)
	window := s.Window()
	return summary.Count(summary.Window{Name: window.String(), Since: window.From(now), Until: until},
		s.kept().closed, s.open(), time.Local)
}

// Notes say what the counts leave out: a window older than the store reaches, merged and
// closed pull requests no read says, and a host whose closed pull requests are not read.
func (s *Summary) Notes(now time.Time) []string {
	var notes []string
	since := s.Window().From(now)
	kept := s.kept()
	switch {
	case s.history == nil:
		notes = append(notes, "No repo is on GitHub, so only the open pull requests count.")
	case !kept.covered:
		notes = append(notes, fmt.Sprintf("No merged or closed pull request is counted until the read of those since %s lands.", day(since)))
	case kept.coverage.From.After(since):
		notes = append(notes, fmt.Sprintf("Merged and closed pull requests count from %s, as far as the cache reaches, until the read back to %s lands.",
			day(kept.coverage.From), day(since)))
	}
	if s.cacheFault != "" {
		notes = append(notes, "Nothing read here is kept for the next start: "+s.cacheFault)
	}
	if n := len(s.board.scopes[registry.Bitbucket].Repos); n > 0 {
		notes = append(notes, fmt.Sprintf("Pull requests on Bitbucket's %d %s count among the open only, because bbkt says no time a pull request closed.", n, plural(n, "repo", "repos")))
	}
	return notes
}

// Faults name each read whose newest attempt failed, or that left open pull requests out, so
// prs summary exits 1 where its counts miss something.
func (s *Summary) Faults() []string {
	faults := s.board.Gaps()
	if s.history != nil {
		if status := s.history.Status(); status.Fault != "" {
			faults = append(faults, status.Name+": "+status.Fault)
		}
	}
	return faults
}

// SummarySnapshot is what prs summary --json prints: the numbers, flattened, then what they
// were counted from. cmd/prs/summary.schema.json is its shape.
type SummarySnapshot struct {
	View     string   `json:"view"`
	Registry string   `json:"registry"`
	Owners   []string `json:"owners,omitempty"`
	summary.Numbers
	// Cache is where the merged and closed pull requests are kept and how far they reach, and
	// null where nothing has been read into it.
	Cache   *SummaryCache        `json:"cache"`
	Notes   []string             `json:"notes"`
	Sources []gotermboard.Status `json:"sources"`
}

// SummaryCache is the store's file, empty where it is kept in memory, and its coverage.
type SummaryCache struct {
	Path string `json:"path"`
	summary.Coverage
}

// Snapshot reads what the tab holds now.
func (s *Summary) Snapshot(now time.Time) SummarySnapshot {
	snap := SummarySnapshot{
		View: SummaryView, Registry: s.board.registry, Owners: s.board.owners,
		Numbers: s.Numbers(now), Notes: s.Notes(now), Sources: s.statuses(),
	}
	if snap.Notes == nil {
		snap.Notes = []string{}
	}
	if kept := s.kept(); kept.covered {
		snap.Cache = &SummaryCache{Path: s.store.Path(), Coverage: kept.coverage}
	}
	return snap
}

// MoveEvery is gotermboard.MovingBoard's. The strip turns its spinner while a read runs.
func (s *Summary) MoveEvery() time.Duration {
	if s.reading() {
		return gotermboard.SpinEvery
	}
	return 0
}

// StripAt is gotermboard.MovingBoard's: the windows with the one counted lit, how each read
// stands, and what the counts leave out.
func (s *Summary) StripAt(now time.Time) []gotermboard.Line {
	name := s.Window().String()
	windows := gotermboard.Line{gotermboard.C("window", gotermboard.Bold)}
	for _, each := range SummaryWindows {
		code := gotermboard.RuleCode
		if each == name {
			code = gotermboard.BoldCyan
		}
		windows = append(windows, gotermboard.C("  "+each, code))
	}
	if !slices.Contains(SummaryWindows, name) {
		windows = append(windows, gotermboard.C("  "+name, gotermboard.BoldCyan))
	}
	until := s.countsTo(now)
	windows = append(windows, gotermboard.C(fmt.Sprintf("   %s to %s", stamp(s.Window().From(now)), untilText(until, now)), ""))
	strip := []gotermboard.Line{windows}
	if status := gotermboard.ReadStatus(s.statuses(), now); len(status) > 0 {
		if s.reading() {
			status = slices.Insert(status, 1, gotermboard.C(" "+gotermboard.SpinnerAt(now), gotermboard.Yellow))
		}
		strip = append(strip, status)
	}
	for _, note := range s.Notes(now) {
		strip = append(strip, gotermboard.Line{gotermboard.C(note, gotermboard.Yellow)})
	}
	return strip
}

// summaryColumns are the table's: what a row counts, then all, the people's and the bots'.
var summaryColumns = []gotermboard.Column{{Title: ""}, {Title: "ALL", Right: true}, {Title: "PEOPLE", Right: true}, {Title: "BOTS", Right: true}}

// Frame is gotermboard.Board's: the strip, the charts, then the counts and each repo's merges.
func (s *Summary) Frame(now time.Time, _ string) gotermboard.Frame {
	n := s.Numbers(now)
	end := endText(n.Until, now)
	return gotermboard.Frame{
		Strip:   s.StripAt(now),
		Charts:  func(width, height int) []gotermboard.Line { return summaryCharts(n, end, width, height) },
		Columns: summaryColumns,
		Groups:  []gotermboard.Group{countsGroup(n, end), reposGroup(n)},
	}
}

// Table is what prs summary prints: the strip's lines, then each group's rows under its
// title, the counts aligned in the tab's columns.
func (s *Summary) Table(now time.Time) []gotermboard.Line {
	n := s.Numbers(now)
	lines := s.StripAt(now)
	header := make([]gotermboard.Cell, len(summaryColumns))
	for i, column := range summaryColumns {
		header[i] = gotermboard.C(column.Title, gotermboard.Bold)
	}
	for _, group := range []gotermboard.Group{countsGroup(n, endText(n.Until, now)), reposGroup(n)} {
		title := append(gotermboard.Line{}, group.Title...)
		if group.Counted {
			title = append(title, gotermboard.C(fmt.Sprintf(" · %d", group.Count), ""))
		}
		rows := [][]gotermboard.Cell{header}
		for _, row := range group.Rows {
			rows = append(rows, row.Cells)
		}
		lines = append(append(lines, gotermboard.Line{}, title), aligned(rows)...)
	}
	return lines
}

// aligned is rows of cells padded into summaryColumns, the first to the left and the rest to
// the right.
func aligned(rows [][]gotermboard.Cell) []gotermboard.Line {
	widths := make([]int, len(summaryColumns))
	for _, cells := range rows {
		for i, cell := range cells {
			widths[i] = max(widths[i], ansi.StringWidth(cell.Text))
		}
	}
	out := make([]gotermboard.Line, 0, len(rows))
	for _, cells := range rows {
		var line gotermboard.Line
		for i, cell := range cells {
			pad := gotermboard.C(strings.Repeat(" ", widths[i]-ansi.StringWidth(cell.Text)), "")
			if i > 0 {
				line = append(line, gotermboard.C(columnGap, ""))
			}
			if summaryColumns[i].Right {
				line = append(line, pad, cell)
			} else {
				line = append(line, cell, pad)
			}
		}
		out = append(out, line)
	}
	return out
}

// countsGroup is the window's counts, a row each. end is when the window ends, as "now" or
// "at" an instant.
func countsGroup(n summary.Numbers, end string) gotermboard.Group {
	counted := func(key, label string, k summary.Kinds, detail string) gotermboard.Row {
		return gotermboard.Row{
			Key:    "counts/" + key,
			Cells:  []gotermboard.Cell{gotermboard.C(label, ""), countCell(k.All), countCell(k.People), countCell(k.Bots)},
			Detail: []gotermboard.Cell{gotermboard.C(detail, "")},
		}
	}
	timed := func(key, label string, v summary.KindValues, detail string) gotermboard.Row {
		return gotermboard.Row{
			Key:    "counts/" + key,
			Cells:  []gotermboard.Cell{gotermboard.C(label, ""), durationCell(v.All), durationCell(v.People), durationCell(v.Bots)},
			Detail: []gotermboard.Cell{gotermboard.C(detail, "")},
		}
	}
	shares := n.ReviewedByRoundShare
	return gotermboard.Group{
		Key: "counts", Title: gotermboard.Line{gotermboard.C("COUNTS", gotermboard.Bold)}, Still: true,
		Rows: []gotermboard.Row{
			counted("open", "open "+end, n.Open, "The pull requests open at the window's end, on every host the board reads."),
			counted("opened", "opened", n.Opened, "The pull requests opened within the window, whether open or closed now."),
			counted("merged", "merged", n.Merged, "The pull requests merged within the window, split by who opened them."),
			counted("closed", "closed unmerged", n.ClosedUnmerged, "The pull requests closed within the window without merging."),
			timed("median", "median to merge", n.MedianSecondsToMerge, "Half the merges within the window landed sooner than this after opening."),
			timed("p90", "p90 to merge", n.P90SecondsToMerge, "Nine in ten merges within the window landed sooner than this after opening."),
			{
				Key: "counts/rounds",
				Cells: []gotermboard.Cell{
					gotermboard.C("reviewed by a round", ""), shareCell(shares.All), shareCell(shares.People), shareCell(shares.Bots),
				},
				Detail: []gotermboard.Cell{gotermboard.C(fmt.Sprintf(
					"%d of the %d merges within the window had a review round's review as their first review.", n.ReviewedByRound.All, n.Merged.All), "")},
			},
		},
	}
}

// reposGroup is each repo with a merge within the window, the most merges first.
func reposGroup(n summary.Numbers) gotermboard.Group {
	g := gotermboard.Group{
		Key: "repos", Title: gotermboard.Line{gotermboard.C("MERGES BY REPO", gotermboard.Bold)},
		Count: len(n.Repos), Counted: true,
	}
	for _, repo := range n.Repos {
		median := repo.MedianSecondsToMerge
		g.Rows = append(g.Rows, gotermboard.Row{
			Key:   "repos/" + strings.ToLower(repo.Slug),
			Cells: []gotermboard.Cell{gotermboard.C(repo.Slug, ""), countCell(repo.Merged.All), countCell(repo.Merged.People), countCell(repo.Merged.Bots)},
			Detail: []gotermboard.Cell{gotermboard.C(fmt.Sprintf("%s merged %d within the window, %d opened by people and %d by apps, at a median of %s from opening.",
				repo.Slug, repo.Merged.All, repo.Merged.People, repo.Merged.Bots, durationText(median)), "")},
		})
	}
	return g
}

func countCell(n int) gotermboard.Cell {
	if n == 0 {
		return gotermboard.C("0", gotermboard.RuleCode)
	}
	return gotermboard.C(fmt.Sprint(n), "")
}

func durationCell(seconds *float64) gotermboard.Cell {
	if seconds == nil {
		return gotermboard.C("-", gotermboard.RuleCode)
	}
	return gotermboard.C(durationText(*seconds), "")
}

func shareCell(share *float64) gotermboard.Cell {
	if share == nil {
		return gotermboard.C("-", gotermboard.RuleCode)
	}
	return gotermboard.C(fmt.Sprintf("%.0f%%", *share*100), "")
}

// durationText is a time to merge as a reader compares them: minutes under an hour, hours to
// a tenth under two days, then days to a tenth.
func durationText(seconds float64) string {
	took := time.Duration(seconds * float64(time.Second))
	switch {
	case took < time.Hour:
		return fmt.Sprintf("%dm", int(took.Minutes()+0.5))
	case took < 48*time.Hour:
		return fmt.Sprintf("%.1fh", took.Hours())
	}
	return fmt.Sprintf("%.1fd", took.Hours()/24)
}

// stamp is an instant on the strip, the date then the time.
func stamp(at time.Time) string { return at.Local().Format("Jan 2 15:04") }

func untilText(until, now time.Time) string {
	if until.Equal(now) {
		return "now"
	}
	return stamp(until)
}

// endText is a count's moment at the window's end: now, or at the instant --until names.
func endText(until, now time.Time) string {
	if until.Equal(now) {
		return "now"
	}
	return "at " + stamp(until)
}

func day(at time.Time) string { return at.Local().Format("Jan 2") }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
