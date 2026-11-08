package board

import (
	"fmt"
	"strings"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// passed are the check states that need nothing from anyone, so the description leaves
// them out and lists what is failing or still running.
var passed = map[string]bool{"SUCCESS": true, "NEUTRAL": true, "SKIPPED": true}

// detailLines are what the pull request's own read adds under its description: each
// review and the commit it was against, who is still asked, each check that has not
// passed, and how much conversation there is. A host that reads no detail adds nothing.
func (b *Board) detailLines(pr pullrequests.PullRequest) []gotermboard.Line {
	provider, err := pullrequests.ProviderOf(pr.Provider)
	if err != nil || provider.Detail == nil {
		return nil
	}
	state := b.details.Of(pr)
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
	lines := []gotermboard.Line{{}, {gotermboard.C("Reviews", gotermboard.Bold)}}
	for _, review := range d.Reviews {
		when := ""
		if review.SubmittedAt != nil {
			when = "  " + gotermboard.Ago(now.Sub(*review.SubmittedAt))
		}
		lines = append(lines, gotermboard.Line{
			gotermboard.C(fmt.Sprintf("  %-10s ", words(review.State)), reviewStateCode(review.State)),
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
		if !passed[check.State] {
			waiting = append(waiting, check)
		}
	}
	if len(waiting) > 0 {
		lines = append(lines, gotermboard.Line{}, gotermboard.Line{gotermboard.C("Checks not passing", gotermboard.Bold)})
		for _, check := range waiting {
			code := gotermboard.Yellow
			if check.State == "FAILURE" || check.State == "ERROR" || check.State == "TIMED_OUT" {
				code = gotermboard.Red
			}
			lines = append(lines, gotermboard.Line{gotermboard.C(fmt.Sprintf("  %-10s ", words(check.State)), code), gotermboard.C(check.Name, "")})
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

// words is an enum as words, CHANGES_REQUESTED as "changes requested".
func words(enum string) string { return strings.ToLower(strings.ReplaceAll(enum, "_", " ")) }

func reviewStateCode(state string) string {
	switch state {
	case "APPROVED":
		return gotermboard.Green
	case "CHANGES_REQUESTED":
		return gotermboard.Red
	}
	return gotermboard.Cyan
}

// short is a commit's first seven characters, as git abbreviates one.
func short(oid string) string {
	if oid == "" {
		return "a commit gone from the branch"
	}
	return oid[:min(len(oid), 7)]
}
