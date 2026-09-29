// Command tower is a personal work control center that turns alerts into
// persistent work items.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
)

// Set via -ldflags by `make build`.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// env abstracts the process environment for tests.
type env struct {
	lookup   config.LookupEnv
	getwd    func() (string, error)
	now      func() time.Time
	lookPath config.LookPath
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, env{
		lookup: os.LookupEnv,
		getwd:  os.Getwd,
		now:    time.Now,
	})
	stop()
	os.Exit(code)
}

type globalFlags struct {
	config   string
	logLevel string
	headless bool
}

func (g *globalFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&g.config, "config", g.config, "path to the config file (default $TOWER_CONFIG, then $HOME/.config/tower/config.yaml)")
	fs.StringVar(&g.logLevel, "log-level", g.logLevel, "log level: debug, info, warn, error")
}

const usage = `Usage: tower [--config <file>] [--log-level debug|info|warn|error] [command]

Commands:
  (none)                                     run the engine (headless until the TUI exists)
  config init                                write a commented example config if none exists
  config validate                            validate the config and print warnings/errors
  prune [--older-than <dur>] [--dry-run]     delete done items older than the duration
  version                                    print version, commit and build date
`

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, usage) }
	return fs
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
	g := &globalFlags{logLevel: "info"}
	fs := newFlagSet("tower", stderr)
	g.register(fs)
	// Hidden: keeps headless operation available once the TUI is the default.
	fs.BoolVar(&g.headless, "headless", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 1
	}
	rest := fs.Args()

	if len(rest) > 0 && rest[0] == "version" {
		if len(rest) > 1 {
			_, _ = fmt.Fprintln(stderr, "error: version takes no arguments")
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "tower %s (commit %s, built %s)\n", version, commit, date)
		return 0
	}

	var cmd func() int
	switch {
	case len(rest) == 0:
		cmd = func() int { return cmdEngine(ctx, g, stderr, e) }
	case rest[0] == "config":
		if len(rest) < 2 {
			_, _ = fmt.Fprintln(stderr, "error: expected \"config init\" or \"config validate\"")
			return 1
		}
		sub := newFlagSet("tower config "+rest[1], stderr)
		g.register(sub)
		if err := sub.Parse(rest[2:]); err != nil {
			return flagExit(err)
		}
		if sub.NArg() > 0 {
			_, _ = fmt.Fprintln(stderr, "error: unexpected arguments")
			return 1
		}
		switch rest[1] {
		case "init":
			cmd = func() int { return cmdConfigInit(g, stdout, stderr, e) }
		case "validate":
			cmd = func() int { return cmdConfigValidate(g, stdout, stderr, e) }
		default:
			_, _ = fmt.Fprintf(stderr, "error: unknown config command %q\n", rest[1])
			return 1
		}
	case rest[0] == "prune":
		sub := newFlagSet("tower prune", stderr)
		g.register(sub)
		olderThan := sub.String("older-than", "", "delete done items whose updated_at is older than this duration (default retention.done_after)")
		dryRun := sub.Bool("dry-run", false, "only print what would be deleted")
		if err := sub.Parse(rest[1:]); err != nil {
			return flagExit(err)
		}
		if sub.NArg() > 0 {
			_, _ = fmt.Fprintln(stderr, "error: unexpected arguments")
			return 1
		}
		cmd = func() int { return cmdPrune(g, *olderThan, *dryRun, stdout, stderr, e) }
	default:
		_, _ = fmt.Fprintf(stderr, "error: unknown command %q\n", rest[0])
		_, _ = fmt.Fprint(stderr, usage)
		return 1
	}

	if _, err := parseLogLevel(g.logLevel); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return cmd()
}

func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 1
}

func configPath(g *globalFlags, e env) (string, error) {
	cwd, err := e.getwd()
	if err != nil {
		return "", fmt.Errorf("determine working directory: %w", err)
	}
	return config.ResolvePath(g.config, e.lookup, cwd)
}

