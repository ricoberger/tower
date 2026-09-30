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
	// StderrTailMaxBytes bounds the content of the shown stderr.log lines,
	// like every other artifact read. The lines are shown in full; when
	// they are larger together, the preview shows an error instead.
	StderrTailMaxBytes = item.MaxRunFileSize
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

// AlertFiles reads an item's alert.md through the store's safe accessor.
type AlertFiles interface {
	OpenAlertMarkdown(id string) (*os.File, error)
}

// alertDoc is the cached alert.md of an item without a run.
type alertDoc struct {
	id string
	// known is true once the file was read; size and mod identify the
	// read version.
	known bool
	size  int64
	mod   time.Time
	text  string
	err   error
}

// loadAlert refreshes the alert.md of an item. The file is only read when
// its size or modification time differs from prev, and never more than
// item.MaxRunFileSize bytes.
func loadAlert(files AlertFiles, prev alertDoc, id string) alertDoc {
	d := prev
	if d.id != id {
		d = alertDoc{id: id}
	}
	d.err = nil
	f, err := files.OpenAlertMarkdown(id)
	if err != nil {
		return alertDoc{id: id, err: err}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return alertDoc{id: id, err: err}
	}
	if d.known && fi.Size() == d.size && fi.ModTime().Equal(d.mod) {
		return d
	}
	data, err := io.ReadAll(io.LimitReader(f, item.MaxRunFileSize+1))
	if err != nil {
		return alertDoc{id: id, err: err}
	}
	if len(data) > item.MaxRunFileSize {
		return alertDoc{id: id, err: fmt.Errorf("alert.md is larger than %d bytes", item.MaxRunFileSize)}
	}
	return alertDoc{id: id, known: true, size: fi.Size(), mod: fi.ModTime(), text: string(data)}
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
// blank lines, with their full content. The file is scanned backwards, so
// the lines are exact however long the earlier content is.
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
	return lastLines(f, fi.Size(), StderrLines, StderrTailMaxBytes)
}

// lastLines returns the last n lines of the first size bytes of r. A final
// newline terminates the last line and does not start another one. The
// lines are returned in full; if they are larger than maxBytes together,
// an error is returned instead of partial lines. The file is scanned
// backwards and every byte is read at most once: at most maxBytes+2 bytes
// are read before an oversized tail is reported.
func lastLines(r io.ReaderAt, size int64, n int, maxBytes int64) ([]string, error) {
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
	tooLarge := fmt.Errorf("the last %d lines are larger than %d bytes; open the log with l", n, maxBytes)
	// The scan never goes below limit: a selected tail starting before it
	// is larger than maxBytes.
	limit := max(end-maxBytes-1, 0)
	// chunks holds the scanned bytes from the end backwards; first is the
	// start of the earliest selected line and found counts the line starts
	// seen from the last line backwards.
	var chunks [][]byte
	first, found := int64(0), 0
	pos := end
scan:
	for pos > limit {
		lo := max(pos-scanChunk, limit)
		chunk := make([]byte, pos-lo)
		if _, err := r.ReadAt(chunk, lo); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		chunks = append(chunks, chunk)
		pos = lo
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				found++
				if found == n {
					first = lo + int64(i) + 1
					break scan
				}
			}
		}
	}
	if found < n && pos > 0 {
		// The scan stopped at the limit without reaching the start of the
		// earliest selected line.
		return nil, tooLarge
	}
	if end-first > maxBytes {
		return nil, tooLarge
	}
	data := make([]byte, 0, end-first)
	for i := len(chunks) - 1; i >= 0; i-- {
		data = append(data, chunks[i]...)
	}
	return strings.Split(string(data[first-pos:]), "\n"), nil
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
