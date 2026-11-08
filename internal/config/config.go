// Package config reads prs's config file, which names the registry of repos or the owners
// read in its place, how often the board reads them, how large a diff a pull request's
// document shows whole, and the keys a person adds to it.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RegistryVariable names the registry for one shell, ahead of the config file.
const RegistryVariable = "PRS_REPOS_REGISTRY"

// Which layer chose the repos prs reads, in the order they are asked.
const (
	FromVariable = RegistryVariable
	FromFile     = "repos_registry in the config file"
	FromOwners   = "owners in the config file"
	FromDefault  = "the default"
	FromViewer   = "no registry at the default, so your own repos on GitHub"
)

// Config is what the environment and the file say, with each path expanded.
type Config struct {
	// File is the path read, whether or not anything was there.
	File string
	// Exists says the file was there to read.
	Exists bool
	// Registry is the repo registry's path. Where it is empty, prs reads Owners whole, and
	// with no Owners either, the repos the GitHub token's own account holds. ReposFrom is
	// the layer that chose.
	Registry  string
	Owners    []string
	ReposFrom string
	// RefreshEvery is how often the board reads each provider again while it is open, and
	// RefreshFrom names the layer that set it.
	RefreshEvery time.Duration
	RefreshFrom  string
	// ClosedRefreshEvery is how often the board reads the recently closed pull requests
	// again, and ClosedRefreshFrom names the layer that set it.
	ClosedRefreshEvery time.Duration
	ClosedRefreshFrom  string
	// DiffSectionMaxLines is the most lines a pull request may change for its document's Diff
	// section to show its diff whole. A larger one's section names its size instead.
	// DiffSectionFrom names the layer that set it.
	DiffSectionMaxLines int
	DiffSectionFrom     string
	// WindowActions are the keys the file adds, each opening a tmux window on a pull request.
	WindowActions []WindowAction
}

// DefaultRefreshEvery is how often the board reads each provider while it is open, where the
// file does not say. Each GitHub read spends points from the token's hourly GraphQL allowance.
const DefaultRefreshEvery = time.Minute

// DefaultClosedRefreshEvery is how often the board reads the recently closed pull requests,
// where the file does not say. That read costs GitHub as much as the open one, and what it
// finds changes only when a pull request closes, which the open read already shows.
const DefaultClosedRefreshEvery = 5 * time.Minute

// DefaultDiffSectionMaxLines is DiffSectionMaxLines where the file does not say: about three
// screens of the side panel, past which nvim's diff, with its file tree, reads faster than
// scrolling.
const DefaultDiffSectionMaxLines = 300

// WindowAction is a key that runs a command in a new tmux window, started in the pull
// request's checkout. It is offered only where every program it needs is on PATH.
type WindowAction struct {
	Key    string `yaml:"key"`
	Label  string `yaml:"label"`
	Detail string `yaml:"detail"`
	// Needs are the programs the command runs, checked before the window opens, because
	// tmux reports a window opened even when its command cannot start.
	Needs []string `yaml:"needs"`
	// Providers limits the key to pull requests these providers host. Empty means any.
	Providers []string `yaml:"providers"`
	// Window is the tmux window's name, and Command the shell command it runs. Both take
	// the names Placeholders lists.
	Window  string `yaml:"window"`
	Command string `yaml:"command"`
}

// file is the document as written.
type file struct {
	Registry                  string         `yaml:"repos_registry"`
	Owners                    []string       `yaml:"owners"`
	RefreshEverySeconds       yaml.Node      `yaml:"refresh_every_seconds"`
	ClosedRefreshEverySeconds yaml.Node      `yaml:"closed_refresh_every_seconds"`
	DiffSectionMaxLines       yaml.Node      `yaml:"diff_section_max_lines"`
	WindowActions             []WindowAction `yaml:"window_actions"`
}

// Fields are what a window action's Window and Command can name of a pull request.
type Fields struct {
	Number int
	Repo   string
	Slug   string
}

