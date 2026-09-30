# tower

tower is a personal work harness for SREs on macOS. It polls Alertmanager
alerts, lets an unattended [GitHub Copilot CLI](https://github.com/github/copilot-cli)
session prepare each alert with the `sre-analyze-alert` skill, and shows the
results in a terminal UI. When you pick an item, tower resumes the prepared
Copilot session in a new [Ghostty](https://ghostty.org) surface, so you
continue exactly where the preparation stopped.

The MVP handles alerts only (Alertmanager, Grafana-managed Alertmanager and a
JSON file source for development). Pull requests, issues and other work item
types are planned for later.

## How it works

- **Foreground process.** `tower` runs one foreground process: the terminal
  UI plus the engine (pollers, reconciler, run queue, notifications). There is
  no daemon. Only one instance per state directory can run; a second one fails
  and names the lock file.
- **Detached preparations.** Each preparation run is a detached Copilot
  process in its own process group. Quitting the UI (`q` / `ctrl+c`) stops
  polling but leaves executing runs running. On the next start tower
  re-attaches to them, or picks up their results if they finished in the
  meantime. Runs interrupted before completion are retried once.
- **Preparation versus decisions.** Automatic preparation is read-only by
  prompt contract: the run investigates up to the skill's first confirmation
  gate and stops there with a report. Every write, rollback, silence or other
  decision happens later in the interactive session that you resume. tower
  itself never executes the proposed actions of a report.

Items move through these groups in the UI: **Needs you** (a finished
preparation waits for you), **Preparing** (queued or executing), **Incoming**
(firing, not yet prepared), **Snoozed** (silenced or inhibited),
**Resolved** (resolved recently) and optionally **Done**.

## Installation

Requirements for building from source:

- Go 1.27.1 or newer (see `go.mod`)
- macOS (process and notification handling use macOS tools)

```sh
make build           # writes ./bin/tower
cp bin/tower /usr/local/bin/tower   # or any directory on your PATH
```

Runtime prerequisites:

- An authenticated **Copilot CLI** (`copilot`) and the user-level SRE skills
  (`sre-analyze-alert`, `sre-grafana`, `sre-kubernetes`, …) installed for
  Copilot.
- **Ghostty** and the external **`ghostty-new`** helper script, configured via
  `ghostty.command`. `ghostty-new` is not part of this repository; it creates
  a split, tab or window and types the resume command into its interactive
  shell. tower calls it as:

  ```sh
  ghostty-new --placement <split|tab|window> [--direction <direction>] \
    --working-dir <item-dir> --title "tower: <item title>" \
    --command "'<runs.command>' '--resume' '<session-id>' '<resume_args>'…"
  ```

- Optional: **terminal-notifier** (`brew install terminal-notifier`). With it,
  clicking a notification resumes the item's session. Without it tower falls
  back to `osascript`; those notifications are shown but clicking them only
  opens Script Editor.

## Configuration

```sh
tower config init       # writes a commented example if no config exists
tower config validate   # prints warnings and errors, exit code 1 on errors
```

The configuration file is the first of `--config <file>`, `$TOWER_CONFIG` and
`~/.config/tower/config.yaml`. A leading `~/` in the selector expands to
`$HOME`; a relative selector is resolved against the current directory. A
missing file is an error with a hint to `tower config init`.

State (items, runs, `tower.log`, `tower.lock`) is stored in `state_dir`,
default `~/.local/state/tower`.

Rules:

- **Durations** use Go syntax: `30s`, `5m`, `24h`, `8760h`.
- **Strict decoding.** Unknown keys and wrong types are errors.
- **Environment interpolation.** String values and list elements support
  `$VAR`, `${VAR}` and `$$` for a literal `$`. It is a single pass, with no
  shell and no defaults. An unset variable is an error; other `$` characters
  (for example regex anchors) stay literal. Errors name the field and the
  variable, never the value.
- **Credential commands are the exception.** `token_command` and
  `password_command` are passed verbatim to `sh -c` at request time; they are
  never interpolated and never run by `config validate`.
- **Paths.** `state_dir`, `sources[].path`, `*_file`, `prompts.alert` and
  `runs.command`, `ghostty.command`, `editor` (when they contain a `/`) are
  interpolated, then a leading `~/` expands to `$HOME`, then a relative path
  is resolved against the **config file's directory**. Bare command names are
  looked up on `PATH`.
- **Executables.** `runs.command` that cannot be resolved is a warning in
  `config validate` and an error at startup. An unresolvable
  `ghostty.command` is a warning; the resume keys then show an error. An
  `alertmanager` source without `grafana_instance` is a warning, because the
  skill has no Grafana to investigate against.
- **Editor.** `editor` is a single executable (default `$EDITOR`, then `vi`),
  never a command line. Use a wrapper script for arguments, for example:

  ```sh
  #!/bin/sh
  # ~/bin/code-wait
  exec code --wait "$@"
  ```

  and `editor: ~/bin/code-wait`.

### Example

All hosts and credentials below are synthetic.

```yaml
# ~/.config/tower/config.yaml

# Where items, logs and the lock file are stored.
state_dir: ~/.local/state/tower

# Editor used to open reports and logs. Defaults to $EDITOR, then "vi".
# A single executable name or path (no arguments, no shell expression); use a
# wrapper script for options such as --wait.
editor: nvim

sources:
  - name: prod-eu                        # unique, [a-z0-9-]+, used in item ids
    type: alertmanager                   # alertmanager | file
    # Name of the Grafana instance in $GRAFANA_INSTANCES (the same variable
    # the sre-grafana skill uses). It is the Grafana the skill investigates
    # against. For Grafana-managed sources it also provides url + auth for
    # polling, so both can be omitted.
    grafana_instance: prod-eu
    # Grafana-managed Alertmanager: alerts are read from
    # {url}/api/alertmanager/{grafana_alertmanager}/api/v2/alerts
    # Omit for a plain Prometheus Alertmanager ({url}/api/v2/alerts).
    grafana_alertmanager: grafana
    # Alertmanager matchers pushed down to the API as repeated filter= params.
    filter:
      - team="core"
      - severity=~"critical|warning"
    # Optional receiver regex pushed down as receiver= param.
    receiver: ".*pager.*"

  - name: prod-us
    type: alertmanager
    grafana_instance: prod-us
    grafana_alertmanager: grafana
    # Explicit url and auth override the values derived from the instance
    # independently; "auth: {type: none}" disables the derived auth.
    url: https://alerts-proxy.example.com
    auth:
      type: bearer
      token_command: security find-generic-password -s tower-prod-us -w

  - name: legacy-am
    type: alertmanager                   # plain Prometheus Alertmanager
    url: https://alertmanager.example.com
    # Polling credentials (never derived for plain sources).
    auth:
      type: basic                        # none | basic | bearer
      # exactly one of token / token_file / token_command for bearer;
      # username + one of password / password_file / password_command for basic
      username: tower
      password_file: ~/.config/tower/legacy-am.password
    # Optional but recommended for plain sources: without it the skill has no
    # Grafana to query metrics / logs / traces. Investigation metadata only.
    grafana_instance: prod-eu
    filter:
      - team="core"

  - name: dev-fixture
    type: file                           # reads alerts from a JSON file (dev / tests)
    path: ./testdata/alerts.json         # same schema as GET /api/v2/alerts; relative to this file's directory

alerts:
  # Poll interval for all alert sources (at least 10s).
  poll_interval: 1m
  # An alert must be firing for at least this long (measured from startsAt)
  # before it is prepared automatically.
  prepare_after: 5m
  # Resolved items stay visible (dimmed) this long before becoming done.
  resolved_linger: 4h
  # A re-fire of the same fingerprint within this window reopens the item.
  reopen_window: 24h
  # Queue priority by severity label value; unknown/missing values go last.
  severity_order: [critical, error, warning, info]

runs:
  # Maximum number of concurrently executing runs (1-10).
  concurrency: 2
  # Runs are killed after this duration and marked failed (at least 1m).
  timeout: 20m
  # Command and extra arguments for prep runs. tower always adds:
  #   -p <prompt> --session-id <uuid> --output-format json --no-ask-user
  command: copilot
  args: [--yolo]
  # Optional; omitted = Copilot CLI default model.
  model: ""
  # Arguments used when resuming a session interactively. tower always adds:
  #   --resume <session-id>
  resume_args: []

notifications:
  enabled: true
  # terminal-notifier is used when found on PATH (clickable), otherwise osascript.
  sound: default                         # "" disables sound

ghostty:
  # Script used to open new Ghostty surfaces (external helper).
  command: ghostty-new
  # Placement for the "resume" key; the "resume in tab" key always uses "tab".
  placement: split                       # split | tab | window
  direction: right                       # used for split placement

retention:
  # Done items older than this are deleted on startup and by `tower prune`.
  done_after: 8760h                      # 1 year

prompts:
  # Optional override of the embedded prep prompt template.
  alert: ""                              # path to a Go text/template file
```

### `$GRAFANA_INSTANCES`

tower reuses the environment variable of the `sre-grafana`,
`sre-analyze-alert` and `sre-kubernetes-rightsizing` skills, so Grafana URLs
and credentials live in one place:

```json
{
  "prod-eu": {
    "url": "https://grafana-eu.example.com",
    "auth": { "tokenCommand": "security find-generic-password -s grafana-prod-eu -w" }
  },
  "prod-us": {
    "url": "https://grafana-us.example.com",
    "auth": { "tokenCommand": "cat ~/.cache/grafana/prod-us.token" }
  }
}
```

- The variable is required only when a source references `grafana_instance`.
  Only referenced records are validated: each needs a string `url`, and
  `auth.tokenCommand` is needed when a Grafana-managed source derives its
  polling auth from it.
- For polling, tower runs `auth.tokenCommand` with `sh -c` at request time and
  sends the output as a bearer token.
- For investigations, tower passes only the **instance name**. `alert.md`
  contains a "Grafana Credentials" line telling the skill to resolve the
  instance from `$GRAFANA_INSTANCES` itself. Preparation runs inherit tower's
  environment; tower never writes tokens into prompts or artifacts.

## Using the terminal UI

Run `tower`. The header shows each source's health (last successful poll,
last error or "never polled") and the runner (`running X/Y · queued N`). The
list is grouped by state; within a group, items are sorted by severity order,
then oldest first. Unseen items are bold. The preview shows the selected
item: metadata, summary, root cause, the gate question with its options,
proposed actions, assumptions and the full `report.md`. While a run executes,
it shows the elapsed time and the latest Copilot message; for a failed run,
the error and the last 20 lines of `stderr.log` (lines longer than 4 KiB are
truncated).

The latest Copilot message comes from the last 256 KiB of the run's
`output.jsonl`; the file is only reread when its size or modification time
changes. When a long session writes more than 256 KiB without a new
assistant message, the preview keeps showing the last message it found for
that run (or none yet) instead of reading the whole file.

A failed desktop notification is shown first in the header
(`notification ✗ <error> (<item>)`) until a later notification succeeds; the
run's result is not affected.

| Key                | Action                                                                                |
| ------------------ | ------------------------------------------------------------------------------------- |
| `j` / `k`, `↓`/`↑` | Move the selection (or scroll the preview when it has focus)                           |
| `g` / `G`          | First / last item                                                                       |
| `tab`              | Switch focus between list and preview                                                   |
| `enter` / `o`      | Open the latest run's `report.md` in the editor                                         |
| `c`                | Resume the latest run's session in Ghostty (`ghostty.placement`)                        |
| `C`                | Resume in a new tab                                                                     |
| `p`                | Prepare now / re-run                                                                    |
| `x`                | Dismiss (confirm with `y`)                                                              |
| `l`                | Open the latest run's `output.jsonl` and `stderr.log` in the editor                     |
| `b`                | Open the alert's runbook URL, else its generator URL, in the browser (http/https only)  |
| `a`                | Open the item directory in the editor                                                   |
| `d`                | Show or hide done items updated in the last 7 days                                      |
| `r`                | Poll all sources now                                                                    |
| `?`                | Help (scroll with `j`/`k`, `↓`/`↑`, `g`/`G`; close with `?` or `esc`)                  |
| `q` / `ctrl+c`     | Quit; executing runs continue in the background                                         |

- **Confirmations.** `x` and resuming a still-executing run ask in the footer.
  Only `y` confirms; `n` or `esc` cancel. While a confirmation or the help is
  open, other action keys do nothing. A resume confirmation is bound to the
  run it was asked for; if a newer run appears meanwhile, the resume is
  cancelled.
- **Seen and history.** Only a successful report open (`enter`/`o`) and a
  successful resume mark an item seen and record `opened-report` or
  `resumed-session`. `p` and `x` are validated by the engine, which records
  `manual-run` and `dismissed`; rejections become footer hints. `l`, `a` and
  `b` record nothing. Failed actions never record history.
- **Engine requests never block the UI.** A slow poll or a busy engine only
  delays the footer result; navigation keeps working.

### Resume outside the UI

```sh
tower resume <item-id> [--placement split|tab|window]
```

`tower resume` opens the item's latest session in Ghostty whether or not the
UI is running. It does not start the engine or take the instance lock. It marks
the item seen and records `resumed-session`. It refuses a run that is still
executing (use the UI, which asks for confirmation). A run without a started
session, such as one that failed before Copilot started, cannot be resumed.

**Notification clicks.** A terminal-notifier notification runs
`'<tower>' --config '<config>' resume <item-id>` with launchd's minimal
environment and no terminal. So before loading the configuration,
`tower resume` captures your **interactive login shell** environment
(`<shell> -l -i -c`), because `zsh -lc` does not read `~/.zshrc`, where PATH
additions and `$GRAFANA_INSTANCES` usually live. The shell is `$SHELL`, else
the login shell from the password database, else `/bin/zsh`. Startup output of
the shell is ignored and the capture has a 10 s limit. The captured
environment is never logged or written to disk. Captured values fill in
missing variables, while values tower was started with win. `PATH` is always
the login shell's, and executables are resolved on it (absolute paths still
work). Ghostty receives the merged environment.

