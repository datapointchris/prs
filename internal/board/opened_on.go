package board

import (
	"context"
	"slices"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// openState is the state an open pull request reads on both providers.
const openState = "OPEN"

// openedOn is the pull request the board opened on, as its own read found it, with its
// detail where the provider reads one.
type openedOn struct {
	pr     pullrequests.PullRequest
	detail *pullrequests.Detail
}

// OpenOn reads the pull request ref names, for the board to start with the cursor on it
// and its description open. An open one sits among the listing's rows. A merged or closed
// one sits in a group of its own above them, as its one read found it. OpenOn refuses one
// it cannot read, so the refusal comes before anything draws.
func (b *Board) OpenOn(ctx context.Context, ref pullrequests.Reference) error {
	pr, detail, err := pullrequests.ReadReference(ctx, ref, b.repos)
	if err != nil {
		return err
	}
	b.openedOn = &openedOn{pr: pr, detail: detail}
	switch pr.State {
	case "MERGED":
		b.settled[pr.Key()] = merged
	case "CLOSED":
		b.settled[pr.Key()] = closed
	}
	return nil
}

func (b *Board) isOpenedOn(pr pullrequests.PullRequest) bool {
	return b.openedOn != nil && b.openedOn.pr.Key() == pr.Key()
}

// startAt is the key the cursor starts on.
func (b *Board) startAt() string {
	if b.openedOn == nil {
		return ""
	}
	return b.openedOn.pr.Key()
}

// withOpenedOn adds an open pull request the board opened on to the listing's, where the
// listing does not hold it: before its provider's first read lands, or on a repo the board
// does not read.
func (b *Board) withOpenedOn(prs []pullrequests.PullRequest) []pullrequests.PullRequest {
	on := b.openedOn
	if on == nil || on.pr.State != openState || slices.ContainsFunc(prs, b.isOpenedOn) {
		return prs
	}
	return append(prs, on.pr)
}

// apart is the group above the listing's that holds the pull request the board opened on,
// where that one is merged or closed.
func (b *Board) apart(parents map[[2]string]int, now time.Time) (gotermboard.Group, bool) {
	on := b.openedOn
	if on == nil || on.pr.State == openState {
		return gotermboard.Group{}, false
	}
	b.shown[on.pr.Key()] = on.pr
	return gotermboard.Group{
		Key: "opened-on", Title: gotermboard.Line{gotermboard.C(words(on.pr.State), gotermboard.Bold)},
		Count: 1, Counted: true, Gap: true,
		Rows: []gotermboard.Row{b.row(on.pr, parents, now)},
	}, true
}

// openedOnRow is the pull request the board opened on as --json prints it, and nil where
// it opened on none.
func (b *Board) openedOnRow(rows []Row, now time.Time) *Row {
	if b.openedOn == nil {
		return nil
	}
	if at := slices.IndexFunc(rows, func(row Row) bool { return b.isOpenedOn(row.PullRequest) }); at >= 0 {
		return &rows[at]
	}
	row := Row{PullRequest: b.openedOn.pr}
	if parent := parentOf(row.PullRequest, stackParents(b.pullRequests(now))); parent != 0 {
		row.StackedOn = &parent
	}
	return &row
}

// openedOnDetail is the detail the board's own read of the pull request it opened on
// found, for its description while the cache holds none.
func (b *Board) openedOnDetail(pr pullrequests.PullRequest) (pullrequests.Detail, bool) {
	if !b.isOpenedOn(pr) || b.openedOn.detail == nil {
		return pullrequests.Detail{}, false
	}
	return *b.openedOn.detail, true
}