// placeholders fills each name a Window or Command may hold.
var placeholders = map[string]func(Fields) string{
	"{number}": func(f Fields) string { return strconv.Itoa(f.Number) },
	"{repo}":   func(f Fields) string { return f.Repo },
	"{slug}":   func(f Fields) string { return f.Slug },
}

var placeholder = regexp.MustCompile(`\{[^}]*\}`)

// Placeholders are the names Fill fills, sorted.
func Placeholders() []string { return slices.Sorted(maps.Keys(placeholders)) }

// Fill puts a pull request's fields into a Window or Command.
func Fill(template string, fields Fields) string {
	var pairs []string
	for name, value := range placeholders {
		pairs = append(pairs, name, value(fields))
	}
	return strings.NewReplacer(pairs...).Replace(template)
}

// Missing is the programs the action needs that are not on PATH. The board offers its key
// only where none is.
func (a WindowAction) Missing() []string { return MissingPrograms(a.Needs) }

// MissingPrograms is those of programs that are not on PATH.
func MissingPrograms(programs []string) []string {
	var missing []string
	for _, program := range programs {
		if _, err := exec.LookPath(program); err != nil {
			missing = append(missing, program)
		}
	}
	return missing
}

// Path is the config file's path under $XDG_CONFIG_HOME, or ~/.config where that is unset.
func Path() (string, error) {
	return xdgPath("XDG_CONFIG_HOME", ".config", "config.yml")
}

// DefaultRegistry is where prs looks for the registry when nothing names one: repos.json
// under $XDG_DATA_HOME, or ~/.local/share where that is unset.
func DefaultRegistry() (string, error) {
	return xdgPath("XDG_DATA_HOME", filepath.Join(".local", "share"), "repos.json")
}

// ListingCacheDir is where the board keeps each provider's last listing, a file for each set
// of repos it reads, so the next start draws it before any read returns: listings under
// $XDG_CACHE_HOME/prs, or ~/.cache/prs where that is unset.
func ListingCacheDir() (string, error) {
	return xdgPath("XDG_CACHE_HOME", ".cache", "listings")
}

func xdgPath(variable, fallback, name string) (string, error) {
	dir := os.Getenv(variable)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding prs's %s: %w", name, err)
		}
		dir = filepath.Join(home, fallback)
	}
	return filepath.Join(dir, "prs", name), nil
}

// Load reads the config file and resolves the registry. A file that is not there is no
// error, and says nothing.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{File: path}
	var doc file
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	default:
		cfg.Exists = true
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		// An empty file says nothing, which yaml reports as io.EOF.
		if err := decoder.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
			return cfg, fmt.Errorf("reading %s: %w", path, err)
		}
	}
	if cfg.Registry, cfg.Owners, cfg.ReposFrom, err = repos(doc); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.RefreshEvery, cfg.RefreshFrom, err = interval(doc.RefreshEverySeconds, "refresh_every_seconds", DefaultRefreshEvery); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.ClosedRefreshEvery, cfg.ClosedRefreshFrom, err = interval(doc.ClosedRefreshEverySeconds, "closed_refresh_every_seconds", DefaultClosedRefreshEvery); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	cfg.DiffSectionMaxLines, cfg.DiffSectionFrom = DefaultDiffSectionMaxLines, FromDefault
	if node := doc.DiffSectionMaxLines; node.Kind != 0 {
		lines, err := wholeNumber("diff_section_max_lines", "lines", node)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
		cfg.DiffSectionMaxLines, cfg.DiffSectionFrom = lines, "diff_section_max_lines in the config file"
	}
	for i, action := range doc.WindowActions {
		if err := action.check(); err != nil {
			return cfg, fmt.Errorf("%s: window_actions[%d]: %w", path, i, err)
		}
	}
	if key := repeatedKey(doc.WindowActions); key != "" {
		return cfg, fmt.Errorf("%s: window_actions: two actions take the key %q", path, key)
	}
	cfg.WindowActions = doc.WindowActions
	return cfg, nil
}

