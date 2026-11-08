package summary

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// Window is the stretch the numbers count, from Since up to Until. Name is how it was given,
// such as 30d.
type Window struct {
	Name         string
	Since, Until time.Time
}

// Kinds is a count of pull requests, whoever opened them, then split by whether a person or an
// app did. One whose author is gone counts as a person's.
type Kinds struct {
	All    int `json:"all"`
	People int `json:"people"`
	Bots   int `json:"bots"`
}

func (k *Kinds) add(bot bool) {
	k.All++
	if bot {
		k.Bots++
	} else {
		k.People++
	}
}

// KindValues is a measure over pull requests, whoever opened them, then by author kind, each
// null where no pull request of that kind gives one.
type KindValues struct {
	All    *float64 `json:"all"`
	People *float64 `json:"people"`
	Bots   *float64 `json:"bots"`
}

// RepoMerges is one repo's merges within the window.
type RepoMerges struct {
	Slug   string `json:"slug"`
	Merged Kinds  `json:"merged"`
	// MedianSecondsToMerge is from opening to merging, across the repo's merges.
	MedianSecondsToMerge float64 `json:"median_seconds_to_merge"`
}

// MergeBucket is the merges that took from FromSeconds up to ToSeconds to land, from opening,
// with no ToSeconds on the last.
type MergeBucket struct {
	Label       string   `json:"label"`
	FromSeconds float64  `json:"from_seconds"`
	ToSeconds   *float64 `json:"to_seconds"`
	Merged      Kinds    `json:"merged"`
}

// PeriodMerges is the merges within the hour or the day starting at Start.
type PeriodMerges struct {
	Start  time.Time `json:"start"`
	Merged Kinds     `json:"merged"`
}

// OpenAt is the pull requests open at At.
type OpenAt struct {
	At   time.Time `json:"at"`
	Open Kinds     `json:"open"`
}

// Numbers is what the Summary tab and prs summary show for one window.
type Numbers struct {
	Window string    `json:"window"`
	Since  time.Time `json:"since"`
	Until  time.Time `json:"until"`
	// Open is the pull requests open at Until, and Opened those opened within the window,
	// whether open or closed now.
	Open           Kinds `json:"open"`
	Opened         Kinds `json:"opened"`
	Merged         Kinds `json:"merged"`
	ClosedUnmerged Kinds `json:"closed_unmerged"`
	// The times to merge run from opening to merging, over the merges within the window.
	MedianSecondsToMerge KindValues `json:"median_seconds_to_merge"`
	P90SecondsToMerge    KindValues `json:"p90_seconds_to_merge"`
	// ReviewedByRound is the merges whose first review was a review round's, and
	// ReviewedByRoundShare that as a share of the merges, from 0 to 1.
	ReviewedByRound      Kinds      `json:"merged_reviewed_by_round"`
	ReviewedByRoundShare KindValues `json:"merged_reviewed_by_round_share"`
	// Repos is each repo with a merge within the window, the most merges first.
	Repos       []RepoMerges  `json:"repos"`
	TimeToMerge []MergeBucket `json:"time_to_merge"`
	// MergedPer is hour or day, the span each of MergedOver counts, and MergedOver runs from
	// the period holding Since to the one holding Until, a day starting at local midnight.
	MergedPer  string         `json:"merged_per"`
	MergedOver []PeriodMerges `json:"merged_over"`
	// OpenOver is the open pull requests at about 120 instants from Since to Until, a whole
	// number of hours apart, and at Until.
	OpenOver []OpenAt `json:"open_over"`
}

// mergeBuckets are the edges the time to merge is counted between, as a reader asks it:
// within minutes, the hour, the morning, the day, a few days and the week.
var mergeBuckets = []struct {
	label string
	upTo  time.Duration
}{
	{"<10m", 10 * time.Minute},
	{"<1h", time.Hour},
	{"<4h", 4 * time.Hour},
	{"<1d", 24 * time.Hour},
	{"<3d", 3 * 24 * time.Hour},
	{"<1w", 7 * 24 * time.Hour},
	{"1w+", 0},
}

