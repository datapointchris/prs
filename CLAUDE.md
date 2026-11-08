# prs — Claude Code instructions

A Go command whose board is a view on gotermboard. `cmd/prs` holds the commands.
`internal/board` is the view: `rows.go` the row grammar, `actions.go` the key table,
`board.go` the sources, the frame and the `--json` snapshot. `internal/pullrequests` reads
each provider through its own client, `internal/registry` reads the registry file, and
`internal/config` reads `$XDG_CONFIG_HOME/prs/config.yml`.

## Constraints that must not regress

- **prs knows nothing of one person's setup.** The registry's path, and every key that
  runs a program beyond gh, bbkt, git, nvim, less and tmux, come from the config. A
  default here naming a path outside prs's own directories is that setup compiled in.
- **A read never asks the user anything.** gh and bbkt run with stdin closed and a timeout,
  because a prompt inside a read would hang the board with nothing on screen to answer it.
- **A repo is matched by owner and name, never the bare name.** The viewer's own search
  covers all of GitHub, so a pull request on another owner's repo of the same name would
  otherwise take this checkout.
- **A stacked pull request finds its parent by repo and branch together.** Branch names
  recur across repos.
- **State is a word on the row, and a glyph never changes with state.** Two glyphs are not
  guaranteed one width, so a glyph that changed would move every column after it. Each
  glyph is a private-use codepoint with its name in a comment beside it.
- **A diff count the provider did not report is null, never 0.** Bitbucket's listing
  carries none, and a zero would read as a pull request that changed nothing.
- **A key that changes a pull request acts only on the row under the cursor, or asks
  first.**
- **No test reads a real account or starts a real program a key runs.** `cmd/prs/tui_test.go`
  puts a stand-in `gh`, `tmux` and `claude` on `PATH`, and `testdata/search.json` is a
  GraphQL answer in GitHub's published shape.

## gotermboard facts the board relies on

- **A board's key takes that key from the table even where it does nothing**, so prs's `p`
  hides the table's side-panel toggle.
- **enter is prs's own key**, because the table's enter opens a reader titled with a row's
  cells, which here are its state words. prs's enter opens it titled with the pull
  request's title, and otherwise does what the table's enter does.
- **A row's `Under` line shows only once `f` turns summaries on.**
- **A board hears no message back from a command a key started**, so every key here runs
  to its end before it says what it did.

## Never write the breaking-change trailer in a commit message

Those two words, either number, colon or not, subject or body, mark a major. On 0.x,
`allow-initial-development-versions` turns that into a minor. From v1 on they tag v2, and
the module path carries no `/v2` suffix, so `go install …@latest` stays on the highest v1
and every installed copy is stranded. **The ban covers a commit that merely discusses the
trailer**; name it some other way and never quote it. Deliberate majors use
`chore(release-major)`.
