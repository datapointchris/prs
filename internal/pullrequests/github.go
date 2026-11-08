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

// githubSearch is GraphQL rather than `gh search prs`, whose fields carry no head or base
// branch. The other way, `gh pr list` per repo, costs one request per repo.
//
// Two searches under aliases go in one request. owned is every open pull request on a
// repo the registry's owners hold, whoever opened it, which is what admits a bot's or a
// contributor's. mine is what the viewer authored. owned already covers every registry
// repo, so mine adds a row only once owned passes its 100-node page.
const githubSearch = `
query($mine: String!, $owned: String!) {
  mine: search(query: $mine, type: ISSUE, first: 100) { ...PullRequests }
  owned: search(query: $owned, type: ISSUE, first: 100) { ...PullRequests }
}

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

// githubSearches is the query string behind each alias. GitHub ORs repeated user:
// qualifiers, and user: matches an organization as well as a person.
func githubSearches(repos []registry.Repo) map[string]string {
	var owners []string
	for _, repo := range repos {
		if !slices.Contains(owners, repo.Owner) {
			owners = append(owners, repo.Owner)
		}
	}
	slices.Sort(owners)
	qualifiers := make([]string, len(owners))
	for i, owner := range owners {
		qualifiers[i] = "user:" + owner
	}
	return map[string]string{
		"mine":  "author:@me is:pr is:open",
		"owned": "is:pr is:open " + strings.Join(qualifiers, " "),
	}
}

// ReadGitHub is every open pull request on the registry owners' repos, plus every one the
// viewer authored, kept to the repos the registry names.
func ReadGitHub(ctx context.Context, repos []registry.Repo) (Listing, error) {
	if len(repos) == 0 {
		return Listing{}, nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return Listing{}, errors.New("gh is not installed, and prs reads GitHub through it")
	}
	searches := githubSearches(repos)
	args := []string{"api", "graphql", "-f", "query=" + githubSearch}
	for _, alias := range []string{"mine", "owned"} {
		args = append(args, "-f", alias+"="+searches[alias])
	}
	out, said, err := run(ctx, "", "gh", args...)
	if err != nil {
		return Listing{}, fmt.Errorf("gh could not list pull requests (check gh auth status): %s", said)
	}
	var answer githubAnswer
	if err := json.Unmarshal(out, &answer); err != nil {
		return Listing{}, fmt.Errorf("reading gh's answer: %w", err)
	}
	return githubListing(answer, searches, repos), nil
}

// githubListing keeps each node the registry names, once.
//
// The filter matches owner/name against nameWithOwner, never the basename. mine covers all
// of GitHub, so a pull request the viewer authored upstream in a project sharing a name
// with a registry repo would otherwise pass, and take that repo's checkout.
func githubListing(answer githubAnswer, searches map[string]string, repos []registry.Repo) Listing {
	var listing Listing
	var nodes []githubNode
	for _, alias := range []string{"mine", "owned"} {
		connection := answer.Data[alias]
		if connection.IssueCount > len(connection.Nodes) {
			listing.Notes = append(listing.Notes, fmt.Sprintf("%q matched %d and only the first %d are listed",
				searches[alias], connection.IssueCount, len(connection.Nodes)))
		}
		nodes = append(nodes, connection.Nodes...)
	}
	wanted := map[string]bool{}
	for _, repo := range repos {
		wanted[repo.Slug] = true
	}
	// A pull request opened on the viewer's own repo matches both searches.
	listed := map[string]bool{}
	for _, node := range nodes {
		if node.Number == nil || !wanted[node.Repository.NameWithOwner] || listed[node.URL] {
			continue
		}
		listed[node.URL] = true
		listing.PullRequests = append(listing.PullRequests, githubPullRequest(node))
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
