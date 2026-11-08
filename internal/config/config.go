// Package config reads prs's config file, which names the registry of repos or the owners
// read in its place, how often the board reads them, how large a diff a pull request's
// document shows whole, the keys a person adds to it, and the fixes and worktree root the menu
// of a pull request that cannot merge uses.
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
	// again, and the Summary tab what changed in the history it counts from.
	// ClosedRefreshFrom names the layer that set it.
	ClosedRefreshEvery time.Duration
	ClosedRefreshFrom  string
	// DiffSectionMaxLines is the most lines a pull request may change for its document's Diff
	// section to show its diff whole. A larger one's section names its size instead.
	// DiffSectionFrom names the layer that set it.
	DiffSectionMaxLines int
	DiffSectionFrom     string
	// WindowActions are the keys the file adds, each opening a tmux window on a pull request.
	WindowActions []WindowAction
	// Recaps are the commands the Recaps tab runs, and empty where the file names none.
	Recaps Recaps
	// WorktreeRoot is where the fix menu adds a worktree to resolve a conflict in, a directory
	// for each repo's checkout under it, and WorktreeRootFrom names the layer that set it.
	WorktreeRoot     string
	WorktreeRootFrom string
	// MergeFixes are the fixes the file adds to the menu a pull request that cannot merge
	// opens, each running its command in a tmux window behind the board.
	MergeFixes []MergeFix
}

// Recaps are the commands the Recaps tab runs, each an argv run with no shell. List prints the
// windows a recap is written for, in the order the tab draws them, one a line as its kind and
// its label, and is left as written, so a list run through sh may hold braces. Show prints one
// window's recap as markdown, and Write writes it, which is a model call. Show and Write take
// the names RecapPlaceholders lists. Write may be left out, and the tab then has no key that
// writes.
type Recaps struct {
	// Needs are the programs the commands run, which an argv starting sh hides. A machine
	// missing one shows no Recaps tab.
	Needs []string `yaml:"needs"`
	List  []string `yaml:"list"`
	Show  []string `yaml:"show"`
	Write []string `yaml:"write"`
}

// Named says the file names the Recaps tab's commands.
func (r Recaps) Named() bool { return len(r.List) > 0 }

// Missing is the programs the commands need, Needs and each argv's first, that are not on
// PATH. prs shows the Recaps tab only where none is.
func (r Recaps) Missing() []string {
	var programs []string
	for _, argv := range [][]string{r.List, r.Show, r.Write} {
		if len(argv) > 0 && !slices.Contains(programs, argv[0]) {
			programs = append(programs, argv[0])
		}
	}
	for _, program := range r.Needs {
		if !slices.Contains(programs, program) {
			programs = append(programs, program)
		}
	}
	return MissingPrograms(programs)
}

// RecapFields are what a recaps Show or Write can name of a window.
type RecapFields struct {
	Kind  string
	Label string
}

// recapPlaceholders fills each name a Show or Write may hold.
var recapPlaceholders = placeholderSet[RecapFields]{
	"{kind}":  func(f RecapFields) string { return f.Kind },
	"{label}": func(f RecapFields) string { return f.Label },
}

// RecapPlaceholders are the names FillRecap fills, sorted.
func RecapPlaceholders() []string { return recapPlaceholders.names() }

// FillRecap is a Show or Write argv with a window's fields in each placeholder.
func FillRecap(argv []string, fields RecapFields) []string {
	filled := make([]string, len(argv))
	for i, arg := range argv {
		filled[i] = recapPlaceholders.fill(arg, fields)
	}
	return filled
}

func (r Recaps) check() error {
	if !r.Named() {
		if len(r.Show) > 0 || len(r.Write) > 0 || len(r.Needs) > 0 {
			return errors.New("list is empty, and the tab needs it to know which windows there are")
		}
		return nil
	}
	if len(r.Show) == 0 {
		return errors.New("show is empty, and the tab needs it to show a window's recap")
	}
	for _, command := range []struct {
		key  string
		argv []string
	}{{"show", r.Show}, {"write", r.Write}} {
		for _, arg := range command.argv {
			if err := recapPlaceholders.check(command.key, arg); err != nil {
				return err
			}
		}
	}
	return nil
}

