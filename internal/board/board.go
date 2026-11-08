// Package board is prs's view on gotermboard: every open pull request as a row, the
// people's above the bots', those recently closed below, each one's document beside the
// list or a key away, and a key per action.
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
// that hosts one, and those recently merged or closed.
type Board struct {
	from     string
	registry string
	owners   []string
	// sources read each provider's open pull requests, and closedSources those merged or
	// closed within since, where ReadClosed is called and the provider's client can say when.
	sources       []*gotermboard.Source[pullrequests.Listing]
	closedSources []*gotermboard.Source[pullrequests.Listing]
	since         *pullrequests.Since
	// scopes is what each provider's read covers, by provider name.
	scopes   map[string]pullrequests.Scope
	repos    []registry.Repo
	renderer *markdown.Renderer
	actions  []gotermboard.Action
	// shown is the last frame's pull requests by key, for a key to find the cursor's, and
	// drawnIn the section each was drawn in.
	shown   map[string]pullrequests.PullRequest
	drawnIn map[string]section
	// closedHere is each pull request a key merged or closed, by key.
	closedHere map[string]closeMadeHere
	// diffed is the head commit d last fetched for a pull request, by key, which its merge
	// pins.
	diffed map[string]string
	// documents is each shown pull request's MarkdownDocument and its inputs, by key.
	documents map[string]documentMemo
	// working is every command a key started that has not ended, oldest first.
	working   []underWay
	workCount int
	// details holds each row's detail, read after each listing once ReadDetails is called.
	details     *pullrequests.Details
	readDetails bool
	// diffs holds each row's diff, read after each listing, and is nil until ReadDiffs is
	// called.
	diffs *pullrequests.Diffs
	// openedOn is the pull request OpenOn read, and nil where the board opened on none.
	openedOn *openedOn
	// every is how often each read runs again while the board is open.
	every time.Duration
	// listings keeps each provider's listing on disk once KeepListings is called, and holds
	// the last run's, which a provider's rows come from until its first read here lands.
	listings *listingCache
	// cursor is the key of the row under the cursor, and held that row where a read moved or
	// dropped it.
	cursor string
	held   *heldRow
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
		from:       repos.From,
		registry:   repos.Registry,
		owners:     repos.Owners,
		repos:      repos.Repos,
		renderer:   markdown.New(nil),
		every:      every,
		shown:      map[string]pullrequests.PullRequest{},
		drawnIn:    map[string]section{},
		closedHere: map[string]closeMadeHere{},
		diffed:     map[string]string{},
		documents:  map[string]documentMemo{},
		details:    pullrequests.NewDetails(),
		scopes:     map[string]pullrequests.Scope{},
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
		b.sources = append(b.sources, b.listingSource(provider, provider.Name, every,
			func(ctx context.Context, last pullrequests.Listing) (pullrequests.Listing, error) {
				return provider.Read(ctx, scope, last)
			}))
	}
	b.actions = b.keyTable(windows)
	return b, nil
}

// listingSource reads a provider's listing with read every interval, from its last good
// one, under name. Each listing that lands has its open rows' details read, once
// ReadDetails is called, and is kept under name for the next start, once KeepListings is.
func (b *Board) listingSource(provider pullrequests.Provider, name string, every time.Duration,
	read func(ctx context.Context, last pullrequests.Listing) (pullrequests.Listing, error),
) *gotermboard.Source[pullrequests.Listing] {
	var source *gotermboard.Source[pullrequests.Listing]
	source = gotermboard.NewSource(name, every, func(ctx context.Context) (pullrequests.Listing, string, error) {
		listing, err := read(ctx, b.listing(source))
		if err != nil {
			return listing, "", err
		}
		if b.readDetails {
			b.details.Prefetch(ctx, provider, slices.DeleteFunc(slices.Clone(listing.PullRequests), func(pr pullrequests.PullRequest) bool {
				return !readsDetail(pr)
			}))
		}
		if b.diffs != nil {
			b.diffs.Prefetch(ctx, name, pullrequests.Finish(slices.Clone(listing.PullRequests), b.repos, time.Now()))
		}
		if b.listings != nil {
			b.listings.keep(name, listing, time.Now())
		}
		return listing, listing.Note(), nil
	})
	return source
}

// ReadClosed has the board read, every interval, the pull requests merged or closed within
// since, from each provider whose client can say when one closed, for the recently closed
// group. prs list leaves it off, because it prints the open pull requests only.
func (b *Board) ReadClosed(since pullrequests.Since, every time.Duration) {
	b.since = &since
	for _, provider := range pullrequests.Providers {
		scope, ok := b.scopes[provider.Name]
		if !ok || provider.ReadClosed == nil {
			continue
		}
		b.closedSources = append(b.closedSources, b.listingSource(provider, provider.Name+" closed", every,
			func(ctx context.Context, last pullrequests.Listing) (pullrequests.Listing, error) {
				return provider.ReadClosed(ctx, scope, since.From(time.Now()), last)
			}))
	}
}

