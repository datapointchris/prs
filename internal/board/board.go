// Package board is prs's view on gotermboard: every open pull request as a row, its
// description a key away, and a key per action.
package board

import (
	"context"
	"time"

	"github.com/datapointchris/gotermboard"
	"github.com/datapointchris/gotermboard/markdown"

	"github.com/datapointchris/prs/internal/config"
	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/registry"
)

// View is the board's one tab, the name --view takes.
const View = "pull-requests"

// readEvery is how often each provider is read while the board is open. One GitHub read
// costs 2 of the 5,000 GraphQL points an hour, so a minute spends 120.
const readEvery = time.Minute

// Board is the open pull requests of the repos a registry names, read from each provider
// that hosts one.
type Board struct {
	registry string
	sources  []*gotermboard.Source[pullrequests.Listing]
	// paths is each repo's checkout, by slug.
	paths    map[string]string
	renderer *markdown.Renderer
	actions  []gotermboard.Action
	// shown is the last frame's pull requests by key, for a key to find the cursor's.
	shown map[string]pullrequests.PullRequest
}

// New is a board over the repos read from the registry at registryPath, with the window
// actions the config adds.
func New(registryPath string, repos []registry.Repo, windows []config.WindowAction) *Board {
	b := &Board{
		registry: registryPath,
		paths:    map[string]string{},
		renderer: markdown.New(nil),
		shown:    map[string]pullrequests.PullRequest{},
	}
	for _, repo := range repos {
		b.paths[repo.Slug] = repo.Path
	}
	readers := []struct {
		name string
		read func(context.Context, []registry.Repo) (pullrequests.Listing, error)
	}{
		{registry.GitHub, pullrequests.ReadGitHub},
		{registry.Bitbucket, pullrequests.ReadBitbucket},
	}
	for _, reader := range readers {
		hosted := registry.Hosted(repos, reader.name)
		if len(hosted) == 0 {
			continue
		}
		b.sources = append(b.sources, gotermboard.NewSource(reader.name, readEvery,
			func(ctx context.Context) (pullrequests.Listing, string, error) {
				listing, err := reader.read(ctx, hosted)
				return listing, listing.Note(), err
			}))
	}
	b.actions = b.keyTable(windows)
	return b
}

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

// pullRequests is every provider's last good read, oldest first.
func (b *Board) pullRequests(now time.Time) []pullrequests.PullRequest {
	var prs []pullrequests.PullRequest
	for _, source := range b.sources {
		prs = append(prs, source.Value().PullRequests...)
	}
	return pullrequests.Finish(prs, b.paths, now)
}

func (b *Board) statuses() []gotermboard.Status {
	statuses := make([]gotermboard.Status, len(b.sources))
	for i, source := range b.sources {
		statuses[i] = source.Status()
	}
	return statuses
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
	if len(prs) == 0 {
		group.Below = []gotermboard.Line{{gotermboard.C(b.emptyReason(), dim)}}
	}
	return gotermboard.Frame{
		Strip:   []gotermboard.Line{gotermboard.ReadStatus(b.statuses(), now)},
		Columns: columns,
		Groups:  []gotermboard.Group{group},
	}
}

func (b *Board) emptyReason() string {
	switch {
	case len(b.sources) == 0:
		return "The registry names no active repo on GitHub or Bitbucket."
	case b.unread():
		return "Reading the open pull requests."
	}
	return "No open pull requests on the registry's repos."
}

// Snapshot is what --json prints: the rows the board draws, and how each read stands.
type Snapshot struct {
	View         string               `json:"view"`
	Registry     string               `json:"registry"`
	PullRequests []SnapshotRow        `json:"pull_requests"`
	Sources      []gotermboard.Status `json:"sources"`
}

// SnapshotRow is one pull request, and the open one it is stacked on where there is one.
type SnapshotRow struct {
	pullrequests.PullRequest
	StackedOn *int `json:"stacked_on"`
}

// Snapshot reads what the board holds now.
func (b *Board) Snapshot(now time.Time) Snapshot {
	prs := b.pullRequests(now)
	parents := stackParents(prs)
	snap := Snapshot{View: View, Registry: b.registry, PullRequests: []SnapshotRow{}, Sources: b.statuses()}
	for _, pr := range prs {
		row := SnapshotRow{PullRequest: pr}
		if parent := parentOf(pr, parents); parent != 0 {
			row.StackedOn = &parent
		}
		snap.PullRequests = append(snap.PullRequests, row)
	}
	return snap
}
