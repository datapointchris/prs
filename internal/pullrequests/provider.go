package pullrequests

import (
	"context"
	"fmt"
	"strconv"

	"github.com/datapointchris/prs/internal/registry"
)

// Provider is everything prs does differently by host, so a host is added in one place.
type Provider struct {
	Name string
	// Read lists the open pull requests on the repos it hosts. last is the previous good
	// listing, for a reader that can keep one repo's rows when only that repo failed.
	Read func(ctx context.Context, repos []registry.Repo, last Listing) (Listing, error)
	// Head is what git fetches from origin as the pull request's head.
	Head func(PullRequest) string
	// Browse is the command that opens the pull request in the browser, and whether it
	// runs in the checkout.
	Browse func(PullRequest) (argv []string, inCheckout bool)
	// Page is a shell command paging the pull request with its comments, and the programs
	// it runs. Nil where the host has no such page.
	Page func(PullRequest) (command string, needs []string)
}

// Providers is a Provider for each name registry.Providers holds.
var Providers = []Provider{
	{
		Name: registry.GitHub,
		Read: ReadGitHub,
		Head: func(pr PullRequest) string { return fmt.Sprintf("pull/%d/head", pr.Number) },
		Browse: func(pr PullRequest) ([]string, bool) {
			return []string{"gh", "pr", "view", strconv.Itoa(pr.Number), "--repo", pr.Slug, "--web"}, false
		},
		// GH_FORCE_TTY makes gh render the page for a pager rather than print key: value
		// lines. A slug holds only the characters GitHub allows in an owner and a name, so
		// it needs no quoting.
		Page: func(pr PullRequest) (string, []string) {
			return fmt.Sprintf("GH_FORCE_TTY=$(tput cols) gh pr view %d --repo %s --comments | less -R", pr.Number, pr.Slug),
				[]string{"gh", "tput", "less"}
		},
	},
	{
		Name: registry.Bitbucket,
		Read: ReadBitbucket,
		// Bitbucket serves no pull request refs, so the head is the source branch on origin.
		Head: func(pr PullRequest) string { return pr.Branch },
		// bbkt finds the repo from the directory it runs in.
		Browse: func(pr PullRequest) ([]string, bool) {
			return []string{"bbkt", "pr", "open", strconv.Itoa(pr.Number)}, true
		},
	},
}

// ProviderOf is the Provider named name.
func ProviderOf(name string) (Provider, error) {
	for _, provider := range Providers {
		if provider.Name == name {
			return provider, nil
		}
	}
	return Provider{}, fmt.Errorf("prs does not read %q", name)
}
