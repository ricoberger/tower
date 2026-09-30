package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/ghostty"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/loginenv"
	"github.com/ricoberger/tower/internal/notify"
	"github.com/ricoberger/tower/internal/ui"
)

// resumeTarget identifies the run a resume hands over. The TUI sets run and
// session to the run the user saw (and confirmed); the CLI resumes whatever
// run is current.
type resumeTarget struct {
	id        string
	run       int
	session   string
	placement string
	// refuseExecuting rejects a run that is still executing (CLI).
	refuseExecuting bool
}

// resumer performs the Ghostty handoff shared by the TUI and `tower resume`.
type resumer struct {
	cfg   *config.Config
	store *item.Store
	now   func() time.Time
	// lookPath resolves runs.command and ghostty.command.
	lookPath config.LookPath
	// env is the Ghostty helper's environment (nil inherits).
	env []string
	// processOwns verifies that a recorded PID still runs the session.
	processOwns func(pid int, sessionID string) bool
}

// resume validates the latest run of the item, launches Ghostty and records
// the resumed-session action. Nothing is recorded when a check or the
// launch fails, and no item lock is held while Ghostty starts.
func (r *resumer) resume(ctx context.Context, t resumeTarget) (int, error) {
	if !ghostty.ValidPlacement(t.placement) {
		return 0, errors.New("placement must be one of split, tab, window")
	}
	l, err := r.store.Load(t.id)
	if err != nil {
		return 0, fmt.Errorf("load item %s: %w", t.id, err)
	}
	it := l.Item
	if it.Runs.Current == 0 {
		return 0, fmt.Errorf("item %s has no preparation run to resume", t.id)
	}
	var run item.Run
	found := false
	for _, x := range l.Runs {
		if x.Number == it.Runs.Current {
			run, found = x, true
		}
	}
	if !found {
		return 0, fmt.Errorf("item %s: the metadata of its latest run %d is missing", t.id, it.Runs.Current)
	}
	if t.run != 0 && (run.Number != t.run || run.SessionID != t.session) {
		return 0, fmt.Errorf("the latest run of item %s changed; resume cancelled", t.id)
	}
	if err := ui.Resumable(run); err != nil {
		return 0, fmt.Errorf("item %s: %w", t.id, err)
	}
	if t.refuseExecuting && run.Outcome == item.OutcomeRunning {
		done, err := r.store.HasRunFile(t.id, run.Number, item.ExitCodeFile)
		if err != nil {
			return 0, fmt.Errorf("item %s run %d: %w", t.id, run.Number, err)
		}
		if !done && r.processOwns(run.PID, run.SessionID) {
			return 0, fmt.Errorf("run %d of item %s is still executing; wait for it to finish, or resume it from the tower TUI, which asks for confirmation", run.Number, t.id)
		}
	}
	dir, err := r.store.ItemPath(t.id)
	if err != nil {
		return 0, fmt.Errorf("item %s: %w", t.id, err)
	}
	lookPath := r.lookPath
	command, err := lookPath(r.cfg.Runs.Command)
	if err != nil {
		return 0, errors.New("runs.command: executable cannot be resolved (not found on PATH or not executable)")
	}
	helper, err := lookPath(r.cfg.Ghostty.Command)
	if err != nil {
		return 0, errors.New("ghostty.command: executable cannot be resolved (not found on PATH or not executable)")
	}
	launcher := &ghostty.Launcher{Env: r.env}
	if err := launcher.Launch(ctx, ghostty.Request{
		Helper:     helper,
		Placement:  t.placement,
		Direction:  r.cfg.Ghostty.Direction,
		WorkingDir: dir,
		Title:      it.Title,
		Command:    command,
		SessionID:  run.SessionID,
		ResumeArgs: r.cfg.Runs.ResumeArgs,
	}); err != nil {
		return 0, err
	}
	if _, err := r.store.AppendAction(t.id, item.ActionResumedSession, run.Number, r.now().UTC().Truncate(time.Second)); err != nil {
		return 0, fmt.Errorf("the Ghostty surface was opened, but recording the resume failed: %w", err)
	}
	return run.Number, nil
}

// parseResumeArgs parses `resume <item-id> [--placement split|tab|window]`;
// flags may precede or follow the item ID.
func parseResumeArgs(g *globalFlags, args []string, stderr io.Writer) (id, placement string, err error) {
	fs := newFlagSet("tower resume", stderr)
	g.register(fs)
	fs.StringVar(&placement, "placement", "", "Ghostty placement: split, tab or window (default ghostty.placement)")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if fs.NArg() == 0 {
		return "", "", errors.New("resume expects an item ID")
	}
	id = fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", "", err
	}
	if fs.NArg() > 0 {
		return "", "", errors.New("resume expects exactly one item ID")
	}
	if _, _, err := item.ParseID(id); err != nil {
		return "", "", fmt.Errorf("invalid item ID: %w", err)
	}
	if placement != "" && !ghostty.ValidPlacement(placement) {
		return "", "", errors.New("--placement must be one of split, tab, window")
	}
	return id, placement, nil
}

