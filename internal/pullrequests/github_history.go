package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// searchCap is the most results GitHub's search returns for one query, however many pages
// are read past the first.
const searchCap = 1000

// historyAliasesPerRequest bounds one history request's searches, because each page carries
// a review body a pull request, and the answer grows with every alias.
const historyAliasesPerRequest = 5

// githubHistoryRow is what the history keeps of a pull request: who opened it, when it
// opened, merged and closed, and its first review, whose marker says whether a review round
// reviewed it. One review is enough to say that, where every review would carry every
// reviewer's body.
const githubHistoryRow = `
fragment HistoryRow on PullRequest {
  number
  title
  url
  state
  createdAt
  updatedAt
  mergedAt
  closedAt
  author { __typename login }
  repository { nameWithOwner }
  reviews(first: 1) { nodes { body authorAssociation author { __typename login } } }
}`

type githubHistoryNode struct {
	Number *int   `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Author *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	MergedAt   *time.Time `json:"mergedAt"`
	ClosedAt   *time.Time `json:"closedAt"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Reviews struct {
		Nodes []struct {
			Body              string `json:"body"`
			AuthorAssociation string `json:"authorAssociation"`
			Author            *struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"reviews"`
}

type githubHistoryConnection struct {
	IssueCount int `json:"issueCount"`
	PageInfo   struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []githubHistoryNode `json:"nodes"`
}

// HistoryRecord is a pull request as the history read it, open, merged or closed.
type HistoryRecord struct {
	Slug   string
	Number int
	Title  string
	URL    string
	// Author is the login that opened it, empty for a deleted account, and Bot whether an app
	// did, nil where the account is gone.
	Author string
	Bot    *bool
	// State is OPEN, MERGED or CLOSED. MergedAt is nil until it merges, and ClosedAt until it
	// merges or closes.
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
	MergedAt  *time.Time
	ClosedAt  *time.Time
	// ReviewRound says its first review was a review round's.
	ReviewRound bool
}

// IsBot says an app opened it, and false where the account is gone.
func (r HistoryRecord) IsBot() bool { return r.Bot != nil && *r.Bot }

// Merged says it merged.
func (r HistoryRecord) Merged() bool { return r.MergedAt != nil }

// historySearch is one search of the history, over one stretch of time. A zero to runs to
// whenever the search is answered. after is the page's cursor, empty for the first page.
type historySearch struct {
	base     string
	from, to time.Time
	after    string
}

func (s historySearch) query() string {
	stretch := ">=" + searchInstant(s.from)
	if !s.to.IsZero() {
		stretch = searchInstant(s.from) + ".." + searchInstant(s.to)
	}
	return s.base + " updated:" + stretch
}

func searchInstant(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// halves are the search split at the middle of its stretch, each from its first page, and
// false where the stretch is too short to split. GitHub's range is inclusive at both ends,
// so a pull request on the middle instant is read twice, which keeping it once absorbs.
func (s historySearch) halves(now time.Time) ([]historySearch, bool) {
	to := s.to
	if to.IsZero() {
		to = now
	}
	if to.Sub(s.from) < 2*time.Second {
		return nil, false
	}
	middle := s.from.Add(to.Sub(s.from) / 2).Truncate(time.Second)
	return []historySearch{{base: s.base, from: s.from, to: middle}, {base: s.base, from: middle, to: to}}, true
}

// ReadGitHubHistory is every pull request in the scope, in any state, last updated from from
// through to, or from from on where to is zero. Updated rather than closed, because GitHub's
// closed: qualifier leaves out some pull requests that closed within its range, and a pull
// request is never last updated before it closed. So it holds every one closed within the
// range, and a reopened one comes back open. roundPost says whether a review is a review
// round's. It reads every page of each search, and splits a search whose matches pass the
// most GitHub's search returns into halves of its stretch, as often as it takes. It fails
// where any search fails, so a caller keeping what it read never takes a stretch with a hole
// in it for one read whole.
func ReadGitHubHistory(ctx context.Context, scope Scope, from, to time.Time, roundPost func(Review) bool) ([]HistoryRecord, error) {
	var pending []historySearch
	for _, search := range githubSearches(scope, "") {
		pending = append(pending, historySearch{base: search.query, from: from, to: to})
	}
	if len(pending) == 0 {
		return nil, nil
	}
	client, host, err := githubClient(ctx)
	if err != nil {
		return nil, err
	}
	var read []HistoryRecord
	for len(pending) > 0 {
		batch := pending[:min(len(pending), historyAliasesPerRequest)]
		pending = pending[len(batch):]
		variables := map[string]any{}
		for i, search := range batch {
			variables[fmt.Sprintf("q%d", i)] = search.query()
			variables[fmt.Sprintf("a%d", i)] = nil
			if search.after != "" {
				variables[fmt.Sprintf("a%d", i)] = search.after
			}
		}
		var data map[string]*githubHistoryConnection
		requestCtx, cancel := context.WithTimeout(ctx, readTimeout)
		err := client.DoWithContext(requestCtx, githubHistoryQuery(len(batch)), variables, &data)
		cancel()
		if err != nil {
			return nil, githubFault(host, err)
		}
		for i, search := range batch {
			connection := data[fmt.Sprintf("s%d", i)]
			if connection == nil {
				return nil, fmt.Errorf("GitHub at %s answered no search for %q", host, search.query())
			}
			if connection.IssueCount > searchCap && search.after == "" {
				halves, ok := search.halves(time.Now())
				if !ok {
					return nil, fmt.Errorf("%d pull requests match %q, past the %d GitHub's search returns, within two seconds",
						connection.IssueCount, search.query(), searchCap)
				}
				pending = append(pending, halves...)
				continue
			}
			for _, node := range connection.Nodes {
				if node.Number != nil {
					read = append(read, historyRecord(node, roundPost))
				}
			}
			if connection.PageInfo.HasNextPage {
				if connection.PageInfo.EndCursor == "" {
					return nil, errors.New("GitHub said another page follows " + search.query() + " and named no cursor for it")
				}
				search.after = connection.PageInfo.EndCursor
				pending = append(pending, search)
			}
		}
	}
	return read, nil
}

// githubHistoryQuery is one GraphQL request holding a search per alias, s0 onward, each
// with its query and its page's cursor.
func githubHistoryQuery(searches int) string {
	var variables, aliases strings.Builder
	for i := range searches {
		fmt.Fprintf(&variables, "$q%d: String!, $a%d: String, ", i, i)
		fmt.Fprintf(&aliases, "  s%d: search(query: $q%d, type: ISSUE, first: 100, after: $a%d) { issueCount pageInfo { hasNextPage endCursor } nodes { ...HistoryRow } }\n", i, i, i)
	}
	return fmt.Sprintf("query(%s) {\n%s}\n%s", strings.TrimSuffix(variables.String(), ", "), aliases.String(), githubHistoryRow)
}

func historyRecord(node githubHistoryNode, roundPost func(Review) bool) HistoryRecord {
	record := HistoryRecord{
		Slug: node.Repository.NameWithOwner, Number: *node.Number, Title: node.Title, URL: node.URL,
		State: node.State, CreatedAt: node.CreatedAt, UpdatedAt: node.UpdatedAt,
		MergedAt: node.MergedAt, ClosedAt: node.ClosedAt,
	}
	if node.Author != nil {
		bot := authorIsBot(node.Author.Typename)
		record.Author, record.Bot = node.Author.Login, &bot
	}
	if len(node.Reviews.Nodes) > 0 {
		first := node.Reviews.Nodes[0]
		review := Review{Body: first.Body, AuthorAssociation: first.AuthorAssociation}
		if first.Author != nil {
			review.Author, review.AuthorType = first.Author.Login, first.Author.Typename
		}
		record.ReviewRound = roundPost(review)
	}
	return record
}
