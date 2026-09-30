// Package loginenv recovers the user's interactive login-shell environment
// for processes started without one (for example from a notification
// click, which inherits launchd's minimal environment). The captured
// environment is only held in memory; it is never logged or written to
// disk.
package loginenv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ricoberger/tower/internal/bounded"
)

const (
	// Timeout bounds the interactive login shell capture.
	Timeout = 10 * time.Second
	// LookupTimeout bounds the password database query.
	LookupTimeout = 5 * time.Second
	// WaitDelay bounds waiting for the shell and its output pipe after it
	// exited or was killed.
	WaitDelay = 500 * time.Millisecond
	// FallbackShell is used when neither $SHELL nor the password database
	// provide a usable shell.
	FallbackShell = "/bin/zsh"
	// maxOutput bounds the captured stdout (startup noise plus payload).
	maxOutput = 4 << 20
	dscl      = "/usr/bin/dscl"
)

// Options configures shell selection and capture. Zero values select the
// production behavior.
type Options struct {
	// Lookup reads the caller's environment (os.LookupEnv).
	Lookup func(string) (string, bool)
	// Environ returns the caller's environment (os.Environ).
	Environ func() []string
	// UserShell returns the login shell of the current user from the
	// password database.
	UserShell func(ctx context.Context) (string, error)
	// Timeout, LookupTimeout and WaitDelay override the defaults.
	Timeout       time.Duration
	LookupTimeout time.Duration
	WaitDelay     time.Duration
}

func (o *Options) defaults() {
	if o.Lookup == nil {
		o.Lookup = os.LookupEnv
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Timeout <= 0 {
		o.Timeout = Timeout
	}
	if o.LookupTimeout <= 0 {
		o.LookupTimeout = LookupTimeout
	}
	if o.WaitDelay <= 0 {
		o.WaitDelay = WaitDelay
	}
	if o.UserShell == nil {
		o.UserShell = func(ctx context.Context) (string, error) {
			return passwdShell(ctx, o.LookupTimeout, o.WaitDelay)
		}
	}
}

// usableShell reports whether path is an absolute executable regular file.
func usableShell(path string) bool {
	return filepath.IsAbs(path) && executable(path)
}

// passwdShell queries the current user's login shell with dscl.
func passwdShell(ctx context.Context, timeout, waitDelay time.Duration) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("determine the current user: %w", err)
	}
	var out limitedBuffer
	out.max = 64 << 10
	// The user name is data: it is passed as part of a single argument.
	cmd, cctx, cancel := bounded.Command(ctx, bounded.Options{Timeout: timeout, WaitDelay: waitDelay}, dscl, ".", "-read", "/Users/"+u.Username, "UserShell")
	defer cancel()
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := bounded.Run(cctx, cmd); err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "UserShell:"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", errors.New("no UserShell entry")
}

// SelectShell returns the shell to capture from: a nonempty $SHELL, else
// the password database's login shell when usable, else FallbackShell.
func SelectShell(ctx context.Context, opts Options) string {
	opts.defaults()
	if s, ok := opts.Lookup("SHELL"); ok && s != "" {
		return s
	}
	if s, err := opts.UserShell(ctx); err == nil && usableShell(s) {
		return s
	}
	return FallbackShell
}

// Env is a captured environment.
type Env struct {
	// Vars maps variable names to values.
	Vars map[string]string
}

// limitedBuffer keeps at most max bytes and reports overflow.
type limitedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func newMarker() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "tower-env-" + hex.EncodeToString(b[:]), nil
}

