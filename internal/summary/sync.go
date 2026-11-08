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
// reach back to since, it reads every pull request last updated from since up to where it
// does. Then it reads every one updated since the last sync. Nothing is kept unless every
// search answered.
func Sync(ctx context.Context, store *Store, scope pullrequests.Scope, since, now time.Time) error {
	coverage, covered, err := store.Coverage()
	if err != nil {
		return err
	}
	next := Coverage{From: since, Synced: now}
	if !covered {
		read, err := pullrequests.ReadGitHubHistory(ctx, scope, since, time.Time{}, reviewrounds.IsRoundPost)
		if err != nil {
			return err
		}
		return store.Keep(read, next)
	}
	var read []pullrequests.HistoryRecord
	if since.Before(coverage.From) {
		if read, err = pullrequests.ReadGitHubHistory(ctx, scope, since, coverage.From, reviewrounds.IsRoundPost); err != nil {
			return err
		}
	} else {
		next.From = coverage.From
	}
	changed, err := pullrequests.ReadGitHubHistory(ctx, scope, coverage.Synced.Add(-syncOverlap), time.Time{}, reviewrounds.IsRoundPost)
	if err != nil {
		return err
	}
	return store.Keep(append(read, changed...), next)
}
