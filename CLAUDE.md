# prs — Claude Code instructions

A Go command whose board is a view on gotermboard. `cmd/prs` holds the commands.
`internal/board` is the view: `rows.go` the row grammar, `actions.go` the key table,
`work.go` what a key runs in the background or hands the terminal to, `board.go` the
sources, the frame and the `--json` snapshot, `listing_cache.go` the last listing kept on
disk for the next start, `table.go` what `prs list` prints, `document.go` each pull
request's markdown document, a section per heading, and `opened_on.go` the pull request
`prs tui --pr` names. `internal/pullrequests` reads each provider: GitHub in-process
through go-gh, Bitbucket through bbkt. `provider.go` holds everything prs does
differently by host, `detail.go` the per-PR detail cache, and `reference.go` what `--pr`
takes and how it is read.
`internal/registry` reads the registry file, and `internal/config` reads
`$XDG_CONFIG_HOME/prs/config.yml` and resolves which repos to read.
`cmd/prs/list.schema.json` is the shape of one row in `prs list --json` and in
`prs tui --json`, and a test holds both outputs to it.

## Constraints that must not regress

- **prs knows nothing of one person's setup.** The repos are `PRS_REPOS_REGISTRY`, then
  `repos_registry` in the config, then `owners` in the config, then `repos.json` under
  prs's own data directory, then the viewer's own repos, so a stranger's first run lists
  something. A key whose program is one person's choice, such as claude, comes from the
  config, and only general tools a key needs are compiled in.
- **A read never asks the user anything.** bbkt, and gh where it is asked for its token,
  run with stdin closed and a timeout, and so does every GitHub request. A prompt inside a
  read would hang the board with nothing on screen to answer it.
- **A read asks the provider for the registry's repos by name.** An owner-wide search
  fills its page with repos the registry does not name, and fails outright for a large
  organization. Only `owners` in the config, or no registry at all, reads an owner whole.
- **A repo is owner/name on both providers.** Two Bitbucket projects can each hold a repo
  of one name. GitHub matches owners and names in any case, so the checkout join does too.
- **A repo whose read failed keeps its last rows, and a note says so.** One repo's failure
  must not take the others' rows with it.
- **`prs tui` draws the last listing before any read returns.** Each read that lands is
  written to `$XDG_CACHE_HOME/prs/listings/<scope digest>.json`, one file for each set of
  repos and owners, and a provider's rows come from there, with their age on the strip,
  until its first read lands. A file is drawn only for the scope it was read for, so a
  registry edit never shows another set's rows. Only the interactive board reads or writes
  it, because `prs list`, `--once` and `--json` wait for their reads.
- **A refresh never moves the row under the cursor.** A pull request a read drops stays
  under the cursor, marked gone, until the cursor leaves it, and m and x refuse it.
- **`prs list`, `--json` and `--once` exit 1 when a read failed or left a pull request
  out**, because a failed read and a repo with no open pull requests otherwise print the
  same rows. A note leaves one out: a refused search, a Bitbucket repo that failed or is
  not checked out, a search past its page.
- **A stacked pull request finds its parent by repo and branch together.** Branch names
  recur across repos.
- **State is a word on the row, and a glyph never changes with state.** Two glyphs are not
  guaranteed one width, so a glyph that changed would move every column after it. Each
  glyph is a private-use codepoint with its name in a comment beside it.
- **A diff count the provider did not report is null, never 0.** Bitbucket's listing
  carries none, and a zero would read as a pull request that changed nothing.
- **A key that changes a pull request acts only on the row under the cursor, or asks
  first.** Merge asks with enter meaning yes, and close with enter meaning no, because a
  reflex merge lands work that was landing anyway and a reflex close discards a branch
  nobody read. A pull request with a change under way refuses another.
- **A key that waits on the network runs in the background**, through `work` in
  `work.go`, so the list stays usable and the footer names it until it ends. Every client
  it runs, git included, goes through `pullrequests.Run`, which a read's client shares.
