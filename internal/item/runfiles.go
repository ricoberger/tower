package item

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

// Run artifact file names.
const (
	PromptFile   = "prompt.md"
	OutputFile   = "output.jsonl"
	StderrFile   = "stderr.log"
	ExitCodeFile = "exit_code"
	ReportFile   = "report.md"
	ResultFile   = "result.json"
)

// MaxRunFileSize bounds how much of a run artifact ReadRunFile reads.
const MaxRunFileSize = 16 << 20

// ErrRunExists is returned by CreateRun when the run directory already exists.
var ErrRunExists = errors.New("run directory already exists")

// checkRunFile validates a run artifact name: a plain file name that is not
// the run metadata.
func checkRunFile(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || name == metaFile {
		return errors.New("invalid run file name")
	}
	return nil
}

func runRel(id string, n int, parts ...string) string {
	return itemRel(id, append([]string{runsDir, strconv.Itoa(n)}, parts...)...)
}

func checkIDRun(id string, n int) error {
	if _, _, err := ParseID(id); err != nil {
		return err
	}
	if n < 1 {
		return errors.New("run number must be at least 1")
	}
	return nil
}

// CreateRun creates runs/<n>/ with its meta.yaml for an existing item. It
// never reuses an existing run directory: if runs/<n> exists, ErrRunExists is
// returned. The directory is prepared under a temporary name and renamed into
// place, so a run directory never exists without its metadata.
func (s *Store) CreateRun(id string, r Run) (err error) {
	if err := checkIDRun(id, r.Number); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	unlock, err := s.lockItem(id)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := s.readItem(id); err != nil {
		return err
	}
	if err := s.mkdir(itemRel(id, runsDir)); err != nil {
		return err
	}
	dst := runRel(id, r.Number)
	if _, err := s.root.Lstat(dst); err == nil {
		return fmt.Errorf("%w: run %d", ErrRunExists, r.Number)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := itemRel(id, runsDir, "."+strconv.Itoa(r.Number)+"."+hex.EncodeToString(rnd[:])+".tmp")
	if err := s.root.Mkdir(tmp, dirPerm); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = s.root.RemoveAll(tmp)
		}
	}()
	if err := s.root.Chmod(tmp, dirPerm); err != nil {
		return err
	}
	data, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.writeAtomic(path.Join(tmp, metaFile), data); err != nil {
		return err
	}
	// The item lock serializes run creation, so dst cannot have appeared.
	if err := s.root.Rename(tmp, dst); err != nil {
		return err
	}
	if d, derr := s.root.Open(itemRel(id, runsDir)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ItemPath returns the absolute directory of an existing item after verifying
// its ID and that it resolves to a real directory inside the items
// directory. Unlike ItemDir, it may be used to hand the directory to other
// processes.
func (s *Store) ItemPath(id string) (string, error) {
	if _, _, err := ParseID(id); err != nil {
		return "", err
	}
	fi, err := s.root.Lstat(itemRel(id))
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", errors.New("not an item directory")
	}
	if err := s.checkContained(id); err != nil {
		return "", err
	}
	return s.ItemDir(id), nil
}

// RunPath returns the absolute directory of an existing run after the same
// checks as ItemPath; the runs directory and the run directory must be real
// directories.
func (s *Store) RunPath(id string, n int) (string, error) {
	if err := checkIDRun(id, n); err != nil {
		return "", err
	}
	dir, err := s.ItemPath(id)
	if err != nil {
		return "", err
	}
	for _, rel := range []string{itemRel(id, runsDir), runRel(id, n)} {
		fi, err := s.root.Lstat(rel)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", errors.New("not a run directory")
		}
	}
	return filepath.Join(dir, runsDir, strconv.Itoa(n)), nil
}