// DefaultRefreshEvery is how often the board reads each provider while it is open, where the
// file does not say. Each GitHub read spends points from the token's hourly GraphQL allowance.
const DefaultRefreshEvery = time.Minute

// DefaultClosedRefreshEvery is how often the board reads the recently closed pull requests,
// and the Summary tab its history, where the file does not say. Each read costs GitHub about
// as much as the open one, and what it finds changes only when a pull request closes, which
// the open read already shows.
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
	// RunsReviewRound says the command runs a Claude review round on the pull request, so the
	// row reads the round as running while the window is open and the round has not finished.
	RunsReviewRound bool `yaml:"runs_review_round"`
}

// MergeFix is a fix the menu of a pull request that cannot merge offers, on its key, for the
// blockers it names. Its command runs in a new tmux window behind the board, started in the
// pull request's checkout, and it is offered only where every program it needs is on PATH.
type MergeFix struct {
	Key    string `yaml:"key"`
	Label  string `yaml:"label"`
	Detail string `yaml:"detail"`
	// Needs are the programs the command runs, as a window action's are.
	Needs []string `yaml:"needs"`
	// Blockers are the kinds of blocker the fix is offered for, from BlockerKinds.
	Blockers []string `yaml:"blockers"`
	// Window is the tmux window's name, which takes the row's placeholders alone, so the board
	// can find it open from the row. Command takes FixPlaceholders.
	Window  string `yaml:"window"`
	Command string `yaml:"command"`
	// RowReads is what the pull request's row reads while the fix's window is open, and
	// "fixing" where the file does not say.
	RowReads string `yaml:"row_reads"`
}

// The kinds of blocker a merge fix names, as the fix's command receives them in {blocker}.
const (
	Conflicts     = "conflicts"
	Behind        = "behind"
	FailingChecks = "failing-checks"
	Draft         = "draft"
)

// BlockerKinds is every kind of blocker a merge fix can name.
var BlockerKinds = []string{Conflicts, Behind, FailingChecks, Draft}

// Reads is what the row reads while the fix's window is open.
func (f MergeFix) Reads() string {
	if f.RowReads == "" {
		return "fixing"
	}
	return f.RowReads
}

// Missing is the programs the fix needs that are not on PATH. The menu offers it only where
// none is.
func (f MergeFix) Missing() []string { return MissingPrograms(f.Needs) }

// file is the document as written.
type file struct {
	Registry                  string         `yaml:"repos_registry"`
	Owners                    []string       `yaml:"owners"`
	RefreshEverySeconds       yaml.Node      `yaml:"refresh_every_seconds"`
	ClosedRefreshEverySeconds yaml.Node      `yaml:"closed_refresh_every_seconds"`
	DiffSectionMaxLines       yaml.Node      `yaml:"diff_section_max_lines"`
	WindowActions             []WindowAction `yaml:"window_actions"`
	Recaps                    Recaps         `yaml:"recaps"`
	WorktreeRoot              string         `yaml:"worktree_root"`
	MergeFixes                []MergeFix     `yaml:"merge_fixes"`
}

// Fields are what a Window or Command can name of a pull request. A window action's take the
// row's alone, and a merge fix's Command the rest as well, each empty where it does not apply.
type Fields struct {
	Number int
	Repo   string
	Slug   string
	// Path is the repo's checkout, and Worktree the one the menu merged its base into, where
	// that one is under way.
	Path, Worktree string
	// Head and Base are the branch the pull request merges from and the one it merges into.
	Head, Base string
	// Blocker is the kind from BlockerKinds, and Log the file holding its failing checks' log.
	Blocker, Log string
}

// rowPlaceholders fills each name a window action's Window or Command, or a merge fix's
// Window, may hold.
var rowPlaceholders = placeholderSet[Fields]{
	"{number}": func(f Fields) string { return strconv.Itoa(f.Number) },
	"{repo}":   func(f Fields) string { return f.Repo },
	"{slug}":   func(f Fields) string { return f.Slug },
}