- **The diff's socket and window name are a contract.** A program split into the tmux
  window, such as a Claude session explaining the diff, reads `diff <repo> #<n>` off the
  window's name and asks nvim on `prs-diff-<repo>-<n>.sock` what is under the cursor. So
  `d` names prs's window that way while nvim has it.
- **`prs tui --pr` is a contract too.** Other terminal programs jump to a pull request by
  running it with `owner/name#number` or a URL, and get their screen back when `q` exits
  0. A pull request it cannot read is refused before anything draws, with one line on
  stderr and exit 1, so a caller can show why.
- **A Provider verb a host's client lacks is nil**, and its key refuses with `lacking`.
- **A per-host fact lives in the Provider table**: the state words a client prints, mapped
  to `Standing`, and the path of a pull request's page, read by `PagePath`. The board
  switches over `Standing` rather than over a host's words.
- **`pull_requests` in `--json` holds only what the listings read.** The pull request
  `--pr` names is `opened_on`, and the board draws its own read only where no listing holds
  it.
- **A pull request's detail never holds up its row.** The board reads each GitHub row's
  detail in the background after the listing lands, a few at a time. It reads one again
  when its head commit, update time or checks state moves, or when its last read failed.
- **No test reads a real account or starts a real program a key runs.** `cmd/prs/tui_test.go`
  puts a stand-in on `PATH` for every program a read or a key in the test runs, and its
  testdata answers in each provider's published shape. GitHub's stand-in is an HTTP server
  on a unix socket, named by `http_unix_socket` in a gh config the test writes, so go-gh
  sends every request there and none reaches GitHub.

## gotermboard facts the board relies on

- **A board's key takes that key from the table even where it does nothing.** A window
  action on a bound key would take it the same way, such as `p`, the side panel's toggle,
  so `board.Refused` refuses one, reading the table's keys from `gotermboard.TableKeys()`
  and adding esc.
- **The side panel and the Reader draw one document, `MarkdownDocument`.** gotermboard
  shows a row's `Panel` beside the list from `PanelFrom` columns, and enter opens its
  `Document` on the Reader wherever the panel shows it, so the two are the same markdown.
  The Reader's `[` and `]` land on each heading, so text written under a section goes
  through `nestedAt`, which keeps its own headings inside that section.
- **A row's `ReaderTitle` is its pull request's title**, because the table's enter would
  otherwise title the Reader with the row's first cells, which here are its state words.
- **A row's `Under` line shows only once `f` turns summaries on.**
- **The message a key's command ends in reaches `Board.Receive`**, gotermboard's
  `ReceivingBoard`, and `Board.UnderWay`, its `BusyBoard`, puts the work under way on the
  footer after a spinner. A key's own `m.Say` shows until the next key, and the under-way
  line until the work ends.
- **`tea.ExecProcess` holds Bubble Tea's event loop until the program exits**, so no read
  lands while nvim or the editor has the terminal. The board reads the providers again
  once a program it handed the terminal to exits, and when the terminal gains focus, except
  a provider whose last good read is under `freshFor` old.
- **A read's spinner turns on the strip, through `MovingBoard`**, so the footer's height
  never changes with a read. It draws `gotermboard.SpinnerAt`, the footer's own spinner.
- **Bubble Tea v2 reports focus only where the View sets `ReportFocus`.** `reportsFocus`
  in `cmd/prs/tui.go` sets it on the board's View.

## Never write the breaking-change trailer in a commit message

Those two words, either number, colon or not, subject or body, mark a major. On 0.x,
`allow-initial-development-versions` turns that into a minor. From v1 on they tag v2, and
the module path carries no `/v2` suffix, so `go install …@latest` stays on the highest v1
and every installed copy is stranded. **The ban covers a commit that merely discusses the
trailer**; name it some other way and never quote it. Deliberate majors use
`chore(release-major)`.
