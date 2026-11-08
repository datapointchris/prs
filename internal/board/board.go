// Package board is prs's view on gotermboard: every open pull request as a row, its
// description a key away, and a key per action.
package board

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermboard/markdown"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/registry"
)

// View is the board's one tab, the name --view takes.
const View = "pull-requests"

// Repos is what the board reads: the repos the registry at Registry names, or with no
// registry, Owners whole. From is the config layer that chose, one of config's From
// constants.
type Repos struct {
	From     string
	Registry string
	Repos    []registry.Repo
	Owners   []string
}

// Board is the open pull requests of the repos a registry names, read from each provider
// that hosts one.
type Board struct {
	from     string
	registry string
	owners   []string
	sources  []*gotermboard.Source[pullrequests.Listing]
	// scopes is what each provider's read covers, by provider name.
	scopes   map[string]pullrequests.Scope
	repos    []registry.Repo
	renderer *markdown.Renderer
	actions  []gotermboard.Action
	// shown is the last frame's pull requests by key, for a key to find the cursor's.
	shown map[string]pullrequests.PullRequest
	// settled is the state a merge or close left a pull request in, by key.
	settled map[string]settledState
	// diffed is the head commit d last fetched for a pull request, by key, which its merge
	// pins.
	diffed map[string]string
	// working is every command a key started that has not ended, oldest first.
	working   []underWay
	workCount int
	// details holds each row's detail, read after each listing once ReadDetails is called.
	details     *pullrequests.Details
	readDetails bool
	// openedOn is the pull request OpenOn read, and nil where the board opened on none.
	openedOn *openedOn
	// every is how often each read runs again while the board is open.
	every time.Duration
	// listings keeps each provider's listing on disk once KeepListings is called, and holds
	// the last run's, which a provider's rows come from until its first read here lands.
	listings *listingCache
	// cursor is the key of the row under the cursor. gone is the pull request it was on when
	// a read stopped listing it, which the board draws, marked, until the cursor leaves it.
	cursor string
	gone   *pullrequests.PullRequest
}

// New is a board over repos, reading each provider every interval, with the window actions
// the config adds. It refuses a window action on a key the board already binds.
func New(repos Repos, every time.Duration, windows []config.WindowAction) (*Board, error) {
	for _, window := range windows {
		if err := Refused(window); err != nil {
			return nil, fmt.Errorf("window action %q: %w", window.Label, err)
		}
	}
	b := &Board{
		from:     repos.From,
		registry: repos.Registry,
		owners:   repos.Owners,
		repos:    repos.Repos,
		renderer: markdown.New(nil),
		every:    every,
		shown:    map[string]pullrequests.PullRequest{},
		settled:  map[string]settledState{},
		diffed:   map[string]string{},
		details:  pullrequests.NewDetails(),
		scopes:   map[string]pullrequests.Scope{},
	}
	for _, provider := range pullrequests.Providers {
		scope := pullrequests.Scope{Repos: registry.Hosted(repos.Repos, provider.Name)}
		if provider.ReadsOwners {
			scope.Owners = repos.Owners
		}
		if scope.Empty() {
			continue
		}
		b.scopes[provider.Name] = scope
		var source *gotermboard.Source[pullrequests.Listing]
		source = gotermboard.NewSource(provider.Name, every,
			func(ctx context.Context) (pullrequests.Listing, string, error) {
				listing, err := provider.Read(ctx, scope, b.listing(source))
				if err != nil {
					return listing, "", err
				}
				if b.readDetails {
					b.details.Prefetch(ctx, provider, listing.PullRequests)
				}
				if b.listings != nil {
					b.listings.keep(provider.Name, listing, time.Now())
				}
				return listing, listing.Note(), nil
			})
		b.sources = append(b.sources, source)
	}
	b.actions = b.keyTable(windows)
	return b, nil
}

// ReadDetails has each listing that lands read its rows' details in the background, for
// the description to show. A board printed once leaves it off, since nothing waits for them.
func (b *Board) ReadDetails() { b.readDetails = true }

// KeepListings has the board draw the listings the last run kept at path until each
// provider's first read lands, and keep each listing that lands there for the next run. A
// board printed once leaves it off, since it waits for its reads.
func (b *Board) KeepListings(path string) { b.listings = openListingCache(path, b.scopes) }

// listing is a provider's rows now: its last good read, or before its first, the listing
// the last run kept.
func (b *Board) listing(source *gotermboard.Source[pullrequests.Listing]) pullrequests.Listing {
	if status := source.Status(); status.ReadAt.IsZero() && b.listings != nil {
		if kept, ok := b.listings.listing(status.Name); ok {
			return kept.listing()
		}
	}
	return source.Value()
}

