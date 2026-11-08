package board

import (
	"cmp"
	"slices"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// section is the group a row sits in.
type section int

const (
	people section = iota
	bots
	recentlyClosed
)

// sections are the groups in the order they draw, each with its key and title. People's
// sit above the bots', because an app opens most of what is open and a person's waits on a
// person.
var sections = []struct {
	in         section
	key, title string
}{
	{people, "people", "people"},
	{bots, "bots", "bots"},
	{recentlyClosed, "recently-closed", "recently closed"},
}

// sectionOf is where a pull request's own record puts it: recently closed once it is merged
// or closed, with the bots where its provider says an app opened it, and otherwise with the
// people. Bitbucket's listing says neither, so its open pull requests sit with the people.
func sectionOf(pr pullrequests.PullRequest) section {
	if standing, err := pr.Standing(); err == nil && standing != pullrequests.Open {
		return recentlyClosed
	}
	if pr.IsBot() {
		return bots
	}
	return people
}

// readsDetail says the board reads the pull request's checks, files and comments: an open
// one's. A merged or closed one's is not read, because the window holds as many as --since
// reaches and each detail read costs GitHub a point.
func readsDetail(pr pullrequests.PullRequest) bool { return sectionOf(pr) != recentlyClosed }

// placed is a pull request and the section its row draws in.
type placed struct {
	pr pullrequests.PullRequest
	in section
}

// closedAt is when the pull request stopped being open. GitHub sets its close on a merge too.
func closedAt(pr pullrequests.PullRequest) time.Time {
	for _, at := range []*time.Time{pr.ClosedAt, pr.MergedAt, pr.UpdatedAt} {
		if at != nil && !at.IsZero() {
			return *at
		}
	}
	return time.Time{}
}

// inWindow says the pull request closed within the --since window, for a board that reads
// one.
func (b *Board) inWindow(pr pullrequests.PullRequest, now time.Time) bool {
	return b.since != nil && !closedAt(pr).Before(b.since.From(now))
}

// closeMadeHere is a pull request a key merged or closed, as the key left it, and when the
// key's command ended.
type closeMadeHere struct {
	pr pullrequests.PullRequest
	at time.Time
}

// records is one record of each pull request the board holds: each listing's, the closed
// ones only within the window, then extra, the newer of two that read one pull request.
//
// A merge or close a key made here stands over the record a listing read before the key,
// which still reads open. A listing read that started after the key reads the pull request's
// state as it is, so once one holds the pull request, its record stands instead. That shows a
// merge a queue only accepted, or a close reopened on the page, for what it is.
func (b *Board) records(now time.Time, extra ...pullrequests.PullRequest) []pullrequests.PullRequest {
	byKey := map[string]pullrequests.PullRequest{}
	var order []string
	// readFrom is when the newest good read holding each pull request started.
	readFrom := map[string]time.Time{}
	add := func(pr pullrequests.PullRequest, started time.Time) {
		key := pr.Key()
		if started.After(readFrom[key]) {
			readFrom[key] = started
		}
		if have, ok := byKey[key]; ok {
			byKey[key] = newer(have, pr)
			return
		}
		byKey[key] = pr
		order = append(order, key)
	}
	for _, source := range b.listingSources() {
		var started time.Time
		if status := source.Status(); status.Fault == "" && !status.ReadAt.IsZero() {
			started = status.TriedAt
		}
		closed := slices.Contains(b.closedSources, source)
		for _, pr := range b.listing(source).PullRequests {
			if !closed || b.inWindow(pr, now) {
				add(pr, started)
			}
		}
	}
	for _, pr := range extra {
		add(pr, time.Time{})
	}
	for key, here := range b.closedHere {
		if !b.inWindow(here.pr, now) || !readFrom[key].Before(here.at) {
			delete(b.closedHere, key)
			continue
		}
		have, ok := byKey[key]
		switch {
		case !ok:
			order = append(order, key)
		case sectionOf(have) == recentlyClosed:
			continue
		}
		byKey[key] = here.pr
	}
	prs := make([]pullrequests.PullRequest, 0, len(order))
	for _, key := range order {
		prs = append(prs, byKey[key])
	}
	return pullrequests.Finish(prs, b.repos, now)
}

// newer is the record of one pull request written last: the one updated later, and where
// neither says or both say the same, the closed one. A reopened pull request moves its update
// time, so an open record can still stand over a closed one read before it.
func newer(a, b pullrequests.PullRequest) pullrequests.PullRequest {
	if a.UpdatedAt != nil && b.UpdatedAt != nil && !a.UpdatedAt.Equal(*b.UpdatedAt) {
		if a.UpdatedAt.After(*b.UpdatedAt) {
			return a
		}
		return b
	}
	if sectionOf(b) == recentlyClosed {
		return b
	}
	return a
}

// ordered is the pull requests in the order the board draws them: the people's then the
// bots', each oldest first because a backlog is a gauge, then the recently closed, newest
// first.
func ordered(rows []placed) []placed {
	slices.SortStableFunc(rows, func(a, b placed) int {
		if a.in != b.in {
			return cmp.Compare(a.in, b.in)
		}
		if a.in == recentlyClosed {
			return closedAt(b.pr).Compare(closedAt(a.pr))
		}
		return a.pr.CreatedAt.Compare(b.pr.CreatedAt)
	})
	return rows
}

// placedBy puts each pull request in the section its own record names.
func placedBy(prs []pullrequests.PullRequest) []placed {
	rows := make([]placed, len(prs))
	for i, pr := range prs {
		rows[i] = placed{pr: pr, in: sectionOf(pr)}
	}
	return rows
}

// heldRow is the row under the cursor that a read moved to another section or dropped. It
// draws where the last frame drew it until the cursor leaves it, with its newest record, or
// where no read lists it any more, its last one, marked gone.
type heldRow struct {
	pr   pullrequests.PullRequest
	in   section
	gone bool
}

// hold keeps the row under the cursor in the section the last frame drew it in, so a
// refresh never moves the row under the cursor. A merge or close a key made here moves it
// at once, and the pull request the board opened on is placed by a read of its own, so
// neither is held.
func (b *Board) hold(rows []placed) []placed {
	key := b.cursor
	_, closedHere := b.closedHere[key]
	switch {
	case key == "" || closedHere || b.openedOn != nil && b.openedOn.key == key:
		b.held = nil
	case b.held != nil && b.held.pr.Key() == key:
	case b.listed(key):
		b.held = &heldRow{pr: b.shown[key], in: b.drawnIn[key]}
	default:
		b.held = nil
	}
	if b.held == nil {
		return rows
	}
	switch at := slices.IndexFunc(rows, func(p placed) bool { return p.pr.Key() == key }); {
	case at >= 0 && rows[at].in == b.held.in:
		b.held = nil
	case at >= 0:
		b.held.pr, b.held.gone = rows[at].pr, false
		rows[at].in = b.held.in
	default:
		b.held.gone = true
		rows = append(rows, placed{pr: b.held.pr, in: b.held.in})
	}
	return rows
}

// groups draws each section that holds a row, people always, with the reason there are no
// open pull requests under it where there are none. Recently closed waits until the open pull
// requests are read or have failed, so the cursor starts on an open one.
func (b *Board) groups(rows []placed, parents map[[2]string]int, now time.Time) []gotermboard.Group {
	var groups []gotermboard.Group
	open := 0
	for _, p := range rows {
		if p.in != recentlyClosed {
			open++
		}
	}
	for _, s := range sections {
		if s.in == recentlyClosed && b.unread() && !b.failed() {
			continue
		}
		group := gotermboard.Group{
			Key: s.key, Title: gotermboard.Line{gotermboard.C(s.title, gotermboard.Bold)}, Counted: true, Gap: true,
			BetweenRows: gotermboard.RuleDotted,
		}
		for _, p := range rows {
			if p.in != s.in {
				continue
			}
			b.drew(p.pr, p.in)
			group.Rows = append(group.Rows, b.row(p.pr, parents, now))
		}
		group.Count = len(group.Rows)
		switch {
		case s.in == people && open == 0:
			group.Below = []gotermboard.Line{{gotermboard.C(b.emptyReason(), dim)}}
		case s.in != people && group.Count == 0:
			continue
		}
		groups = append(groups, group)
	}
	groups[len(groups)-1].Gap = false
	return groups
}
