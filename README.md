# tower

A personal work board for the terminal. Work items — alerts, GitHub pull
requests and tasks today, Jira tickets and more later — land in **TO DO**. You hand an item to
an agent (GitHub Copilot CLI) with one key. When the agent exits, an item
still **IN PROGRESS** moves to **WAITING** so you can resume its session.
Items end in **DONE** when the source resolves them or when you close them.

```
╭─ TO DO (3) ─────────────╮╭─ IN PROGRESS (1) ───────╮╭─ WAITING (1) ───────────╮╭─ DONE (1) ──────────────╮
│ ▌ KubePodCrashLoop… 12m ││   PostgreSQLCacheHi… 4m ││   FluxCdReconciliat… 2m ││   KubePodNotReady: … 1h │
│ ▌ prod-de1 · Pod        ││   stage-de1 · Cache hit ││   prod-ae1 ·            ││   prod-us1 · Pod        │
│ ▌ api/api-0 is          ││   ratio below 95%       ││   Kustomization         ││   kubenurse/kubenurse-x │
│ ▌ restarting 5 times /… ││                         ││   kubenurse fails to …  ││   is not ready          │
│                         ││                         ││                         ││                         │
│   KubeHpaMaxedOut: … 9h ││                         ││                         ││                         │
│   prod-core · HPA       ││                         ││                         ││                         │
│   monitoring/grafana    ││                         ││                         ││                         │
│   has been running at … ││                         ││                         ││                         │
│                         ││                         ││                         ││                         │
╰─────────────────────────╯╰─────────────────────────╯╰─────────────────────────╯╰─────────────────────────╯
p progress · r resume · d done · t todo · n new · o open · K details · L log · R refresh · q quit
```

Each column is a state. A card shows the item's title with the time since it
entered its state and up to three lines of its description (see "Alert
sources" and "Pull request sources", the description for tasks).

The statusline shows the keys. Failed polls and agent/task actions are written
to `state_dir/tower.log`; keys that don't apply to the selected item (e.g. `p`
outside TO DO) do nothing.

## States

| State | How items get there |
| --- | --- |
| TO DO | New items from a source, tasks (`n`), `t` on any item, an alert that fires again within `reopen_window` after it resolved, or a retained pull request that matches its query again |
| IN PROGRESS | Only manually: `p` on a TO DO item starts `run_command` in the background |
| WAITING | When the agent command exits while the item is still IN PROGRESS; failed on a non-zero exit, a dead wrapper, or an abandoned start recovered at startup |
| DONE | The source resolved the item (also while an agent is running; the agent is not stopped), or `d` |

DONE items and their run logs are deleted after `retention`, checked at
startup and hourly. An alert that fires again after it was resolved for longer
than `reopen_window` becomes a new item. Keep `retention` at least as long as
`reopen_window`, otherwise the old item may be deleted before it could be
reopened.

## Keys

| Key | Action |
| --- | --- |
| `l` / `h`, `→` / `←` | Next / previous column |
| `j` / `k`, `↓` / `↑`, `g` / `G` | Move / first / last |
| `ctrl+d` / `ctrl+u`, `PgDn` / `PgUp` | Move half the visible cards down / up |
| `p` | Start an agent on a TO DO item (moves it to IN PROGRESS) |
| `r` | Resume an existing session of a TO DO, WAITING or DONE item, without changing its state |
| `d` | Move to DONE |
| `t` | Move back to TO DO |
| `n` | New task in `$EDITOR` (first non-empty line is the title, the rest the description; empty cancels) |
| `o` | Open the item's URL in the browser (alerts: the generator URL; pull requests: the pull request; tasks have none) |
| `K` | Show the item's details (tasks: title and description) in a popup; `j`/`k`, `ctrl+d`/`ctrl+u` and `g`/`G` scroll, `K`/`esc`/`q` close |
| `L` | Open the run log in `$EDITOR` |
| `R` | Poll all sources now |
| `q` / `ctrl+c` | Quit. Running agents keep running and move items still IN PROGRESS to WAITING when they exit |

## Install

