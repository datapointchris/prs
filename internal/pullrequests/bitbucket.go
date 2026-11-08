package pullrequests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/datapointchris/prs/internal/registry"
)

// ReadBitbucket lists each repo's open pull requests with bbkt, one call per repo, run in
// the repo's checkout because that is how bbkt finds the repo.
//
// A repo bbkt cannot list keeps its rows from last, the previous good listing, with a note
// saying so, so one repo's failure does not take the others' rows with it. A repo not
// checked out here cannot be read, and the notes name it.
func ReadBitbucket(ctx context.Context, repos []registry.Repo, last Listing) (Listing, error) {
	if len(repos) == 0 {
		return Listing{}, nil
	}
	if _, err := exec.LookPath("bbkt"); err != nil {
		return Listing{}, errors.New("bbkt is not installed, and prs reads Bitbucket through it")
	}
	var (
		listing   Listing
		absent    []string
		tried     int
		failed    int
		lastFault string
	)
	for _, repo := range repos {
		if info, err := os.Stat(repo.Path); err != nil || !info.IsDir() {
			absent = append(absent, repo.Name)
			continue
		}
		tried++
		prs, fault := readBitbucketRepo(ctx, repo)
		if fault != "" {
			failed++
			lastFault = fault
			listing.Notes = append(listing.Notes, fault+", so its rows are from the last read")
			for _, pr := range last.PullRequests {
				if pr.Slug == repo.Slug {
					listing.PullRequests = append(listing.PullRequests, pr)
				}
			}
			continue
		}
		listing.PullRequests = append(listing.PullRequests, prs...)
	}
	if tried > 0 && failed == tried {
		return Listing{}, errors.New(lastFault)
	}
	if len(absent) > 0 {
		listing.Notes = append(listing.Notes, "not checked out here, so not read: "+strings.Join(absent, ", "))
	}
	return listing, nil
}

// readBitbucketRepo is one repo's open pull requests, or what went wrong reading them.
func readBitbucketRepo(ctx context.Context, repo registry.Repo) ([]PullRequest, string) {
	out, said, err := run(ctx, repo.Path, "bbkt", "pr", "list", "--json")
	if err != nil {
		return nil, fmt.Sprintf("bbkt could not list %s: %s", repo.Name, said)
	}
	var payload any
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Sprintf("bbkt's answer for %s is not JSON", repo.Name)
	}
	prs, ok := bitbucketPullRequests(repo, payload)
	if !ok {
		return nil, fmt.Sprintf("bbkt's answer for %s holds no list of pull requests", repo.Name)
	}
	return prs, ""
}

// bitbucketPullRequests maps bbkt's field names once, so nothing past here sees them. Each
// field is read under every name bbkt has printed it as. It is false where the payload is
// neither a list nor an object holding one.
func bitbucketPullRequests(repo registry.Repo, payload any) ([]PullRequest, bool) {
	rows, ok := payload.([]any)
	if object, isObject := payload.(map[string]any); isObject {
		rows, ok = object["values"].([]any)
		if !ok {
			rows, ok = object["pull_requests"].([]any)
		}
	}
	if !ok {
		return nil, false
	}
	prs := make([]PullRequest, 0, len(rows))
	for _, entry := range rows {
		row, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		number, _ := first(row, "id", "number").(float64)
		draft, _ := row["draft"].(bool)
		prs = append(prs, PullRequest{
			Provider: registry.Bitbucket, Repo: repo.Name, Slug: repo.Slug,
			Number: int(number), Title: text(row["title"]),
			Author: text(first(row, "author.user.name", "author.nickname")),
			Body:   text(row["description"]),
			URL:    text(first(row, "links.self.0.href", "url")),
			Branch: text(first(row, "fromRef.displayId", "source.branch.name")),
			Base:   text(first(row, "toRef.displayId", "destination.branch.name")),
			Draft:  draft, CreatedAt: bitbucketCreated(row),
			// Bot stays null: Bitbucket types a service account, and bbkt's listing drops
			// the type. The diff counts stay null too, because the listing carries none.
		})
	}
	return prs, true
}

// first is the value at the first of these dotted paths that holds one. A number in a
// path indexes a list.
func first(row map[string]any, paths ...string) any {
	for _, path := range paths {
		if value := dig(row, path); value != nil && value != "" {
			return value
		}
	}
	return nil
}

func dig(value any, path string) any {
	for _, key := range strings.Split(path, ".") {
		switch at := value.(type) {
		case map[string]any:
			value = at[key]
		case []any:
			var index int
			if _, err := fmt.Sscanf(key, "%d", &index); err != nil || index >= len(at) {
				return nil
			}
			value = at[index]
		default:
			return nil
		}
	}
	return value
}

func text(value any) string {
	s, _ := value.(string)
	return s
}

// bitbucketCreated reads createdDate as milliseconds since the epoch, which Bitbucket
// Server prints, or created_at as a timestamp.
func bitbucketCreated(row map[string]any) time.Time {
	if ms, ok := row["createdDate"].(float64); ok {
		return time.UnixMilli(int64(ms)).UTC()
	}
	for _, key := range []string{"created_at", "createdDate"} {
		if stamp, ok := row[key].(string); ok {
			if at, err := time.Parse(time.RFC3339, stamp); err == nil {
				return at
			}
		}
	}
	return time.Time{}
}
