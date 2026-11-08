package board

import (
	"context"
	"slices"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// openedOn is the pull request the board opened on: its key, its launch read, and the
// source that reads it again on the board's timer while no listing holds it.
type openedOn struct {
	key    string
	launch pullrequests.Named
	source *gotermboard.Source[pullrequests.Named]
}

// OpenOn reads the pull request ref names, for the board to start with the cursor on it
// and its document open. OpenOn refuses one it cannot read, so the refusal comes before
// anything draws.
//
// While a listing holds it, it is that listing's row. Otherwise its own read draws it: an
// open one among the listing's rows, and a merged or closed one in a group above them. That
// read runs on the board's timer, so a pull request merged elsewhere, or on a repo the board
// does not read, moves as it does.
func (b *Board) OpenOn(ctx context.Context, ref pullrequests.Reference) error {
	launch, err := pullrequests.ReadReference(ctx, ref, b.repos)
	if err != nil {
		return err
	}
	on := &openedOn{key: launch.PullRequest.Key(), launch: launch}
	on.source = gotermboard.NewSource(ref.String(), b.every, func(ctx context.Context) (pullrequests.Named, string, error) {
		switch {
		case on.source.Status().ReadAt.IsZero():
			return launch, "", nil
		case b.inListing(on.key):
			return on.source.Value(), "", nil
		}
		named, err := pullrequests.ReadReference(ctx, ref, b.repos)
		return named, "", err
	})
	b.openedOn = on
	return nil
}

// named is the newest read of the pull request the board opened on, and false where it
// opened on none.
func (b *Board) named() (pullrequests.Named, bool) {
	on := b.openedOn
	switch {
	case on == nil:
		return pullrequests.Named{}, false
	case on.source.Status().ReadAt.IsZero():
		return on.launch, true
	}
	return on.source.Value(), true
}

func (b *Board) isOpenedOn(pr pullrequests.PullRequest) bool {
	return b.openedOn != nil && b.openedOn.key == pr.Key()
}

// inListing says a provider's last good listing holds the pull request keyed key.
func (b *Board) inListing(key string) bool {
	for _, source := range b.sources {
		if slices.ContainsFunc(source.Value().PullRequests, func(pr pullrequests.PullRequest) bool { return pr.Key() == key }) {
			return true
		}
	}
	return false
}

// startAt is the key the cursor starts on.
func (b *Board) startAt() string {
	if b.openedOn == nil {
		return ""
	}
	return b.openedOn.key
}

// withOpenedOn adds the pull request the board opened on to the listing's rows, where its
// own read says it is open, no listing holds it, and no key here has merged or closed it.
func (b *Board) withOpenedOn(prs []pullrequests.PullRequest, now time.Time) []pullrequests.PullRequest {
	named, ok := b.named()
	if !ok || named.Standing != pullrequests.Open || b.settled[b.openedOn.key] != "" || slices.ContainsFunc(prs, b.isOpenedOn) {
		return prs
	}
	return pullrequests.Finish(append(prs, named.PullRequest), b.repos, now)
}

// apart is the group above the listing's that holds the pull request the board opened on,
// where no listing holds it and it is merged or closed, by its own read or by a key here.
func (b *Board) apart(prs []pullrequests.PullRequest, parents map[[2]string]int, now time.Time) (gotermboard.Group, bool) {
	named, ok := b.named()
	if !ok || slices.ContainsFunc(prs, b.isOpenedOn) {
		return gotermboard.Group{}, false
	}
	state := b.settled[b.openedOn.key]
	if state == "" {
		switch named.Standing {
		case pullrequests.Open:
			return gotermboard.Group{}, false
		case pullrequests.Merged:
			state = merged
		case pullrequests.Closed:
			state = closed
		}
		b.settled[b.openedOn.key] = state
	}
	b.shown[b.openedOn.key] = named.PullRequest
	return gotermboard.Group{
		Key: "opened-on", Title: gotermboard.Line{gotermboard.C(string(state), gotermboard.Bold)},
		Count: 1, Counted: true, Gap: true,
		Rows: []gotermboard.Row{b.row(named.PullRequest, parents, now)},
	}, true
}

// openedOnRow is the pull request the board opened on as --json prints it, and nil where
// it opened on none.
func (b *Board) openedOnRow(rows []Row, now time.Time) *Row {
	named, ok := b.named()
	if !ok {
		return nil
	}
	if at := slices.IndexFunc(rows, func(row Row) bool { return b.isOpenedOn(row.PullRequest) }); at >= 0 {
		return &rows[at]
	}
	row := Row{PullRequest: named.PullRequest}
	if parent := parentOf(row.PullRequest, stackParents(b.pullRequests(now))); parent != 0 {
		row.StackedOn = &parent
	}
	return &row
}

// openedOnDetail is the detail the newest read of the pull request the board opened on
// found, for its document while the cache holds none.
func (b *Board) openedOnDetail(pr pullrequests.PullRequest) (pullrequests.Detail, bool) {
	named, ok := b.named()
	if !ok || !b.isOpenedOn(pr) || named.Detail == nil {
		return pullrequests.Detail{}, false
	}
	return *named.Detail, true
}
