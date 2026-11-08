package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/datapointchris/prs/internal/registry"
)

// Reference names one pull request as a person or another program hands it to prs.
type Reference struct {
	// Provider is empty for owner/name#number, which the registry resolves.
	Provider string
	// Host is the URL's, and empty for owner/name#number.
	Host   string
	Slug   string
	Number int
}

func (r Reference) String() string { return fmt.Sprintf("%s#%d", r.Slug, r.Number) }

// shortReference is owner/name#number, as GitHub writes a reference across repos.
var shortReference = regexp.MustCompile(`^([^/#\s]+/[^/#\s]+)#([0-9]+)$`)

// ParseReference reads owner/name#number, or the URL of a pull request on GitHub, Bitbucket
// Server or Bitbucket Cloud. A tab of the pull request's page, such as /files, is the same
// pull request.
func ParseReference(text string) (Reference, error) {
	text = strings.TrimSpace(text)
	if match := shortReference.FindStringSubmatch(text); match != nil {
		return reference("", "", match[1], match[2])
	}
	page, err := url.Parse(text)
	if err != nil || (page.Scheme != "https" && page.Scheme != "http") || page.Host == "" {
		return Reference{}, fmt.Errorf("%q is neither owner/name#number nor a pull request's URL", text)
	}
	parts := strings.Split(strings.Trim(page.Path, "/"), "/")
	at := func(i int, word string) bool { return len(parts) > i+1 && parts[i] == word }
	switch {
	case at(2, "pull"):
		return reference(registry.GitHub, page.Host, parts[0]+"/"+parts[1], parts[3])
	case at(0, "projects") && at(2, "repos") && at(4, "pull-requests"):
		return reference(registry.Bitbucket, page.Host, parts[1]+"/"+parts[3], parts[5])
	case at(2, "pull-requests"):
		return reference(registry.Bitbucket, page.Host, parts[0]+"/"+parts[1], parts[3])
	}
	return Reference{}, fmt.Errorf("%s is not the URL of a pull request on GitHub or Bitbucket", text)
}

func reference(provider, host, slug, number string) (Reference, error) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 {
		return Reference{}, fmt.Errorf("%q is not a pull request's number", number)
	}
	return Reference{Provider: provider, Host: host, Slug: slug, Number: n}, nil
}

// ReadReference reads the pull request ref names, open or not. owner/name#number is the
// registry's repo of that slug, on whichever provider hosts it, and GitHub where the
// registry names none. A provider with ReadOne reads it whole, with its detail. Any other
// finds it among its repo's open pull requests, read in the checkout the registry names,
// and refuses one they do not hold.
func ReadReference(ctx context.Context, ref Reference, repos []registry.Repo) (PullRequest, *Detail, error) {
	name, err := ref.provider(repos)
	if err != nil {
		return PullRequest{}, nil, err
	}
	provider, err := ProviderOf(name)
	if err != nil {
		return PullRequest{}, nil, err
	}
	if provider.ReadOne != nil {
		pr, detail, err := provider.ReadOne(ctx, ref)
		if err != nil {
			return PullRequest{}, nil, err
		}
		return Finish([]PullRequest{pr}, repos, time.Now())[0], &detail, nil
	}
	at := slices.IndexFunc(repos, func(repo registry.Repo) bool {
		return repo.Provider == name && strings.EqualFold(repo.Slug, ref.Slug)
	})
	if at < 0 {
		return PullRequest{}, nil, fmt.Errorf("%s: prs reads %s only in a checkout the registry names, and it names no %s", ref, name, ref.Slug)
	}
	listing, err := provider.Read(ctx, Scope{Repos: repos[at : at+1]}, Listing{})
	if err != nil {
		return PullRequest{}, nil, err
	}
	for _, pr := range listing.PullRequests {
		if pr.Number == ref.Number {
			return Finish([]PullRequest{pr}, repos, time.Now())[0], nil, nil
		}
	}
	why := fmt.Sprintf("%s is not among %s's open pull requests on %s, and prs can read no other there, because its client has no read for a single pull request",
		ref, repos[at].Slug, name)
	if note := listing.Note(); note != "" {
		why += ": " + note
	}
	return PullRequest{}, nil, errors.New(why)
}

// provider is the reference's own, or for owner/name#number the provider the registry
// names that slug under.
func (r Reference) provider(repos []registry.Repo) (string, error) {
	if r.Provider != "" {
		return r.Provider, nil
	}
	var hosts []string
	for _, repo := range repos {
		if strings.EqualFold(repo.Slug, r.Slug) && !slices.Contains(hosts, repo.Provider) {
			hosts = append(hosts, repo.Provider)
		}
	}
	switch len(hosts) {
	case 0:
		return registry.GitHub, nil
	case 1:
		return hosts[0], nil
	}
	return "", fmt.Errorf("the registry names %s on %s, so give the pull request's URL", r.Slug, strings.Join(hosts, " and on "))
}