// WriteRunFile atomically writes a tower-owned artifact (such as prompt.md)
// of an existing run with 0600 permissions.
func (s *Store) WriteRunFile(id string, n int, name string, data []byte) error {
	if err := checkIDRun(id, n); err != nil {
		return err
	}
	if err := checkRunFile(name); err != nil {
		return err
	}
	unlock, err := s.lockItem(id)
	if err != nil {
		return err
	}
	defer unlock()
	fi, err := s.root.Lstat(runRel(id, n))
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return errors.New("not a run directory")
	}
	return s.writeAtomic(runRel(id, n, name), data)
}

// lstatRunFile checks that the directories leading to a run artifact are
// real directories and that the artifact itself is a regular file. Symlinks
// are rejected, even when their target stays inside the state directory.
func (s *Store) lstatRunFile(id string, n int, name string) (fs.FileInfo, error) {
	if err := checkIDRun(id, n); err != nil {
		return nil, err
	}
	if err := checkRunFile(name); err != nil {
		return nil, err
	}
	for _, rel := range []string{itemsDir, itemRel(id), itemRel(id, runsDir), runRel(id, n)} {
		fi, err := s.root.Lstat(rel)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			return nil, errors.New("unsafe run directory")
		}
	}
	fi, err := s.root.Lstat(runRel(id, n, name))
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	return fi, nil
}

// OpenRunFile opens a regular run artifact for reading. Symlinked artifacts
// or run directories are rejected, and the opened file must be the one that
// was checked.
func (s *Store) OpenRunFile(id string, n int, name string) (*os.File, error) {
	want, err := s.lstatRunFile(id, n, name)
	if err != nil {
		return nil, err
	}
	// O_NONBLOCK: opening a FIFO (or another special file) swapped in after
	// the check must not block before its type is checked; it has no effect
	// on regular files. O_NOFOLLOW rejects a symlink swapped in meanwhile.
	f, err := s.root.OpenFile(runRel(id, n, name), os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(want, fi) {
		_ = f.Close()
		return nil, fmt.Errorf("%s changed while it was opened", name)
	}
	return f, nil
}

// ReadRunFile reads a regular run artifact of at most MaxRunFileSize bytes.
func (s *Store) ReadRunFile(id string, n int, name string) ([]byte, error) {
	f, err := s.OpenRunFile(id, n, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxRunFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxRunFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, MaxRunFileSize)
	}
	return data, nil
}

// StatRunFile returns the file info of a regular run artifact without
// following symlinks.
func (s *Store) StatRunFile(id string, n int, name string) (fs.FileInfo, error) {
	return s.lstatRunFile(id, n, name)
}

// ReadRunFileTail reads at most limit bytes from the end of a regular run
// artifact. It returns the data, the offset of its first byte in the file
// and the file info observed when it was opened.
func (s *Store) ReadRunFileTail(id string, n int, name string, limit int64) ([]byte, int64, fs.FileInfo, error) {
	if limit <= 0 {
		return nil, 0, nil, errors.New("invalid tail size")
	}
	f, err := s.OpenRunFile(id, n, name)
	if err != nil {
		return nil, 0, nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, nil, err
	}
	off := max(fi.Size()-limit, 0)
	data, err := io.ReadAll(io.NewSectionReader(f, off, limit))
	if err != nil {
		return nil, 0, nil, err
	}
	return data, off, fi, nil
}

// HasRunFile reports whether a run artifact exists as a regular file (not a
// symlink).
func (s *Store) HasRunFile(id string, n int, name string) (bool, error) {
	if err := checkIDRun(id, n); err != nil {
		return false, err
	}
	if err := checkRunFile(name); err != nil {
		return false, err
	}
	fi, err := s.root.Lstat(runRel(id, n, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return fi.Mode().IsRegular(), nil
}

// ReadAlertMarkdown reads alert.md of an item.
func (s *Store) ReadAlertMarkdown(id string) ([]byte, error) {
	if _, _, err := ParseID(id); err != nil {
		return nil, err
	}
	return s.root.ReadFile(itemRel(id, markdownFile))
}

// AlertMarkdownPath returns the absolute path of an item's alert.md after the
// checks of ItemPath.
func (s *Store) AlertMarkdownPath(id string) (string, error) {
	dir, err := s.ItemPath(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, markdownFile), nil
}
