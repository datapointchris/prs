# prs — Claude Code instructions

A Go command whose board is a view on gotermboard. `cmd/prs` holds the commands.
`internal/board` is the view: `rows.go` the row grammar, `actions.go` the key table,
`work.go` what a key runs in the background or hands the terminal to, `board.go` the
sources, the frame and the `--json` snapshot, `sections.go` the people, bots and recently
closed groups, one record per pull request and the row held under the cursor,
`listing_cache.go` the last listing kept on
disk for the next start, `table.go` what `prs list` prints, `document.go` each pull
request's markdown document, a section per heading, `review_rounds.go` the round's cell
and the Review section, `tmux_window_watch.go` the tmux windows that run a round or a fix,
`fix_menu.go` the menu of fixes `m` and `f` open on a pull request GitHub will not merge,
`diff_files.go` the document and review threads a diff in nvim is handed, `opened_on.go`
the pull request `prs tui --pr` names, `summary_tab.go` and `summary_charts.go` the
Summary tab and its charts, and `recaps_tab.go` the recaps tab, which runs the three
commands `recaps` in the config names. `internal/summary` counts a window's pull requests,
and keeps the merged and closed ones in SQLite, brought up to date through `pullrequests`'
`github_history.go`. `internal/reviewrounds` reads a pull request's Claude review rounds out of its
detail read. `internal/pullrequests` reads each provider: GitHub in-process
through go-gh, Bitbucket through bbkt. `provider.go` holds everything prs does
differently by host, `detail.go` the per-PR detail cache, `diff.go` the per-PR diff cache
the Diff section reads, `fetch.go` a pull request's checkout and the fetch of its head that
the diff keys and the Diff section share, `merge_worktree.go` the files a conflict names
and the worktree it is resolved in, `reference.go` what `--pr` takes and how it is read,
and `since.go` what `--since` takes.
`internal/registry` reads the registry file, and `internal/config` reads
`$XDG_CONFIG_HOME/prs/config.yml` and resolves which repos to read.
`cmd/prs/list.schema.json` is the shape of one row in `prs list --json` and in
`prs tui --json`, and a test holds both outputs to it. `cmd/prs/diff-threads.schema.json`
is the shape of the threads file a diff in nvim is handed, held the same way, and
`cmd/prs/summary.schema.json` the shape of `prs summary --json`, and
`cmd/prs/recaps.schema.json` the shape of `prs tui --view recaps --json`.

A tab is an entry in `tuiTabs` in `cmd/prs/tui.go`: the name `--view` takes, the flags its
`--once` and `--json` read, why a config leaves it out, and how it opens on the board into
what gotermboard draws, what `--json` prints and why either exits 1. `--view`, its help and
`gotermboard.NewTabs` read that one list, and `drawnTabs` drops each tab `absent` names a
reason for. The pull requests' board is `board.View` and the Summary tab
`board.SummaryView`. A tab reading what the board reads lists the board's sources in its
own `Sources`, as the Summary tab does, so a read is not made twice.

## Constraints that must not regress

- **prs knows nothing of one person's setup.** The repos are `PRS_REPOS_REGISTRY`, then
  `repos_registry` in the config, then `owners` in the config, then `repos.json` under
  prs's own data directory, then the viewer's own repos, so a stranger's first run lists
  something. A key whose program is one person's choice, such as claude, comes from the
  config, and only general tools a key needs are compiled in.
- **A read never asks the user anything.** bbkt, and gh where it is asked for its token,
  run with stdin closed and a timeout, and so does every GitHub request. Every client runs
  in a session of its own, so ssh, which asks on `/dev/tty` rather than stdin, finds no
  terminal and fails. A prompt inside a read would hang the board with nothing on screen to
  answer it, and would read the keys meant for the board.
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
- **A refresh never moves the row under the cursor.** A pull request a read moves to
  another group stays under the cursor in the group it was drawn in, in its new state, and
  one no read lists stays there marked gone, each until the cursor leaves it. m and x
  refuse a gone one. A merge or close a key made here moves its row at once, and a cursor
  on it moves on to the next open row in its group, through `closeHere`, so a queue takes
  one key a pull request.
- **A pull request's group comes from its own record**: recently closed once its state is
  merged or closed, whichever listing read it, then bots where `bot` is true, then people.
  Where two listings read one pull request, the record updated last stands, because
  GitHub's open search still finds a pull request for a while after it merges. A merge or
  close a key made here stands over a record read before the key, and gives way once a
  listing read that started after the key holds the pull request, because that read is
  the evidence the change took.
- **The recently closed read runs on its own timer**, `closed_refresh_every_seconds`,
  because it costs GitHub as much as the open one. The Summary tab's history read shares
  it. Focus and a handover reread only the
  open listings. Its rows read no detail, because each detail read costs a point and the
  window holds as many rows as `--since` reaches. `readsDetail` decides which rows read
  one, for the prefetch and the document alike.