// hourlyUpTo is the longest window whose merges are counted by the hour, and openSamples about
// how many instants the open pull requests are counted at.
const (
	hourlyUpTo  = 48 * time.Hour
	openSamples = 120
)

// Count is the numbers for the window, from the merged and closed pull requests in closed and
// the open ones in open. A pull request in both counts as closed, because GitHub's open search
// still finds one for a while after it merges. Days start at midnight in loc.
func Count(window Window, closed []Closed, open []pullrequests.PullRequest, loc *time.Location) Numbers {
	n := Numbers{
		Window: window.Name, Since: window.Since, Until: window.Until,
		Repos: []RepoMerges{}, MergedOver: []PeriodMerges{}, OpenOver: []OpenAt{},
	}
	within := func(at time.Time) bool { return !at.Before(window.Since) && at.Before(window.Until) }
	closedKeys := map[string]bool{}
	for _, c := range closed {
		closedKeys[key(c.Slug, c.Number)] = true
	}
	var stillOpen []pullrequests.PullRequest
	for _, pr := range open {
		if !closedKeys[key(pr.Slug, pr.Number)] {
			stillOpen = append(stillOpen, pr)
		}
	}
	for _, pr := range stillOpen {
		bot := pr.Bot != nil && *pr.Bot
		if pr.CreatedAt.Before(window.Until) {
			n.Open.add(bot)
		}
		if within(pr.CreatedAt) {
			n.Opened.add(bot)
		}
	}
	var all, people, bots []float64
	repos := map[string]*RepoMerges{}
	repoTimes := map[string][]float64{}
	periods := periodsOf(window, loc)
	n.MergedPer = "day"
	if window.Until.Sub(window.Since) <= hourlyUpTo {
		n.MergedPer = "hour"
	}
	for _, start := range periods {
		n.MergedOver = append(n.MergedOver, PeriodMerges{Start: start})
	}
	n.TimeToMerge = make([]MergeBucket, len(mergeBuckets))
	var from time.Duration
	for i, bucket := range mergeBuckets {
		n.TimeToMerge[i] = MergeBucket{Label: bucket.label, FromSeconds: from.Seconds()}
		if bucket.upTo > 0 {
			to := bucket.upTo.Seconds()
			n.TimeToMerge[i].ToSeconds = &to
		}
		from = bucket.upTo
	}
	for _, c := range closed {
		bot := c.Bot != nil && *c.Bot
		if within(c.CreatedAt) {
			n.Opened.add(bot)
		}
		if c.CreatedAt.Before(window.Until) && !c.ClosedAt.Before(window.Until) {
			n.Open.add(bot)
		}
		if !c.Merged() {
			if within(c.ClosedAt) {
				n.ClosedUnmerged.add(bot)
			}
			continue
		}
		if !within(*c.MergedAt) {
			continue
		}
		n.Merged.add(bot)
		took := c.MergedAt.Sub(c.CreatedAt)
		seconds := took.Seconds()
		all = append(all, seconds)
		if bot {
			bots = append(bots, seconds)
		} else {
			people = append(people, seconds)
		}
		if c.ReviewRound {
			n.ReviewedByRound.add(bot)
		}
		repo := repos[strings.ToLower(c.Slug)]
		if repo == nil {
			repo = &RepoMerges{Slug: c.Slug}
			repos[strings.ToLower(c.Slug)] = repo
		}
		repo.Merged.add(bot)
		repoTimes[strings.ToLower(c.Slug)] = append(repoTimes[strings.ToLower(c.Slug)], seconds)
		n.TimeToMerge[bucketOf(took)].Merged.add(bot)
		if at := periodAt(periods, *c.MergedAt); at >= 0 {
			n.MergedOver[at].Merged.add(bot)
		}
	}
	n.MedianSecondsToMerge = KindValues{All: percentile(all, 0.5), People: percentile(people, 0.5), Bots: percentile(bots, 0.5)}
	n.P90SecondsToMerge = KindValues{All: percentile(all, 0.9), People: percentile(people, 0.9), Bots: percentile(bots, 0.9)}
	n.ReviewedByRoundShare = KindValues{
		All:    share(n.ReviewedByRound.All, n.Merged.All),
		People: share(n.ReviewedByRound.People, n.Merged.People),
		Bots:   share(n.ReviewedByRound.Bots, n.Merged.Bots),
	}
	for slug, repo := range repos {
		repo.MedianSecondsToMerge = *percentile(repoTimes[slug], 0.5)
		n.Repos = append(n.Repos, *repo)
	}
	slices.SortFunc(n.Repos, func(a, b RepoMerges) int {
		return cmp.Or(cmp.Compare(b.Merged.All, a.Merged.All), strings.Compare(strings.ToLower(a.Slug), strings.ToLower(b.Slug)))
	})
	for _, at := range openInstants(window) {
		sample := OpenAt{At: at}
		for _, pr := range stillOpen {
			if !pr.CreatedAt.After(at) {
				sample.Open.add(pr.Bot != nil && *pr.Bot)
			}
		}
		for _, c := range closed {
			if !c.CreatedAt.After(at) && c.ClosedAt.After(at) {
				sample.Open.add(c.Bot != nil && *c.Bot)
			}
		}
		n.OpenOver = append(n.OpenOver, sample)
	}
	return n
}

