// Package config reads prs's config file, which names the registry of repos and the
// keys a person adds to the board.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is what the file says, with each path expanded.
type Config struct {
	// File is the path read, whether or not anything was there.
	File string
	// Exists says the file was there to read.
	Exists bool
	// Registry is the repo registry's path, empty where the file names none.
	Registry string
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
	// {number}, {repo} and {slug}.
	Window  string `yaml:"window"`
	Command string `yaml:"command"`
}

// file is the document as written.
type file struct {
	Registry      string         `yaml:"registry"`
	WindowActions []WindowAction `yaml:"window_actions"`
}

// ReservedKeys are the keys prs and its board bind, which a window action cannot take.
var ReservedKeys = []string{
	"d", "p", "o", "y", "c", "x", "m",
	"j", "k", "g", "G", "J", "K", "R", "q", "v", "f", "/", "?", "[", "]",
	"enter", "space", "esc", "tab", "shift+tab",
}

var placeholder = regexp.MustCompile(`\{[^}]*\}`)

// Placeholders are what a window action's Window and Command may name.
var Placeholders = []string{"{number}", "{repo}", "{slug}"}

// Path is the config file's path under $XDG_CONFIG_HOME, or ~/.config where that is unset.
func Path() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding the config file: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "prs", "config.yml"), nil
}

// Load reads the config file. A file that is not there is no error, and says nothing.
func Load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, err
	}
	cfg := Config{File: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg.Exists = true

	var doc file
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	// An empty file says nothing, which yaml reports as io.EOF.
	if err := decoder.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("reading %s: %w", path, err)
	}
	if doc.Registry != "" {
		if cfg.Registry, err = expand(doc.Registry); err != nil {
			return cfg, fmt.Errorf("%s: registry: %w", path, err)
		}
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
	case slices.Contains(ReservedKeys, a.Key):
		return fmt.Errorf("key %q is one prs binds itself", a.Key)
	case a.Label == "":
		return errors.New("label is empty")
	case a.Command == "":
		return errors.New("command is empty")
	case a.Window == "":
		return errors.New("window is empty")
	}
	for _, field := range []string{a.Window, a.Command} {
		for _, name := range placeholder.FindAllString(field, -1) {
			if !slices.Contains(Placeholders, name) {
				return fmt.Errorf("%s names %s, and only %s are filled in", field, name, strings.Join(Placeholders, ", "))
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

// Fill puts a pull request's number, repo and slug into a Window or Command.
func Fill(template string, number int, repo, slug string) string {
	return strings.NewReplacer(
		"{number}", fmt.Sprint(number),
		"{repo}", repo,
		"{slug}", slug,
	).Replace(template)
}
