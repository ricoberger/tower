package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// wrapperScript is the product §7.2 wrapper. The command and its arguments
// are passed as positional parameters ("$0" "$@") and RUN through the
// environment, so none of them is ever interpolated into shell source.
const wrapperScript = `umask 077; "$0" "$@" >"$RUN/output.jsonl" 2>"$RUN/stderr.log"; echo $? >"$RUN/exit_code.tmp" && mv "$RUN/exit_code.tmp" "$RUN/exit_code"`

// ownershipTimeout bounds one ps invocation of the ownership check.
const ownershipTimeout = 5 * time.Second

// newSessionID returns a random UUIDv4.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// commandArgs returns the arguments of the wrapper shell after "-c script":
// the command as $0 followed by the fixed preparation flags, the optional
// model and the configured arguments.
func commandArgs(command, prompt, sessionID, model string, args []string) []string {
	out := []string{"-c", wrapperScript, command, "-p", prompt, "--session-id", sessionID, "--output-format", "json", "--no-ask-user"}
	if model != "" {
		out = append(out, "--model", model)
	}
	return append(out, args...)
}

// childEnv returns env without inherited RUN entries plus RUN=runDir.
func childEnv(env []string, runDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "RUN=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "RUN="+runDir)
}

// ProcessOwns reports whether the command line of pid, as shown by
// `ps -o command= -p <pid>`, contains the nonempty session ID. Any failure
// is reported as not owned.
func ProcessOwns(pid int, sessionID string) bool {
	if pid <= 0 || sessionID == "" {
		return false
	}
	ps := "/bin/ps"
	if _, err := os.Stat(ps); err != nil {
		ps = "ps"
	}
	ctx, cancel := context.WithTimeout(context.Background(), ownershipTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, ps, "-o", "command=", "-p", strconv.Itoa(pid)).Output() // #nosec G204 -- fixed executable, numeric argument
	return err == nil && strings.Contains(string(out), sessionID)
}

// alive reports whether a process (pid > 0) or process group (pid < 0)
// exists, using signal 0. EPERM means it exists but belongs to someone else.
func alive(signal func(int, syscall.Signal) error, pid int) bool {
	if pid == 0 {
		return false
	}
	err := signal(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// FormatDuration renders d without zero minute/second suffixes (20m, 1h,
// 1h30m, 90s → 1m30s).
func FormatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
