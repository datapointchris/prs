package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
)

// githubDetailQuery reads one pull request by repo and number, whatever its state, with
// the row the listing reads and everything the listing leaves out. The lists alias the
// row's own reviews and commits fields, which take other arguments.
const githubDetailQuery = `
query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      ...Row
      reviewRequests(first: 20) {
        nodes { requestedReviewer { __typename ... on User { login } ... on Bot { login } ... on Mannequin { login } ... on Team { slug } } }
      }
      submittedReviews: reviews(last: 100) {
        totalCount
        nodes { author { login } state body submittedAt url commit { oid } }
      }
      comments(last: 50) { totalCount nodes { author { login } body createdAt url } }
      reviewThreads(last: 50) {
        totalCount
        nodes { path line diffSide isResolved isOutdated comments(first: 30) { totalCount nodes { author { login } body createdAt url } } }
      }
      files(first: 20) { totalCount nodes { path additions deletions changeType } }
      history: commits(last: 100) {
        totalCount
        nodes { commit { oid messageHeadline committedDate author { name user { login } } } }
      }
      head: commits(last: 1) {
        nodes { commit { statusCheckRollup { contexts(first: 100) { nodes {
          __typename
          ... on CheckRun { name status conclusion detailsUrl }
          ... on StatusContext { context state targetUrl }
        } } } } }
      }
    }
  }
}` + githubRow

type githubActor struct {
	Login string `json:"login"`
}

func (a *githubActor) login() string {
	if a == nil {
		return ""
	}
	return a.Login
}

type githubComments struct {
	TotalCount int `json:"totalCount"`
	Nodes      []struct {
		Author    *githubActor `json:"author"`
		Body      string       `json:"body"`
		CreatedAt time.Time    `json:"createdAt"`
		URL       string       `json:"url"`
	} `json:"nodes"`
}

