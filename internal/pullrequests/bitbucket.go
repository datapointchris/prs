package pullrequests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/datapointchris/prs/internal/registry"
)

// bitbucketAtOnce is how many bbkt calls run at the same time.
const bitbucketAtOnce = 8

// ReadBitbucket lists each repo's open pull requests with bbkt, one call per repo, run in
// the repo's checkout because that is how bbkt finds the repo. The calls run at the same
// time, and their rows keep the registry's order.
//
// A repo bbkt cannot list keeps its rows from last, the previous good listing, with a note
// saying so, so one repo's failure does not take the others' rows with it. A repo not
// checked out here cannot be read, and the notes name it.
func ReadBitbucket(ctx context.Context, scope Scope, last Listing) (Listing, error) {
	repos := scope.Repos
	if len(repos) == 0 {
		return Listing{}, nil
	}
	if _, err := exec.LookPath("bbkt"); err != nil {
		return Listing{}, errors.New("bbkt is not installed, and prs reads Bitbucket through it")
	}
	var (
		listing   Listing
		absent    []string
		present   []registry.Repo
		tried     int
		failed    int
		lastFault string
	)
	for _, repo := range repos {
		if info, err := os.Stat(repo.Path); err != nil || !info.IsDir() {
			absent = append(absent, repo.Slug)
			continue
		}
		present = append(present, repo)
	}
	type answer struct {
		prs   []PullRequest
		fault string
	}
	answers := make([]answer, len(present))
	slots := make(chan struct{}, bitbucketAtOnce)
	var wg sync.WaitGroup
	for i, repo := range present {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			prs, fault := readBitbucketRepo(ctx, repo)
			answers[i] = answer{prs: prs, fault: fault}
		})
	}
	wg.Wait()
	for i, repo := range present {
		tried++
		prs, fault := answers[i].prs, answers[i].fault
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

// readBitbucketReference is Provider.ReadOne for Bitbucket. --repo names the repo, so bbkt
// reads one whose repo is not checked out here too. The slug is the one bbkt answers with,
// so a slug typed in another case keys the same row the listing does.
func readBitbucketReference(ctx context.Context, ref Reference) (PullRequest, *Detail, error) {
	if _, err := exec.LookPath("bbkt"); err != nil {
		return PullRequest{}, nil, errors.New("bbkt is not installed, and prs reads Bitbucket through it")
	}
	out, said, err := run(ctx, "", "bbkt", "pr", "view", strconv.Itoa(ref.Number), "--json", "--repo", ref.Slug, "--no-input")
	if err != nil {
		return PullRequest{}, nil, fmt.Errorf("bbkt could not read %s: %s", ref, said)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		return PullRequest{}, nil, fmt.Errorf("bbkt's answer for %s is not a JSON object", ref)
	}
	slug := ref.Slug
	if key, name := text(dig(payload, "toRef.repository.project.key")), text(dig(payload, "toRef.repository.slug")); key != "" && name != "" {
		slug = key + "/" + name
	}
	owner, name, _ := strings.Cut(slug, "/")
	prs, _ := bitbucketPullRequests(registry.Repo{Name: name, Owner: owner, Slug: slug, Provider: registry.Bitbucket}, []any{payload})
	if len(prs) != 1 || prs[0].Number != ref.Number {
		return PullRequest{}, nil, fmt.Errorf("bbkt's answer for %s holds no pull request", ref)
	}
	return prs[0], nil, nil
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
		pr := PullRequest{
			Provider: registry.Bitbucket, Repo: repo.Name, Slug: repo.Slug,
			Number: int(number), Title: text(row["title"]),
			Author: text(first(row, "author.user.name", "author.nickname")),
			Body:   text(row["description"]),
			URL:    text(first(row, "links.self.0.href", "url")),
			Branch: text(first(row, "fromRef.displayId", "source.branch.name")),
			Base:   text(first(row, "toRef.displayId", "destination.branch.name")),
			Draft:  draft, CreatedAt: bitbucketTime(row, "createdDate", "created_on", "created_at"),
			State: strings.ToUpper(text(row["state"])),
			// Bot stays null: Bitbucket types a service account, and bbkt's listing drops
			// the type. The diff counts and the merge state stay null too, because the
			// listing carries none.
		}
		if pr.State == "" {
			pr.State = "OPEN"
		}
		if updated := bitbucketTime(row, "updatedDate", "updated_on", "updated_at"); !updated.IsZero() {
			pr.UpdatedAt = &updated
		}
		if head := text(first(row, "fromRef.latestCommit", "source.commit.hash")); head != "" {
			pr.HeadOID = &head
		}
		prs = append(prs, pr)
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

// bitbucketTime reads the first of keys that holds a time: milliseconds since the epoch,
// which Bitbucket Server prints, or a timestamp, which Bitbucket Cloud prints. It is to the
// second, as GitHub's times are.
func bitbucketTime(row map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		switch value := row[key].(type) {
		case float64:
			return time.UnixMilli(int64(value)).UTC().Truncate(time.Second)
		case string:
			if at, err := time.Parse(time.RFC3339, value); err == nil {
				return at.UTC().Truncate(time.Second)
			}
		}
	}
	return time.Time{}
}
