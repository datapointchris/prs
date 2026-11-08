package board

import (
	"fmt"
	"strings"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// detailLines are what the pull request's own read adds under its description: each
// review and the commit it was against, who is still asked, each check that has not
// passed, and how much conversation there is. A host that reads no detail adds nothing.
func (b *Board) detailLines(pr pullrequests.PullRequest) []gotermboard.Line {
	provider, err := pullrequests.ProviderOf(pr.Provider)
	if err != nil || provider.Detail == nil {
		return nil
	}
	if b.isGone(pr) {
		return []gotermboard.Line{{}, {gotermboard.C("It is gone from the open pull requests, so its reviews and checks are not read.", dim)}}
	}
	state := b.details.Of(pr)
	if detail, own := b.openedOnDetail(pr); own && !state.Held {
		state.Detail, state.Held = detail, true
	}
	if !state.Held {
		switch {
		case state.Fault != "":
			return []gotermboard.Line{{}, {gotermboard.C("Could not read its reviews and checks: "+state.Fault, dim)}}
		case b.readDetails:
			return []gotermboard.Line{{}, {gotermboard.C("Reading its reviews and checks.", dim)}}
		}
		return nil
	}
	d := state.Detail
	now := time.Now()
	lines := []gotermboard.Line{{}}
	if state.Fault != "" {
		lines = append(lines, gotermboard.Line{gotermboard.C(fmt.Sprintf(
			"Read %s ago. The newest read failed: %s", gotermboard.Ago(now.Sub(d.ReadAt)), state.Fault), dim)}, gotermboard.Line{})
	}
	lines = append(lines, gotermboard.Line{gotermboard.C("Reviews", gotermboard.Bold)})
	for _, review := range d.Reviews {
		when := ""
		if review.SubmittedAt != nil {
			when = "  " + gotermboard.Ago(now.Sub(*review.SubmittedAt))
		}
		lines = append(lines, gotermboard.Line{
			gotermboard.C(fmt.Sprintf("  %-10s ", words(review.State)), reviewMeaning(review.State).code),
			gotermboard.C(orDash(review.Author), ""),
			gotermboard.C(fmt.Sprintf("  at %s%s", short(review.CommitOID), when), dim),
		})
	}
	if len(d.Reviews) == 0 {
		lines = append(lines, gotermboard.Line{gotermboard.C("  none yet", dim)})
	}
	if len(d.ReviewRequests) > 0 {
		lines = append(lines, gotermboard.Line{gotermboard.C("  asked      ", gotermboard.Cyan), gotermboard.C(strings.Join(d.ReviewRequests, ", "), "")})
	}
	var waiting []pullrequests.Check
	for _, check := range d.Checks {
		if !checkMeaning(check.State).passed {
			waiting = append(waiting, check)
		}
	}
	if len(waiting) > 0 {
		lines = append(lines, gotermboard.Line{}, gotermboard.Line{gotermboard.C("Checks not passing", gotermboard.Bold)})
		for _, check := range waiting {
			lines = append(lines, gotermboard.Line{
				gotermboard.C(fmt.Sprintf("  %-10s ", words(check.State)), checkMeaning(check.State).code),
				gotermboard.C(check.Name, ""),
			})
		}
	}
	unresolved := 0
	for _, thread := range d.Threads {
		if !thread.Resolved {
			unresolved++
		}
	}
	return append(lines, gotermboard.Line{}, gotermboard.Line{gotermboard.C(fmt.Sprintf(
		"%d comments · %d review threads, %d unresolved · %d files · %d commits",
		d.CommentCount, d.ThreadCount, unresolved, d.FileCount, d.CommitCount), dim)})
}

// short is a commit's first seven characters, as git abbreviates one.
func short(oid string) string {
	if oid == "" {
		return "a commit gone from the branch"
	}
	return oid[:min(len(oid), 7)]
}