// cmdResume runs `tower resume`. It never starts the engine or takes the
// instance lock. Because notification clicks start it without a terminal
// and with a minimal PATH, it recovers the interactive login-shell
// environment before loading the configuration and reports failures as a
// desktop notification in addition to stderr.
func cmdResume(ctx context.Context, g *globalFlags, args []string, stdout, stderr io.Writer, e env) int {
	callerPATH, _ := e.lookup("PATH")
	notifyLookPath := func(f string) (string, error) { return loginenv.LookPath(f, callerPATH) }
	fail := func(msg string) int {
		_, _ = fmt.Fprintln(stderr, "error:", msg)
		e.resumeError(ctx, notifyLookPath, msg)
		return 1
	}

	id, placement, err := parseResumeArgs(g, args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return fail("resume: " + err.Error())
	}
	if _, err := parseLogLevel(g.logLevel); err != nil {
		return fail(err.Error())
	}

	captured, err := e.captureEnv(ctx)
	if err != nil {
		return fail("resume " + id + ": recover the login shell environment: " + err.Error())
	}
	vars := loginenv.Merge(e.environ(), captured)
	loginPATH := vars["PATH"]
	lookPath := func(f string) (string, error) { return loginenv.LookPath(f, loginPATH) }
	notifyLookPath = func(f string) (string, error) {
		if p, err := lookPath(f); err == nil {
			return p, nil
		}
		return loginenv.LookPath(f, callerPATH)
	}
	lookup := func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}

	cwd, err := e.getwd()
	if err != nil {
		return fail("resume " + id + ": determine working directory: " + err.Error())
	}
	path, err := config.ResolvePath(g.config, lookup, cwd)
	if err != nil {
		return fail("resume " + id + ": " + err.Error())
	}
	cfg, report, err := config.Load(path, lookup)
	if err != nil {
		return fail("resume " + id + ": " + err.Error())
	}
	for _, w := range report.Warnings {
		_, _ = fmt.Fprintln(stderr, "warning:", w)
	}
	if !report.OK() {
		for _, msg := range report.Errors[1:] {
			_, _ = fmt.Fprintln(stderr, "error:", msg)
		}
		return fail(fmt.Sprintf("resume %s: invalid configuration %s: %s", id, path, report.Errors[0]))
	}
	if placement == "" {
		placement = cfg.Ghostty.Placement
	}

	store, err := item.Open(cfg.StateDir)
	if err != nil {
		return fail("resume " + id + ": " + err.Error())
	}
	defer func() { _ = store.Close() }()
	r := &resumer{
		cfg:         cfg,
		store:       store,
		now:         e.now,
		lookPath:    lookPath,
		env:         loginenv.List(vars),
		processOwns: e.processOwns,
	}
	n, err := r.resume(ctx, resumeTarget{id: id, placement: placement, refuseExecuting: true})
	if err != nil {
		return fail("resume " + id + ": " + err.Error())
	}
	_, _ = fmt.Fprintf(stdout, "resumed run %d of %s in Ghostty (%s)\n", n, id, placement)
	return 0
}

// deliverResumeError shows a resume failure as a desktop notification
// without a click action. Delivery failures are ignored: the error was
// already printed.
func deliverResumeError(ctx context.Context, lookPath config.LookPath, msg string) {
	n := &notify.Notifier{LookPath: lookPath}
	_ = n.DeliverError(ctx, notify.TitleResumeFailed, msg)
}

// captureLoginEnv captures the interactive login-shell environment.
func captureLoginEnv(e env) func(context.Context) (loginenv.Env, error) {
	return func(ctx context.Context) (loginenv.Env, error) {
		opts := loginenv.Options{Lookup: e.lookup, Environ: e.environ}
		shell := loginenv.SelectShell(ctx, opts)
		return loginenv.Capture(ctx, shell, opts)
	}
}

// apiHint turns engine request errors into footer hints.
func apiHint(err error) string {
	var notAllowed *ManualRunNotAllowedError
	switch {
	case errors.Is(err, ErrUnknownItem):
		return "the item no longer exists or cannot be read"
	case errors.As(err, &notAllowed):
		return fmt.Sprintf("cannot prepare now: the item is %s", notAllowed.State)
	case errors.Is(err, ErrRunExecuting):
		return "a preparation run is still executing; wait for it to finish"
	case errors.Is(err, ErrItemDone):
		return "the item is already done"
	case errors.Is(err, ErrEngineStopped):
		return "the engine is not running; restart tower"
	case errors.Is(err, ErrPollInProgress):
		return "a poll is already in progress"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "the request was cancelled"
	}
	return "request failed: " + strings.TrimSpace(err.Error())
}
