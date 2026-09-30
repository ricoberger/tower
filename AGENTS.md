# AGENTS.md

Guidance for AI coding agents working on `tower`.

## What tower is

A personal work harness for an SRE. It polls sources of work (MVP:
Alertmanager alerts), turns each unit of work into an **item**, prepares items
with unattended, read-only Copilot CLI runs (the `sre-analyze-alert` skill),
and hands them to the user for decisions (resume the Copilot session in a
Ghostty split).

## Source of truth

- The product specification is `SPEC.md` in the repository root. It is
  **gitignored** and does not exist in other worktrees — always read it via
  `/Users/ricoberger/Documents/GitHub/ricoberger/tower/SPEC.md`.
- `MILESTONES.md` (also gitignored) holds the implementation plan.
- If the code and `SPEC.md` disagree, or the spec is ambiguous, raise it as a
  question instead of guessing.
- Do not reference spec section numbers, transition IDs (T1–T14) or review
  findings in code, comments or test names. Those documents are not in the
  repository. Describe rules by name ("resolve", "linger expiry",
  "manual run"; see the `internal/reconcile` package doc).

## Layout

| Path | Purpose |
| --- | --- |
| `main.go` | CLI (`version`, `config init/validate`, `prune`, `resume`, hidden `--headless`) |
| `engine.go`, `engine_runs.go` | Engine loop: polls, reconciliation, runner, notifications, API |
| `tui.go` | Runs the engine with the Bubble Tea TUI and wires the UI to the engine, store and Ghostty |
| `resume.go` | `tower resume` and the shared Ghostty resume handoff (validation, `resumed-session` recording) |
| `logging.go` | slog setup (JSON log file + stderr) |
| `internal/config` | Config loading, `$VAR` interpolation, path resolution, validation, `$GRAFANA_INSTANCES` |
| `internal/item` | Item store: IDs, `item.yaml`, run directories/artifacts, locking, pruning |
| `internal/reconcile` | **Pure** lifecycle reconciler (items + snapshots + events + now → changes + effects) |
| `internal/source` | Alert sources (`alertmanager`, `file`) |
| `internal/prompt` | `alert.md` rendering; `prompt/prep` — the embedded preparation prompt |
| `internal/runner` | Priority queue, detached runs, completion, timeout/cancel, recovery |
| `internal/result` | `result.json` types, schema and validation |
| `internal/ghostty` | Ghostty handoff through the external `ghostty-new` helper (quoted resume command) |
| `internal/loginenv` | Captures the interactive login-shell environment (in memory only) for processes started without one |
| `internal/bounded` | Runs short-lived external commands in their own process group with a time limit and bounded cleanup |
| `internal/notify` | macOS notifications (terminal-notifier / osascript) |
| `internal/snapshot` | Read-only snapshot of the engine's applied state and the non-blocking feed from engine to TUI (the engine never imports `internal/ui`) |
| `internal/ui` | Bubble Tea TUI: renders snapshots (Markdown via glamour, sanitized), calls the engine API, editor handoff |
| `testdata/fake-copilot.sh` | Fake Copilot CLI for runner tests (controlled by `FAKE_COPILOT_*` env vars) |

## Commands

```sh
make build   # ./bin/tower
make test    # go test -race ./...
make lint    # go vet + golangci-lint (.golangci.yml); must report 0 issues
```

`make test` and `make lint` must pass before committing. While iterating,
run targeted tests directly and keep `-race`, e.g.
`go test -race -run TestName ./internal/runner`.

## Architecture invariants

- **The engine loop is the only place that mutates engine state.** Background
  work (poll rounds, `cmd.Wait()`, ownership checks, notification delivery,
  pruning) runs in goroutines and reports back through channels that the loop
  selects on.
- **Nothing blocks the engine loop.** No network calls, credential commands,
  `ps`, notification commands or signal grace periods on the loop. New slow
  work goes into a goroutine with a bounded timeout. Its result is delivered
  by channel, and sends must not block after shutdown.
- **The reconciler stays pure.** No I/O, no clock reads (`now` is an input),
  no randomness. Side effects are returned as effects/intents and executed by
  the engine only after the item change was persisted successfully.
- Poll rounds run in the background: one round in flight at most, and ticks
  are skipped while one is running. A source failure freezes only that
  source's transitions.
- Shutdown never kills or waits for detached preparation runs.

## Processes and safety

- Every external command runs with a timeout, in its own process group
  (`Setpgid`/`Setsid`), with a bounded `WaitDelay`. On timeout the whole
  group is killed.
- Preparation runs: `cmd.Wait()` is used for runs this process started. The
  PID liveness check is used only for runs re-attached after a restart.
- **Never signal a process whose ownership is not proven.** For re-attached
  runs, `ps -o command= -p <pid>` must contain the run's session ID before
  **every** signal. On any failed check, send no signal and abandon the run.
- Content is always passed as arguments or environment, never interpolated
  into shell source or AppleScript code.
- Never log or persist secrets: credentials, credential-command output, URL
  userinfo, inherited environment, full prompts/argv. Redact error excerpts.
- Preparation runs must stay read-only; that guarantee lives in the prompt
  contract — do not weaken the embedded prompt's rules.

## Persistence

- State and item directories are `0700`, files `0600`; runs use `umask 077`.
- Tower-owned documents are written atomically: same-directory temp file,
  `fsync`, rename.
- Item changes use the store's per-item `flock` read-modify-write
  (`Store.Update`). Other processes (e.g. `tower resume`) may write
  concurrently — merge, never overwrite blindly.
- Validate item IDs and run numbers, and resolve all paths through the
  store's safe accessors (containment, no symlinks). `Store.ItemDir` is for
  display only.
- Unreadable or corrupt items are logged and skipped, never repaired or
  overwritten.

## Testing conventions

- Use isolated temp state/config directories. Never touch real
  `~/.config/tower`, credentials, Grafana, Copilot or desktop notifications.
- Runner/engine tests use `testdata/fake-copilot.sh` via `runs.command`. Test
  controls are env vars, never user configuration.
- Timing is injected (`engineOptions`, `runner.Options`: clock, tick/monitor
  channels, grace, `Owns`, `Signal`, `deliver`). Prefer injected channels and
  hooks over sleeps. Production defaults must not change for tests.
- Tests that spawn process groups must clean them up, including on failure.
- Table-driven tests with named subtests; names describe behavior.

## Code conventions

- Go version per `go.mod`; formatting via `gofmt`/`goimports` (enforced by
  the linter). Keep dependencies minimal.
- Logging: `log/slog` with `item_id`, `run` and `source` attributes where
  applicable. Warn for recoverable failures, error for failed persistence.
- Comments explain *why*, not *what*. Doc comments on exported identifiers.
- Commits follow Conventional Commits (`feat(runner): …`, `fix(engine): …`,
  `test: …`, `docs: …`), with a body explaining the reason for the change.
