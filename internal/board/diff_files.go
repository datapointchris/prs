package board

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// The variables nvim's diff is started with, each naming a file d wrote. An nvim config
// that finds them shows the pull request beside the diff, and one that does not ignores them.
const (
	documentVariable = "PRS_DIFF_DOCUMENT"
	threadsVariable  = "PRS_DIFF_THREADS"
)

// diffThreads is what the threads file holds, in the shape cmd/prs/diff-threads.schema.json
// gives it.
type diffThreads struct {
	Slug   string `json:"slug"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	// Base and Head are the revisions the diff compares.
	Base string `json:"base"`
	Head string `json:"head"`
	// ThreadsRead is false where no detail read was held when the diff opened, so an empty
	// Threads says nothing about the pull request.
	ThreadsRead bool                  `json:"threads_read"`
	Threads     []pullrequests.Thread `json:"threads"`
}

// diffFiles are the two files d hands nvim, named from the socket's stem.
type diffFiles struct {
	document string
	threads  string
}

func diffFilesAt(stem string) diffFiles {
	return diffFiles{document: stem + ".md", threads: stem + ".threads.json"}
}

// write puts the pull request's title over its MarkdownDocument in the document file, and its
// review threads in the threads file. roundWindow is the open tmux window running a review
// round on it, where one is. base and head are the revisions the diff compares.
func (f diffFiles) write(pr pullrequests.PullRequest, detail *pullrequests.DetailState, roundWindow, base, head string, now time.Time) error {
	threads := diffThreads{
		Slug: pr.Slug, Number: pr.Number, Title: pr.Title, URL: pr.URL, Base: base, Head: head,
		Threads: []pullrequests.Thread{},
	}
	if detail != nil && detail.Held {
		threads.ThreadsRead = true
		threads.Threads = slices.Clone(detail.Detail.Threads)
	}
	encoded, err := json.MarshalIndent(threads, "", "  ")
	if err != nil {
		return err
	}
	document := "# " + pr.Title + "\n\n" + MarkdownDocument(pr, detail, nil, roundWindow, now)
	err = errors.Join(os.WriteFile(f.document, []byte(document), 0o600), os.WriteFile(f.threads, append(encoded, '\n'), 0o600))
	if err != nil {
		f.remove()
	}
	return err
}

// env is the variables naming the files, as exec.Cmd's Env takes them.
func (f diffFiles) env() []string {
	return []string{documentVariable + "=" + f.document, threadsVariable + "=" + f.threads}
}

// remove deletes the files once nvim has exited, so nothing reads a pull request as it stood
// at an earlier diff.
func (f diffFiles) remove() {
	_ = os.Remove(f.document)
	_ = os.Remove(f.threads)
}
