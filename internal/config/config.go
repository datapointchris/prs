// Package config reads prs's config file, which names the registry of repos and the
// keys a person adds to the board.
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

	"gopkg.in/yaml.v3"
)

// RegistryVariable names the registry for one shell, ahead of the config file.
const RegistryVariable = "PRS_REPOS_REGISTRY"

// Where the registry's path came from, in the order they are asked.
const (
	FromVariable = RegistryVariable
	FromFile     = "repos_registry in the config file"
	FromDefault  = "the default"
)

// Config is what the environment and the file say, with each path expanded.
type Config struct {
	// File is the path read, whether or not anything was there.
	File string
	// Exists says the file was there to read.
	Exists bool
	// Registry is the repo registry's path, and RegistryFrom the layer that named it.
	Registry     string
	RegistryFrom string
	// WindowActions are the keys the file adds, each opening a tmux window on a pull request.
	WindowActions []WindowAction
}

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
	Registry      string         `yaml:"repos_registry"`
	WindowActions []WindowAction `yaml:"window_actions"`
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
	if cfg.Registry, cfg.RegistryFrom, err = registryPath(doc.Registry); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
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

// registryPath is the registry the variable names, else the file's repos_registry, else
// the default, and which of them it was.
func registryPath(fromFile string) (string, string, error) {
	if named := os.Getenv(RegistryVariable); named != "" {
		path, err := expand(named)
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", RegistryVariable, err)
		}
		return path, FromVariable, nil
	}
	if fromFile != "" {
		path, err := expand(fromFile)
		if err != nil {
			return "", "", fmt.Errorf("repos_registry: %w", err)
		}
		return path, FromFile, nil
	}
	path, err := DefaultRegistry()
	return path, FromDefault, err
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
