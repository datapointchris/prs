// Package registry reads the repos a registry file names: which exist, who owns each,
// where each is checked out, and which provider hosts it.
package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The providers a repo can name. A file names its default, and an entry can override it.
const (
	GitHub    = "github"
	Bitbucket = "bitbucket"
)

// Repo is one active registry entry.
type Repo struct {
	Name  string
	Owner string
	// Slug identifies the repo on its provider: owner/name on GitHub, and the name alone on
	// Bitbucket, whose listing names no owner.
	Slug     string
	Path     string
	Provider string
}

// document is the registry as written. status and owner are read plainly rather than
// defaulted, because readers that defaulted a field they filter on once disagreed about
// which repos exist while every list looked plausible.
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
		repo := Repo{Name: entry.Name, Owner: *entry.Owner, Path: expand(entry.Path), Provider: provider}
		repo.Slug = repo.Owner + "/" + repo.Name
		if provider == Bitbucket {
			repo.Slug = repo.Name
		}
		repos = append(repos, repo)
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