Requires Go 1.27.1 or newer to build, and macOS for browser opening (`open`)
and desktop notifications (`osascript`). The example configuration also needs
an authenticated Copilot CLI, its SRE skills, and Ghostty with the external
`ghostty-new` helper; that helper is not included in this repository.
Pull request sources need the [`gh`](https://cli.github.com) CLI, logged in.

```sh
go install .                  # from the repository root
tower                         # uses ~/.config/tower/config.yaml
tower --config path/to/config.yaml
```

Start tower from your interactive shell: agents and `resume_command` inherit
its environment (`PATH`, `$GRAFANA_INSTANCES`, …). Only one tower runs per
state directory. The board needs at least 80 columns and four rows; the details
popup shrinks its margins in short terminals.

### Upgrading from the previous design

The kanban rewrite is incompatible with the old configuration and filesystem
state. There is no automatic migration. Back up the old configuration and
state, then use the example below with a **fresh state directory**. Keep the old
artifacts until you no longer need their sessions and reports.

The old `config init`, `config validate`, `resume`, `prune` and `version`
commands, `$TOWER_CONFIG`, `--log-level` and `--headless` are gone. Select the
configuration with `--config`; resume and refresh from the board. Alert sources
now support Grafana-managed Alertmanagers only, not plain Alertmanagers or file
fixtures.

Agents start manually with `p`, and their prompts are configured by you. The
old embedded read-only preparation/confirmation contract is no longer supplied.
The example's `--yolo` option is not a sandbox; configure prompts and agent
permissions appropriate for your environment.

## Configuration

Unknown options are errors; there are no defaults, so set every option.
Duration ranges, source-name uniqueness and required command placeholders are
not fully validated at startup. Use positive retention and polling durations:
omitted or zero retention deletes existing DONE items, and a nonpositive poll
interval crashes tower.

`state_dir` must be nonempty. `$VAR` and `${VAR}` are expanded, then `~` or a
leading `~/` expands to the home directory. Relative paths are resolved against
the directory tower was started in, **not** the configuration file's directory.
The resulting absolute path is shared with detached wrappers. Characters such
as `?`, `#` and `%` are treated as ordinary filename characters.

```yaml
# SQLite database, lock file, tower.log and runs/<item>.log. $VAR and ~/ expand.
state_dir: $HOME/.local/state/tower

app:
  # DONE items are deleted after this duration.
  retention: 48h

agent:
  # Started when an item moves to IN PROGRESS.
  run_command: copilot --yolo --remote --session-id={{.SessionID}} -p {{.Prompt}}
  # Run when a TO DO, WAITING or DONE item with a session is resumed.
  resume_command: ghostty-new --placement=tab --title={{.Title}} --command 'copilot --yolo --remote --resume={{.SessionID}}'
  # Working directory of run_command and resume_command. $VAR and ~/ expand.
  working_dir: $HOME

providers:
  alerts:
    poll_interval: 1m
    # A resolved alert that fires again within this window reopens its item.
    reopen_window: 24h
    sources:
      - name: dev-de1                  # must be nonempty and unique
        grafana_instance: dev-de1      # key in $GRAFANA_INSTANCES
        grafana_alertmanager: grafana
        filter:
          - team="product-core-infra"
        receiver: "incidentio"         # "" for all receivers
    # Go template, see "Prompts".
    prompt: |
      Investigate the alert {{.Title}} using the `sre-analyze-alert` skill ...
      {{.Details}}

  pullrequests:
    poll_interval: 5m
    # Only pull requests updated within this duration are shown.
    max_age: 336h
    sources:
      - name: authored                 # must be nonempty and unique
        query: is:open author:@me      # gh search prs terms
      - name: review-requested
        query: is:open review-requested:@me -author:app/dependabot
    # One prompt for all sources; branch on the source name.
    prompt: |
      {{if eq .Source "review-requested"}}Review {{.URL}} using the `github-pr-review` skill.
      {{- else}}Address the review feedback on {{.URL}} using the `github-pr-review-reviews` skill.{{end}}

      {{.Details}}

  tasks:
    prompt: |
      Task: {{.Title}}

      {{.Description}}
```

### Commands

`run_command` and `resume_command` are split into arguments like a shell
would (whitespace, `'…'`, `"…"` and `\` escapes) and are run **without a
shell**. Unquoted `; | & ( )` and backticks are rejected, and environment
variables are not expanded. Wrap anything more complex in a script or
`sh -c '…'`.

Each argument is a [Go template](https://pkg.go.dev/text/template). Template
actions (`{{ … }}`) may contain spaces, quotes and literal `}}` inside quoted
strings or comments; they are kept together while splitting, except inside
single quotes. A rendered value always stays one argument, whatever it contains:

| Field | Value | Available in |
| --- | --- | --- |
| `{{.Prompt}}` | The rendered prompt | `run_command` (required) |
| `{{.SessionID}}` | A new UUID per run | both (required) |
| `{{.ID}}` | The item's number | both |
| `{{.Title}}` | The item's title | both |

`ghostty-new --command '…'` types its argument into a shell. Wrap item data
in `shquote` there, which quotes a value as one shell word, e.g.
`--command 'copilot --resume={{.SessionID}} --name={{shquote .Title}}'`.

Both commands run in `working_dir`, independent of the directory tower was
started in, so agents that scope sessions to their working directory always
find them again. It is expanded like `state_dir` and must be an existing
directory at startup. `$PWD` is set to it, which launchers such as
`ghostty-new` use as the new terminal's directory.

`run_command` runs detached in `working_dir`, with stdin
from `/dev/null` and its output in `state_dir/runs/<item>.log`. There is no
timeout and no concurrency limit. The wrapper records its PID before starting
the command. At startup, tower marks abandoned starts with no claimed wrapper
as failed in WAITING; a late wrapper cannot launch one of those recovered
sessions. While running, tower recovers dead wrappers only when the observed
state, session and PID still match.

When the command exits, its failure status and state transition are recorded
atomically. If the item is still IN PROGRESS in that session, it moves to
WAITING and shows a macOS notification (osascript). Otherwise its current state
and time in state are preserved. Exact exit codes are in the run log; the
store keeps only whether the run failed.

`r` reuses the stored session, including after `t` moved an item back to TO DO;
it neither starts a new run nor changes the column. By contrast, `p` starts a
new session. Moving an item does not stop its running agent, so a TO DO or DONE
item's session may still be active when you resume it.

`resume_command` is a launcher, not an interactive command in tower's terminal:
its stdin is `/dev/null`, and its output is captured for error diagnostics. It
has a 30-second timeout. After it exits or is cancelled, output pipes held by
descendants are closed after at most 250 ms; tower does not signal those
descendants.

### Alert sources

Sources are Grafana-managed Alertmanagers. The URL and the token command come
from `$GRAFANA_INSTANCES`, a JSON object keyed by instance name with `url` and
`auth.tokenCommand` (shared with the SRE skills). The token command runs with
`sh -c` on every poll. Silenced and inhibited alerts are shown like firing
ones. A failed poll changes nothing; the error is written to `tower.log`.
Currently `$GRAFANA_INSTANCES` must contain valid JSON even with no alert
sources; use `{}` in that case and still configure a positive poll interval.

Alert titles are `alertname · severity · source`, with whitespace collapsed to
a single line. The description is the `summary` and `description` annotations,
and the URL is the generator URL. The details are the alert as Markdown:
Grafana instance, URL and Alertmanager datasource of the source, severity,
state, start time, receivers, generator URL, summary, description, labels and
the other annotations.

### Pull request sources

Each source is a GitHub pull request search run with the
[`gh`](https://cli.github.com) CLI and its existing login; tower handles no
tokens. `gh` is only needed when sources are configured. Without sources the
other `pullrequests` options may be omitted; with sources `poll_interval`,
`max_age` and `prompt` are required and source names must be nonempty and
unique. A missing `gh`, an expired login or an invalid query is a failed poll,
written to `tower.log`.

Every poll runs, without a shell and with a 30-second timeout:

```sh
gh search prs --json … --limit 1000 -- <query terms> updated:>=<today − max_age>
```

The query is split at whitespace; quotes are not supported, so qualifiers with
spaces (`label:"needs review"`) don't work. Because the terms follow `--`,
exclusions work: `-author:app/dependabot` hides Dependabot (GitHub apps are
matched as `app/<name>`, not by their `dependabot[bot]` login). `updated:` is
day-granular (UTC). A search that reaches 1000 results is treated as a failed
poll, because the truncated snapshot would close the missing pull requests;
narrow the query or `max_age`. Mind GitHub's search rate limit (30 requests
per minute) when choosing the number of sources and `poll_interval`.

Items are per source: a pull request matched by two queries shows up as two
cards, one per role. Titles are `owner/repo#number · title`. The description
is `@author` (plus `· draft`) followed by the pull request body. The URL is the
pull request, and the details contain the repository, number, URL, author,
source and query, state, creation and update times and the body. Branches and
review state are not included; the skills fetch them.

Pull requests that are merged, closed or not updated within `max_age` move to
DONE. A pull request that matches again goes back to TO DO with its session
as long as its item is retained (there is no `reopen_window`); after
`retention` deleted it, it becomes a new item.

### Prompts

Prompts are Go templates (`text/template`); unknown fields are errors. Every
provider's prompt (`providers.<kind>.prompt`) gets the same fields:

| Field | Value |
| --- | --- |
| `{{.ID}}` | The item's number |
| `{{.Kind}}` | The item kind (`alert`, `pullrequest`, `task`) |
| `{{.Source}}` | The source, e.g. the alert or pull request source name |
| `{{.Title}}` | The one-line title |
| `{{.Description}}` | The short description shown on the board |
| `{{.Details}}` | The item as Markdown (empty for tasks) |
| `{{.URL}}` | The item's link (empty for tasks) |

Providers store these fields with the item. Current polled items (alerts,
pull requests) update them on every poll while they are reported, including
items manually closed while still reported. Field-only updates preserve the
state, session, time in state and retention timestamp; historical episodes
replaced by a new item remain unchanged.

## Development

```sh
go build -o ./bin/tower .
go test -race ./...
go vet ./... && golangci-lint run ./...
```
