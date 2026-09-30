// Package runner executes detached preparation runs, monitors them for
// completion, timeout and cancellation, and terminates their process groups.
//
// A Runner is not safe for concurrent use: it is driven by the engine loop
// only. Wait goroutines of locally started runs communicate through WaitC
// and never block once the runner is closed.
package runner

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/prompt/prep"
	"github.com/ricoberger/tower/internal/result"
)

// Production defaults.
const (
	// MonitorInterval is the cadence of periodic run checks.
	MonitorInterval = 2 * time.Second
	// Grace is the delay between TERM and KILL of a process group.
	Grace = 10 * time.Second
	// Skill is the skill of preparation runs.
	Skill = "sre-analyze-alert"
	// ErrCancelled is the error of a cancelled run.
	ErrCancelled = "run cancelled"
)

// Options configures a Runner.
type Options struct {
	Store *item.Store
	// Command, Model and Args are the normalized runs.command, runs.model
	// and runs.args.
	Command string
	Model   string
	Args    []string
	// Timeout is runs.timeout.
	Timeout time.Duration
	// Template is the parsed preparation prompt.
	Template *template.Template
	Log      *slog.Logger

	// Grace overrides the TERM-to-KILL grace (Grace).
	Grace time.Duration
	// Now overrides the clock (time.Now).
	Now func() time.Time
	// Environ overrides the inherited environment (os.Environ).
	Environ func() []string
	// Shell overrides the wrapper shell (/bin/sh).
	Shell string
	// Signal overrides syscall.Kill for signals and liveness probes.
	Signal func(pid int, sig syscall.Signal) error
	// Owns overrides the ownership check of re-attached runs
	// (ProcessOwns).
	Owns func(pid int, sessionID string) bool
}

// Completion is a finished run whose metadata was persisted.
type Completion struct {
	ItemID string
	Source string
	Run    item.Run
}

// WaitResult is the cmd.Wait result of a locally started run.
type WaitResult struct {
	itemID string
	run    int
	state  *os.ProcessState
}

// StartError is a failed start. If Run is set, the failed attempt was
// persisted as a failed run and should be surfaced through the normal
// failure lifecycle; otherwise nothing usable was created.
type StartError struct {
	Run *item.Run
	Err error
}

func (e *StartError) Error() string { return e.Err.Error() }
func (e *StartError) Unwrap() error { return e.Err }

// Request is a run to start.
type Request struct {
	Item     *item.Item
	Number   int
	Reason   item.RunReason
	RetryOf  int
	QueuedAt time.Time
}

type tracked struct {
	itemID string
	source string
	run    item.Run
	// local is true for runs started by this runner (owned through Wait);
	// false for runs re-attached after a restart.
	local     bool
	since     time.Time
	waitDone  bool
	waitState *os.ProcessState
	// forced is the outcome of a termination by tower (timeout or
	// cancellation) and forcedErr its error.
	forced    item.Outcome
	forcedErr string
	completed bool
	// checks counts periodic checks since completion.
	checks int
	termAt time.Time
	killed bool
	// abandoned is set when a re-attached run's ownership could not be
	// verified before a signal: it is never signalled again.
	abandoned bool
	// cleaning is set once post-completion cleanup signalled the group.
	cleaning bool
	clean    bool
}

// Runner starts and monitors preparation runs.
type Runner struct {
	opts  Options
	runs  map[string]*tracked
	waitC chan WaitResult
	done  chan struct{}
	close sync.Once
}

