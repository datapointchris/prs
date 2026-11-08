package pullrequests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/datapointchris/prs/internal/registry"
)

// reposPerSearch is how many repos one search names. Each search has its own page of 100,
// so a few busy repos fill their own page rather than every repo's.
const reposPerSearch = 20

// githubFields is what each search reads of a pull request.
const githubFields = `
fragment PullRequests on SearchResultItemConnection {
  issueCount
  nodes {
    ... on PullRequest {
      number
      title
      author { __typename login }
      body
      url
      headRefName
      baseRefName
      isDraft
      createdAt
      additions
      deletions
      changedFiles
      reviewDecision
      reviews { totalCount }
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      repository { name nameWithOwner }
    }
  }
}`

type githubAnswer struct {
	Data map[string]githubConnection `json:"data"`
}

type githubConnection struct {
	IssueCount int          `json:"issueCount"`
	Nodes      []githubNode `json:"nodes"`
}

// githubNode is one search result. A `type: ISSUE` search reads issues and pull requests
// from one index, so a result the fragment does not match arrives as {} and has no number.
type githubNode struct {
	Number *int   `json:"number"`
	Title  string `json:"title"`
	Author *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
	Body           string    `json:"body"`
	URL            string    `json:"url"`
	HeadRefName    string    `json:"headRefName"`
	BaseRefName    string    `json:"baseRefName"`
	IsDraft        bool      `json:"isDraft"`
	CreatedAt      time.Time `json:"createdAt"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changedFiles"`
	ReviewDecision string    `json:"reviewDecision"`
	Reviews        struct {
		TotalCount int `json:"totalCount"`
	} `json:"reviews"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Repository struct {
		Name          string `json:"name"`
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// githubSearches names each search's repos with repo: qualifiers, which GitHub ORs, so
// GitHub answers only for the registry's repos and no other repo of an owner's takes a
// place on the page.
func githubSearches(repos []registry.Repo) []string {
	slugs := make([]string, len(repos))
	for i, repo := range repos {
		slugs[i] = "repo:" + repo.Slug
	}
	slices.Sort(slugs)
	var searches []string
	for chunk := range slices.Chunk(slugs, reposPerSearch) {
		searches = append(searches, "is:pr is:open "+strings.Join(chunk, " "))
	}
	return searches
}

// githubQuery is one GraphQL request holding a search per alias, s0 onward. GraphQL rather
// than `gh search prs`, whose fields carry no head or base branch, and one request rather
// than `gh pr list`'s one per repo.
func githubQuery(searches int) string {
	var variables, aliases strings.Builder
	for i := range searches {
		fmt.Fprintf(&variables, "$s%d: String!, ", i)
		fmt.Fprintf(&aliases, "  s%d: search(query: $s%d, type: ISSUE, first: 100) { ...PullRequests }\n", i, i)
	}
	return fmt.Sprintf("query(%s) {\n%s}\n%s", strings.TrimSuffix(variables.String(), ", "), aliases.String(), githubFields)
}

// ReadGitHub is every open pull request on the registry's GitHub repos, in one request.
func ReadGitHub(ctx context.Context, repos []registry.Repo, _ Listing) (Listing, error) {
	if len(repos) == 0 {
		return Listing{}, nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return Listing{}, errors.New("gh is not installed, and prs reads GitHub through it")
	}
	searches := githubSearches(repos)
	args := []string{"api", "graphql", "-f", "query=" + githubQuery(len(searches))}
	for i, search := range searches {
		args = append(args, "-f", fmt.Sprintf("s%d=%s", i, search))
	}
	out, said, err := run(ctx, "", "gh", args...)
	if err != nil {
		// gh says auth when its login is the cause. Otherwise the cause is GitHub's answer,
		// and a hint at the login would send the reader the wrong way.
		if strings.Contains(strings.ToLower(said), "auth") || strings.Contains(said, "401") {
			return Listing{}, fmt.Errorf("gh is not logged in to GitHub (gh auth status says more): %s", said)
		}
		return Listing{}, fmt.Errorf("gh could not list pull requests: %s", said)
	}
	var answer githubAnswer
	if err := json.Unmarshal(out, &answer); err != nil {
		return Listing{}, fmt.Errorf("reading gh's answer: %w", err)
	}
	return githubListing(answer, len(searches)), nil
}

// githubListing is every pull request the searches found, and a note for each search whose
// matches ran past its page.
func githubListing(answer githubAnswer, searches int) Listing {
	var listing Listing
	for i := range searches {
		connection := answer.Data[fmt.Sprintf("s%d", i)]
		if connection.IssueCount > len(connection.Nodes) {
			listing.Notes = append(listing.Notes, fmt.Sprintf("%d of %d open pull requests on one set of repos are listed",
				len(connection.Nodes), connection.IssueCount))
		}
		for _, node := range connection.Nodes {
			if node.Number != nil {
				listing.PullRequests = append(listing.PullRequests, githubPullRequest(node))
			}
		}
	}
	return listing
}

func githubPullRequest(node githubNode) PullRequest {
	pr := PullRequest{
		Provider: registry.GitHub, Repo: node.Repository.Name, Slug: node.Repository.NameWithOwner,
		Number: *node.Number, Title: node.Title, Body: node.Body, URL: node.URL,
		Branch: node.HeadRefName, Base: node.BaseRefName, Draft: node.IsDraft, CreatedAt: node.CreatedAt,
		Additions: &node.Additions, Deletions: &node.Deletions, ChangedFiles: &node.ChangedFiles,
		Review: node.ReviewDecision, Reviews: node.Reviews.TotalCount,
	}
	// A deleted account has no author, which says nothing about whether an app opened it.
	if node.Author != nil {
		pr.Author = node.Author.Login
		bot := node.Author.Typename == "Bot"
		pr.Bot = &bot
	}
	// No rollup until a workflow runs.
	if len(node.Commits.Nodes) > 0 && node.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		pr.Checks = node.Commits.Nodes[0].Commit.StatusCheckRollup.State
	}
	return pr
}