// ReadDetails has each listing that lands read its rows' details in the background, for the
// document's sections that detail fills. A board printed once leaves it off, since nothing
// waits for them.
func (b *Board) ReadDetails() { b.readDetails = true }

// ReadDiffs has each listing that lands read its rows' diffs in the background, for the
// document's Diff section, which shows one whole up to maxLines changed lines. A diff is
// read in the repo's checkout, fetched as d fetches it, and from GitHub where the repo is
// not checked out. A board printed once leaves it off, since nothing waits for them.
func (b *Board) ReadDiffs(maxLines int) { b.diffs = pullrequests.NewDiffs(maxLines) }

// KeepListings has the board draw the listings the last run on these repos kept in dir until
// each provider's first read lands, and keep each listing that lands there for the next run. A
// board printed once leaves it off, since it waits for its reads.
func (b *Board) KeepListings(dir string) { b.listings = openListingCache(dir, b.scopes) }

// keptFor is the listing the last run kept for the source's provider, while it stands in for
// a read here: until the provider's first read lands.
func (b *Board) keptFor(source *gotermboard.Source[pullrequests.Listing]) (keptListing, bool) {
	status := source.Status()
	if b.listings == nil || !status.ReadAt.IsZero() {
		return keptListing{}, false
	}
	return b.listings.listing(status.Name)
}

// listing is a provider's rows now: its last good read, or before its first, the listing
// the last run kept.
func (b *Board) listing(source *gotermboard.Source[pullrequests.Listing]) pullrequests.Listing {
	if kept, ok := b.keptFor(source); ok {
		return kept.listing()
	}
	return source.Value()
}

// listingSources is every listing's source: the open ones, then the closed.
func (b *Board) listingSources() []*gotermboard.Source[pullrequests.Listing] {
	return slices.Concat(b.sources, b.closedSources)
}

// Sources is gotermboard.Board's.
func (b *Board) Sources() []gotermboard.Refresher {
	var refreshers []gotermboard.Refresher
	for _, source := range b.listingSources() {
		refreshers = append(refreshers, source)
	}
	if b.openedOn != nil {
		refreshers = append(refreshers, b.openedOn.source)
	}
	return refreshers
}

// Groupings is gotermboard.Board's. The board has one.
func (b *Board) Groupings() []string { return nil }

// Row is one pull request as both --json outputs print it, with the open pull request it
// is stacked on where there is one. cmd/prs/list.schema.json is its shape.
type Row struct {
	pullrequests.PullRequest
	StackedOn *int `json:"stacked_on"`
}

// Rows is every pull request the listings hold, in the order the board draws them, and
// empty rather than nil, so no rows print as [].
func (b *Board) Rows(now time.Time) []Row {
	prs := b.records(now)
	parents := stackParents(prs)
	rows := make([]Row, 0, len(prs))
	for _, p := range ordered(placedBy(prs)) {
		row := Row{PullRequest: p.pr}
		if parent := parentOf(p.pr, parents); parent != 0 {
			row.StackedOn = &parent
		}
		rows = append(rows, row)
	}
	return rows
}

// statusOf is how one listing's read stands. A provider whose rows still come from the last
// run's listing reads as read when that listing was, with that listing's notes, so the strip
// gives the rows' age and what they left out, and names a first read that fails as failing
// on rows that old.
func (b *Board) statusOf(source *gotermboard.Source[pullrequests.Listing]) gotermboard.Status {
	status := source.Status()
	if kept, ok := b.keptFor(source); ok {
		status.ReadAt = kept.ReadAt
		if status.Note == "" {
			status.Note = kept.listing().Note()
		}
	}
	return status
}

// statuses is how each read stands: each listing's, then the pull request the board opened
// on.
func (b *Board) statuses() []gotermboard.Status {
	var statuses []gotermboard.Status
	for _, source := range b.listingSources() {
		statuses = append(statuses, b.statusOf(source))
	}
	if b.openedOn != nil {
		statuses = append(statuses, b.openedOn.source.Status())
	}
	return statuses
}

// reading says a read is under way.
func (b *Board) reading() bool {
	return slices.ContainsFunc(b.listingSources(), func(source *gotermboard.Source[pullrequests.Listing]) bool {
		return source.Status().Reading
	})
}

