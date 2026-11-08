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
   `github` unless the entry or the file says `bitbucket`, whose `owner` is the project key.

   ```json
   {
     "repos": [
       { "name": "widget", "owner": "acme", "path": "~/code/widget", "status": "active" }
     ]
   }
   ```

4. **Put it where prs looks.** That is `$XDG_DATA_HOME/prs/repos.json`, which is
   `~/.local/share/prs/repos.json` by default. Or name it elsewhere in prs's config, the
   YAML file `prs config path` prints:

   ```yaml
   repos_registry: ~/repos.json
   ```

   `PRS_REPOS_REGISTRY` names it for one shell, ahead of the config. `prs config show`
   prints the path prs resolved and which of the three named it.

5. **Open the board.**

   ```bash
   prs tui
   ```

   `?` lists every key. `q` quits.

## How the board reads

- **The rows are every open pull request on a repo the registry names, whoever opened
  it**, oldest first, because a backlog is a gauge and age is what it measures. One GitHub
  request asks for the registry's repos by name, and `bbkt` lists each Bitbucket repo in
  its checkout.
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

`m` merges and `x` closes without merging. Each names the pull request and asks first:
enter alone merges, and enter alone leaves it open rather than closing it. `c` opens
`$EDITOR` on an empty file and posts what was written when the editor exits. Merging,
closing, commenting and `o`, which opens the page in the browser, each run while the list
stays usable, named on the footer until they end. A merged or closed row reads `merged` or
`closed` at once, and a failure says the client's own reason on the footer. Bitbucket's
client has no verb to close a pull request, so `x` refuses there.

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

`prs tui --json` prints what the board shows, so a script or another tool reads what a
person reads. `prs tui --once` prints one frame at `COLUMNS` by `LINES`. Both exit 1 after
printing when a provider could not be read, and `sources[].note` in the snapshot names
what a read that succeeded could not cover.

```bash
prs tui --json | jq '.pull_requests[] | {slug, number, checks, review}'
```