Resume failures are printed and also shown as an error notification without a
click action, because a notification click has no terminal. The typed resume
command single-quotes every word. Commands, session IDs and resume arguments
that contain control characters (newline, CR, NUL, …) are rejected before
anything is launched.

### Other commands

```sh
tower prune [--older-than <dur>] [--dry-run]   # delete old done items
tower version
```

Global flags: `--config <file>`, `--log-level debug|info|warn|error`. In the
UI, logs go only to `<state_dir>/tower.log`.

## Preparation contract

Each preparation run executes Copilot with the embedded prompt, which asks it
to use the **unchanged** `sre-analyze-alert` skill:

- The run is **read-only by prompt contract**: no mutations anywhere (no
  kubectl/helm/flux changes, silences, pushes, GitHub/Jira/Grafana writes).
  This is an instruction, not a technical sandbox; runs use `--yolo` so they
  never stall on tool permissions, and `--no-ask-user` so they never stall on
  questions.
- The run stops at the skill's **first** confirmation gate or missing-input
  question and never answers it itself. The skill's request to restate the
  alert is the exception: the restatement goes into the report.
- Missing inputs are never guessed. The run then ends `blocked` with the exact
  question.
- The run always writes `report.md` and `result.json` into its run directory,
  `result.json` last. The status is `ready` or `blocked`. A missing or invalid
  file makes the run `failed`.