// Sources is gotermboard.Board's.
func (b *Board) Sources() []gotermboard.Refresher {
	refreshers := make([]gotermboard.Refresher, len(b.sources))
	for i, source := range b.sources {
		refreshers[i] = source
	}
	if b.openedOn != nil {
		refreshers = append(refreshers, b.openedOn.source)
	}
	return refreshers
}

// Groupings is gotermboard.Board's. The board has one.
func (b *Board) Groupings() []string { return nil }

// pullRequests is every provider's rows, oldest first.
func (b *Board) pullRequests(now time.Time) []pullrequests.PullRequest {
	var prs []pullrequests.PullRequest
	for _, source := range b.sources {
		prs = append(prs, b.listing(source).PullRequests...)
	}
	return pullrequests.Finish(prs, b.repos, now)
}

// Row is one pull request as both --json outputs print it, with the open pull request it
// is stacked on where there is one. cmd/prs/list.schema.json is its shape.
type Row struct {
	pullrequests.PullRequest
	StackedOn *int `json:"stacked_on"`
}

// Rows is every pull request the board holds, oldest first, and empty rather than nil, so
// no rows print as [].
func (b *Board) Rows(now time.Time) []Row {
	prs := b.pullRequests(now)
	parents := stackParents(prs)
	rows := make([]Row, 0, len(prs))
	for _, pr := range prs {
		row := Row{PullRequest: pr}
		if parent := parentOf(pr, parents); parent != 0 {
			row.StackedOn = &parent
		}
		rows = append(rows, row)
	}
	return rows
}

// statuses is how each read stands: each listing's, then the pull request the board opened
// on. A provider whose rows still come from the last run's listing reads as read when that
// listing was, so the strip gives the rows' age, and names a first read that fails as failing
// on rows that old.
func (b *Board) statuses() []gotermboard.Status {
	statuses := make([]gotermboard.Status, len(b.sources))
	for i, source := range b.sources {
		statuses[i] = source.Status()
		if statuses[i].ReadAt.IsZero() && b.listings != nil {
			if kept, ok := b.listings.listing(statuses[i].Name); ok {
				statuses[i].ReadAt = kept.ReadAt
			}
		}
	}
	if b.openedOn != nil {
		statuses = append(statuses, b.openedOn.source.Status())
	}
	return statuses
}

// reading says a read is under way.
func (b *Board) reading() bool {
	return slices.ContainsFunc(b.sources, func(source *gotermboard.Source[pullrequests.Listing]) bool {
		return source.Status().Reading
	})
}

// Gaps names each provider whose newest read failed, or read and noted pull requests it
// left out, with why. A note leaves rows out as a failure does, so both count.
func (b *Board) Gaps() []string {
	var gaps []string
	for _, status := range b.statuses() {
		switch {
		case status.Fault != "":
			gaps = append(gaps, status.Name+": "+status.Fault)
		case status.Note != "":
			gaps = append(gaps, status.Name+": "+status.Note)
		}
	}
	return gaps
}

// unread says no provider has answered yet, here or in the run that kept its listing.
func (b *Board) unread() bool {
	for _, status := range b.statuses() {
		if !status.ReadAt.IsZero() {
			return false
		}
	}
	return len(b.sources) > 0
}

// failed says every provider was tried and none could be read.
func (b *Board) failed() bool {
	for _, source := range b.sources {
		if status := source.Status(); status.Fault == "" || !status.ReadAt.IsZero() {
			return false
		}
	}
	return len(b.sources) > 0
}

// Frame is gotermboard.Board's: one group of every open pull request, oldest first, and the
// one under the cursor where a read stopped listing it, under the merged or closed pull
// request the board opened on, where there is one.
func (b *Board) Frame(now time.Time, _ string) gotermboard.Frame {
	prs := b.keepGone(b.withOpenedOn(b.pullRequests(now), now), now)
	parents := stackParents(prs)
	clear(b.shown)
	group := gotermboard.Group{
		Key: "open", Title: gotermboard.Line{gotermboard.C("open", gotermboard.Bold)},
		Count: len(prs), Counted: true, Still: true,
	}
	for _, pr := range prs {
		b.shown[pr.Key()] = pr
		group.Rows = append(group.Rows, b.row(pr, parents, now))
	}
	groups := []gotermboard.Group{group}
	if apart, ok := b.apart(prs, parents, now); ok {
		groups = append([]gotermboard.Group{apart}, groups...)
	}
	maps.DeleteFunc(b.settled, func(key string, _ settledState) bool { return !b.listed(key) })
	maps.DeleteFunc(b.diffed, func(key, _ string) bool { return !b.listed(key) })
	if len(prs) == 0 {
		group.Below = []gotermboard.Line{{gotermboard.C(b.emptyReason(), dim)}}
	}
	return gotermboard.Frame{
		Strip:   b.StripAt(now),
		Columns: columns,
		Groups:  groups,
		StartAt: b.startAt(),
	}
}