// periodsOf is the start of each hour, for a window of two days or less, or else each day,
// from the one holding the window's start to the one holding its end.
func periodsOf(window Window, loc *time.Location) []time.Time {
	since := window.Since.In(loc)
	hourly := window.Until.Sub(window.Since) <= hourlyUpTo
	start := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, loc)
	if hourly {
		start = time.Date(since.Year(), since.Month(), since.Day(), since.Hour(), 0, 0, 0, loc)
	}
	var out []time.Time
	for at := start; at.Before(window.Until); {
		out = append(out, at)
		if hourly {
			at = at.Add(time.Hour)
		} else {
			at = time.Date(at.Year(), at.Month(), at.Day()+1, 0, 0, 0, 0, loc)
		}
	}
	return out
}

// periodAt is the index of the period holding at, and -1 where none does.
func periodAt(periods []time.Time, at time.Time) int {
	i, found := slices.BinarySearchFunc(periods, at, func(start, at time.Time) int { return start.Compare(at) })
	if found {
		return i
	}
	return i - 1
}

// openInstants are about openSamples instants from the window's start, a whole number of
// hours apart, then its end.
func openInstants(window Window) []time.Time {
	span := window.Until.Sub(window.Since)
	if span <= 0 {
		return nil
	}
	step := max(time.Duration(math.Ceil(span.Hours()/openSamples))*time.Hour, time.Hour)
	var out []time.Time
	for at := window.Since; at.Before(window.Until); at = at.Add(step) {
		out = append(out, at)
	}
	return append(out, window.Until)
}

func bucketOf(took time.Duration) int {
	for i, bucket := range mergeBuckets {
		if bucket.upTo == 0 || took < bucket.upTo {
			return i
		}
	}
	return len(mergeBuckets) - 1
}

// percentile is the p-th percentile of values, joined straight between the two nearest
// ranks, and nil where there are none.
func percentile(values []float64, p float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	sorted := slices.Sorted(slices.Values(values))
	at := p * float64(len(sorted)-1)
	low := int(math.Floor(at))
	high := min(low+1, len(sorted)-1)
	v := sorted[low] + (sorted[high]-sorted[low])*(at-float64(low))
	return &v
}

func share(part, whole int) *float64 {
	if whole == 0 {
		return nil
	}
	v := float64(part) / float64(whole)
	return &v
}