- **`prs list`, `--json` and `--once` exit 1 when a read failed or left an open pull
  request out**, because a failed read and a repo with no open pull requests otherwise
  print the same rows. A note leaves one out: a refused search, a Bitbucket repo that
  failed or is not checked out, a search past its page. A recently closed read past its
  page is not a gap, because it sorts the newest first so its page keeps them.
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
  nobody read. A pull request with a change under way refuses another. m on marked rows
  asks once, listing every title it merges and each it leaves out with why.
- **space marks only a row whose checks pass and whose merge state is ready**, so a batch
  holds only what would merge on its own. `mergeMeanings` decides which states are ready,
  for the row's word and the mark alike. A host with no merge state, as Bitbucket's
  listing carries none, is never marked.
- **A batch merges one pull request at a time, reading each again just before**, because
  a merge can leave the next on the same repo behind, conflicting, or based on a branch
  the batch just merged. One no longer ready with passing checks, or based on such a
  branch, is left and named, and the rest go on.
- **Every merge pins a head, through `pinFor`**: the one a diff key last fetched, or else
  the one the Diff section was read at, or else the one the board's record holds, so a
  commit pushed since the screen showed it is not merged unseen.
- **`i` diffs from the newest review's commit only where the fetched head holds it and took
  in no base since.** The row's count reads the detail, which can be older than the head
  `i` fetches, so `i` asks git through `HeadHolds` and `BaseMergedSince` after its fetch.
  Where the head does not hold that commit, a merge brought in the base, or no review was
  submitted, it opens the whole diff and says why. Where git cannot answer, it opens
  nothing and names git's failure, because a failure read as no would claim a rebase. Its
  threads file reads `since_review`, leaves out each LEFT thread and counts them, because
  its left side is the reviewed commit rather than the base. `D` is left unbound, so a key
  that deletes elsewhere does nothing here.
- **What keeps GitHub from merging a pull request is `blockerOf`'s alone**, so `m`, `f` and
  a refused merge open one menu. `m` on a row `blockerOf` names reads the pull request
  again first, because the listing's merge state can be minutes old, and asks to merge
  where the new read is ready. A refused merge reads it again too, and puts gh's words over
  the menu. Each menu choice is one key, and a menu built of nothing says why instead. No
  choice exits prs. A host with no merge state has no menu.
- **A conflict is resolved in a worktree detached at the pull request's head**, at
  `<worktree_root>/<checkout's directory>/pr-<n>`, never on its branch, because the branch
  is often checked out in another worktree and a local branch can hold commits nobody
  pushed. Its merge is `origin/<base>` into the head, never a rebase, and its push a plain
  `HEAD:refs/heads/<branch>`, which fails rather than overwrites where the branch moved. A
  merge left unmerged waits in the worktree, and `f` then `n` opens it again rather than
  merging twice. The commit runs the repo's hooks, so it waits up to `hooksTimeout`.
- **A merge fix's command quotes `ShellQuotedPlaceholders` for the shell**, because a
  branch name can hold `;` or `$`, and the config refuses a command that puts one inside
  quotes, where prs's quotes would end the command's. The rest are a number, a repo or a
  blocker kind, filled bare, so `'/review-pr {number}'` keeps working. A fix's window opens
  behind the board, through `gotermboard.OpenWindowBehind`, and its row reads `row_reads`
  while `tmuxWindows` lists it.
- **Whether a key may change a pull request is `refusal`'s alone**, which `writable` and
  `mergeable` both ask, so a single key and a batch never disagree.
- **A key that waits on the network runs in the background**, through `work` in
  `work.go`, so the list stays usable and the footer names it until it ends. Every client
  it runs, git included, goes through `pullrequests.Run`, which a read's client shares.
- **The diff's socket and window name are a contract.** A program split into the tmux
  window, such as a Claude session explaining the diff, reads `diff <repo> #<n>` off the
  window's name and asks nvim on `prs-diff-<repo>-<n>.sock` what is under the cursor. So
  a diff key names prs's window that way while nvim has it.
- **The variables nvim's diff starts with are a contract.** `PRS_DIFF_DOCUMENT` names
  `prs-diff-<repo>-<n>.md` and `PRS_DIFF_THREADS` names `prs-diff-<repo>-<n>.threads.json`,
  both beside the socket. An nvim config reads them to show the pull request beside the
  diff and each review thread on its line, so the threads file keeps each thread's `path`,
  `line` and `side`.
- **`prs tui --pr` is a contract too.** Other terminal programs jump to a pull request by
  running it with `owner/name#number` or a URL, and get their screen back when `q` exits
  0. A pull request it cannot read is refused before anything draws, with one line on
  stderr and exit 1, so a caller can show why.