- Earlier reports of the same alert (up to three, most recent first) are
  referenced in the prompt, and the run states what changed.
- `prompts.alert` overrides the template (Go `text/template`) with the same
  data: `RunDir`, `AlertFile`, `AlertMarkdown`, `SourceName`, `Fingerprint`,
  `PreviousReports`, `ResultSchema`.
- When you resume, you continue at the gate with the skill's normal
  interactive flow. Proposed actions in the report are suggestions; tower
  never executes them.

## Development

```sh
make build   # build ./bin/tower
make test    # go test -race ./...
make lint    # go vet and golangci-lint
```

Tests never use real credentials, Grafana, Copilot, Ghostty, your login shell,
a browser or desktop notifications.

For manual experiments, use an isolated configuration with a file source, a
separate state directory and the fake Copilot fixture:

```yaml
# /tmp/tower-dev/config.yaml
state_dir: ./state
sources:
  - name: dev
    type: file
    path: ./alerts.json          # GET /api/v2/alerts format
alerts:
  prepare_after: 0s
runs:
  command: /path/to/tower/testdata/fake-copilot.sh
notifications:
  enabled: false
ghostty:
  command: /usr/bin/true
```

```sh
FAKE_COPILOT_MODE=ready tower --config /tmp/tower-dev/config.yaml
```

