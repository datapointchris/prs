// Package board is prs's view on gotermboard: every open pull request as a row, its
// description a key away, and a key per action.
package board

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermboard/markdown"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/registry"
)

// View is the board's one tab, the name --view takes.
const View = "pull-requests"

// readEvery is how often each provider is read while the board is open. A GitHub read is
// one GraphQL request, whose cost `rateLimit { cost }` on the query reports, and a read a
// minute stays well inside GitHub's hourly allowance.
const readEvery = time.Minute

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
}

// New is a board over repos, with the window actions the config adds. It refuses a window
// action on a key the board already binds.
func New(repos Repos, windows []config.WindowAction) (*Board, error) {
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
		shown:    map[string]pullrequests.PullRequest{},
		settled:  map[string]settledState{},
		diffed:   map[string]string{},
		details:  pullrequests.NewDetails(),
	}
	for _, provider := range pullrequests.Providers {
		scope := pullrequests.Scope{Repos: registry.Hosted(repos.Repos, provider.Name)}
		if provider.ReadsOwners {
			scope.Owners = repos.Owners
		}
		if scope.Empty() {
			continue
		}
		var source *gotermboard.Source[pullrequests.Listing]
		source = gotermboard.NewSource(provider.Name, readEvery,
			func(ctx context.Context) (pullrequests.Listing, string, error) {
				listing, err := provider.Read(ctx, scope, source.Value())
				if err == nil && b.readDetails {
					b.details.Prefetch(ctx, provider, listing.PullRequests)
				}
				return listing, listing.Note(), err
			})
		b.sources = append(b.sources, source)
	}
	b.actions = b.keyTable(windows)
	return b, nil
}

// ReadDetails has each listing that lands read its rows' details in the background, for
// the description to show. A board printed once leaves it off, since nothing waits for them.
func (b *Board) ReadDetails() { b.readDetails = true }

// Sources is gotermboard.Board's.
func (b *Board) Sources() []gotermboard.Refresher {
	refreshers := make([]gotermboard.Refresher, len(b.sources))
	for i, source := range b.sources {
		refreshers[i] = source
	}
	return refreshers
}

// Groupings is gotermboard.Board's. The board has one.
func (b *Board) Groupings() []string { return nil }

// pullRequests is every provider's last good read, and an open pull request the board
// opened on that no read holds, oldest first.
func (b *Board) pullRequests(now time.Time) []pullrequests.PullRequest {
	var prs []pullrequests.PullRequest
	for _, source := range b.sources {
		prs = append(prs, source.Value().PullRequests...)
	}
	return pullrequests.Finish(b.withOpenedOn(prs), b.repos, now)
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

func (b *Board) statuses() []gotermboard.Status {
	statuses := make([]gotermboard.Status, len(b.sources))
	for i, source := range b.sources {
		statuses[i] = source.Status()
	}
	return statuses
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

// unread says no provider has answered yet.
func (b *Board) unread() bool {
	for _, source := range b.sources {
		if !source.Status().ReadAt.IsZero() {
			return false
		}
	}
	return len(b.sources) > 0
}

// failed says every provider was tried and none could be read.
func (b *Board) failed() bool {
	for _, status := range b.statuses() {
		if status.Fault == "" || !status.ReadAt.IsZero() {
			return false
		}
	}
	return len(b.sources) > 0
}

// Frame is gotermboard.Board's: one group of every open pull request, oldest first.
func (b *Board) Frame(now time.Time, _ string) gotermboard.Frame {
	prs := b.pullRequests(now)
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
	if apart, ok := b.apart(parents, now); ok {
		groups = append([]gotermboard.Group{apart}, groups...)
	}
	maps.DeleteFunc(b.settled, func(key string, _ settledState) bool { return !b.listed(key) })
	maps.DeleteFunc(b.diffed, func(key, _ string) bool { return !b.listed(key) })
	if len(prs) == 0 {
		group.Below = []gotermboard.Line{{gotermboard.C(b.emptyReason(), dim)}}
	}
	return gotermboard.Frame{
		Strip:   []gotermboard.Line{gotermboard.ReadStatus(b.statuses(), now)},
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