- **A review round is read from the reviews and comments the detail read already holds**,
  so it costs no read of its own. A round's posts are read from the JSON block each closes
  with, never the markdown, and a post whose marker names a version other than
  `reviewrounds.Version` shows whole rather than read wrong. The Review section shows the
  newest round and each post read whole, and Comments leaves exactly those out, so no post
  reads twice. `reviewShows` is that one set.
- **A round's post counts only from an app on the repo or someone who can write to it**:
  an author GitHub types Bot, or associates as OWNER, MEMBER or COLLABORATOR. Anyone can
  comment with a round's marker, so anyone else's marked post shows whole and moves no cell.
- **A round with no round comment reads running only for `roundPostsWithin` after its
  newest post**, or while a window runs one, because a round cut short looks the same and
  is continued by running it again. After that it reads unfinished. Where the detail read
  holds only the newest comments, it reads its counts instead, because its round comment
  may sit outside the read.
- **A round record's statuses stay apart.** A skipped reviewer was skipped on purpose and
  reads as skipped. One not run or not posted counts as a gap beside the findings, and a
  round none ran reads `claude did not run`, never as a clean one.
- **`prs list`'s CLAUDE column draws only while a row fills it**, so a list of repos no
  round reviews never has it. On the board the round follows the review on a row's second
  line. `listColumns` and `boardColumns` hold every column with the cell it draws, so a
  column's header and its cells cannot drift apart.
- **A pull request is two lines on the board, and a field keeps its line.** The first line
  is `boardColumns` and the second is `secondLine`. A new field goes at the end of
  `secondLine`'s flow before the bot, or into `boardColumns`. The second line is cut at the
  table's edge, so the checkout's length decides what a narrow screen loses.
- **A field the board and `prs list` both show has one cell builder**, such as `numberCell`
  or `botCell`, so the two layouts choose only where it sits. `prs list` places them through
  `listColumns` and `titleLine`.
- **A window marked `runs_review_round` reads running only until a round finishes after
  prs opened it.** A session that ran a round keeps its window afterwards, so one prs did
  not open reads running only where no round has finished. tmux is asked every
  `tmuxWindowsEvery`, through `pullrequests.Run`, and only inside tmux.
- **A Provider verb a host's client lacks is nil**, and its key refuses with `lacking`.
- **A per-host fact lives in the Provider table**: the state words a client prints, mapped
  to `Standing`, and the path of a pull request's page, read by `PagePath`. The board
  switches over `Standing` rather than over a host's words.
- **`pull_requests` in `--json` holds only the open pull requests the listings read**, the
  set `prs list --json` prints, and `recently_closed` those merged or closed within the
  window. The pull request `--pr` names is `opened_on`, and the board draws its own read
  only where no listing holds it. Once merged or closed it sits in its own group above the
  others, never under recently closed.
- **A pull request's detail never holds up its row.** The board reads each GitHub row's
  detail in the background after the listing lands, a few at a time. It reads one again
  when its head commit, update time or checks state moves, or when its last read failed.
- **A pull request's diff is read from its host, and from its checkout only where the
  host's client has no diff.** The board writes nothing into a checkout nobody asked it to,
  so `FetchHead` fetches a Bitbucket head into `refs/remotes/origin/pr/<n>` alone, with no
  `FETCH_HEAD` and no base, while `d`'s `Fetch` also moves `origin/<base>`. Both resolve
  the checkout, head and base through `LocalDiffOf`, and fetch one at a time per checkout,
  because two at once contend for its ref locks. A merged Bitbucket one is refused, because
  in the checkout a merged branch diffs as nothing against its base. A diff the listing's
  counts put past `diff_section_max_lines` is never read, and a failed read waits a minute,
  doubling to half an hour, before it is read again for the same head.
- **A closed pull request is searched for by `updated:`, never `closed:`.** GitHub's
  `closed:` leaves out some pull requests that closed within its range, and a pull request
  is never last updated before it closed. The history reads every state, so a reopened
  one's newer open record stands over its closed row, and the recently closed read drops
  what closed before `--since`.
- **The closed history's coverage moves only when every search of a read answered.** The
  store's coverage claims every pull request updated since its start, so `Keep` writes the
  rows and the coverage in one transaction, and a read with one failed search keeps
  nothing. After the first read of a window, a read asks only for what was updated since
  the last, and one reaching past the coverage's start reads back to it. The coverage only
  widens, because a board and a `prs summary` can sync one file at once.
- **A history search past GitHub's 1,000-result cap splits its stretch in halves**, as
  often as it takes, because the search returns no more than that however many pages
  are read. A pull request on a split's middle instant is read by both halves, and the
  store keeps one row of it.