// listed says the last frame holds the pull request keyed key.
func (b *Board) listed(key string) bool {
	_, ok := b.shown[key]
	return ok
}

// CursorOn is gotermboard.CursorBoard's.
func (b *Board) CursorOn(key string) { b.cursor = key }

// keepGone adds the pull request under the cursor where prs, a read's rows, leaves it out,
// so a refresh never takes the row from under the cursor. It stays, marked gone, until the
// cursor leaves it or a read lists it again. The pull request the board opened on is placed
// by a read of its own, so it is never kept this way.
func (b *Board) keepGone(prs []pullrequests.PullRequest, now time.Time) []pullrequests.PullRequest {
	switch {
	case b.cursor == "" || b.openedOn != nil && b.openedOn.key == b.cursor ||
		slices.ContainsFunc(prs, func(pr pullrequests.PullRequest) bool { return pr.Key() == b.cursor }):
		b.gone = nil
	case b.gone != nil && b.gone.Key() == b.cursor:
	case b.listed(b.cursor):
		pr := b.shown[b.cursor]
		b.gone = &pr
	default:
		b.gone = nil
	}
	if b.gone == nil {
		return prs
	}
	return pullrequests.Finish(append(prs, *b.gone), b.repos, now)
}

// isGone says the pull request is the one under the cursor that a read stopped listing.
func (b *Board) isGone(pr pullrequests.PullRequest) bool {
	return b.gone != nil && b.gone.Key() == pr.Key()
}

// spinner is the drawings the strip turns through after the rows' age while a read runs,
// each one cell wide, as gotermboard's footer turns through beside work under way.
var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinEvery is how long the spinner holds each drawing.
const spinEvery = 100 * time.Millisecond

// MoveEvery is gotermboard.MovingBoard's. The strip turns its spinner while a read runs and
// holds still otherwise, so a read never grows the footer and takes a row's line.
func (b *Board) MoveEvery() time.Duration {
	if b.reading() {
		return spinEvery
	}
	return 0
}

// StripAt is gotermboard.MovingBoard's: how each read stands, with a spinner after the
// rows' age while a read runs, and why the listing could not be kept for the next start
// where it could not.
func (b *Board) StripAt(now time.Time) []gotermboard.Line {
	line := gotermboard.ReadStatus(b.statuses(), now)
	if len(line) == 0 {
		return nil
	}
	if b.reading() {
		at := now.UnixMilli() / spinEvery.Milliseconds() % int64(len(spinner))
		line = slices.Insert(line, 1, gotermboard.C(" "+spinner[at], gotermboard.Yellow))
	}
	if b.listings != nil {
		if fault := b.listings.writeFault(); fault != "" {
			line = append(line, gotermboard.C(" · ", ""), gotermboard.C("the next start cannot draw these rows at once: "+fault, gotermboard.Yellow))
		}
	}
	return []gotermboard.Line{line}
}

func (b *Board) emptyReason() string {
	switch {
	case len(b.sources) == 0:
		return "The registry names no active repo."
	case b.failed():
		return "No provider could be read, and the line above says why."
	case b.unread():
		return "Reading the open pull requests."
	}
	switch b.from {
	case config.FromOwners:
		return "No open pull requests on the owners' repos."
	case config.FromViewer:
		return "No open pull requests on your own repos."
	}
	return "No open pull requests on the registry's repos."
}

// Snapshot is what --json prints: the rows the board draws, and how each read stands.
// Registry is empty where the board reads owners whole, and Owners names them, with @me
// for the token's own account. OpenedOn is the pull request --pr named, which may be
// merged or closed, and null without one.
type Snapshot struct {
	View         string               `json:"view"`
	Registry     string               `json:"registry"`
	Owners       []string             `json:"owners,omitempty"`
	PullRequests []Row                `json:"pull_requests"`
	OpenedOn     *Row                 `json:"opened_on"`
	Sources      []gotermboard.Status `json:"sources"`
}

// Snapshot reads what the board holds now.
func (b *Board) Snapshot(now time.Time) Snapshot {
	rows := b.Rows(now)
	return Snapshot{
		View: View, Registry: b.registry, Owners: b.owners, PullRequests: rows,
		OpenedOn: b.openedOnRow(rows, now), Sources: b.statuses(),
	}
}
