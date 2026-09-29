package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/ricoberger/tower/internal/item"
)

const (
	logFile     = "tower.log"
	logMaxBytes = 10 * 1024 * 1024
	// logFiles is the number of retained log files (tower.log, tower.log.1,
	// tower.log.2).
	logFiles = 3
)

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid --log-level: must be one of debug, info, warn, error")
}

// rotatingWriter writes to STATE_DIR/tower.log and rotates it when it would
// exceed maxBytes, keeping at most files log files.
type rotatingWriter struct {
	mu       sync.Mutex
	store    *item.Store
	name     string
	maxBytes int64
	files    int
	f        *os.File
	size     int64
}

func newRotatingWriter(store *item.Store, name string, maxBytes int64, files int) (*rotatingWriter, error) {
	w := &rotatingWriter{store: store, name: name, maxBytes: maxBytes, files: files}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := w.store.OpenFile(w.name, os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.f, w.size = f, fi.Size()
	return nil
}

func (w *rotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	oldest := fmt.Sprintf("%s.%d", w.name, w.files-1)
	if err := w.store.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for i := w.files - 2; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.name, i)
		if err := w.store.Rename(from, fmt.Sprintf("%s.%d", w.name, i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := w.store.Rename(w.name, w.name+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return w.open()
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// newLogger returns a logger writing JSON to the log file and human-readable
// text to stderr.
func newLogger(file io.Writer, stderr io.Writer, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	return slog.New(slog.NewMultiHandler(
		slog.NewJSONHandler(file, opts),
		slog.NewTextHandler(stderr, opts),
	))
}
