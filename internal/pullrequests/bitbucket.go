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
// the repo's checkout because that is how bbkt finds the repo. A repo not checked out here
// is passed over, and one bbkt cannot list is named in the listing's notes.
func ReadBitbucket(ctx context.Context, repos []registry.Repo) (Listing, error) {
	if len(repos) == 0 {
		return Listing{}, nil
	}
	if _, err := exec.LookPath("bbkt"); err != nil {
		return Listing{}, errors.New("bbkt is not installed, and prs reads Bitbucket through it")
	}
	var listing Listing
	for _, repo := range repos {
		if info, err := os.Stat(repo.Path); err != nil || !info.IsDir() {
			continue
		}
		out, said, err := run(ctx, repo.Path, "bbkt", "pr", "list", "--json")
		if err != nil {
			listing.Notes = append(listing.Notes, fmt.Sprintf("bbkt could not list %s: %s", repo.Name, said))
			continue
		}
		var payload any
		if err := json.Unmarshal(out, &payload); err != nil {
			listing.Notes = append(listing.Notes, fmt.Sprintf("bbkt's answer for %s is not JSON", repo.Name))
			continue
		}
		listing.PullRequests = append(listing.PullRequests, bitbucketPullRequests(repo.Name, payload)...)
	}
	return listing, nil
}

// bitbucketPullRequests maps bbkt's field names once, so nothing past here sees them. Each
// field is read under every name bbkt has printed it as.
func bitbucketPullRequests(repo string, payload any) []PullRequest {
	rows, _ := payload.([]any)
	if object, ok := payload.(map[string]any); ok {
		rows, _ = object["values"].([]any)
		if rows == nil {
			rows, _ = object["pull_requests"].([]any)
		}
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
			Provider: registry.Bitbucket, Repo: repo, Slug: repo,
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
	return prs
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