// loadConfig loads the config and prints its findings. It returns nil if the
// config cannot be used.
func loadConfig(g *globalFlags, stderr io.Writer, e env) *config.Config {
	path, err := configPath(g, e)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return nil
	}
	cfg, report, err := config.Load(path, e.lookup)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return nil
	}
	for _, w := range report.Warnings {
		_, _ = fmt.Fprintln(stderr, "warning:", w)
	}
	for _, msg := range report.Errors {
		_, _ = fmt.Fprintln(stderr, "error:", msg)
	}
	if !report.OK() {
		_, _ = fmt.Fprintf(stderr, "error: invalid configuration %s\n", path)
		return nil
	}
	return cfg
}

func cmdEngine(ctx context.Context, g *globalFlags, stderr io.Writer, e env) int {
	level, _ := parseLogLevel(g.logLevel)
	cfg := loadConfig(g, stderr, e)
	if cfg == nil {
		return 1
	}
	if err := runEngine(ctx, engineOptions{
		cfg:      cfg,
		level:    level,
		stderr:   stderr,
		now:      e.now,
		lookPath: e.lookPath,
	}); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

func cmdConfigInit(g *globalFlags, stdout, stderr io.Writer, e env) int {
	path, err := configPath(g, e)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := config.WriteExample(path); err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "wrote example configuration to %s\n", path)
	return 0
}

func cmdConfigValidate(g *globalFlags, stdout, stderr io.Writer, e env) int {
	path, err := configPath(g, e)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	cfg, report, err := config.Load(path, e.lookup)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	warnings := report.Warnings
	if report.OK() {
		f := config.CheckExecutables(cfg, e.lookPath)
		for _, w := range []string{f.RunsCommand, f.GhosttyCommand} {
			if w != "" {
				warnings = append(warnings, w)
			}
		}
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintln(stdout, "warning:", w)
	}
	for _, msg := range report.Errors {
		_, _ = fmt.Fprintln(stdout, "error:", msg)
	}
	if !report.OK() {
		_, _ = fmt.Fprintf(stdout, "%s: %d error(s), %d warning(s)\n", path, len(report.Errors), len(warnings))
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s: valid (%d warning(s))\n", path, len(warnings))
	return 0
}

func cmdPrune(g *globalFlags, olderThan string, dryRun bool, stdout, stderr io.Writer, e env) int {
	cfg := loadConfig(g, stderr, e)
	if cfg == nil {
		return 1
	}
	d := cfg.Retention.DoneAfter
	if olderThan != "" {
		v, err := time.ParseDuration(olderThan)
		if err != nil || v <= 0 {
			_, _ = fmt.Fprintln(stderr, "error: --older-than must be a positive Go duration (e.g. 720h)")
			return 1
		}
		d = v
	}
	store, err := item.Open(cfg.StateDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer func() { _ = store.Close() }()
	cutoff := e.now().UTC().Add(-d)

	if dryRun {
		cands, err := store.PruneCandidates(cutoff)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		for _, c := range cands {
			_, _ = fmt.Fprintf(stdout, "would delete %s (updated_at %s)\n", c.ID, c.UpdatedAt.Format(time.RFC3339))
		}
		_, _ = fmt.Fprintf(stdout, "%d done item(s) older than %s would be deleted\n", len(cands), d)
		return 0
	}

	lock, err := store.TryLockInstance()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error: refusing to prune:", err)
		return 1
	}
	defer func() { _ = lock.Release() }()

	cands, err := store.PruneCandidates(cutoff)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	deleted, failed := 0, false
	for _, c := range cands {
		ok, err := store.PruneItem(c.ID, cutoff)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "error: prune %s: %v\n", c.ID, err)
			failed = true
			continue
		}
		if ok {
			deleted++
			_, _ = fmt.Fprintf(stdout, "deleted %s (updated_at %s)\n", c.ID, c.UpdatedAt.Format(time.RFC3339))
		}
	}
	_, _ = fmt.Fprintf(stdout, "%d done item(s) older than %s deleted\n", deleted, d)
	if failed {
		return 1
	}
	return 0
}