The fixture never contacts any service. It is controlled by environment
variables:

- `FAKE_COPILOT_MODE`: `ready` (default), `blocked`, `invalid`,
  `missing-result`, `missing-report`, `nonzero`, `hang` or `descendant`
- `FAKE_COPILOT_DELAY`: seconds before finishing
- `FAKE_COPILOT_DESCENDANT=1`: leave a background process in the group
- `FAKE_COPILOT_EARLY=1`: write the result files before the delay
- `FAKE_COPILOT_DIR`: a control directory for per-item modes, holding and a
  start/end log

For development without the UI, `tower --headless --config …` runs the same
engine and logs to stderr and `tower.log`. The flag is intentionally not
listed in `tower --help`.

## Manual live check (post-merge, not yet performed)

The automated tests use fakes only. This check verifies the preparation
contract against a real Grafana-managed source and the real skills. It has
**not** been performed yet.

1. Configure a Grafana-managed source with `grafana_instance` and
   `grafana_alertmanager`. Narrow its `filter` so that only one real, harmless
   alert matches. Make sure `$GRAFANA_INSTANCES` contains the instance and that
   the Copilot CLI and SRE skills are installed and authenticated.
2. Run `tower` in the foreground and wait until the alert's preparation run
   finishes.
3. The run must reach its gate with `ready`. Open the item directory (`a`)
   and inspect:
   - `alert.md`: the "Grafana Credentials" line names the instance and tells
     the skill to resolve it from `$GRAFANA_INSTANCES`.
   - `runs/<n>/report.md`: a report in the skill's format.
   - `runs/<n>/result.json`: valid, `"status": "ready"`.
4. Check the session (`l` or resume with `c`): the skill understood the
   instance-name line and never asked for credentials.

A `blocked` or `failed` run does not pass this check. If the skill does not
understand the instance-name line, adjust the wording of that line in
`alert.md`, not the skill. Never put a token into `alert.md`, the prompt or
any other artifact.