type githubDetailNode struct {
	githubNode
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer *struct {
				Login string `json:"login"`
				Slug  string `json:"slug"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	SubmittedReviews struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Author      *githubActor `json:"author"`
			State       string       `json:"state"`
			Body        string       `json:"body"`
			SubmittedAt *time.Time   `json:"submittedAt"`
			URL         string       `json:"url"`
			Commit      *struct {
				OID string `json:"oid"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"submittedReviews"`
	Comments      githubComments `json:"comments"`
	ReviewThreads struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Path       string         `json:"path"`
			Line       *int           `json:"line"`
			DiffSide   string         `json:"diffSide"`
			IsResolved bool           `json:"isResolved"`
			IsOutdated bool           `json:"isOutdated"`
			Comments   githubComments `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	Files struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Path       string `json:"path"`
			Additions  int    `json:"additions"`
			Deletions  int    `json:"deletions"`
			ChangeType string `json:"changeType"`
		} `json:"nodes"`
	} `json:"files"`
	History struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Commit struct {
				OID             string    `json:"oid"`
				MessageHeadline string    `json:"messageHeadline"`
				CommittedDate   time.Time `json:"committedDate"`
				Author          struct {
					Name string       `json:"name"`
					User *githubActor `json:"user"`
				} `json:"author"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"history"`
	Head struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []struct {
							Name       string `json:"name"`
							Status     string `json:"status"`
							Conclusion string `json:"conclusion"`
							DetailsURL string `json:"detailsUrl"`
							Context    string `json:"context"`
							State      string `json:"state"`
							TargetURL  string `json:"targetUrl"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"head"`
}

// ReadGitHubPullRequest is one pull request's row and detail, read by its owner/name slug
// and number, open or not. The row has no Path or AgeDays until Finish sets them.
func ReadGitHubPullRequest(ctx context.Context, slug string, number int) (PullRequest, Detail, error) {
	owner, name, ok := strings.Cut(slug, "/")
	if !ok {
		return PullRequest{}, Detail{}, fmt.Errorf("%q is not owner/name", slug)
	}
	client, host, err := githubClient(ctx)
	if err != nil {
		return PullRequest{}, Detail{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var data struct {
		Repository *struct {
			PullRequest *githubDetailNode `json:"pullRequest"`
		} `json:"repository"`
	}
	variables := map[string]any{"owner": owner, "name": name, "number": number}
	err = client.DoWithContext(ctx, githubDetailQuery, variables, &data)
	var refused *api.GraphQLError
	if err != nil && !errors.As(err, &refused) {
		return PullRequest{}, Detail{}, githubFault(host, err)
	}
	if data.Repository == nil || data.Repository.PullRequest == nil {
		if refused != nil {
			return PullRequest{}, Detail{}, fmt.Errorf("GitHub has no pull request %s#%d: %w", slug, number, refused)
		}
		return PullRequest{}, Detail{}, fmt.Errorf("GitHub shows this token no pull request %s#%d", slug, number)
	}
	node := data.Repository.PullRequest
	if node.Number == nil {
		return PullRequest{}, Detail{}, fmt.Errorf("GitHub's answer for %s#%d holds no pull request", slug, number)
	}
	pr := githubPullRequest(node.githubNode)
	detail := githubDetail(*node)
	detail.ChecksRollup = pr.Checks
	return pr, detail, nil
}

// readGitHubReference is Provider.ReadOne for GitHub.
func readGitHubReference(ctx context.Context, ref Reference) (PullRequest, *Detail, error) {
	pr, detail, err := ReadGitHubPullRequest(ctx, ref.Slug, ref.Number)
	if err != nil {
		return PullRequest{}, nil, err
	}
	return pr, &detail, nil
}

// readGitHubDetail is Provider.Detail for GitHub.
func readGitHubDetail(ctx context.Context, pr PullRequest) (Detail, error) {
	_, detail, err := ReadGitHubPullRequest(ctx, pr.Slug, pr.Number)
	return detail, err
}

func githubDetail(node githubDetailNode) Detail {
	detail := Detail{
		HeadOID: node.HeadRefOid, UpdatedAt: node.UpdatedAt, ReadAt: time.Now().UTC(),
		ReviewRequests: []string{}, Reviews: []Review{}, Threads: []Thread{}, Files: []File{}, Commits: []Commit{}, Checks: []Check{},
		ReviewCount: node.SubmittedReviews.TotalCount, Comments: githubCommentList(node.Comments),
		CommentCount: node.Comments.TotalCount, ThreadCount: node.ReviewThreads.TotalCount,
		FileCount: node.Files.TotalCount, CommitCount: node.History.TotalCount,
	}
	for _, request := range node.ReviewRequests.Nodes {
		// A team has a slug where a person and an app have a login.
		if who := request.RequestedReviewer; who != nil {
			detail.ReviewRequests = append(detail.ReviewRequests, who.Login+who.Slug)
		}
	}
	for _, review := range node.SubmittedReviews.Nodes {
		r := Review{Author: review.Author.login(), State: review.State, Body: review.Body, SubmittedAt: review.SubmittedAt, URL: review.URL}
		if review.Commit != nil {
			r.CommitOID = review.Commit.OID
		}
		detail.Reviews = append(detail.Reviews, r)
	}
	for _, thread := range node.ReviewThreads.Nodes {
		detail.Threads = append(detail.Threads, Thread{
			Path: thread.Path, Line: thread.Line, Side: thread.DiffSide, Resolved: thread.IsResolved, Outdated: thread.IsOutdated,
			Comments: githubCommentList(thread.Comments), CommentCount: thread.Comments.TotalCount,
		})
	}
	for _, file := range node.Files.Nodes {
		detail.Files = append(detail.Files, File{Path: file.Path, Additions: file.Additions, Deletions: file.Deletions, Change: file.ChangeType})
	}
	for _, entry := range node.History.Nodes {
		commit := entry.Commit
		author := commit.Author.User.login()
		if author == "" {
			author = commit.Author.Name
		}
		detail.Commits = append(detail.Commits, Commit{OID: commit.OID, Headline: commit.MessageHeadline, Author: author, CommittedAt: commit.CommittedDate})
	}
	if len(node.Head.Nodes) > 0 && node.Head.Nodes[0].Commit.StatusCheckRollup != nil {
		for _, check := range node.Head.Nodes[0].Commit.StatusCheckRollup.Contexts.Nodes {
			c := Check{Name: check.Name, State: check.Conclusion, URL: check.DetailsURL}
			if c.State == "" {
				c.State = check.Status
			}
			if check.Context != "" {
				c = Check{Name: check.Context, State: check.State, URL: check.TargetURL}
			}
			detail.Checks = append(detail.Checks, c)
		}
	}
	return detail
}

func githubCommentList(connection githubComments) []Comment {
	comments := make([]Comment, 0, len(connection.Nodes))
	for _, node := range connection.Nodes {
		comments = append(comments, Comment{Author: node.Author.login(), Body: node.Body, CreatedAt: node.CreatedAt, URL: node.URL})
	}
	return comments
}