// Gaps names each read of the open pull requests, or of the one the board opened on, whose
// newest attempt failed, or that read and noted pull requests it left out, with why. A note
// leaves rows out as a failure does, so both count.
func (b *Board) Gaps() []string {
	var statuses []gotermboard.Status
	for _, source := range b.sources {
		statuses = append(statuses, b.statusOf(source))
	}
	if b.openedOn != nil {
		statuses = append(statuses, b.openedOn.source.Status())
	}
	var gaps []string
	for _, status := range statuses {
		switch {
		case status.Fault != "":
			gaps = append(gaps, status.Name+": "+status.Fault)
		case status.Note != "":
			gaps = append(gaps, status.Name+": "+status.Note)
		}
	}
	return gaps
}

// ClosedFaults names each read of the recently closed whose newest attempt failed, with why.
// A full page is not one, because the read sorts the newest first so a page keeps them.
func (b *Board) ClosedFaults() []string {
	var faults []string
	for _, source := range b.closedSources {
		if status := source.Status(); status.Fault != "" {
			faults = append(faults, status.Name+": "+status.Fault)
		}
	}
	return faults
}

// unread says no provider's open pull requests have been read yet, here or in the run that
// kept its listing.
func (b *Board) unread() bool {
	for _, source := range b.sources {
		if !b.statusOf(source).ReadAt.IsZero() {
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

// Frame is gotermboard.Board's: the people's open pull requests, then the bots', then those
// merged or closed within the window, each a group whose header counts it and folds it. The
// merged or closed pull request the board opened on sits in a group of its own above them.
func (b *Board) Frame(now time.Time, _ string) gotermboard.Frame {
	var extra []pullrequests.PullRequest
	if named, ok := b.named(); ok {
		extra = append(extra, named.PullRequest)
	}
	prs := b.records(now, extra...)
	parents := stackParents(prs)
	rows := ordered(b.hold(placedBy(prs)))
	clear(b.shown)
	clear(b.drawnIn)
	var groups []gotermboard.Group
	if at := slices.IndexFunc(rows, func(p placed) bool { return b.isOpenedOn(p.pr) && p.in == recentlyClosed }); at >= 0 {
		groups = append(groups, b.apart(rows[at].pr, parents, now))
		rows = slices.Delete(rows, at, at+1)
	}
	groups = append(groups, b.groups(rows, parents, now)...)
	maps.DeleteFunc(b.diffed, func(key, _ string) bool { return !b.listed(key) })
	maps.DeleteFunc(b.documents, func(key string, _ documentMemo) bool { return !b.listed(key) })
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

// MoveEvery is gotermboard.MovingBoard's. The strip turns its spinner while a read runs and
// holds still otherwise, so a read never grows the footer and takes a row's line.
func (b *Board) MoveEvery() time.Duration {
	if b.reading() {
		return gotermboard.SpinEvery
	}
	return 0
}

// StripAt is gotermboard.MovingBoard's: how each read stands, with a spinner after the
// rows' age while a read runs, and why the listing cache could not be read or written, until
// a write succeeds.
func (b *Board) StripAt(now time.Time) []gotermboard.Line {
	line := gotermboard.ReadStatus(b.statuses(), now)
	if len(line) == 0 {
		return nil
	}
	if b.reading() {
		line = slices.Insert(line, 1, gotermboard.C(" "+gotermboard.SpinnerAt(now), gotermboard.Yellow))
	}
	if b.listings != nil {
		if fault := b.listings.fault(); fault != "" {
			line = append(line, gotermboard.C(" · ", ""), gotermboard.C(fault, gotermboard.Yellow))
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
// PullRequests is the open ones, as prs list --json prints them, and RecentlyClosed those
// merged or closed within --since, newest first, each a row of the same shape. Registry is
// empty where the board reads owners whole, and Owners names them, with @me for the token's
// own account. OpenedOn is the pull request --pr named, which may be merged or closed, and
// null without one.
type Snapshot struct {
	View           string               `json:"view"`
	Registry       string               `json:"registry"`
	Owners         []string             `json:"owners,omitempty"`
	PullRequests   []Row                `json:"pull_requests"`
	RecentlyClosed []Row                `json:"recently_closed"`
	OpenedOn       *Row                 `json:"opened_on"`
	Sources        []gotermboard.Status `json:"sources"`
}

// Snapshot reads what the board holds now.
func (b *Board) Snapshot(now time.Time) Snapshot {
	rows := b.Rows(now)
	at := slices.IndexFunc(rows, func(row Row) bool { return sectionOf(row.PullRequest) == recentlyClosed })
	if at < 0 {
		at = len(rows)
	}
	return Snapshot{
		View: View, Registry: b.registry, Owners: b.owners,
		PullRequests: rows[:at], RecentlyClosed: slices.Clip(rows[at:]),
		OpenedOn: b.openedOnRow(rows, now), Sources: b.statuses(),
	}
}
