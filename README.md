# tower

A personal work board for the terminal. Work items land in **TO DO**. You hand
an item to an agent with one key. When the agent exits, an item still **IN
PROGRESS** moves to **WAITING** so you can resume its session. Items end in
**DONE** when the source resolves them or when you close them.

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
entered its state and up to three lines of its description. The statusline shows
the keys.

## States

| State       | How items get there                                                                        |
| ----------- | ------------------------------------------------------------------------------------------ |
| TO DO       | New items from a source and tasks (`n`)                                                    |
| IN PROGRESS | Items moved manually from TO DO to IN PROGRESS via `p`, starts the agent in the background |
| WAITING     | Items where the agent finished it's work, agents can be resumed using `r`                  |
| DONE        | Items which were resolved or manually move via `d`                                         |

## Keys

| Key                                  | Action                                                                                                                                         |
| ------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `l` / `h`, `→` / `←`                 | Next / previous column                                                                                                                         |
| `j` / `k`, `↓` / `↑`, `g` / `G`      | Move / first / last                                                                                                                            |
| `ctrl+d` / `ctrl+u`, `PgDn` / `PgUp` | Move half the visible cards down / up                                                                                                          |
| `p`                                  | Start an agent on a TO DO item (moves it to IN PROGRESS)                                                                                       |
| `r`                                  | Resume an existing session of a TO DO, WAITING or DONE item, without changing its state                                                        |
| `d`                                  | Move to DONE                                                                                                                                   |
| `t`                                  | Move back to TO DO                                                                                                                             |
| `n`                                  | New task in `$EDITOR` (first non-empty line is the title, the rest the description; empty cancels)                                             |
| `o`                                  | Open the item's URL in the browser (alerts: the generator URL; pull requests: the pull request; Jira: the ticket, when known; tasks have none) |
| `K`                                  | Show the item's details (tasks: title and description) in a popup; `j`/`k`, `ctrl+d`/`ctrl+u` and `g`/`G` scroll, `K`/`esc`/`q` close          |
| `L`                                  | Open the run log in `$EDITOR`                                                                                                                  |
| `R`                                  | Poll all sources now                                                                                                                           |
| `q` / `ctrl+c`                       | Quit. Running agents keep running and move items still IN PROGRESS to WAITING when they exit                                                   |

## Install

