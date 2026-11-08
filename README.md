# prs

Every open pull request across the repos you work on, on one terminal board that keeps
itself current: each one's checks and review in words, its description a key away, and a
key per action.

It is for reading a backlog of pull requests, yours and the ones an app opens for you,
and acting on each one without leaving the terminal. It reads GitHub through
[`gh`](https://cli.github.com) and Bitbucket through `bbkt`, so it holds no credentials
of its own.

## First run

1. **Install it.** A release binary for Linux or macOS is on the releases page. With Go:

   ```bash
   go install github.com/datapointchris/prs/cmd/prs@latest
   ```

2. **Log `gh` in**, if it is not already. prs runs it for every read.

   ```bash
   gh auth login
   ```

3. **Write a registry of your repos.** It is a JSON file naming each repo you want on the
   board. `status` and `owner` are required, because prs filters on both. `provider` is
   `github` unless the entry or the file says `bitbucket`.

   ```json
   {
     "repos": [
       { "name": "widget", "owner": "acme", "path": "~/code/widget", "status": "active" }
     ]
   }
   ```

4. **Name it in prs's config**, the YAML file `prs config path` prints.

   ```yaml
   registry: ~/repos.json
   ```

   `prs config show` prints what each setting resolved to and where it came from.

5. **Open the board.**

   ```bash
   prs tui
   ```

   `?` lists every key. `q` quits.

## How the board reads

- **The rows are every open pull request on a repo the registry names, whoever opened
  it**, oldest first, because a backlog is a gauge and age is what it measures. One GitHub
  search finds them: every open pull request on the registry owners' repos, and every one
  you authored. A pull request you opened on someone else's repo of the same name stays
  off, because a repo is matched by owner and name.
- **Each provider is read again every minute while the board is open.** The line at the
  top says how old the newest read is, and names a read that failed. A failed read keeps
  the last good rows.
- **State is a word, never a color alone.** `checks passing`, `review required` and
  `review 2 posted` read under every theme, and the colors are the terminal's own 16.
- **A pull request whose base is another open one's branch names that one**, as `↑ #12`
  after its title. A pull request an app opened names the app.
- **The branch and the checkout are under the row**, dim, where `f` shows them.
- **The glyphs are Nerd Font glyphs**, so the board wants a Nerd Font in the terminal.

## Keys

`enter` opens the pull request's description, in place when it is short and full screen
when it runs past the screen. From 160 columns a side panel shows it beside the list.

`d` opens the diff in nvim (with the Diffview plugin) and `p` GitHub's page with its
comments, each in a new tmux window, so those two need prs to run inside tmux. `o` opens
the page in the browser, and `y` copies the branch.

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

`prs tui --json` prints what the board shows, so a script or another tool reads what a
person reads. `prs tui --once` prints one frame at `COLUMNS` by `LINES`.

```bash
prs tui --json | jq '.pull_requests[] | {slug, number, checks, review}'
```