// interval reads a key of whole seconds as written, and the layer that set it: the file, or
// fallback where the file does not say.
func interval(node yaml.Node, key string, fallback time.Duration) (time.Duration, string, error) {
	if node.Kind == 0 {
		return fallback, FromDefault, nil
	}
	seconds, err := wholeNumber(key, "seconds", node)
	if err != nil {
		return 0, "", err
	}
	return time.Duration(seconds) * time.Second, key + " in the config file", nil
}

// wholeNumber reads the key's value as written, a whole number of units, 1 or more. yaml
// would truncate 0.5 into an int without a word, so anything not tagged an integer is
// refused with the value it held.
func wholeNumber(key, units string, node yaml.Node) (int, error) {
	var n int
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!int" || node.Decode(&n) != nil || n < 1 {
		return 0, fmt.Errorf("%s is %q, and it takes a whole number of %s, 1 or more", key, node.Value, units)
	}
	return n, nil
}

// ownerName is what GitHub allows in a user or organization's login.
var ownerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// repos is the registry the variable names, else the file's repos_registry, else the
// file's owners, else the registry at the default path, else nothing, which reads the
// token's own account. It says which of them it was. A file naming both a registry and
// owners is refused rather than one silently winning.
func repos(doc file) (string, []string, string, error) {
	if named := os.Getenv(RegistryVariable); named != "" {
		path, err := expand(named)
		if err != nil {
			return "", nil, "", fmt.Errorf("%s: %w", RegistryVariable, err)
		}
		return path, nil, FromVariable, nil
	}
	if doc.Registry != "" && len(doc.Owners) > 0 {
		return "", nil, "", errors.New("repos_registry and owners each say which repos to read, so name one of them")
	}
	if doc.Registry != "" {
		path, err := expand(doc.Registry)
		if err != nil {
			return "", nil, "", fmt.Errorf("repos_registry: %w", err)
		}
		return path, nil, FromFile, nil
	}
	if len(doc.Owners) > 0 {
		for _, owner := range doc.Owners {
			if !ownerName.MatchString(owner) {
				return "", nil, "", fmt.Errorf("owners: %q is not a GitHub user or organization", owner)
			}
		}
		return "", doc.Owners, FromOwners, nil
	}
	path, err := DefaultRegistry()
	if err != nil {
		return "", nil, "", err
	}
	switch _, err := os.Stat(path); {
	case err == nil:
		return path, nil, FromDefault, nil
	case errors.Is(err, fs.ErrNotExist):
		return "", nil, FromViewer, nil
	default:
		return "", nil, "", fmt.Errorf("reading the registry at the default %s: %w", path, err)
	}
}

// expand turns a ~-rooted path absolute, and refuses one that is neither, because a
// relative path in a file under $XDG_CONFIG_HOME has nothing sensible to be relative to.
func expand(path string) (string, error) {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, rest), nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is neither absolute nor ~-rooted", path)
	}
	return path, nil
}

func (a WindowAction) check() error {
	switch {
	case a.Key == "":
		return errors.New("key is empty")
	case a.Label == "":
		return errors.New("label is empty")
	case a.Command == "":
		return errors.New("command is empty")
	case a.Window == "":
		return errors.New("window is empty")
	}
	for _, field := range []string{a.Window, a.Command} {
		for _, name := range placeholder.FindAllString(field, -1) {
			if _, ok := placeholders[name]; !ok {
				return fmt.Errorf("%s names %s, and only %s are filled in", field, name, strings.Join(Placeholders(), ", "))
			}
		}
	}
	return nil
}

func repeatedKey(actions []WindowAction) string {
	seen := map[string]bool{}
	for _, action := range actions {
		if seen[action.Key] {
			return action.Key
		}
		seen[action.Key] = true
	}
	return ""
}
