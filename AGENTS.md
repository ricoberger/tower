# AGENTS.md

Guidance for AI coding agents working on `tower`.

## What tower is

A personal kanban board in the terminal for an SRE. Sources (today:
Grafana-managed Alertmanagers, GitHub pull request searches and Jira JQL
searches, plus tasks)
push items into **TO DO**.
The user starts an agent (Copilot CLI) on an item, which moves it to **IN
PROGRESS**; when the agent exits an item still in that state moves to
**WAITING**. Existing sessions can be resumed from TO DO, WAITING or DONE
without changing state; resolved or closed items end in **DONE**. `README.md`
describes the behavior and configuration and is the source of truth.
`SPEC.md` and `MILESTONES.md` (gitignored) describe the previous, replaced
design — do not implement from them.

## Layout

| Path | Purpose |
| --- | --- |
| `main.go` | Flags, lock, startup recovery of unclaimed starts, wiring, TUI start; hidden `exec` wrapper subcommand |
| `internal/config` | Strict config loading and resolution of nonempty `state_dir` to an absolute filesystem path (`$VAR`, `${VAR}`, `~`); package-owned settings remain zero when omitted and are parsed by their constructors |
| `internal/config/helpers` | Shared config types and functions for config and providers (`Duration`) |
| `internal/store` | SQLite item store (`items` table), atomic completion and conditional recovery, `Sync` rules for source snapshots, retention, `flock` lock; `Get` is retained as the single-item lookup API, also used by integration tests |
| `internal/provider` | `Provider` interface (one per item kind: `Kind`, `Poll`, `Prompt`), the shared prompt data (`PromptData`, `RenderPrompt`) and `Set`. Providers fill the stored item fields `title`, `description`, `details` (Markdown for the prompt) and `url`; everything else uses only these fields |
| `internal/provider/alerts` | Alerts provider: config (`Config`, `$GRAFANA_INSTANCES`), Alertmanager client, alert → item conversion (details Markdown template in Go), prompt, `Poll` loop pushing `store.Update`s on a channel |
| `internal/provider/pullrequests` | Pull requests provider: config and validation in `New`, `gh search prs` runner (injectable for tests, no shell, `--` before the query terms, `updated:>=` from `max_age`), pull request → item conversion, prompt, `Poll` loop |
| `internal/provider/jira` | Jira provider: config and validation in `New`, `acli jira workitem search` runner (injectable for tests, no shell, JQL as one argument, `--paginate`), strict decoding of key/status category, issue → item conversion, built-in ADF → Markdown description renderer (`adf.go`), prompt, `Poll` loop |
| `internal/provider/tasks` | Tasks provider (no polling): config, parses editor text into tasks, prompt |
| `internal/agent` | Agent config (`Config`: `run_command`/`resume_command`/`working_dir`, both commands run in `working_dir`), shell-like command splitting and Go-template commands (`shquote`). Starts agents via the detached `tower exec` wrapper (passes the title for the notification), the wrapper itself (`Exec`), resume, PID liveness |
| `internal/notify` | macOS notifications via osascript |
| `internal/app` | Config (`retention`). Consumes source updates into the store (poll errors are logged), retention loop; implements `ui.Backend` |
| `internal/ui` | Bubble Tea TUI: four kanban columns (one per state; cards with title, time in state and description) a details popup (`K`, composited with lipgloss layers) and a statusline with the keys; failed actions are logged |

New item kinds (e.g. GitLab merge requests) are a self-contained package under
`internal/provider/<kind>` implementing `provider.Provider`, registered in the
`provider.Set` in `main.go`. The package owns its `Config` struct;
the `config.Config.Providers` field holds it under `providers.<kind>`.
`internal/config` decodes, checks keys and resolves `state_dir`; package-owned
parsing and validation belong in each package's `New` (templates, commands,
environment). Validation is still incomplete: the pull requests and Jira
providers validate their options (positive durations, nonempty prompt,
nonempty and unique source names, nonempty queries/JQL) when sources are
configured, but the
alerts provider, `app` and `agent` do not check duration ranges, source-name
uniqueness or required command placeholders. Packages whose config
`internal/config` embeds (providers, `agent`, `app`) must not import
`internal/config` (import cycle); shared helpers live in
`internal/config/helpers`. `Poll` sends complete snapshots as
`store.Update`s on the shared channel and `app` syncs them with `store.Sync`;
`agent` and `ui` only reach the provider through the item's `kind`.

## Commands

```sh
go build -o ./bin/tower .
go test -race ./...
go vet ./... && golangci-lint run ./...   # .golangci.yml; must report 0 issues
```

Tests and lint must pass before committing. While iterating, run
targeted tests with `-race`, e.g. `go test -race -run TestSync ./internal/store`.

## Invariants

