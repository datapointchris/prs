package summary

import (
	"context"
	"time"

	"github.com/datapointchris/prs/internal/pullrequests"
	"github.com/datapointchris/prs/internal/reviewrounds"
)

// syncOverlap reads back past the last sync, so a pull request GitHub stamped a moment
// before the clock here read it is still read.
const syncOverlap = 2 * time.Minute

// Sync brings the store up to now for a window starting at since. Where the store does not
// reach back to since, it reads every pull request closed from since up to where it does.
// Then it reads every closed pull request updated since the last sync, which after the first
// is a page or so. Nothing is kept unless every search answered.
func Sync(ctx context.Context, store *Store, scope pullrequests.Scope, since, now time.Time) error {
	coverage, covered, err := store.Coverage()
	if err != nil {
		return err
	}
	next := Coverage{From: since, Synced: now}
	var read []pullrequests.ClosedPullRequest
	if !covered {
		if read, err = pullrequests.ReadGitHubHistory(ctx, scope, pullrequests.ClosedBetween, since, time.Time{}); err != nil {
			return err
		}
		return store.Keep(closedOf(read), next)
	}
	if since.Before(coverage.From) {
		older, err := pullrequests.ReadGitHubHistory(ctx, scope, pullrequests.ClosedBetween, since, coverage.From)
		if err != nil {
			return err
		}
		read = append(read, older...)
	} else {
		next.From = coverage.From
	}
	changed, err := pullrequests.ReadGitHubHistory(ctx, scope, pullrequests.UpdatedBetween, coverage.Synced.Add(-syncOverlap), time.Time{})
	if err != nil {
		return err
	}
	return store.Keep(closedOf(append(read, changed...)), next)
}

func closedOf(read []pullrequests.ClosedPullRequest) []Closed {
	out := make([]Closed, 0, len(read))
	for _, pr := range read {
		out = append(out, Closed{
			Slug: pr.Slug, Number: pr.Number, Title: pr.Title, URL: pr.URL, Author: pr.Author, Bot: pr.Bot,
			State: pr.State, CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt, MergedAt: pr.MergedAt, ClosedAt: pr.ClosedAt,
			ReviewRound: pr.FirstReview != nil && reviewrounds.IsRoundPost(*pr.FirstReview),
		})
	}
	return out
}