// fixPlaceholders fills each name a merge fix's Command may hold: the row's, and these.
var fixPlaceholders = rowPlaceholders.with(placeholderSet[Fields]{
	"{path}":     func(f Fields) string { return f.Path },
	"{worktree}": func(f Fields) string { return f.Worktree },
	"{head}":     func(f Fields) string { return f.Head },
	"{base}":     func(f Fields) string { return f.Base },
	"{blocker}":  func(f Fields) string { return f.Blocker },
	"{log}":      func(f Fields) string { return f.Log },
})

// Placeholders are the names a window action's Window and Command, and a merge fix's Window,
// take, sorted.
func Placeholders() []string { return rowPlaceholders.names() }

// FixPlaceholders are the names a merge fix's Command takes, sorted.
func FixPlaceholders() []string { return fixPlaceholders.names() }

// Fill puts a pull request's fields into a Window, which tmux takes as a name.
func Fill(template string, fields Fields) string { return fixPlaceholders.fill(template, fields) }

// FillCommand puts a pull request's fields into a Command, with each of
// ShellQuotedPlaceholders quoted for the shell. A branch name may hold ; or $, and anyone who
// can open a pull request names one.
func FillCommand(template string, fields Fields) string {
	return fixPlaceholders.fillAs(template, fields, func(name, value string) string {
		if slices.Contains(ShellQuotedPlaceholders, name) {
			return shellQuoted(value)
		}
		return value
	})
}

// ShellQuotedPlaceholders are the names whose values come from outside prs, a path or a
// branch, so FillCommand quotes each and a Command may not put one inside quotes of its own.
// The rest are a number, a repo or a blocker kind, filled as they are.
var ShellQuotedPlaceholders = []string{"{base}", "{head}", "{log}", "{path}", "{worktree}"}

// placeholderSet is the names a templated value may hold, each filled from fields of F. Every
// templated key in the file matches, fills and refuses its names through one, so the keys
// cannot drift apart on what a placeholder is.
type placeholderSet[F any] map[string]func(F) string

var placeholder = regexp.MustCompile(`\{[^}]*\}`)

func (s placeholderSet[F]) names() []string { return slices.Sorted(maps.Keys(s)) }

// with is the set with extra's names added.
func (s placeholderSet[F]) with(extra placeholderSet[F]) placeholderSet[F] {
	joined := maps.Clone(s)
	maps.Copy(joined, extra)
	return joined
}

// fill puts the fields into each name text holds.
func (s placeholderSet[F]) fill(text string, fields F) string {
	return s.fillAs(text, fields, func(_, value string) string { return value })
}

