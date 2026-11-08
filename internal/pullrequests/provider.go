package pullrequests

import (
	"context"
	"fmt"
	"strconv"

	"github.com/datapointchris/prs/internal/registry"
)

// Provider is everything prs does differently by host, so a host is added in one place.
// Name, Standings, PagePath, Read and Head every host has. Every other function is a verb a
// host's client may lack, and is nil there. The key for a nil verb says the host cannot do
// it.
type Provider struct {
	Name string
	// Standings is each state word the host's client prints, and where it puts the pull
	// request.
	Standings map[string]Standing
	// PagePath reads owner/name and the number out of the path of a pull request's page,
	// split at its slashes, and is false for a path that is not one.
	PagePath func(parts []string) (slug, number string, ok bool)
	// Read lists the open pull requests in the scope. last is the previous good listing,
	// for a reader that can keep one repo's rows when only that repo failed.
	Read func(ctx context.Context, scope Scope, last Listing) (Listing, error)
	// Detail reads what the listing leaves out of one pull request. Nil where the host's
	// client reads no more than its listing.
	Detail func(ctx context.Context, pr PullRequest) (Detail, error)
	// ReadOne reads the one pull request ref names, open or not, with its detail where the
	// host has one.
	ReadOne func(ctx context.Context, ref Reference) (PullRequest, *Detail, error)
	// ReadsOwners says Read can read an owner whole, for a config that names owners.
	ReadsOwners bool
	// Head is what git fetches from origin as the pull request's head.
	Head func(PullRequest) string
	// Browse is the command that opens the pull request in the browser, and whether it
	// runs in the checkout.
	Browse func(PullRequest) (argv []string, inCheckout bool)
	// Page is a shell command paging the pull request with its comments, and the programs
	// it runs.
	Page func(PullRequest) (command string, needs []string)
	// Merge, Close and Comment are the commands that change the pull request, and whether
	// each runs in the checkout. Each runs with no terminal, so none may prompt. Where head
	// is not empty and the client can pin a commit, Merge refuses a head other than head.
	// Comment posts the file at path.
	Merge   func(pr PullRequest, head string) (argv []string, inCheckout bool)
	Close   func(PullRequest) (argv []string, inCheckout bool)
	Comment func(pr PullRequest, path string) (argv []string, inCheckout bool)
}

// Providers is a Provider for each name registry.Providers holds.
var Providers = []Provider{
	{
		Name:      registry.GitHub,
		Standings: map[string]Standing{"OPEN": Open, "MERGED": Merged, "CLOSED": Closed},
		// github.com/<owner>/<name>/pull/<number>, then a tab such as /files.
		PagePath: func(parts []string) (string, string, bool) {
			if len(parts) < 4 || parts[2] != "pull" {
				return "", "", false
			}
			return parts[0] + "/" + parts[1], parts[3], true
		},
		Read:        ReadGitHub,
		Detail:      readGitHubDetail,
		ReadOne:     readGitHubReference,
		ReadsOwners: true,
		Head:        func(pr PullRequest) string { return fmt.Sprintf("pull/%d/head", pr.Number) },
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
		// --merge names the method, so gh asks nothing. A pull request lands as a merge
		// commit, and the repo's own setting deletes the remote branch.
		Merge: func(pr PullRequest, head string) ([]string, bool) {
			argv := []string{"gh", "pr", "merge", strconv.Itoa(pr.Number), "--repo", pr.Slug, "--merge"}
			if head != "" {
				argv = append(argv, "--match-head-commit", head)
			}
			return argv, false
		},
		Close: func(pr PullRequest) ([]string, bool) {
			return []string{"gh", "pr", "close", strconv.Itoa(pr.Number), "--repo", pr.Slug}, false
		},
		Comment: func(pr PullRequest, path string) ([]string, bool) {
			return []string{"gh", "pr", "comment", strconv.Itoa(pr.Number), "--repo", pr.Slug, "--body-file", path}, false
		},
	},
	{
		Name: registry.Bitbucket,
		// Bitbucket Cloud supersedes a pull request where Bitbucket Server only declines one.
		Standings: map[string]Standing{"OPEN": Open, "MERGED": Merged, "DECLINED": Closed, "SUPERSEDED": Closed},
		// Server's /projects/<KEY>/repos/<name>/pull-requests/<number>, or Cloud's
		// /<workspace>/<name>/pull-requests/<number>, then a tab such as /overview.
		PagePath: func(parts []string) (string, string, bool) {
			switch {
			case len(parts) >= 6 && parts[0] == "projects" && parts[2] == "repos" && parts[4] == "pull-requests":
				return parts[1] + "/" + parts[3], parts[5], true
			case len(parts) >= 4 && parts[2] == "pull-requests":
				return parts[0] + "/" + parts[1], parts[3], true
			}
			return "", "", false
		},
		Read:    ReadBitbucket,
		ReadOne: readBitbucketReference,
		// Bitbucket serves no pull request refs, so the head is the source branch on origin.
		Head: func(pr PullRequest) string { return pr.Branch },
		// bbkt finds the repo from the directory it runs in.
		Browse: func(pr PullRequest) ([]string, bool) {
			return []string{"bbkt", "pr", "open", strconv.Itoa(pr.Number)}, true
		},
		// bbkt refuses to merge without a terminal unless --yes says prs already asked. It
		// cannot pin the head.
		Merge: func(pr PullRequest, _ string) ([]string, bool) {
			return []string{"bbkt", "pr", "merge", strconv.Itoa(pr.Number), "--yes"}, true
		},
		Comment: func(pr PullRequest, path string) ([]string, bool) {
			return []string{"bbkt", "pr", "comment", "create", strconv.Itoa(pr.Number), "--body-file", path}, true
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