- **Transitions:** TO DO → IN PROGRESS only by the user. `store.Finish`
  atomically records failure status and moves a matching IN PROGRESS session
  to WAITING; user/source moves retain their state and timestamp. Dead-wrapper
  recovery uses `store.Recover`, conditional on the observed state, session
  and PID, so a stale observation cannot overwrite a completed run. The
  database stores a failure boolean; exact exit codes go to the run log.
  Sources resolve items missing from a **successful** snapshot; failed polls
  change nothing.
- **Startup claims:** `store.RecoverStarts` runs once with the TUI lock held,
  before accepting starts, and moves unclaimed IN PROGRESS sessions (PID 0)
  to WAITING as failed. The exec wrapper must successfully claim/confirm its
  PID with `store.SetPID` before executing the command. A late wrapper cannot
  claim a recovered session. Periodic liveness checks skip PID 0 because a
  new start may still be in flight.
- **Item keys are globally unique and built by the source**
  (`alert:<source>:<fingerprint>`,
  `pullrequest:<source>:<owner>/<repo>#<number>`,
  `jira:<source>:<issue-key>`, `task:<uuid>`). `Sync` matches items by
  `key`; `kind` + `source` of the `store.Update` scope which items a snapshot
  can resolve.
- **Agents outlive the TUI.** `run_command` runs under `tower exec` in its own
  session (`Setsid`), not bound to any TUI context. Quitting never stops
  agents. The TUI never signals run agents; liveness uses `kill(pid, 0)`.
  Wrappers forward received SIGINT/SIGTERM/SIGHUP to their agent command.
  Resume launchers are separate: they have a 30-second timeout and a 250 ms
  output-pipe wait limit, without signaling their descendants.
- **No shell for configured commands.** `run_command`/`resume_command` are
  split by the `agent` package and every argument is rendered as a Go
  template (`command.render`). Never interpolate item data into shell source;
  use `shquote` for values deliberately passed to a shell-based launcher.
  The Grafana token command and `$EDITOR` launcher use `sh -c`; both command
  strings are user configuration, and the editor path is a positional argument.
- The SQLite database is written concurrently by the TUI and by `tower exec`
  processes (WAL, busy timeout). Keep transactions short; use conditional
  `UPDATE … WHERE state = ? AND session_id = ?` instead of read-modify-write.
- Pull request snapshots must be complete: a search reaching the 1000-result
  limit fails the poll instead of resolving the cut-off items. Pull requests
  use an unlimited reopen window, so retention alone bounds reopening.
- Jira snapshots must be complete too: searches always use `--paginate`
  (never `--limit`), and a result with a missing/invalid issue key or status
  category key fails the poll instead of dropping the ticket. Jira also uses
  an unlimited reopen window.
- Providers emit one-line titles (collapsed whitespace); the UI also
  normalizes loaded titles for old records and future providers. Raw alert
  details remain unchanged.
- Polled fields update even on manually closed, still-reported DONE items.
  Field-only updates must not reset the state, session or retention timestamp.
- Never log or persist secrets (tokens, token-command output) deliberately.
- State files must be `0600`, directories `0700`. SQLite file modes and the
  permissions of existing directories are not yet explicitly enforced; the
  current implementation relies on a private state directory for isolation.

## Behavior reference

User-visible behavior that changes must stay consistent with these rules
(and with `README.md` where it documents them).

### Lifecycle and retention

- Alerts: a resolved alert that fires again within `reopen_window` reopens its
  item (back to TO DO, keeping its session); after the window it becomes a new
  item. Pull requests and Jira tickets pass an unlimited window. Retention should be at least
  `reopen_window`, otherwise items are deleted before they can reopen.
- Retention runs at startup and hourly and deletes DONE items together with
  their run logs. Omitted or zero `retention` deletes all DONE items.
- Sources may move an item to DONE while its agent runs; the agent is not
  stopped. User moves (`t`, `d`) never stop agents either.
- `p` always starts a new session; `r` reuses the stored session (TO DO,
  WAITING or DONE, including after `t`) and never changes state.
- The macOS notification is shown only when a run moves its item to WAITING.

### Agent commands