// Capture runs `<shell> -l -i -c` with a script that prints the NUL
// separated `env -0` output between two unique markers. Stdin is /dev/null,
// stderr is discarded and any startup output around the markers is ignored.
func Capture(ctx context.Context, shell string, opts Options) (Env, error) {
	opts.defaults()
	marker, err := newMarker()
	if err != nil {
		return Env{}, fmt.Errorf("create capture marker: %w", err)
	}
	begin, end := marker+":begin\x00", marker+":end\x00"
	script := "printf '%s:begin\\0' " + marker + "; env -0; printf '%s:end\\0' " + marker

	var out limitedBuffer
	out.max = maxOutput
	cmd, cctx, cancel := bounded.Command(ctx, bounded.Options{Timeout: opts.Timeout, WaitDelay: opts.WaitDelay}, shell, "-l", "-i", "-c", script)
	defer cancel()
	cmd.Env = opts.Environ()
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	runErr := bounded.Run(cctx, cmd)
	if cctx.Err() != nil {
		// Timeout or cancellation: the group was killed; never use a
		// partial capture.
		if runErr == nil {
			runErr = cctx.Err()
		}
		return Env{}, fmt.Errorf("capture the login shell environment (%s -l -i): %w", filepath.Base(shell), runErr)
	}
	vars, err := parse(out.Bytes(), begin, end)
	if err != nil {
		if runErr != nil {
			return Env{}, fmt.Errorf("capture the login shell environment (%s -l -i): %w", filepath.Base(shell), runErr)
		}
		if out.overflow {
			return Env{}, fmt.Errorf("capture the login shell environment (%s -l -i): output exceeds %d bytes", filepath.Base(shell), maxOutput)
		}
		return Env{}, fmt.Errorf("capture the login shell environment (%s -l -i): %w", filepath.Base(shell), err)
	}
	return Env{Vars: vars}, nil
}

// parse extracts the NUL separated records between the markers. Output
// outside the markers is ignored; its content never appears in errors.
func parse(out []byte, begin, end string) (map[string]string, error) {
	i := bytes.Index(out, []byte(begin))
	if i < 0 {
		return nil, errors.New("malformed output: environment start marker not found")
	}
	payload := out[i+len(begin):]
	j := bytes.Index(payload, []byte(end))
	if j < 0 {
		return nil, errors.New("malformed output: environment end marker not found")
	}
	payload = payload[:j]
	if len(payload) > 0 && payload[len(payload)-1] != 0 {
		return nil, errors.New("malformed output: environment is not NUL terminated")
	}
	vars := map[string]string{}
	for rec := range bytes.SplitSeq(payload, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		k, v, ok := bytes.Cut(rec, []byte("="))
		if !ok || len(k) == 0 {
			return nil, errors.New("malformed output: invalid environment record")
		}
		vars[string(k)] = string(v)
	}
	if len(vars) == 0 {
		return nil, errors.New("malformed output: empty environment")
	}
	return vars, nil
}

// Merge combines the caller's environment with a captured one: variables
// missing from the caller are taken from the capture, caller values
// (including explicitly empty ones) win, except PATH, which is always the
// captured login-shell PATH when the capture has one.
func Merge(caller []string, captured Env) map[string]string {
	out := map[string]string{}
	for k, v := range captured.Vars {
		out[k] = v
	}
	for _, kv := range caller {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if k == "PATH" {
			if _, has := captured.Vars["PATH"]; has {
				continue
			}
		}
		out[k] = v
	}
	return out
}

// List returns vars as sorted KEY=VALUE entries.
func List(vars map[string]string) []string {
	out := make([]string, 0, len(vars))
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		out = append(out, k+"="+vars[k])
	}
	return out
}

// LookPath resolves file like exec.LookPath, but searches pathList (a PATH
// value) instead of the process PATH. Names containing a slash are checked
// directly.
func LookPath(file, pathList string) (string, error) {
	if strings.Contains(file, "/") {
		if executable(file) {
			return file, nil
		}
		return "", fmt.Errorf("%s: %w", filepath.Base(file), exec.ErrNotFound)
	}
	for dir := range strings.SplitSeq(pathList, string(filepath.ListSeparator)) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, file)
		if executable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", file, exec.ErrNotFound)
}

func executable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}
