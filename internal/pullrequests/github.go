package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"

	"github.com/datapointchris/prs/internal/registry"
)

// ReposPerSearch is how many repos one search names. Each search has its own page of 100,
// so a few busy repos fill their own page rather than every repo's.
const ReposPerSearch = 20

// githubRow is what both the listing and the one-pull-request read take of a pull
// request, so a row reads the same from either.
const githubRow = `
fragment Row on PullRequest {
  number
  title
  author { __typename login }
  body
  url
  headRefName
  headRefOid
  baseRefName
  isDraft
  state
  createdAt
  updatedAt
  mergedAt
  closedAt
  additions
  deletions
  changedFiles
  mergeable
  mergeStateStatus
  isInMergeQueue
  reviewDecision
  reviews { totalCount }
  commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
  repository { name nameWithOwner }
}`

// githubNode is one pull request as githubRow reads it. A `type: ISSUE` search reads issues
// and pull requests from one index, so a result the fragment does not match arrives as {}
// and has no number.
type githubNode struct {
	Number *int   `json:"number"`
	Title  string `json:"title"`
	Author *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
	Body             string     `json:"body"`
	URL              string     `json:"url"`
	HeadRefName      string     `json:"headRefName"`
	HeadRefOid       string     `json:"headRefOid"`
	BaseRefName      string     `json:"baseRefName"`
	IsDraft          bool       `json:"isDraft"`
	State            string     `json:"state"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	MergedAt         *time.Time `json:"mergedAt"`
	ClosedAt         *time.Time `json:"closedAt"`
	Additions        int        `json:"additions"`
	Deletions        int        `json:"deletions"`
	ChangedFiles     int        `json:"changedFiles"`
	Mergeable        string     `json:"mergeable"`
	MergeStateStatus string     `json:"mergeStateStatus"`
	IsInMergeQueue   bool       `json:"isInMergeQueue"`
	ReviewDecision   string     `json:"reviewDecision"`
	Reviews          struct {
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

type githubConnection struct {
	IssueCount int          `json:"issueCount"`
	Nodes      []githubNode `json:"nodes"`
}

// githubSearch is one alias's search, and what it covers, so a search GitHub refused keeps
// the rows it found last time.
type githubSearch struct {
	query string
	// slugs are the repos it names, in lower case, and owner the owner it reads whole.
	slugs []string
	owner string
}

// covers says the pull request came from this search's repos.
func (s githubSearch) covers(pr PullRequest) bool {
	slug := strings.ToLower(pr.Slug)
	if s.owner == "" {
		return slices.Contains(s.slugs, slug)
	}
	owner, _, _ := strings.Cut(slug, "/")
	return s.owner == ViewerOwner || owner == s.owner
}

// githubSearches names the scope's repos with repo: qualifiers, which GitHub ORs, so it
// answers only for those repos and no other repo of an owner's takes a place on the page.
// Each owner a scope reads whole gets a search of its own. which is the qualifiers that pick
// open or closed pull requests, and empty for every state.
func githubSearches(scope Scope, which string) []githubSearch {
	repos, owners := scope.Names()
	pick := strings.TrimSpace("is:pr " + which)
	var searches []githubSearch
	for chunk := range slices.Chunk(repos, ReposPerSearch) {
		qualifiers := make([]string, len(chunk))
		for i, slug := range chunk {
			qualifiers[i] = "repo:" + slug
		}
		searches = append(searches, githubSearch{query: pick + " " + strings.Join(qualifiers, " "), slugs: chunk})
	}
	for _, owner := range owners {
		searches = append(searches, githubSearch{query: pick + " user:" + owner, owner: owner})
	}
	return searches
}

const openQualifiers = "is:open"

// closedQualifiers pick the merged and closed pull requests updated at or after since, the
// newest first, so a page that cannot hold them all keeps the newest. GitHub's closed:
// qualifier leaves out some pull requests that closed within its range, and updated: finds
// them, because a pull request is never last updated before it closed. ReadGitHubClosed
// drops those that closed before since.
func closedQualifiers(since time.Time) string {
	return "is:closed updated:>=" + since.UTC().Format(time.RFC3339) + " sort:updated-desc"
}

// githubListQuery is one GraphQL request holding a search per alias, s0 onward. GraphQL
// rather than REST's search, whose results carry no head or base branch, and one request
// rather than one per repo.
func githubListQuery(searches int) string {
	var variables, aliases strings.Builder
	for i := range searches {
		fmt.Fprintf(&variables, "$s%d: String!, ", i)
		fmt.Fprintf(&aliases, "  s%d: search(query: $s%d, type: ISSUE, first: 100) { issueCount nodes { ...Row } }\n", i, i)
	}
	return fmt.Sprintf("query(%s) {\n%s}\n%s", strings.TrimSuffix(variables.String(), ", "), aliases.String(), githubRow)
}

// ReadGitHub is every open pull request in the scope, in one request. A search GitHub
// refused while others answered keeps the rows it found in last.
func ReadGitHub(ctx context.Context, scope Scope, last Listing) (Listing, error) {
	return readGitHub(ctx, githubSearches(scope, openQualifiers), "open", last)
}

// ReadGitHubClosed is every pull request in the scope merged or closed at or after since, in
// one request, as ReadGitHub reads the open ones.
func ReadGitHubClosed(ctx context.Context, scope Scope, since time.Time, last Listing) (Listing, error) {
	listing, err := readGitHub(ctx, githubSearches(scope, closedQualifiers(since)), "closed", last)
	listing.PullRequests = slices.DeleteFunc(listing.PullRequests, func(pr PullRequest) bool {
		return pr.ClosedAt == nil || pr.ClosedAt.Before(since)
	})
	return listing, err
}

// readGitHub runs the searches in one request. kind names what they find, for a note.
func readGitHub(ctx context.Context, searches []githubSearch, kind string, last Listing) (Listing, error) {
	if len(searches) == 0 {
		return Listing{}, nil
	}
	client, host, err := githubClient(ctx)
	if err != nil {
		return Listing{}, err
	}
	variables := make(map[string]any, len(searches))
	for i, search := range searches {
		variables[fmt.Sprintf("s%d", i)] = search.query
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var data map[string]*githubConnection
	err = client.DoWithContext(ctx, githubListQuery(len(searches)), variables, &data)
	var refused *api.GraphQLError
	if err != nil && !errors.As(err, &refused) {
		return Listing{}, githubFault(host, err)
	}
	return githubListing(searches, data, refused, kind, last)
}

// githubListing is every pull request the searches found, a note for each search whose
// matches ran past its page, and a note for each search GitHub refused, whose rows come
// from last. It fails only where every search was refused.
func githubListing(searches []githubSearch, data map[string]*githubConnection, refused *api.GraphQLError, kind string, last Listing) (Listing, error) {
	var listing Listing
	var faults []string
	for i, search := range searches {
		alias := fmt.Sprintf("s%d", i)
		connection := data[alias]
		if connection == nil {
			fault := fmt.Sprintf("GitHub refused the search %q: %s", search.query, refusal(refused, alias))
			faults = append(faults, fault)
			listing.Notes = append(listing.Notes, fault+", so its rows are from the last read")
			for _, pr := range last.PullRequests {
				if search.covers(pr) {
					listing.PullRequests = append(listing.PullRequests, pr)
				}
			}
			continue
		}
		if connection.IssueCount > len(connection.Nodes) {
			listing.Notes = append(listing.Notes, fmt.Sprintf("%d of %d %s pull requests matching %q are listed",
				len(connection.Nodes), connection.IssueCount, kind, search.query))
		}
		for _, node := range connection.Nodes {
			if node.Number != nil {
				listing.PullRequests = append(listing.PullRequests, githubPullRequest(node))
			}
		}
	}
	if len(faults) == len(searches) {
		return Listing{}, errors.New(faults[0])
	}
	return listing, nil
}

// refusal is what GitHub said about one alias, or everything it said where no error names it.
func refusal(refused *api.GraphQLError, alias string) string {
	if refused == nil {
		return "no answer"
	}
	var said []string
	for _, item := range refused.Errors {
		if len(item.Path) > 0 && item.Path[0] == alias {
			said = append(said, item.Message)
		}
	}
	if len(said) == 0 {
		return refused.Error()
	}
	return strings.Join(said, "; ")
}

func githubPullRequest(node githubNode) PullRequest {
	pr := PullRequest{
		Provider: registry.GitHub, Repo: node.Repository.Name, Slug: node.Repository.NameWithOwner,
		Number: *node.Number, Title: node.Title, Body: node.Body, URL: node.URL,
		Branch: node.HeadRefName, Base: node.BaseRefName, Draft: node.IsDraft, CreatedAt: node.CreatedAt,
		Additions: &node.Additions, Deletions: &node.Deletions, ChangedFiles: &node.ChangedFiles,
		Review: node.ReviewDecision, Reviews: node.Reviews.TotalCount,
		State: node.State, MergedAt: node.MergedAt, ClosedAt: node.ClosedAt, UpdatedAt: &node.UpdatedAt,
		Mergeable: &node.Mergeable, MergeStateStatus: &node.MergeStateStatus,
		InMergeQueue: &node.IsInMergeQueue, HeadOID: &node.HeadRefOid,
	}
	// A deleted account has no author, which says nothing about whether an app opened it.
	if node.Author != nil {
		pr.Author = node.Author.Login
		bot := authorIsBot(node.Author.Typename)
		pr.Bot = &bot
	}
	// No rollup until a workflow runs.
	if len(node.Commits.Nodes) > 0 && node.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		pr.Checks = node.Commits.Nodes[0].Commit.StatusCheckRollup.State
	}
	return pr
}

// githubClients holds the clients every read shares, built on the first read that finds a
// token and dropped when GitHub refuses that token, so a login made while the board is open
// is picked up on the next read. graphQL reads the listings and each pull request's detail,
// and diff asks REST for a pull request's diff, which only the diff media type gives.
var githubClients struct {
	sync.Mutex
	graphQL *api.GraphQLClient
	diff    *api.RESTClient
	host    string
}

// diffMediaType is the media type under which GitHub's REST API answers a pull request's
// read with its diff rather than its JSON.
const diffMediaType = "application/vnd.github.diff"

// githubClient is the shared GraphQL client, and the host it reads.
func githubClient(ctx context.Context) (*api.GraphQLClient, string, error) {
	githubClients.Lock()
	defer githubClients.Unlock()
	host, err := connectGitHub(ctx)
	return githubClients.graphQL, host, err
}

// githubDiffClient is the shared REST client asking for diffs, and the host it reads.
func githubDiffClient(ctx context.Context) (*api.RESTClient, string, error) {
	githubClients.Lock()
	defer githubClients.Unlock()
	host, err := connectGitHub(ctx)
	return githubClients.diff, host, err
}

// connectGitHub builds the shared clients where none are held, and returns the host they
// read. The host is gh's: GH_HOST, or the one host gh is logged in to, or github.com. The
// caller holds githubClients' lock.
func connectGitHub(ctx context.Context) (string, error) {
	if githubClients.graphQL != nil {
		return githubClients.host, nil
	}
	host, _ := auth.DefaultHost()
	token, err := githubToken(ctx, host)
	if err != nil {
		return host, err
	}
	// Host and token are given, so go-gh reads only the rest of gh's config, such as
	// http_unix_socket.
	options := api.ClientOptions{Host: host, AuthToken: token, Timeout: readTimeout}
	graphQL, err := api.NewGraphQLClient(options)
	if err != nil {
		return host, fmt.Errorf("could not reach GitHub at %s: %w", host, err)
	}
	options.Headers = map[string]string{"Accept": diffMediaType}
	diff, err := api.NewRESTClient(options)
	if err != nil {
		return host, fmt.Errorf("could not reach GitHub at %s: %w", host, err)
	}
	githubClients.graphQL, githubClients.diff, githubClients.host = graphQL, diff, host
	return host, nil
}

func forgetGitHubClient() {
	githubClients.Lock()
	defer githubClients.Unlock()
	githubClients.graphQL, githubClients.diff = nil, nil
}

// readGitHubDiff is Provider.Diff for GitHub: the pull request's diff from where its branch
// left its base, as GitHub's REST API sends it under the diff media type.
func readGitHubDiff(ctx context.Context, pr PullRequest) (string, error) {
	client, host, err := githubDiffClient(ctx)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	response, err := client.RequestWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/pulls/%d", pr.Slug, pr.Number), nil)
	if err != nil {
		return "", githubFault(host, err)
	}
	defer func() { _ = response.Body.Close() }()
	diff, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("could not read the diff of %s#%d from GitHub at %s: %w", pr.Slug, pr.Number, host, err)
	}
	return string(diff), nil
}

// githubToken is the token gh would use for host: the one go-gh finds in the environment or
// gh's config, else the one gh keeps in the system keyring. gh runs as every client here
// does, with stdin closed and a timeout, which go-gh's own lookup does not give it.
func githubToken(ctx context.Context, host string) (string, error) {
	if token, _ := auth.TokenFromEnvOrConfig(host); token != "" {
		return token, nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("no GitHub token for %s: set GH_TOKEN, or install gh and run gh auth login", host)
	}
	out, said, err := run(ctx, "", "gh", "auth", "token", "--hostname", host)
	token := strings.TrimSpace(string(out))
	if err != nil || token == "" {
		return "", fmt.Errorf("gh is not logged in to %s, and gh auth login logs it in: %s", host, said)
	}
	return token, nil
}

// githubFault says why a request failed. A refused token drops the shared client, so the
// next read looks for a token again.
func githubFault(host string, err error) error {
	var status *api.HTTPError
	if errors.As(err, &status) && status.StatusCode == http.StatusUnauthorized {
		forgetGitHubClient()
		return fmt.Errorf("GitHub at %s refused the token, and gh auth status says more: %w", host, err)
	}
	return fmt.Errorf("could not read GitHub at %s: %w", host, err)
}
