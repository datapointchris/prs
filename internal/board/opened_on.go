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
		named := launch
		switch {
		case on.source.Status().ReadAt.IsZero():
		case b.inListing(on.key):
			return on.source.Value(), "", nil
		default:
			var err error
			if named, err = pullrequests.ReadReference(ctx, ref, b.repos); err != nil {
				return named, "", err
			}
		}
		if b.diffs != nil {
			b.diffs.Prefetch(ctx, openedOnRead, pullrequests.Finish([]pullrequests.PullRequest{named.PullRequest}, b.repos, time.Now()))
		}
		return named, "", nil
	})
	b.openedOn = on
	return nil
}

// openedOnRead names the read of the pull request the board opened on among those that list
// diffs, so a provider's listing never drops its diff.
const openedOnRead = "--pr"

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

// inListing says a last good listing, open or closed, holds the pull request keyed key.
func (b *Board) inListing(key string) bool {
	for _, source := range b.listingSources() {
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

// apart is the group above the others that holds the pull request the board opened on once
// it is merged or closed, by its own read, a listing or a key here, so a jump to one lands
// on it however long ago it closed. Its title is its state.
func (b *Board) apart(pr pullrequests.PullRequest, parents map[[2]string]int, now time.Time) gotermboard.Group {
	b.drew(pr, recentlyClosed)
	return gotermboard.Group{
		Key: "opened-on", Title: gotermboard.Line{gotermboard.C(string(b.stateOf(pr)), gotermboard.Bold)},
		Count: 1, Counted: true, Gap: true,
		Rows: []gotermboard.Row{b.row(pr, parents, now)},
	}
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
	if parent := parentOf(row.PullRequest, stackParents(b.records(now))); parent != 0 {
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
