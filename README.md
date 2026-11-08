# prs

Every open pull request across the repos you work on, on one terminal board that keeps
itself current: each one's checks and review in words, its description a key away, and a
key per action.

It is for reading a backlog of pull requests, yours and the ones an app opens for you,
and acting on each one without leaving the terminal. It reads GitHub with the login
[`gh`](https://cli.github.com) already holds, and Bitbucket through `bbkt`, so it keeps
no credentials of its own.

## First run

1. **Install it.** A release binary for Linux or macOS is on the releases page. With Go:

   ```bash
   go install github.com/datapointchris/prs/cmd/prs@latest
   ```

2. **Log `gh` in**, if it is not already. prs reads GitHub with gh's token, the way gh
   finds it: `GH_TOKEN`, then gh's own config, then the system keyring. `GH_HOST` points
   both at a GitHub Enterprise host.

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

`owners` reads each owner whole, so a large organization's search can fill its page of 100
with repos you never touch, or fail outright. A registry names each repo instead, and a
search names 20 of them at a time.

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

- **The rows are every open pull request on a repo the registry names, whoever opened
  it**, oldest first, because a backlog is a gauge and age is what it measures. One GitHub
  request asks for the registry's repos by name, and `bbkt` lists each Bitbucket repo in
  its checkout, several repos at once.
- **Each GitHub row's reviews, checks and conversation are read after the list lands**,
  four pull requests at a time, so the list never waits on them. The description shows
  each review and the commit it was against, who is still asked, each check not passing,
  and how many comments, threads, files and commits there are. A pull request is read
  again only once its head commit or its update time moves.
- **Each provider is read again every minute while the board is open.** The line at the
  top says how old the newest read is, and names a read that failed. A failed read keeps
  the last good rows, and so does one Bitbucket repo that failed while the rest read.
- **State is a word, never a color alone.** `checks passing`, `review required` and
  `review 2 posted` read under every theme, and the colors are the terminal's own 16.
- **A pull request whose base is another open one's branch names that one**, as `↑ #12`
  after its title. A pull request an app opened names the app.
- **The branch and the checkout are under the row**, dim, where `f` shows them.
- **The glyphs are Nerd Font glyphs**, so the board wants a Nerd Font in the terminal.

## Keys

`enter` opens the pull request's description, in place when it is short and full screen
when it runs past the screen. On a wide terminal a side panel shows it beside the list.

`d` fetches the pull request's head and hands the terminal to nvim's diff (with the
Diffview plugin) against the base it targets. `:qa` comes back to the list with the same
row under the cursor, and every provider is read again. It never checks the branch out.
nvim listens on `$TMPDIR/prs-diff-<repo>-<number>.sock`, so another program can ask it
what is under the cursor. Inside tmux, prs's window is named `diff <repo> #<number>` while
nvim has it.

`m` merges and `x` closes without merging. Each names the pull request and asks first:
enter alone merges, and enter alone leaves it open rather than closing it. A second press
while one runs is refused. On GitHub a merge after `d` pins the head the diff showed, so it
fails where the branch moved since. `c` opens your editor on an empty file and posts what
was written when it exits. Merging, closing, commenting and `o`, which opens the page in
the browser, each run while the list stays usable, named on the footer until they end. A
merged or closed row reads `merged` or `closed` at once, and a failure says the client's
own reason on the footer. Bitbucket's client has no verb to close a pull request, so `x`
refuses there.

`p` opens GitHub's page with its comments in a pager in a new tmux window, so it needs prs
to run inside tmux. Bitbucket has no such page, and `o` opens one there. `y` copies the
branch.

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
```

`needs` are the programs the command runs. A machine missing one is not offered the key,
because tmux would open the window and the command would die in it. `{number}`, `{repo}`
and `{slug}` are filled in. Quote a value holding ` #`, which YAML reads as a comment.

## Reading it from a program

`prs list` reads each provider once and prints the board's rows as a table, under a line
naming any provider it could not read. `prs list --json` prints them as a JSON array, one
object per pull request, in the shape [`cmd/prs/list.schema.json`](cmd/prs/list.schema.json)
gives. Beside the row's columns it carries each pull request's state, its merge state and
its head commit.

`prs tui --json` prints the board's snapshot instead: the same rows with the open pull
request each is stacked on, and how each read stands. `prs tui --once` prints one frame at
`COLUMNS` by `LINES`.

Each of these exits 1 after printing when a provider could not be read, because a failed
read and a repo with no open pull requests otherwise print the same rows. The other
provider's rows still print.

```bash
prs list --json | jq -r '.[] | select(.merge_state_status == "BEHIND") | .url'
prs tui --json | jq '.pull_requests[] | {slug, number, checks, review, stacked_on}'
```
