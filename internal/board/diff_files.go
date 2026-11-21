package board

import (
	"fmt"
	"time"

	"github.com/datapointchris/gotermboard"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// nvimDiffOf is the pull request's diff as nvim is handed it: a tab of opened's revisions in the
// checkout, its title over its MarkdownDocument, and its review threads. roundWindow is the open
// tmux window running a review round on it, where one is. head is the commit fetched for it.
func nvimDiffOf(pr pullrequests.PullRequest, detail *pullrequests.DetailState, roundWindow, checkout string, opened diffOpened, head string, now time.Time) gotermboard.NvimDiff {
	return gotermboard.NvimDiff{
		Subject:  fmt.Sprintf("%s #%d", pr.Repo, pr.Number),
		Tabs:     []gotermboard.NvimDiffTab{{Label: pr.Repo, Checkout: checkout, Revisions: opened.revisions}},
		Document: "# " + pr.Title + "\n\n" + MarkdownDocument(pr, detail, nil, roundWindow, now),
		Threads:  nvimThreadsOf(pr, detail, opened.base, head, opened.kind),
	}
}

// nvimThreadsOf is the threads file of the pull request's diff, kind, which compares base to head. A
// LEFT thread's line counts in the base branch's copy of its file, so the file holds one only
// where kind's left side is that copy, and counts the rest.
func nvimThreadsOf(pr pullrequests.PullRequest, detail *pullrequests.DetailState, base, head string, kind gotermboard.NvimDiffKind) gotermboard.NvimDiffThreads {
	threads := gotermboard.NvimDiffThreads{Diff: kind, Title: pr.Title, Slug: pr.Slug, Number: pr.Number, URL: pr.URL, Base: base, Head: head}
	if detail == nil || !detail.Held {
		return threads
	}
	threads.ThreadsRead = true
	for _, thread := range detail.Detail.Threads {
		if kind != gotermboard.NvimDiffWhole && thread.Side == "LEFT" {
			threads.ThreadsLeftOut++
			continue
		}
		comments := make([]gotermboard.NvimDiffComment, len(thread.Comments))
		for i, comment := range thread.Comments {
			comments[i] = gotermboard.NvimDiffComment(comment)
		}
		threads.Threads = append(threads.Threads, gotermboard.NvimDiffThread{
			Path: thread.Path, Line: thread.Line, Side: thread.Side, Resolved: thread.Resolved,
			Outdated: thread.Outdated, Comments: comments, CommentCount: thread.CommentCount,
		})
	}
	return threads
}