// New returns a Runner.
func New(opts Options) *Runner {
	if opts.Grace <= 0 {
		opts.Grace = Grace
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Environ == nil {
		opts.Environ = os.Environ
	}
	if opts.Shell == "" {
		opts.Shell = "/bin/sh"
	}
	if opts.Signal == nil {
		opts.Signal = syscall.Kill
	}
	if opts.Owns == nil {
		opts.Owns = ProcessOwns
	}
	if opts.Template == nil {
		opts.Template = prep.Default()
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return &Runner{
		opts:  opts,
		runs:  map[string]*tracked{},
		waitC: make(chan WaitResult),
		done:  make(chan struct{}),
	}
}

// WaitC delivers the Wait results of locally started runs; pass them to
// HandleWait.
func (r *Runner) WaitC() <-chan WaitResult { return r.waitC }

// Close stops the runner. Tracked processes are neither signalled nor
// waited for; pending Wait goroutines drop their result.
func (r *Runner) Close() { r.close.Do(func() { close(r.done) }) }

// Busy returns the number of occupied slots: executing runs and completed
// runs whose process group is still being cleaned up.
func (r *Runner) Busy() int { return len(r.runs) }

// Tracking reports whether a run of the item occupies a slot.
func (r *Runner) Tracking(itemID string) bool {
	_, ok := r.runs[itemID]
	return ok
}

// Executing reports whether a run of the item is executing (tracked and not
// yet completed).
func (r *Runner) Executing(itemID string) bool {
	t, ok := r.runs[itemID]
	return ok && !t.completed
}

// TrackedIDs returns the IDs of items with a tracked run.
func (r *Runner) TrackedIDs() []string { return slices.Sorted(maps.Keys(r.runs)) }

func (r *Runner) now() time.Time { return r.opts.Now() }

func stamp(t time.Time) *time.Time {
	s := t.UTC().Truncate(time.Second)
	return &s
}

func (r *Runner) logger(t *tracked) *slog.Logger {
	return r.opts.Log.With("item_id", t.itemID, "run", t.run.Number, "source", t.source)
}

// Start creates run req.Number of an item and starts it detached. On
// success the persisted running metadata is returned. Errors are
// *StartError.
func (r *Runner) Start(req Request) (item.Run, error) {
	it := req.Item
	if _, ok := r.runs[it.ID]; ok {
		return item.Run{}, &StartError{Err: errors.New("the item already has a tracked run")}
	}
	sid, err := newSessionID()
	if err != nil {
		return item.Run{}, &StartError{Err: fmt.Errorf("create session ID: %w", err)}
	}
	run := item.Run{
		Number:    req.Number,
		SessionID: sid,
		Reason:    req.Reason,
		Skill:     Skill,
		QueuedAt:  req.QueuedAt.UTC(),
		Outcome:   item.OutcomeRunning,
		RetryOf:   req.RetryOf,
	}
	store := r.opts.Store
	if err := store.CreateRun(it.ID, run); err != nil {
		return item.Run{}, &StartError{Err: fmt.Errorf("create run directory: %w", err)}
	}
	fail := func(err error) (item.Run, error) {
		f := run.Clone()
		f.FinishedAt = stamp(r.now())
		f.Outcome = item.OutcomeFailed
		f.Error = err.Error()
		if werr := store.WriteRun(it.ID, f); werr != nil {
			r.opts.Log.Error("persist failed run start", "item_id", it.ID, "run", run.Number,
				"source", it.Source.Name, "error", werr)
			return item.Run{}, &StartError{Err: err}
		}
		return f, &StartError{Run: &f, Err: err}
	}

	runDir, err := store.RunPath(it.ID, run.Number)
	if err != nil {
		return fail(fmt.Errorf("resolve run directory: %w", err))
	}
	itemDir, err := store.ItemPath(it.ID)
	if err != nil {
		return fail(fmt.Errorf("resolve item directory: %w", err))
	}
	data, err := r.promptData(it, run.Number, runDir)
	if err != nil {
		return fail(err)
	}
	prompt, err := prep.Render(r.opts.Template, data)
	if err != nil {
		return fail(err)
	}
	if err := store.WriteRunFile(it.ID, run.Number, item.PromptFile, prompt); err != nil {
		return fail(fmt.Errorf("write prompt.md: %w", err))
	}

	// The run is detached and outlives this process, so it is not bound to
	// a context.
	cmd := exec.Command(r.opts.Shell, commandArgs(r.opts.Command, string(prompt), sid, r.opts.Model, r.opts.Args)...) //nolint:noctx,gosec // fixed wrapper; command and arguments are positional parameters
	cmd.Dir = itemDir
	cmd.Env = childEnv(r.opts.Environ(), runDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start run: %w", err))
	}
	now := r.now()
	run.PID = cmd.Process.Pid
	run.StartedAt = stamp(now)
	if err := store.WriteRun(it.ID, run); err != nil {
		// The start is written again when it is applied to the item.
		r.opts.Log.Error("persist run start", "item_id", it.ID, "run", run.Number, "source", it.Source.Name, "error", err)
	}
	t := &tracked{itemID: it.ID, source: it.Source.Name, run: run.Clone(), local: true, since: now}
	r.runs[it.ID] = t
	go r.wait(cmd, it.ID, run.Number)
	r.logger(t).Info("run started", "pid", run.PID, "reason", string(run.Reason))
	return run.Clone(), nil
}

// wait collects the exit of a locally started run exactly once.
func (r *Runner) wait(cmd *exec.Cmd, itemID string, n int) {
	_ = cmd.Wait()
	select {
	case r.waitC <- WaitResult{itemID: itemID, run: n, state: cmd.ProcessState}:
	case <-r.done:
	}
}

// promptData collects the template data of a new run.
func (r *Runner) promptData(it *item.Item, n int, runDir string) (prep.Data, error) {
	md, err := r.opts.Store.ReadAlertMarkdown(it.ID)
	if err != nil {
		return prep.Data{}, fmt.Errorf("read alert.md: %w", err)
	}
	alertFile, err := r.opts.Store.AlertMarkdownPath(it.ID)
	if err != nil {
		return prep.Data{}, fmt.Errorf("resolve alert.md: %w", err)
	}
	return prep.Data{
		RunDir:          runDir,
		AlertFile:       alertFile,
		AlertMarkdown:   string(md),
		SourceName:      it.Source.Name,
		Fingerprint:     it.Source.Fingerprint,
		PreviousReports: PreviousReports(r.opts.Store, it, n),
		ResultSchema:    result.Schema,
	}, nil
}

// PreviousReports returns the absolute paths of existing reports of earlier
// runs of the item (before run n), newest first and at most
// prep.MaxSameItemReports, followed by the latest existing report of the
// previous item, if any. Unreadable evidence is skipped.
func PreviousReports(store *item.Store, it *item.Item, n int) []string {
	var out []string
	if runs, err := store.ReadRuns(it.ID); err == nil {
		for i := len(runs) - 1; i >= 0 && len(out) < prep.MaxSameItemReports; i-- {
			if runs[i].Number >= n {
				continue
			}
			if p, ok := reportPath(store, it.ID, runs[i].Number); ok {
				out = append(out, p)
			}
		}
	}
	if it.PreviousItem == nil || *it.PreviousItem == it.ID {
		return out
	}
	if _, _, err := item.ParseID(*it.PreviousItem); err == nil {
		if runs, err := store.ReadRuns(*it.PreviousItem); err == nil {
			for i := len(runs) - 1; i >= 0; i-- {
				if p, ok := reportPath(store, *it.PreviousItem, runs[i].Number); ok {
					out = append(out, p)
					break
				}
			}
		}
	}
	return out
}

func reportPath(store *item.Store, id string, n int) (string, bool) {
	if ok, err := store.HasRunFile(id, n, item.ReportFile); err != nil || !ok {
		return "", false
	}
	dir, err := store.RunPath(id, n)
	if err != nil {
		return "", false
	}
	return dir + string(os.PathSeparator) + item.ReportFile, true
}

// Adopt tracks a persisted running run found on startup whose process was
// verified (or which already wrote its exit code). No process is started.
func (r *Runner) Adopt(itemID, source string, run item.Run) {
	r.runs[itemID] = &tracked{itemID: itemID, source: source, run: run.Clone(), since: r.now()}
	r.logger(r.runs[itemID]).Info("run re-attached", "pid", run.PID)
}

// Recover classifies a persisted running run on startup. A run that wrote
// its exit code, or whose PID is alive and verifiably belongs to its
// session, is adopted (adopted is true). Otherwise the run is returned
// with outcome interrupted, a finish time and an explanatory error; its PID
// is never signalled. The interrupted metadata is not persisted here.
func (r *Runner) Recover(itemID, source string, run item.Run) (adopted bool, interrupted item.Run) {
	if ok, _ := r.opts.Store.HasRunFile(itemID, run.Number, item.ExitCodeFile); ok {
		r.Adopt(itemID, source, run)
		return true, item.Run{}
	}
	var reason string
	switch {
	case run.PID <= 0:
		reason = "no process was recorded for the run"
	case run.SessionID == "":
		reason = "the run has no session ID to verify its process"
	case !alive(r.opts.Signal, run.PID):
		reason = fmt.Sprintf("process %d is no longer running", run.PID)
	case !r.opts.Owns(run.PID, run.SessionID):
		reason = fmt.Sprintf("process %d could not be verified as this run", run.PID)
	default:
		r.Adopt(itemID, source, run)
		return true, item.Run{}
	}
	out := run.Clone()
	out.Outcome = item.OutcomeInterrupted
	out.FinishedAt = stamp(r.now())
	out.Error = "interrupted while tower was stopped: " + reason
	return false, out
}

// Cancel terminates the executing run n of an item (lifecycle or user
// cancellation). Its completion is reported by a later Check or HandleWait.
func (r *Runner) Cancel(itemID string, n int) {
	t, ok := r.runs[itemID]
	if !ok || t.run.Number != n || t.completed || t.forced != "" {
		return
	}
	t.forced, t.forcedErr = item.OutcomeCancelled, ErrCancelled
	r.logger(t).Info("cancelling run; terminating its process group")
	r.terminate(t, r.now())
}

// HandleWait processes the Wait result of a locally started run.
func (r *Runner) HandleWait(w WaitResult) []Completion {
	t, ok := r.runs[w.itemID]
	if !ok || !t.local || t.run.Number != w.run || t.waitDone {
		return nil
	}
	t.waitDone, t.waitState = true, w.state
	return r.step(t, false)
}

// Check performs the periodic check of all tracked runs: completion
// evidence, timeouts, TERM/KILL escalation and group cleanup.
func (r *Runner) Check() []Completion {
	var out []Completion
	for _, id := range r.TrackedIDs() {
		out = append(out, r.step(r.runs[id], true)...)
	}
	return out
}

// step advances one tracked run.
func (r *Runner) step(t *tracked, periodic bool) []Completion {
	now := r.now()
	if periodic && t.completed {
		t.checks++
	}
	ended := t.completed || r.ended(t)
	if !ended && t.forced == "" && r.expired(t, now) {
		t.forced = item.OutcomeFailed
		t.forcedErr = "timeout after " + FormatDuration(r.opts.Timeout)
		r.logger(t).Info("run timed out; terminating its process group", "timeout", FormatDuration(r.opts.Timeout))
	}
	switch {
	case t.abandoned:
	case ended:
		r.cleanup(t, now)
	case t.forced != "":
		r.terminate(t, now)
	}

	var out []Completion
	if !t.completed && (ended || t.abandoned) {
		c, err := r.finalize(t, now)
		if err != nil {
			r.logger(t).Error("persist run completion; retrying", "error", err)
			return nil
		}
		out = append(out, c)
	}
	if t.completed && (t.abandoned || t.clean) {
		delete(r.runs, t.itemID)
		r.logger(t).Debug("run slot released")
	}
	return out
}

// ended reports completion evidence: the exit_code file, the Wait result of
// a locally started run, or the death of a re-attached run's PID.
func (r *Runner) ended(t *tracked) bool {
	if ok, _ := r.opts.Store.HasRunFile(t.itemID, t.run.Number, item.ExitCodeFile); ok {
		return true
	}
	if t.local {
		return t.waitDone
	}
	return !alive(r.opts.Signal, t.run.PID)
}

func (r *Runner) expired(t *tracked, now time.Time) bool {
	start := t.since
	if t.run.StartedAt != nil {
		start = *t.run.StartedAt
	}
	return r.opts.Timeout > 0 && now.Sub(start) >= r.opts.Timeout
}

// signal sends sig to the run's process group. Re-attached runs are
// verified afresh before every signal; on failure the run is abandoned
// without signalling. It reports whether the signal was sent.
func (r *Runner) signal(t *tracked, sig syscall.Signal) bool {
	if t.abandoned {
		return false
	}
	if !t.local && !r.opts.Owns(t.run.PID, t.run.SessionID) {
		t.abandoned = true
		r.logger(t).Info("skipping process group signal: the re-attached run's ownership could not be verified; releasing its slot",
			"pid", t.run.PID, "signal", sig.String())
		return false
	}
	if err := r.opts.Signal(-t.run.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		r.logger(t).Warn("signal process group", "pid", t.run.PID, "signal", sig.String(), "error", err)
	}
	return true
}

// terminate sends TERM to the group of an executing run, then KILL after
// the grace period while members remain.
func (r *Runner) terminate(t *tracked, now time.Time) {
	if t.abandoned || t.killed {
		return
	}
	if t.termAt.IsZero() {
		if r.signal(t, syscall.SIGTERM) {
			t.termAt = now
		}
		return
	}
	if now.Sub(t.termAt) >= r.opts.Grace && alive(r.opts.Signal, -t.run.PID) {
		if r.signal(t, syscall.SIGKILL) {
			t.killed = true
			r.logger(t).Info("process group did not exit after TERM; sent KILL", "pid", t.run.PID)
		}
	}
}

// cleanup terminates remaining members of an ended run's process group and
// sets clean once the group is empty.
func (r *Runner) cleanup(t *tracked, now time.Time) {
	if t.clean || t.abandoned {
		return
	}
	// The wrapper exits right after publishing exit_code; give it until the
	// next periodic check before treating it as a leftover.
	wrapperGone := t.waitDone
	if !t.local {
		wrapperGone = !alive(r.opts.Signal, t.run.PID)
	}
	if !wrapperGone && t.checks < 1 && t.termAt.IsZero() {
		return
	}
	if !alive(r.opts.Signal, -t.run.PID) {
		if t.local && !t.waitDone {
			return // not yet reaped
		}
		t.clean = true
		if t.cleaning {
			r.logger(t).Info("process group cleanup complete")
		}
		return
	}
	if t.termAt.IsZero() {
		if r.signal(t, syscall.SIGTERM) {
			t.termAt, t.cleaning = now, true
			r.logger(t).Info("processes remain in the run's group after completion; sent TERM", "pid", t.run.PID)
		}
		return
	}
	if !t.killed && now.Sub(t.termAt) >= r.opts.Grace {
		if r.signal(t, syscall.SIGKILL) {
			t.killed, t.cleaning = true, true
			r.logger(t).Info("processes remain in the run's group after TERM; sent KILL", "pid", t.run.PID)
		}
	}
}

// finalize determines the outcome and persists the finished metadata.
func (r *Runner) finalize(t *tracked, now time.Time) (Completion, error) {
	run := t.run.Clone()
	run.FinishedAt = stamp(now)
	run.ExitCode = nil
	run.Error = ""

	code, exitErr := r.readExit(t)
	var waitErr string
	if code == nil && exitErr == nil && t.local && t.waitState != nil {
		if t.waitState.Exited() {
			c := t.waitState.ExitCode()
			code = &c
		} else {
			waitErr = "the run's process ended abnormally: " + t.waitState.String()
		}
	}
	run.ExitCode = code

	switch {
	case t.forced != "":
		run.Outcome, run.Error = t.forced, t.forcedErr
	case code != nil && *code != 0:
		run.Outcome, run.Error = item.OutcomeFailed, fmt.Sprintf("copilot exited with status %d", *code)
	case waitErr != "":
		run.Outcome, run.Error = item.OutcomeFailed, waitErr
	case exitErr != nil:
		run.Outcome, run.Error = item.OutcomeFailed, exitErr.Error()
	default:
		run.Outcome, run.Error = r.artifacts(t)
	}

	if err := r.opts.Store.WriteRun(t.itemID, run); err != nil {
		if _, perr := r.opts.Store.ItemPath(t.itemID); !errors.Is(perr, fs.ErrNotExist) {
			return Completion{}, err
		}
		r.logger(t).Warn("the run's item no longer exists; its completion is not persisted")
	}
	t.run = run.Clone()
	t.completed = true
	attrs := []any{"outcome", string(run.Outcome)}
	if run.ExitCode != nil {
		attrs = append(attrs, "exit_code", *run.ExitCode)
	}
	if run.Error != "" {
		attrs = append(attrs, "error", run.Error)
	}
	r.logger(t).Info("run finished", attrs...)
	return Completion{ItemID: t.itemID, Source: t.source, Run: run.Clone()}, nil
}

// readExit reads the exit_code file: nil without error if it is absent.
func (r *Runner) readExit(t *tracked) (*int, error) {
	data, err := r.opts.Store.ReadRunFile(t.itemID, t.run.Number, item.ExitCodeFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("exit_code is unreadable")
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || code < 0 {
		return nil, errors.New("exit_code is malformed")
	}
	return &code, nil
}

// artifacts evaluates result.json and report.md.
func (r *Runner) artifacts(t *tracked) (item.Outcome, string) {
	store, id, n := r.opts.Store, t.itemID, t.run.Number
	data, err := store.ReadRunFile(id, n, item.ResultFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return item.OutcomeFailed, "result.json is missing"
	case err != nil:
		return item.OutcomeFailed, "result.json is unreadable"
	}
	res, err := result.Parse(data)
	if err != nil {
		return item.OutcomeFailed, "invalid result.json: " + err.Error()
	}
	if _, err := store.ReadRunFile(id, n, item.ReportFile); errors.Is(err, fs.ErrNotExist) {
		return item.OutcomeFailed, "report.md is missing"
	} else if err != nil {
		return item.OutcomeFailed, "report.md is unreadable"
	}
	if res.Status == result.StatusBlocked {
		return item.OutcomeBlocked, ""
	}
	return item.OutcomeReady, ""
}
