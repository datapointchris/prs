# prs

Every open pull request across the repos you work on, on one terminal board that keeps
itself current: the ones people opened above the ones apps opened, those merged or closed
lately below both, where each stands in words, its description, checks, files and comments
beside the list, and a key per action.

It is for reading a backlog of pull requests, yours and the ones an app opens for you,
and acting on each one without leaving the terminal. It reads GitHub with the login
[`gh`](https://cli.github.com) already holds, and Bitbucket through `bbkt`, so it keeps
no credentials of its own.

## First run

1. **Install it.** A release binary for Linux or macOS is on the releases page. With Go:

   ```bash
   go install github.com/datapointchris/prs/cmd/prs@latest
   ```

2. **Log `gh` in**, if it is not already. prs reads GitHub with the token gh would use
   for the same host, so a working `gh auth login` or a `GH_TOKEN` is all it needs.
   `GH_HOST` points both at a GitHub Enterprise host.

   ```bash
   gh auth login
   ```

3. **Open the board.** With nothing else set up, it lists the open pull requests on
   every repo your GitHub login owns.

   ```bash
   prs tui
   ```

   `?` lists every key. `q` quits. `prs config show` says which repos prs is reading,
   and why.

4. **Name the repos you work on**, once your own repos are not the set you want. A
   registry names each repo, on GitHub or Bitbucket, and where it is checked out.
   `owners` in the config reads every repo of some GitHub users or organizations instead.

## Which repos prs reads

prs asks these in order, and the first that answers wins:

1. `PRS_REPOS_REGISTRY`, naming a registry for one shell.
2. `repos_registry` in prs's config, the YAML file `prs config path` prints.
3. `owners` in that config. A config naming both `repos_registry` and `owners` is refused.
4. A registry at `$XDG_DATA_HOME/prs/repos.json`, which is `~/.local/share/prs/repos.json`
   by default.
5. Every repo your GitHub login owns.

```yaml
repos_registry: ~/repos.json
# or, in its place:
owners: [acme, octo]
```

`owners` reads each owner whole, so a large organization's search can fill its one page
with repos you never touch, or fail outright. A registry names each repo instead, so each
search asks only for those repos.

### The registry

The registry is a JSON object holding `repos`, an array with one entry per repo. prs reads
these fields of an entry and ignores the rest:

| Field | Required | What prs does with it |
| --- | --- | --- |
| `name` | yes | The repo's name on its provider. |
| `owner` | yes | The GitHub owner, or the Bitbucket project key. |
| `status` | yes | Only `active` repos are read. Every other value skips the entry. |
| `path` | no | Where it is checked out, `~`-rooted or absolute. `d` and your own keys run there, and bbkt reads a Bitbucket repo there. |
| `provider` | no | `github` or `bitbucket`. An entry without one takes the file's top-level `provider`, and `github` where the file names none. |

`status` and `owner` are required rather than defaulted, because a reader that defaults a
field it filters on drops a repo silently, and every list it prints still looks plausible.

```json
{
  "repos": [
    { "name": "widget", "owner": "acme", "path": "~/code/widget", "status": "active" },
    { "name": "ledger", "owner": "FIN", "path": "~/work/ledger", "status": "active", "provider": "bitbucket" }
  ]
}
```

## How the board reads

- **The rows are every open pull request on a repo prs reads, in three groups.** `people`
  holds those a person opened and `bots` those an app opened, each oldest first, because a
  backlog is a gauge and age is what it measures. `recently closed` holds those merged or
  closed within `--since`, newest first. Each header counts its rows, and enter on it folds
  them. One GitHub request asks for the registry's repos by name, and `bbkt` lists each
  Bitbucket repo in its checkout, several repos at once. Bitbucket's listing does not say
  who is an app, so its pull requests sit under `people`.
- **`--since` is how far back `recently closed` reaches**, and `prs tui --help` prints its
  default. It takes a span such as `12h`, `2d` or `1w`, counted back from each read, or a
  day or an RFC 3339 instant. A second GitHub request reads the closed ones. It costs as
  much as the open one, so it runs on a timer of its own, `closed_refresh_every_seconds` in
  the config. Its rows read no checks, files or comments, because each would cost a request
  of its own. prs reads no close time from Bitbucket, so a Bitbucket pull request reaches
  `recently closed` only when `m` here merged it.
- **Each GitHub row's checks, files and conversation are read after the list lands**,
  a few pull requests at a time, so the list never waits on them. A pull request's detail
  is read again when its head commit, its update time or its checks state moves, or when
  its last read failed.
- **The board opens on the rows the last run on the same repos read**, kept under
  `$XDG_CACHE_HOME/prs/listings/` in a file for each set of repos, and reads each provider
  behind them, so the first frame never waits on GitHub. Only `prs tui` keeps them.
  `prs list`, `--once` and `--json` wait for their reads.
- **Each provider's open pull requests are read again on a timer while the board is
  open**, whenever its terminal gains focus, and whenever a program a key handed the
  terminal to exits. `refresh_every_seconds` in the config sets the timer, and
  `prs config path` prints its default. A return within a few seconds of the last read does not read again. Returning
  from a tmux window reaches prs as focus where tmux's `focus-events` option is on. The
  line at the top says how old the newest read is, turns a spinner while a read runs, and
  names a read that failed. A failed read keeps the last good rows, and so does one
  Bitbucket repo that failed while the rest read.
- **A refresh leaves the cursor on its pull request, in the group it was in.** One a read
  moved to another group stays under the cursor where it was, in its new state, and one no
  read lists any more stays there marked `gone`, each until the cursor moves off it. `m`
  and `x` refuse a `gone` one.
- **State is a word, never a color alone.** The first column says where a pull request
  stands: `ready`, `behind`, `conflicts`, `blocked`, `draft` or `unknown` while open, or
  `mergeable` where it can merge with its checks not passing, and `merged` or `closed`
  after. A Bitbucket row reads `open`, because prs reads no merge state from Bitbucket. `checks
  passing`, `review required` and `review 2 posted` follow it. Each reads under every
  theme and under `NO_COLOR`, and the colors are the terminal's own 16.
- **A Claude review round reads on its row.** Where one reviewed the pull request, a
  `CLAUDE` cell after the review gives the newest round. Once its round comment posts, the
  cell is its findings by section, as `claude 2 correctness · 1 design`. It adds `1 not
  run` or `1 unread` where a reviewer did not run or its review could not be read, and
  reads `claude did not run` where none ran. Before the round comment, it reads `claude
  running` while the reviews post, and `claude unfinished` once they have stopped for an
  hour. It reads `claude reviewed` where the newest review is of a version prs does not
  read. The column shows only while a row fills it.
- **A pull request whose base is another open one's branch names that one**, as `↑ #12`
  after its title. A pull request an app opened names the app.
- **The branch and the checkout are under the row**, dim, where `f` shows them.
- **The glyphs are Nerd Font glyphs**, so the board wants a Nerd Font in the terminal.

## Each pull request's document

A pull request's document is markdown, under a line naming its repo, author and branches:

- **Description** is its body, drawn as soon as the list lands.
- **Review** is the newest Claude review round, where one reviewed it or a window is
  running one: where it stands, its synthesis, then every reviewer's findings under a
  heading for their section, read from each review's JSON block rather than its markdown.
  A line follows for each earlier round. A post whose marker names a version prs does not
  read shows whole. So does a marked post by anyone other than an app on the repo or
  someone who can write to it, and it counts in no round, because anyone can comment.
- **Checks** is each check on its head commit and its state, those not passing first.
- **Files** is each changed path with its lines added and removed, and says how many more
  the read left out.
- **Diff** is the diff itself, each file under a heading of its own, so `[` and `]` move
  between files. A pull request changing more lines than `diff_section_max_lines` in the
  config gets its size and the key that opens it instead. `prs config path` prints its
  default.
- **Comments** names who is still asked to review, then the conversation oldest first with
  each submitted review in it, then each review thread, unresolved first. A review round's
  post the Review section shows is left out of the conversation.

Checks, Files and Comments fill in once the pull request's own read lands, and say they
are reading until then. Review fills in then too, except that a window running a review
round shows it at once. Bitbucket's client reads no more than
its listing, so a Bitbucket pull request's document has no Review, Checks, Files or
Comments.

Each listed pull request's diff, recently closed ones included, is read once its list
lands, a few at a time. It is read
again when its head commit or base moves. A read that failed is tried again after a
minute, then after a wait that doubles up to half an hour. GitHub sends the diff, so prs
fetches nothing into a GitHub checkout. Bitbucket's client has no diff, so prs fetches a
Bitbucket pull request's head into `refs/remotes/origin/pr/<number>` in its checkout and
diffs it against the `origin/<base>` already there. It moves no other ref, and writes no
`FETCH_HEAD`. A Bitbucket pull request whose repo is not checked out says so in its Diff.

On a wide terminal a side panel beside the list shows the document of the row under the
cursor, under its title, and `p` hides or shows it. `enter` opens the document full screen,
where `[` and `]` jump between its headings and `/` searches it. On a narrower terminal
`enter` opens it in place under the row where it fits on the screen, and full screen where
it runs past.

## Keys

`d` fetches the pull request's head and hands the terminal to nvim's diff (with the
Diffview plugin) against the base it targets. `:qa` comes back to the list with the same
row under the cursor, and reads again as any return to the list does. It never checks the
branch out.
nvim listens on `$TMPDIR/prs-diff-<repo>-<number>.sock`, so another program can ask it
what is under the cursor. Inside tmux, prs's window is named `diff <repo> #<number>` while
nvim has it.
Beside the socket, prs writes the pull request as `prs-diff-<repo>-<number>.md`, its title
over the document the side panel shows less its Diff, which nvim shows already, and its
review threads as
`prs-diff-<repo>-<number>.threads.json`, in the shape `cmd/prs/diff-threads.schema.json`
gives. nvim starts with `PRS_DIFF_DOCUMENT` and `PRS_DIFF_THREADS` naming them, so an nvim
config can show the description beside the diff and each thread on its line. prs removes
both once nvim exits.

`m` merges and `x` closes without merging. Each names the pull request and asks first:
enter alone merges, and enter alone leaves it open rather than closing it. A second press
while one runs is refused. On GitHub every merge pins a head: the one `d` fetched, or else
the one the Diff section showed, or else the one the list showed, so it fails where the
branch moved since. `c` opens your editor on an empty file and posts what
was written when it exits. Merging, closing, commenting and `o`, which opens the page in
the browser, each run while the list stays usable, named on the footer until they end. A
row merged or closed here moves to `recently closed` at once, reading `merged` or `closed`.
A cursor on it moves on to the next open row in its group, or the one above where it was
the last, so a queue takes one key a pull request. A read that starts after the key then
says where it stands, so a merge a queue only accepted, or a close reopened on its page,
returns it to its group. A failure says the client's own reason on the footer. Bitbucket's
client has no verb to close a pull request, so `x` refuses there.

`space` marks a row for `m` to merge with the others marked, and a second press takes the
mark off. It marks only a row whose checks pass and whose state reads `ready`, which is
GitHub's `CLEAN` or `HAS_HOOKS`, and names why on the footer where it refuses one. prs
reads no merge state from Bitbucket, so a Bitbucket row is never marked. On a group's header `space` folds the group,
and on a row open in place it closes the row.

With rows marked, `m` asks once. The question lists every pull request it will merge, then
each marked or refused one it leaves out, with why. A stacked one names the branch it
merges into. Enter alone merges them, one at a time, in the order the board draws them. Each
is read again just before its merge, because merging one can leave the next on the same
repo behind or conflicting. One that no longer reads `ready` with its checks passing is
left, and the rest go on. So is one whose base is a branch the batch already merged, which
would otherwise land on that branch rather than where it went. When the batch ends, the
footer names what merged and what was left and why, and the open listings are read again.

`y` copies the branch.

### Keys of your own

A window action in the config is a key that runs a command in a new tmux window, started
in the pull request's checkout:

```yaml
window_actions:
  - key: r
    label: review
    needs: [claude]
    providers: [github]
    window: "review {repo} #{number}"
    command: "claude '/review-pr {number}'"
    runs_review_round: true
```

`needs` are the programs the command runs. A machine missing one is not offered the key,
because tmux would open the window and the command would die in it. `{number}`, `{repo}`
and `{slug}` are filled in. Quote a value holding ` #`, which YAML reads as a comment.

`runs_review_round` marks a command that runs a Claude review round. Its row then reads
`claude running` while the window is open, which prs asks tmux every few seconds, until a
round finishes after the key opened it. A session that ran a round keeps its window
afterwards, so a window prs did not open reads running only where no round has finished.

## Opening on one pull request

`prs tui --pr` opens the board on one pull request, named as `owner/name#number` or by its
URL. The cursor starts on it with its document showing, in the side panel on a wide
terminal and open under its row otherwise, and `q` exits 0, so another terminal program
can run prs to show a pull request and get its screen back.

```bash
prs tui --pr 'acme/widget#12'
prs tui --pr https://github.com/acme/widget/pull/12
prs tui --pr https://bitbucket.example.com/projects/FIN/repos/ledger/pull-requests/4
```

- **An open pull request sits where the list puts it.** One on a repo prs does not read
  is drawn among the list's rows too.
- **A merged or closed one sits in a group above the list**, however long ago it closed,
  and not under `recently closed`. Its row reads `merged` or `closed`, and `m` and `x`
  refuse it. Bitbucket's `declined` reads `closed`.
- **While no listing holds it, prs reads it again on the board's timer**, so one merged
  elsewhere moves above the list, and one on a repo prs does not read stays current. One
  merged with `m` moves there at once.
- **`owner/name#number` is the registry's repo of that name**, on whichever provider hosts
  it, and GitHub where the registry names none. A URL says its provider itself. bbkt reads
  a Bitbucket one by its project and name, so its repo need not be checked out.
- **A pull request prs cannot read is refused before the board draws**, with one line on
  stderr and exit 1. So is a URL on a host other than the one prs read the pull request
  from, because a repo of the same name there holds a different pull request.

## Reading it from a program

`prs list` reads each provider once and prints the board's open rows as a table, the
people's then the bots', under a line naming any provider it could not read. It reads no
closed pull requests. `prs list --json` prints them as a JSON array, one
object per pull request, in the shape [`cmd/prs/list.schema.json`](cmd/prs/list.schema.json)
gives. Beside the row's columns it carries each pull request's state, its merge state, its
head commit and the open pull request it is stacked on.

`prs tui --json` prints the board's snapshot instead: the open rows under `pull_requests`,
as `prs list --json` prints them, those merged or closed within `--since` under
`recently_closed`, newest first, and how each read stands. Both arrays hold rows of one
shape. With `--pr`, `opened_on` is that pull request in the shape of one of those rows,
merged or closed where it is, and null without `--pr`. Either array holds it only where a
listing does. `prs tui --once` prints one frame at `COLUMNS` by `LINES`.

Each of these exits 1 after printing when a read failed or left open pull requests out,
because a failed read and a repo with no open pull requests otherwise print the same rows.
A refused search, a Bitbucket repo that failed or is not checked out, and a search past
its page each leave pull requests out. The rest of the rows still print. A recently closed
read past its page is not counted, because it keeps the newest, and the line at the top
says how many it left out.

```bash
prs list --json | jq -r '.[] | select(.merge_state_status == "BEHIND") | .url'
prs tui --json | jq '.pull_requests[] | {slug, number, checks, review, stacked_on}'
```
