package ui

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/item"
)

// countingFiles counts content reads and records the requested limits.
type countingFiles struct {
	*item.Store
	reads  int
	limits []int64
	bytes  int
}

func (c *countingFiles) ReadRunFileTail(id string, n int, name string, limit int64) ([]byte, int64, fs.FileInfo, error) {
	c.reads++
	c.limits = append(c.limits, limit)
	data, off, fi, err := c.Store.ReadRunFileTail(id, n, name, limit)
	c.bytes += len(data)
	return data, off, fi, err
}

func TestLastAssistantMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		cut  bool
		want string
		ok   bool
	}{
		{"empty", "", false, "", false},
		{"only partial line", `{"type":"assistant.message","data":{"content":"x"}}`, false, "", false},
		{"first line of content", assistant("\n\n  first\nsecond"), false, "first", true},
		{"last complete wins", assistant("one") + assistant("two"), false, "two", true},
		{"partial last line ignored", assistant("one") + `{"type":"assistant.message","data":{"content":"two"`, false, "one", true},
		{"malformed and unrelated lines skipped", assistant("one") + "not json\n{bad json}\n" +
			`{"type":"assistant.message_delta","data":{"content":"delta"}}` + "\n" +
			`{"type":"result","data":{"content":"final"}}` + "\n" +
			`{"type":"tool.execution_start","data":{"content":"tool"}}` + "\n", false, "one", true},
		{"empty content skipped", assistant("one") + assistant("  \n "), false, "one", true},
		{"cut first line ignored", assistant("cut") + assistant("kept"), true, "kept", true},
		{"cut single line", assistant("cut"), true, "", false},
		{"control characters dropped", assistant("a\x1b[31mb\tc"), false, "a[31mb    c", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lastAssistantMessage([]byte(tc.data), tc.cut)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q %v, want %q %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestProgressBoundedTail(t *testing.T) {
	f := newFixture(t, nil)
	id := f.add("a", item.StatePreparing, "critical", t0, runningRun(1, t0))
	files := &countingFiles{Store: f.store}

	// No file yet: no read, no message.
	p := loadProgress(files, progress{}, id, 1)
	if p.message != "" || p.err != nil || files.reads != 0 {
		t.Fatalf("missing file: %+v reads=%d", p, files.reads)
	}

	// A large file is read only up to the tail limit; the message before
	// the tail is not searched for.
	early := assistant("too early")
	filler := strings.Repeat(`{"type":"tool.execution_start","data":{"content":"`+strings.Repeat("x", 200)+`"}}`+"\n", 2000)
	f.writeRunFile(id, 1, item.OutputFile, early+filler)
	p = loadProgress(files, p, id, 1)
	if files.reads != 1 || files.limits[0] != ProgressTailBytes || files.bytes != ProgressTailBytes {
		t.Fatalf("reads=%d limits=%v bytes=%d", files.reads, files.limits, files.bytes)
	}
	if p.message != "" {
		t.Fatalf("message fabricated from outside the tail: %q", p.message)
	}

	// A message in the tail is found; the partial first line at the tail
	// boundary is ignored.
	f.writeRunFile(id, 1, item.OutputFile, early+filler+assistant("Looking at logs")+`{"type":"assistant.message","data":{"content":"half`)
	p = loadProgress(files, p, id, 1)
	if p.message != "Looking at logs" || files.reads != 2 {
		t.Fatalf("message %q reads=%d", p.message, files.reads)
	}

	// Unchanged size and mtime: no content read.
	p = loadProgress(files, p, id, 1)
	p = loadProgress(files, p, id, 1)
	if files.reads != 2 || p.message != "Looking at logs" {
		t.Fatalf("unchanged file reread: reads=%d", files.reads)
	}

	// A change without a newer complete message keeps the last message
	// of the same run.
	f.writeRunFile(id, 1, item.OutputFile, early+filler+assistant("Looking at logs")+filler)
	p = loadProgress(files, p, id, 1)
	if files.reads != 3 || p.message != "Looking at logs" {
		t.Fatalf("message not retained: %q reads=%d", p.message, files.reads)
	}

	// Same size but a newer mtime invalidates the cache.
	f.writeRunFile(id, 1, item.OutputFile, filler+assistant("Version A"))
	p = loadProgress(files, p, id, 1)
	if files.reads != 4 || p.message != "Version A" {
		t.Fatalf("change: %q reads=%d", p.message, files.reads)
	}
	f.writeRunFile(id, 1, item.OutputFile, filler+assistant("Version B"))
	runPath, _ := f.store.RunPath(id, 1)
	later := t0.Add(time.Hour)
	if err := os.Chtimes(filepath.Join(runPath, item.OutputFile), later, later); err != nil {
		t.Fatal(err)
	}
	p = loadProgress(files, p, id, 1)
	if files.reads != 5 || p.message != "Version B" {
		t.Fatalf("mtime change: %q reads=%d", p.message, files.reads)
	}

	// Another run does not inherit the message.
	other := loadProgress(files, p, id, 2)
	if other.message != "" || other.run != 2 {
		t.Fatalf("message leaked to another run: %+v", other)
	}
}

func TestStderrTailBoundary(t *testing.T) {
	f := newFixture(t, nil)
	id := f.add("a", item.StateNeedsYou, "critical", t0, finishedRun(1, item.OutcomeFailed, t0))
	// Exactly 20 lines.
	var b strings.Builder
	for i := range 20 {
		b.WriteString("line " + string(rune('a'+i)) + "\n")
	}
	f.writeRunFile(id, 1, item.StderrFile, b.String())
	lines, err := stderrTail(f.store, id, 1)
	if err != nil || len(lines) != 20 || lines[0] != "line a" || lines[19] != "line t" {
		t.Fatalf("%v %q", err, lines)
	}
	// Trailing blank lines are lines too: a diagnostic followed by 20 blank
	// lines is no longer shown.
	f.writeRunFile(id, 1, item.StderrFile, "old diagnostic\n"+strings.Repeat("\n", 20))
	lines, err = stderrTail(f.store, id, 1)
	if err != nil || len(lines) != 20 || strings.Join(lines, "") != "" {
		t.Fatalf("blank lines: %v %q", err, lines)
	}
	f.writeRunFile(id, 1, item.StderrFile, "old diagnostic\n"+strings.Repeat("\n", 19))
	lines, err = stderrTail(f.store, id, 1)
	if err != nil || len(lines) != 20 || lines[0] != "old diagnostic" {
		t.Fatalf("19 blank lines: %v %q", err, lines)
	}
	// Lines far beyond any fixed byte window still count as lines and are
	// shown in full, including a diagnostic at their end.
	huge := strings.Repeat("y", 300<<10)
	var long strings.Builder
	long.WriteString("before\n")
	for i := range 18 {
		fmt.Fprintf(&long, "%s diagnostic %02d\n", huge, i)
	}
	long.WriteString("last")
	f.writeRunFile(id, 1, item.StderrFile, long.String())
	lines, err = stderrTail(f.store, id, 1)
	if err != nil || len(lines) != 20 || lines[0] != "before" || lines[19] != "last" {
		t.Fatalf("long lines: %v %d lines", err, len(lines))
	}
	for i, l := range lines[1:19] {
		if l != fmt.Sprintf("%s diagnostic %02d", huge, i) {
			t.Fatalf("long line %d not complete: ...%q", i, l[max(len(l)-40, 0):])
		}
	}
	// Selected lines larger than the read bound together are an error, not
	// partial content.
	if _, err := lastLines(strings.NewReader(long.String()), int64(long.Len()), 20, 1<<20); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversized tail: %v", err)
	}
	if lines, err := lastLines(strings.NewReader(long.String()), int64(long.Len()), 1, 4); err != nil || !slices.Equal(lines, []string{"last"}) {
		t.Fatalf("bound applies to the selected lines only: %v %q", err, lines)
	}
	// An oversized tail is detected without reading more than the bound
	// (plus the final byte and one byte before the bound), also for a
	// single line much larger than the bound; a tail within the bound is
	// read once.
	for _, tc := range []struct {
		name     string
		data     string
		maxBytes int64
		wantErr  bool
		maxRead  int64
	}{
		{"one 8 MiB line", strings.Repeat("x", 8<<20) + "\n", 1 << 20, true, 1<<20 + 2},
		{"one 8 MiB line without final newline", strings.Repeat("x", 8<<20), 1 << 20, true, 1<<20 + 2},
		{"tail just over the bound", "a\n" + strings.Repeat("x", 1<<20+1), 1 << 20, true, 1<<20 + 2},
		{"tail exactly at the bound", "a\n" + strings.Repeat("x", 1<<20), 1 << 20, false, 1<<20 + 2},
		{"small tail of a large file", strings.Repeat("x\n", 4<<20) + "end\n", 1 << 20, false, scanChunk + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &countingReaderAt{r: strings.NewReader(tc.data)}
			lines, err := lastLines(r, int64(len(tc.data)), 1, tc.maxBytes)
			if tc.wantErr != (err != nil) {
				t.Fatalf("error %v, want error %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "larger than") {
				t.Fatalf("error %v", err)
			}
			if err == nil {
				want := strings.TrimSuffix(tc.data[strings.LastIndexByte(strings.TrimSuffix(tc.data, "\n"), '\n')+1:], "\n")
				if len(lines) != 1 || lines[0] != want {
					t.Fatalf("got %d lines", len(lines))
				}
			}
			if r.n > tc.maxRead {
				t.Fatalf("read %d bytes, want at most %d", r.n, tc.maxRead)
			}
		})
	}
	for _, tc := range []struct {
		data string
		want []string
	}{
		{"", []string{}},
		{"\n", []string{""}},
		{"a", []string{"a"}},
		{"a\n\nb", []string{"a", "", "b"}},
		{"a\r\n", []string{"a\r"}},
	} {
		f.writeRunFile(id, 1, item.StderrFile, tc.data)
		if lines, err := stderrTail(f.store, id, 1); err != nil || !slices.Equal(lines, tc.want) {
			t.Errorf("%q: %v %q", tc.data, err, lines)
		}
	}
	// Lines spanning scan chunk boundaries.
	var chunked strings.Builder
	for i := range 40 {
		fmt.Fprintf(&chunked, "%d:%s\n", i, strings.Repeat("z", scanChunk/3+i))
	}
	lines, err = lastLines(strings.NewReader(chunked.String()), int64(chunked.Len()), 25, 1<<30)
	if err != nil || len(lines) != 25 || !strings.HasPrefix(lines[0], "15:") || !strings.HasPrefix(lines[24], "39:") || len(lines[24]) != len("39:")+scanChunk/3+39 {
		t.Fatalf("chunked: %v %d", err, len(lines))
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize("a\tb\x1b[2Jc\r\nd\x00"); got != "a    b[2Jc\nd" {
		t.Fatalf("%q", got)
	}
	if got := SanitizeLine("a\nb"); got != "a b" {
		t.Fatalf("%q", got)
	}
}

func TestFormatting(t *testing.T) {
	for d, want := range map[time.Duration]string{
		4 * time.Second: "4s", 125 * time.Second: "2m05s", 62*time.Minute + 3*time.Second: "1h02m", -time.Second: "0s",
	} {
		if got := FormatElapsed(d); got != want {
			t.Errorf("FormatElapsed(%s)=%q, want %q", d, got, want)
		}
	}
	if got := Truncate("日本語テキスト", 5); got != "日本…" {
		t.Errorf("Truncate=%q", got)
	}
}

// countingReaderAt counts the bytes read through it.
type countingReaderAt struct {
	r io.ReaderAt
	n int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}