// fillAs is fill with each value passed through as, which sees the name it fills. One pass
// fills every name, so a value holding a name is not filled again.
func (s placeholderSet[F]) fillAs(text string, fields F, as func(name, value string) string) string {
	var pairs []string
	for name, value := range s {
		pairs = append(pairs, name, as(name, value(fields)))
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// check refuses a name text holds that the set does not fill, saying where through where.
func (s placeholderSet[F]) check(where, text string) error {
	for _, name := range placeholder.FindAllString(text, -1) {
		if _, ok := s[name]; !ok {
			return fmt.Errorf("%s names %s, and only %s are filled in", where, name, strings.Join(s.names(), ", "))
		}
	}
	return nil
}

// insideQuotes is the first of names the shell command puts inside single or double quotes,
// where the quotes FillCommand adds would end the command's own rather than nest in them.
func insideQuotes(command string, names []string) string {
	var quote byte
	for i := 0; i < len(command); i++ {
		switch c := command[i]; {
		case c == '\\' && quote != '\'':
			i++
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case c == quote:
			quote = 0
		case quote != 0 && c == '{':
			for _, name := range names {
				if strings.HasPrefix(command[i:], name) {
					return name
				}
			}
		}
	}
	return ""
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9@%+=:,./_-]+$`)

// shellQuoted is value as one shell word, left bare where nothing in it is special, as
// Python's shlex.quote does.
func shellQuoted(value string) string {
	if shellSafe.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
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

// RecapWritesDir is where the recaps tab keeps each write's output, one file a window, which
// that window's next write replaces: recap-writes under $XDG_CACHE_HOME/prs, or ~/.cache/prs
// where that is unset.
func RecapWritesDir() (string, error) {
	return xdgPath("XDG_CACHE_HOME", ".cache", "recap-writes")
}

// ClosedPullRequestsDir is where the Summary tab and prs summary keep the merged and closed
// pull requests they count, a SQLite file for each set of repos: closed-pull-requests under
// $XDG_CACHE_HOME/prs, or ~/.cache/prs where that is unset.
func ClosedPullRequestsDir() (string, error) {
	return xdgPath("XDG_CACHE_HOME", ".cache", "closed-pull-requests")
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
	if key := repeatedKey(doc.WindowActions, func(a WindowAction) string { return a.Key }); key != "" {
		return cfg, fmt.Errorf("%s: window_actions: two actions take the key %q", path, key)
	}
	cfg.WindowActions = doc.WindowActions
	if err := doc.Recaps.check(); err != nil {
		return cfg, fmt.Errorf("%s: recaps: %w", path, err)
	}
	cfg.Recaps = doc.Recaps
	for i, fix := range doc.MergeFixes {
		if err := fix.check(); err != nil {
			return cfg, fmt.Errorf("%s: merge_fixes[%d]: %w", path, i, err)
		}
	}
	if key := repeatedKey(doc.MergeFixes, func(f MergeFix) string { return f.Key }); key != "" {
		return cfg, fmt.Errorf("%s: merge_fixes: two fixes take the key %q", path, key)
	}
	cfg.MergeFixes = doc.MergeFixes
	if cfg.WorktreeRoot, cfg.WorktreeRootFrom, err = worktreeRoot(doc.WorktreeRoot); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// worktreeRoot is the file's worktree_root, or worktrees under $XDG_DATA_HOME/prs where the
// file does not say, and the layer that set it.
func worktreeRoot(named string) (string, string, error) {
	if named == "" {
		root, err := xdgPath("XDG_DATA_HOME", filepath.Join(".local", "share"), "worktrees")
		return root, FromDefault, err
	}
	root, err := expand(named)
	if err != nil {
		return "", "", fmt.Errorf("worktree_root: %w", err)
	}
	return root, "worktree_root in the config file", nil
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
	if err := keyed(a.Key, a.Label, a.Window, a.Command); err != nil {
		return err
	}
	for _, field := range []string{a.Window, a.Command} {
		if err := rowPlaceholders.check(field, field); err != nil {
			return err
		}
	}
	return nil
}

func (f MergeFix) check() error {
	if err := keyed(f.Key, f.Label, f.Window, f.Command); err != nil {
		return err
	}
	if len(f.Blockers) == 0 {
		return fmt.Errorf("blockers is empty, and it names which of %s the fix is offered for", strings.Join(BlockerKinds, ", "))
	}
	for _, kind := range f.Blockers {
		if !slices.Contains(BlockerKinds, kind) {
			return fmt.Errorf("blockers names %q, and a fix is offered only for %s", kind, strings.Join(BlockerKinds, ", "))
		}
	}
	if err := rowPlaceholders.check(f.Window, f.Window); err != nil {
		return err
	}
	if name := insideQuotes(f.Command, ShellQuotedPlaceholders); name != "" {
		return fmt.Errorf("command puts %s inside quotes, and prs quotes it already, so it goes outside them", name)
	}
	return fixPlaceholders.check(f.Command, f.Command)
}

// keyed refuses a key, label, window or command left empty.
func keyed(key, label, window, command string) error {
	switch {
	case key == "":
		return errors.New("key is empty")
	case label == "":
		return errors.New("label is empty")
	case command == "":
		return errors.New("command is empty")
	case window == "":
		return errors.New("window is empty")
	}
	return nil
}

func repeatedKey[T any](entries []T, key func(T) string) string {
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[key(entry)] {
			return key(entry)
		}
		seen[key(entry)] = true
	}
	return ""
}
