// Package registry reads the repos a registry file names: which exist, who owns each,
// where each is checked out, and which provider hosts it.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The providers a repo can name. A file names its default, and an entry can override it.
const (
	GitHub    = "github"
	Bitbucket = "bitbucket"
)

// Providers is every provider a registry may name.
var Providers = []string{GitHub, Bitbucket}

// Repo is one active registry entry.
type Repo struct {
	Name string
	// Owner is the GitHub owner, or the Bitbucket project key.
	Owner string
	// Slug is owner/name, which identifies the repo on its provider. Two Bitbucket projects
	// can each hold a repo of one name.
	Slug     string
	Path     string
	Provider string
}

// document is the registry as written. status and owner are required rather than
// defaulted, because a reader that defaults a field it filters on drops a repo silently,
// and every list it prints still looks plausible.
type document struct {
	Provider string `json:"provider"`
	Repos    []struct {
		Name     string  `json:"name"`
		Owner    *string `json:"owner"`
		Path     string  `json:"path"`
		Status   *string `json:"status"`
		Provider string  `json:"provider"`
	} `json:"repos"`
}

// Load reads every active repo the file names, in the file's order.
func Load(path string) ([]Repo, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the registry: %w", err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading the registry %s: %w", path, err)
	}
	fallback := doc.Provider
	if fallback == "" {
		fallback = GitHub
	}
	var repos []Repo
	for _, entry := range doc.Repos {
		if entry.Status == nil {
			return nil, fmt.Errorf("the registry %s gives %q no status", path, entry.Name)
		}
		if *entry.Status != "active" {
			continue
		}
		if entry.Owner == nil {
			return nil, fmt.Errorf("the registry %s gives %q no owner", path, entry.Name)
		}
		provider := entry.Provider
		if provider == "" {
			provider = fallback
		}
		if !slices.Contains(Providers, provider) {
			return nil, fmt.Errorf("the registry %s puts %q on %q, and prs reads %s",
				path, entry.Name, provider, strings.Join(Providers, " and "))
		}
		repos = append(repos, Repo{
			Name: entry.Name, Owner: *entry.Owner, Slug: *entry.Owner + "/" + entry.Name,
			Path: expand(entry.Path), Provider: provider,
		})
	}
	return repos, nil
}

// expand reads ~ only at the start, so a tilde inside a path stays literal.
func expand(path string) string {
	rest, ok := strings.CutPrefix(path, "~")
	if !ok {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, rest)
}

// Hosted is the repos one provider hosts.
func Hosted(repos []Repo, provider string) []Repo {
	var hosted []Repo
	for _, repo := range repos {
		if repo.Provider == provider {
			hosted = append(hosted, repo)
		}
	}
	return hosted
}
