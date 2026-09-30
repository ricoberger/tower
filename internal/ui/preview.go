package ui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/result"
)

const (
	// ProgressTailBytes is the maximum number of bytes read from the end
	// of output.jsonl for the progress line. The file is only read again
	// when its size or modification time changed.
	ProgressTailBytes = 256 << 10
	// StderrLines is the number of stderr.log lines shown for a failed run.
	StderrLines = 20
	// StderrLineBytes bounds how much of each shown stderr.log line is
	// read; longer lines are shown truncated with a marker.
	StderrLineBytes = 4 << 10
	// scanChunk is the block size of the backward line scan.
	scanChunk = 64 << 10
)

// RunFiles reads run artifacts through the store's safe accessors.
type RunFiles interface {
	ReadRunFile(id string, n int, name string) ([]byte, error)
	StatRunFile(id string, n int, name string) (fs.FileInfo, error)
	ReadRunFileTail(id string, n int, name string, limit int64) ([]byte, int64, fs.FileInfo, error)
	OpenRunFile(id string, n int, name string) (*os.File, error)
}

// previewKey identifies the artifacts a preview was loaded for.
type previewKey struct {
	id      string
	run     int
	outcome item.Outcome
}

// artifacts are the loaded files of a finished run.
type artifacts struct {
	key previewKey
	// result and report of a ready/blocked run.
	result    *result.Result
	resultErr error
	report    string
	reportErr error
	// stderr holds the last StderrLines lines of stderr.log of a
	// failed/interrupted/cancelled run.
	stderr    []string
	stderrErr error
}

// progress is the cached progress line of an executing run.
type progress struct {
	id  string
	run int
	// known is true once the file was read; size and mod identify the
	// read version.
	known   bool
	size    int64
	mod     time.Time
	message string
	err     error
}

// loadArtifacts reads the artifacts a finished run's preview needs.
func loadArtifacts(files RunFiles, key previewKey) artifacts {
	a := artifacts{key: key}
	switch key.outcome {
	case item.OutcomeReady, item.OutcomeBlocked:
		if data, err := files.ReadRunFile(key.id, key.run, item.ResultFile); err != nil {
			a.resultErr = err
		} else if r, err := result.Parse(data); err != nil {
			a.resultErr = err
		} else {
			a.result = r
		}
		if data, err := files.ReadRunFile(key.id, key.run, item.ReportFile); err != nil {
			a.reportErr = err
		} else {
			a.report = string(data)
		}
	case item.OutcomeFailed, item.OutcomeInterrupted, item.OutcomeCancelled:
		a.stderr, a.stderrErr = stderrTail(files, key.id, key.run)
	}
	return a
}

// stderrTail returns the last StderrLines lines of stderr.log, including
// blank lines. The file is scanned backwards, so the lines are exact however
// long they are, while at most StderrLineBytes of each line are kept.
func stderrTail(files RunFiles, id string, run int) ([]string, error) {
	f, err := files.OpenRunFile(id, run, item.StderrFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return lastLines(f, fi.Size(), StderrLines, StderrLineBytes)
}

// lastLines returns the last n lines of the first size bytes of r. A final
// newline terminates the last line and does not start another one. Lines
// longer than maxLine bytes are truncated and marked.
func lastLines(r io.ReaderAt, size int64, n, maxLine int) ([]string, error) {
	if size <= 0 || n <= 0 {
		return []string{}, nil
	}
	end := size
	last := make([]byte, 1)
	if _, err := r.ReadAt(last, size-1); err != nil {
		return nil, err
	}
	if last[0] == '\n' {
		end--
	}
	// starts collects line start offsets from the last line backwards.
	starts := []int64{}
	buf := make([]byte, scanChunk)
	pos := end
scan:
	for pos > 0 {
		lo := max(pos-scanChunk, 0)
		chunk := buf[:pos-lo]
		if _, err := r.ReadAt(chunk, lo); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				starts = append(starts, lo+int64(i)+1)
				if len(starts) == n {
					break scan
				}
			}
		}
		pos = lo
	}
	if len(starts) < n {
		starts = append(starts, 0)
	}
	lines := make([]string, len(starts))
	stop := end
	for i, start := range starts {
		length := stop - start
		keep := min(length, int64(maxLine))
		b := make([]byte, keep)
		if _, err := r.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		line := string(b)
		if length > keep {
			line += fmt.Sprintf(" … (%d more bytes)", length-keep)
		}
		lines[len(starts)-1-i] = line
		stop = start - 1
	}
	return lines, nil
}

// loadProgress refreshes the progress line of an executing run. The file is
// only read when its size or modification time differs from prev; a newer
// tail without a usable message keeps the previous message of the same run.
func loadProgress(files RunFiles, prev progress, id string, run int) progress {
	p := prev
	if p.id != id || p.run != run {
		p = progress{id: id, run: run}
	}
	p.err = nil
	fi, err := files.StatRunFile(id, run, item.OutputFile)
	if errors.Is(err, fs.ErrNotExist) {
		return p
	}
	if err != nil {
		p.err = err
		return p
	}
	if p.known && fi.Size() == p.size && fi.ModTime().Equal(p.mod) {
		return p
	}
	data, off, fi, err := files.ReadRunFileTail(id, run, item.OutputFile, ProgressTailBytes)
	if err != nil {
		p.err = err
		return p
	}
	p.known, p.size, p.mod = true, fi.Size(), fi.ModTime()
	if msg, ok := lastAssistantMessage(data, off > 0); ok {
		p.message = msg
	}
	return p
}

// lastAssistantMessage returns the first line of the content of the last
// complete assistant.message event with nonempty content. The incomplete
// last line and, when cut is true, the first line (cut at the tail
// boundary) are ignored; malformed and unrelated lines are skipped.
func lastAssistantMessage(data []byte, cut bool) (string, bool) {
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return "", false
	}
	data = data[:end]
	if cut {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return "", false
		}
		data = data[i+1:]
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Content string `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type != "assistant.message" {
			continue
		}
		for l := range strings.SplitSeq(ev.Data.Content, "\n") {
			if l = strings.TrimSpace(Sanitize(l)); l != "" {
				return l, true
			}
		}
	}
	return "", false
}

// Sanitize makes file or alert text safe to render: tabs become spaces and
// other control characters (including escape sequences' ESC) are dropped.
// Newlines are kept.
func Sanitize(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SanitizeLine is Sanitize with newlines replaced by spaces.
func SanitizeLine(s string) string {
	return strings.ReplaceAll(Sanitize(s), "\n", " ")
}
