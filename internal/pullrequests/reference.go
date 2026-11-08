package pullrequests

import (
	"context"
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

// ParseReference reads owner/name#number, or the URL of a pull request's page on a host
// whose PagePath reads it.
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
	for _, provider := range Providers {
		if slug, number, ok := provider.PagePath(parts); ok {
			return reference(provider.Name, page.Host, slug, number)
		}
	}
	return Reference{}, fmt.Errorf("%s is not the URL of a pull request on any host prs reads", text)
}

func reference(provider, host, slug, number string) (Reference, error) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 {
		return Reference{}, fmt.Errorf("%q is not a pull request's number", number)
	}
	return Reference{Provider: provider, Host: host, Slug: slug, Number: n}, nil
}

// Named is one pull request a reference names, as its own read found it. Detail is nil
// where the provider reads none.
type Named struct {
	PullRequest PullRequest
	Detail      *Detail
	Standing    Standing
}

// ReadReference reads the pull request ref names, open or not. owner/name#number is the
// registry's repo of that slug, on whichever provider hosts it, and GitHub where the
// registry names none. A URL whose host is not the one the pull request was read from is
// refused, because a repo of the same name there holds a different pull request.
func ReadReference(ctx context.Context, ref Reference, repos []registry.Repo) (Named, error) {
	name, err := ref.provider(repos)
	if err != nil {
		return Named{}, err
	}
	provider, err := ProviderOf(name)
	if err != nil {
		return Named{}, err
	}
	if provider.ReadOne == nil {
		return Named{}, fmt.Errorf("prs cannot read one pull request on %s, because its client has no verb for it", name)
	}
	pr, detail, err := provider.ReadOne(ctx, ref)
	if err != nil {
		return Named{}, err
	}
	if page, err := url.Parse(pr.URL); ref.Host != "" && err == nil && page.Host != "" && !strings.EqualFold(page.Host, ref.Host) {
		return Named{}, fmt.Errorf("%s is on %s, and prs reads %s at %s", ref, ref.Host, name, page.Host)
	}
	standing, err := pr.Standing()
	if err != nil {
		return Named{}, err
	}
	return Named{PullRequest: Finish([]PullRequest{pr}, repos, time.Now())[0], Detail: detail, Standing: standing}, nil
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