Check the [releases](https://github.com/ricoberger/tower/releases) page for the
full list of pre-built binaries or install with `go install`:

```sh
go install github.com/ricoberger/tower@latest
```

## Configuration

```yaml
# SQLite database, lock file, tower.log and runs/<item>.log. $VAR and ~/ expand.
state_dir: $HOME/.local/state/tower

app:
  # DONE items are deleted after this duration.
  retention: 48h

agent:
  # Working directory of run_command and resume_command. $VAR and ~/ expand.
  working_dir: $HOME/.local/state/tower/pi/workspace
  # Started when an item moves to IN PROGRESS.
  #
  # Each argument of the `run_command` and `resume_command` is a Go remplate.
  # The following fileds are available in the template context:
  # - `{{.Prompt}}`: the rendered prompt (required for run_command)
  # - `{{.SessionID}}`: a new UUID per run (required)
  # - `{{.ID}}`: the item's number
  # - `{{.Title}}`: the item's title
  run_command: >
    pi -e /Users/ricoberger/.config/tower/pi-progress.ts --session-dir
    ../sessions --session-id {{.SessionID}} -p -- {{.Prompt}}
  # Run when a TO DO, WAITING or DONE item with a session is resumed.
  resume_command: >
    ghostty-new --placement=tab --title={{.Title}} --command 'pi --session-dir
    ../sessions --session {{shquote .SessionID}}'

providers:
  # Configuration for alerts, which are polled from Grafana-managed
  # Alertmanagers.
  alerts:
    # Interval between polls.
    poll_interval: 1m
    # A resolved alert that fires again within this window reopens its item.
    reopen_window: 24h
    # A list of alert sources. Each source must have a unique name and a valid
    # `grafana_instance` key in `$GRAFANA_INSTANCES`. The `grafana_alertmanager`
    # is the name of the Alertmanager in that Grafana instance. The `filter` is
    # a list of label matchers to filter the alerts. The `receiver` is the name
    # of the receiver to filter the alerts. If empty, all receivers are
    # included.
    sources:
      - name: dev-de1
        grafana_instance: dev-de1
        grafana_alertmanager: grafana
        filter:
          - team="product-core-infra"
        receiver: "incidentio"
    # The prompt which is passed to the agent when the item is moved to
    # IN PROGRESS. The following fields are available in the template context:
    # - `{{.ID}}`: the item's number
    # - `{{.Kind}}`: the item kind (alert, pullrequest, jira, task)
    # - `{{.Source}}`: the source, e.g. the alert or pull request source name
    # - `{{.Title}}`: the one-line title
    # - `{{.Description}}`: the short description shown on the board
    # - `{{.Details}}`: the item as Markdown (empty for tasks)
    # - `{{.URL}}`: the item's link (empty for tasks)
    prompt: |
      Investigate the alert {{.Title}} using the `sre-analyze-alert` skill ...
      {{.Details}}

  # Configuration for pull requests, which are polled from GitHub using the `gh`
  # CLI.
  pullrequests:
    # Interval between polls.
    poll_interval: 5m
    # Only pull requests updated within this duration are shown.
    max_age: 336h
    # A list of pull request sources. Each source must have a unique name and a
    # valid `query`. The query is a GitHub pull request search query, which is
    # passed to the `gh search prs` command.
    sources:
      - name: authored
        query: is:open author:@me
      - name: review-requested
        query: is:open review-requested:@me -author:app/dependabot
    # The prompt which is passed to the agent when the item is moved to
    # IN PROGRESS.
    prompt: |
      {{if eq .Source "review-requested"}}Review {{.URL}} using the `github-pr-review` skill.
      {{- else}}Address the review feedback on {{.URL}} using the `github-pr-review-reviews` skill.{{end}}

      {{.Details}}

  # Configuration for Jira tickets, which are polled using the Atlassian CLI
  # (`acli`). Tower uses the Jira account and site `acli` is logged in to
  # (`acli jira auth login`) and never handles Jira credentials itself.
  #
  # Every search fetches all pages of results (`--paginate`). Tickets whose
  # status category is done (whatever the status is called, e.g. "Closed" or
  # "Cancelled") are not shown: their cards move to DONE, also when the JQL
  # still returns them, and completed tickets never create new cards. Tickets
  # that no longer match a source's JQL move to DONE as well. A DONE ticket that
  # matches again (and is not done) reopens its card in TO DO with its session
  # as long as the card is retained. A ticket matched by two sources is two
  # cards. A failed search changes nothing.
  #
  # Cards show `KEY · summary`, the status and assignee and the description;
  # the details contain the key, source, status, type, priority, assignee,
  # reporter, labels, timestamps and the description (no comments, subtasks or
  # links). Jira Cloud descriptions (ADF) are rendered to Markdown by tower
  # itself. The browser link (`o`) is derived from the site of the ticket's API
  # URL; when acli only reports an API gateway URL, there is no link.
  jira:
    # Interval between polls.
    poll_interval: 5m
    # A list of Jira sources. Each source must have a unique name and a `jql`
    # query, which is passed unchanged to `acli jira workitem search --jql`.
    sources:
      - name: assigned
        jql: "assignee = currentUser() ORDER BY updated DESC"
      - name: team
        jql:
          'project = DEMO AND labels = "needs-attention" ORDER BY priority DESC'
    # The prompt which is passed to the agent when the item is moved to
    # IN PROGRESS.
    prompt: |
      Work on the Jira ticket {{.Title}}.

      {{.Details}}

  # Configuration for tasks, which are created manually in `$EDITOR`. The first
  # non-empty line is the title, the rest is the description. Empty tasks are
  # cancelled.
  tasks:
    # The prompt which is passed to the agent when the item is moved to
    # IN PROGRESS.
    prompt: |
      Task: {{.Title}}

      {{.Description}}
```

## Development

```sh
go build -o ./bin/tower .
go test -race ./...
go vet ./... && golangci-lint run ./...
```
