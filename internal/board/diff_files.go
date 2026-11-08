package board

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/datapointchris/prs/internal/pullrequests"
)

// The variables nvim's diff is started with, each naming a file its key wrote. An nvim config
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
	// Diff is which diff the key opened, and Base and Head the revisions it compares.
	Diff diffKind `json:"diff"`
	Base string   `json:"base"`
	Head string   `json:"head"`
	// ThreadsRead is false where no detail read was held when the diff opened, so an empty
	// Threads says nothing about the pull request. ThreadsLeftOut counts the threads the
	// detail read holds on a side the diff does not show.
	ThreadsRead    bool                  `json:"threads_read"`
	Threads        []pullrequests.Thread `json:"threads"`
	ThreadsLeftOut int                   `json:"threads_left_out"`
}

// diffFiles are the two files a diff in nvim is handed, named from the socket's stem.
type diffFiles struct {
	document string
	threads  string
}

func diffFilesAt(stem string) diffFiles {
	return diffFiles{document: stem + ".md", threads: stem + ".threads.json"}
}

// write puts the pull request's title over its MarkdownDocument in the document file, and its
// review threads in the threads file. roundWindow is the open tmux window running a review
// round on it, where one is. kind is the diff opened, and base and head the revisions it
// compares. A LEFT thread's line counts in the base branch's copy of its file, so the threads
// file holds one only where kind's left side is that copy.
func (f diffFiles) write(pr pullrequests.PullRequest, detail *pullrequests.DetailState, roundWindow, base, head string, kind diffKind, now time.Time) error {
	threads := diffThreads{
		Slug: pr.Slug, Number: pr.Number, Title: pr.Title, URL: pr.URL, Diff: kind, Base: base, Head: head,
		Threads: []pullrequests.Thread{},
	}
	if detail != nil && detail.Held {
		threads.ThreadsRead = true
		threads.Threads = slices.DeleteFunc(slices.Clone(detail.Detail.Threads), func(thread pullrequests.Thread) bool {
			return kind != whole && thread.Side == "LEFT"
		})
		threads.ThreadsLeftOut = len(detail.Detail.Threads) - len(threads.Threads)
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