- **The closed history is a cache.** Its version is a digest of its schema, so a file of
  another shape is emptied and read again rather than migrated. There is one per set of
  repos, named by `Scope.Names`, so a registry edit starts a new one.
- **The recaps tab reads nothing its commands print but the list's `<kind> <label>`
  lines.** A recap is the markdown show printed, drawn whole. The commands are the
  config's, so prs knows nothing of the program behind them, and a config naming no list,
  or a machine missing a program `needs` names, draws no recaps tab rather than an empty
  one.
- **The recaps tab's write is a model call, so `w` asks first, naming it, with enter
  meaning no**, because a reflex enter would spend the call. It runs through
  `pullrequests.RunDetached`, in a session of its own with its output in a file under the
  cache, so quitting prs leaves it to finish, and nothing bounds it. The list runs again
  once it ends, so a window written for the first time joins it.
- **Each run of the recaps list reads every recap again.** A recap read before the newest
  list run began is due, so `R`, the timer and a write's end all reach every recap, and a
  read under way when a write ended is read again.
- **No test reads a real account or starts a real program a key runs.** `cmd/prs/tui_test.go`
  puts a stand-in on `PATH` for every program a read or a key in the test runs, and its
  testdata answers in each provider's published shape. GitHub's stand-in is an HTTP server
  on a unix socket, named by `http_unix_socket` in a gh config the test writes, so go-gh
  sends every request there and none reaches GitHub. It answers GraphQL and REST's diff
  media type, and the stand-in git answers a diff from the same testdata file.

## gotermboard facts the board relies on

- **A board's key takes that key from the table even where it does nothing.** A window
  action on a bound key would take it the same way, such as `p`, the side panel's toggle,
  so `board.Refused` refuses one, reading the table's keys from `gotermboard.TableKeys()`
  and adding esc.
- **prs binds space, so the table's fold and close reach it through the model.**
  `m.Toggle` folds a group's header. A row `--pr` opens can start open in place, and the
  table's space is what closes it, so prs's space calls `m.CloseInPlace` before it marks.
- **The side panel and the Reader draw one document, `MarkdownDocument`.** A row sets
  `Document` and no `Panel`, so from `PanelFrom` columns gotermboard's panel draws the
  `Document` under the row's `ReaderTitle`. Such a row then starts folded beside the panel
  rather than open from `StartsOpen`, and enter opens it on the Reader. A `Panel` would end
  all three. The Reader's `[` and `]` land on each heading, so text written under a section
  goes through `nestedAt`, which keeps its own headings inside that section.
- **The panel calls a row's `Document` on every frame**, so `markdownOf` keeps each pull
  request's markdown until its row, its detail state or the minute moves.
- **A row's `ReaderTitle` is its pull request's title**, because the table's enter would
  otherwise title the Reader with the row's first cells, which here are its state words.
- **`Board.Look` is gotermboard's `LookBoard`**: a margin, the columns spread across the
  width, rules under the titles and over the keys, the cursor's row bold on the selection,
  and no row chevrons. `PanelFrom` is measured inside the margin, so the margin gives way
  from `PanelFrom` columns and the panel shows wherever it would with no margin.
- **A column with no title continues the one before it under `Look.Spread`**, so the
  diff's three counts take two untitled columns after DIFF and stay together.
- **The table's `f` applies only where a row or group sets `Under`**, and no prs row does,
  so prs binds `f` to the fix menu and the table loses nothing by it.
- **A group header that is not `Still` takes the cursor**, so `g` lands on the people
  header and enter there folds the group. The cursor starts on the first row holding cells,
  of whichever group draws first, so recently closed waits for the first open listing.
- **The message a key's command ends in reaches `Board.Receive`**, gotermboard's
  `ReceivingBoard`, and `Board.UnderWay`, its `BusyBoard`, puts the work under way on the
  footer after a spinner. A key's own `m.Say` shows until the next key, and the under-way
  line until the work ends.
- **`tea.ExecProcess` holds Bubble Tea's event loop until the program exits**, so no read
  lands while nvim or the editor has the terminal. The board reads the providers again
  once a program it handed the terminal to exits, and when the terminal gains focus, except
  a provider whose last good read is under `freshFor` old.
- **A `Question` with `Choices` answers on the key naming one**, handing `Answer` that key,
  and enter and every other key do nothing, so a reflex enter chooses no fix. esc cancels.
- **`m.HandOver` is how a key hands the terminal over, and `gotermboard.OpenWindow` how it
  opens a tmux window.** A window action's command is one shell command, so OpenWindow
  looks for the config's `needs` rather than the command. HandOver keeps the program's
  stderr through a pipe, so a failure says its last line through `gotermboard.LastLine`. Its `gotermboard.HandedBack` reaches `Board.Receive`
  for the read again, and the model runs the key's own `then` after.
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
