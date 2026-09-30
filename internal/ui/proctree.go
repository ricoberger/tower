package ui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// treePollInterval is how often a stopping editor invocation is checked.
	treePollInterval = 25 * time.Millisecond
	// psTimeout bounds one process table read.
	psTimeout = 2 * time.Second
)

// procInfo is one row of the process table.
type procInfo struct {
	ppid, pgid int
	zombie     bool
}

// processTable reads all processes with their parent, process group and
// whether they already exited (zombies).
func processTable() (map[int]procInfo, error) {
	ps := "/bin/ps"
	if _, err := os.Stat(ps); err != nil {
		ps = "ps"
	}
	ctx, cancel := context.WithTimeout(context.Background(), psTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, ps, "-A", "-o", "pid=,ppid=,pgid=,stat=").Output() // #nosec G204 -- fixed executable and arguments
	if err != nil {
		return nil, err
	}
	table := map[int]procInfo{}
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		table[pid] = procInfo{ppid: ppid, pgid: pgid, zombie: strings.HasPrefix(f[3], "Z")}
	}
	return table, nil
}

// processTree tracks the descendants of a process while it is stopped.
// Descendants are remembered with their process group when they are first
// seen, so they are still found after their parent exited and they were
// reparented, and a reused PID in another group is never signalled.
type processTree struct {
	root  int
	known map[int]int // pid -> process group
	table map[int]procInfo
}

// refresh rereads the process table and adds every new descendant of the
// root (while it has not been reaped, so its PID cannot have been reused) or
// of an already known process. It returns the newly found PIDs.
func (t *processTree) refresh(rootRunning bool) []int {
	table, err := processTable()
	if err != nil {
		return nil
	}
	t.table = table
	children := map[int][]int{}
	for pid, p := range table {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	var queue []int
	if rootRunning {
		queue = append(queue, t.root)
	}
	for pid := range t.known {
		if t.alive(pid) {
			queue = append(queue, pid)
		}
	}
	var added []int
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if _, ok := t.known[c]; ok || c == t.root {
				continue
			}
			t.known[c] = table[c].pgid
			added = append(added, c)
			queue = append(queue, c)
		}
	}
	return added
}

// alive reports whether a known descendant still runs in its original
// process group, according to the last table read.
func (t *processTree) alive(pid int) bool {
	p, ok := t.table[pid]
	return ok && !p.zombie && p.pgid == t.known[pid]
}

func (t *processTree) signal(pids []int, sig syscall.Signal) {
	for _, pid := range pids {
		if t.alive(pid) {
			_ = syscall.Kill(pid, sig)
		}
	}
}

func (t *processTree) living() []int {
	var out []int
	for pid := range t.known {
		if t.alive(pid) {
			out = append(out, pid)
		}
	}
	return out
}

// stopProcessTree terminates proc and all its descendants: SIGTERM first,
// SIGKILL for whatever still runs after delay. It returns once everything
// ended or was killed, so a caller that waits for it leaves no editor
// behind. Only processes that were found as descendants are signalled; the
// process group is left alone because it is shared with the caller. The
// returned error is the one of signalling proc.
func stopProcessTree(proc *os.Process, delay time.Duration) error {
	t := &processTree{root: proc.Pid, known: map[int]int{}}
	t.refresh(true)
	err := proc.Signal(syscall.SIGTERM)
	t.signal(t.living(), syscall.SIGTERM)
	deadline := time.Now().Add(delay)
	for time.Now().Before(deadline) {
		time.Sleep(treePollInterval)
		rootDone := errors.Is(proc.Signal(syscall.Signal(0)), os.ErrProcessDone)
		t.signal(t.refresh(!rootDone), syscall.SIGTERM)
		if rootDone && len(t.living()) == 0 {
			return err
		}
	}
	_ = proc.Kill()
	t.refresh(false)
	t.signal(t.living(), syscall.SIGKILL)
	return err
}