- Splitting: whitespace, `'…'`, `"…"` and `\` escapes; unquoted `; | & ( )` and
  backticks are rejected; environment variables are not expanded. Template
  actions (`{{ … }}`) stay together while splitting (also with spaces, quotes
  or `}}` in strings/comments), except inside single quotes. A rendered value
  is always exactly one argument.
- Template fields: `.Prompt` (`run_command` only, required there),
  `.SessionID` (new UUID per run, required in both), `.ID`, `.Title`; plus the
  `shquote` function.
- `working_dir` is expanded like `state_dir`, must be an existing directory at
  startup, and is set as `$PWD` (launchers such as `ghostty-new` rely on it).
  Agents inherit tower's environment.
- `run_command`: detached, stdin `/dev/null`, output to
  `state_dir/runs/<item>.log`, no timeout, no concurrency limit.
- `resume_command`: stdin `/dev/null`, output captured only for error
  diagnostics, limits as described under Invariants.

### Prompts

- `text/template` per provider (`providers.<kind>.prompt`); unknown fields are
  errors. All providers get the same `provider.PromptData`: `ID`, `Kind`,
  `Source`, `Title`, `Description`, `Details`, `URL` (`Details` and `URL`
  are empty for tasks).

### Providers

- Alerts: Grafana-managed Alertmanagers only. URL and token command come from
  `$GRAFANA_INSTANCES` (JSON keyed by instance name with `url` and
  `auth.tokenCommand`); it must be valid JSON even without alert sources
  (known limitation). The token command runs via `sh -c` on every poll; HTTP
  requests time out after 15 s. Silenced and inhibited alerts are treated like
  firing ones. Title: `alertname · severity · source`; description: `summary`
  and `description` annotations; URL: generator URL.
- Pull requests: `gh search prs` with a 30-second timeout and `gh`'s own login
  (tower handles no tokens). `gh` and the other options are only required when
  sources are configured. The query is split at whitespace (no quote support);
  `updated:` is day-granular (UTC). Items are per source, so one pull request
  matched by two queries is two cards. Title: `owner/repo#number · title`;
  description: `@author` (plus `· draft`) and the body. Branches and review
  state are not fetched. Merged, closed or stale (older than `max_age`) pull
  requests resolve to DONE.
- Jira: `acli jira workitem search --jql <jql> --fields … --paginate --json`
  with a 30-second timeout per invocation and `acli`'s active Jira login and
  site (tower handles no credentials and never starts a login). `acli` and the
  other options are only required when sources are configured. The JQL is one
  unchanged argument; tower adds no filters. Only the fields needed for the
  card are requested (no comments, subtasks, links or custom fields). Tickets
  whose `status.statusCategory.key` is `done` are left out of the snapshot, so
  they resolve existing cards (also when the JQL still returns them) and never
  create new ones; tickets that stop matching resolve too. Status names and
  colors are never used for completion. Items are per source. Title:
  `KEY · summary`; description: `status · assignee` (`Unassigned` when
  absent) and the description. Plain-string descriptions are kept; ADF
  descriptions are rendered by the built-in renderer (no external converter),
  unreadable ones become a placeholder. URL: `<site>/browse/<KEY>` derived
  from the issue's `self` URL; empty for API-gateway (`api.atlassian.com`,
  `/ex/jira/`) or missing `self` URLs. `acli` search rejects the `created`
  and `updated` fields ("not allowed"), so they are not requested and new
  cards use the poll time; if `acli` reports them anyway, `created` (Jira
  `+0100` offsets and RFC3339) and both timestamps in the details are used.
- Tasks: created with `n` in `$EDITOR`; the first non-empty line is the title,
  the rest the description; empty input cancels.

### Configuration

- Unknown options are errors and there are no defaults.
- A relative `state_dir` resolves against the working directory tower was
  started in, not the config file's directory; `?`, `#` and `%` are literal
  filename characters. Only one tower runs per state directory (lock).
- A nonpositive alerts `poll_interval` crashes tower (known limitation).

### UI

- The board needs at least 20 columns per state (80 total) and 4 rows,
  otherwise it renders `terminal too small`. Cards show the title, time in
  state and up to three description lines.
- Keys that don't apply to the selected item do nothing; failures are logged,
  not shown. `o` opens the item URL with macOS `open`; `L` opens the run log in
  `$EDITOR`; `R` polls all sources now.

## Testing conventions

- Use temp directories for state and config. Never touch the real
  `~/.config/tower`, Grafana, GitHub (`gh`), Copilot or desktop notifications
  (tests pass a fake `agent.Notifier` to `Exec` and a fake runner to the pull
  requests provider).
- `internal/agent` tests use the test binary as the `tower exec` wrapper
  (see `TestMain`) and `sh -c` as the agent.
- Store tests cover stale recovery observations across connections, atomic
  failure handling and both orderings of startup recovery versus wrapper claims.
  Keep recovery outside periodic reads of pending starts.
- UI tests drive the model with a fake `Backend` and check `Render()` output
  with ANSI stripped, including short-terminal resizes and multiline titles.
- Table-driven tests where it fits; names describe behavior.

## Code conventions

- Go version per `go.mod`; `gofmt`/`goimports` are enforced by the linter.
  Keep dependencies minimal (SQLite is `modernc.org/sqlite`, pure Go).
- Logging goes to `state_dir/tower.log` via `log/slog` (the TUI owns the
  terminal).
- Comments explain *why*, not *what*. Doc comments on exported identifiers.
- Commits follow Conventional Commits with a body explaining the reason.
