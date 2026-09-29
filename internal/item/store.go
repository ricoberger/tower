package item

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	dirPerm  = 0o700
	filePerm = 0o600

	itemsDir     = "items"
	itemFile     = "item.yaml"
	alertFile    = "alert.json"
	runsDir      = "runs"
	metaFile     = "meta.yaml"
	lockFile     = ".lock"
	countersFile = "counters.yaml"

	// InstanceLockFile is the name of the single-instance lock file.
	InstanceLockFile = "tower.lock"
)

// ErrLocked is returned when the instance lock is held by another process.
var ErrLocked = errors.New("lock is held by another process")

// Store is the on-disk item store rooted at the state directory. All file
// access goes through an os.Root, so paths can never escape the state
// directory, even through symlinks.
type Store struct {
	dir  string
	root *os.Root
	// mu serializes counter bookkeeping within this process. Across processes
	// counter updates are only made while holding the instance lock.
	mu sync.Mutex
}

// Open creates (if needed) and opens the state directory.
func Open(stateDir string) (*Store, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("set state directory permissions: %w", err)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, fmt.Errorf("open state directory: %w", err)
	}
	s := &Store{dir: abs, root: root}
	if err := s.mkdir(itemsDir); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("create items directory: %w", err)
	}
	return s, nil
}

// Close releases the store.
func (s *Store) Close() error { return s.root.Close() }

// Dir returns the absolute state directory.
func (s *Store) Dir() string { return s.dir }

// ItemDir returns the absolute directory of an item (for display only).
func (s *Store) ItemDir(id string) string {
	return filepath.Join(s.dir, itemsDir, id)
}

// ItemsDir returns the absolute items directory (for display only).
func (s *Store) ItemsDir() string { return filepath.Join(s.dir, itemsDir) }

// mkdir creates a directory with 0700 permissions, tolerating existence.
func (s *Store) mkdir(rel string) error {
	if err := s.root.Mkdir(rel, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return s.root.Chmod(rel, dirPerm)
}

// OpenFile opens (creating when needed) a 0600 file in the state directory,
// e.g. the log file.
func (s *Store) OpenFile(name string, flag int) (*os.File, error) {
	f, err := s.root.OpenFile(name, flag, filePerm)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(filePerm); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Rename renames a file within the state directory.
func (s *Store) Rename(oldName, newName string) error {
	return s.root.Rename(oldName, newName)
}

// Remove removes a file within the state directory.
func (s *Store) Remove(name string) error {
	return s.root.Remove(name)
}

// Stat stats a file within the state directory.
func (s *Store) Stat(name string) (fs.FileInfo, error) {
	return s.root.Stat(name)
}

// InstanceLock is a held single-instance lock.
type InstanceLock struct {
	f *os.File
}

// Release releases the lock.
func (l *InstanceLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// InstanceLockPath returns the absolute path of the instance lock file.
func (s *Store) InstanceLockPath() string {
	return filepath.Join(s.dir, InstanceLockFile)
}

// TryLockInstance takes the exclusive, non-blocking instance lock. It returns
// an error wrapping ErrLocked if another process holds it. The lock is
// released when the returned lock is released or the process exits.
func (s *Store) TryLockInstance() (*InstanceLock, error) {
	f, err := s.OpenFile(InstanceLockFile, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", s.InstanceLockPath(), err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- file descriptors fit in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s (is another tower instance running?)", ErrLocked, s.InstanceLockPath())
		}
		return nil, fmt.Errorf("lock %s: %w", s.InstanceLockPath(), err)
	}
	return &InstanceLock{f: f}, nil
}

// lockItem takes the exclusive per-item lock <item-dir>/.lock. It never
// creates the item directory.
func (s *Store) lockItem(id string) (func(), error) {
	f, err := s.root.OpenFile(path.Join(itemsDir, id, lockFile), os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, fmt.Errorf("open item lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { // #nosec G115 -- file descriptors fit in int
		_ = f.Close()
		return nil, fmt.Errorf("lock item: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

// beforeRename is a test hook called before an atomic rename.
var beforeRename func(tmp, dst string) error

// writeAtomic writes data to rel via a temporary file in the same directory,
// fsyncs it and renames it over the destination.
func (s *Store) writeAtomic(rel string, data []byte) (err error) {
	dir, base := path.Split(rel)
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := path.Join(dir, "."+base+"."+hex.EncodeToString(rnd[:])+".tmp")
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = s.root.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(filePerm); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		if err = beforeRename(tmp, rel); err != nil {
			return err
		}
	}
	if err = s.root.Rename(tmp, rel); err != nil {
		return err
	}
	if dir == "" {
		dir = "."
	}
	if d, derr := s.root.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func itemRel(id string, parts ...string) string {
	return path.Join(append([]string{itemsDir, id}, parts...)...)
}

// decodeItem strictly decodes and validates item.yaml content.
func decodeItem(id string, data []byte) (*Item, error) {
	var it Item
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&it); err != nil {
		return nil, fmt.Errorf("decode %s: %w", itemFile, err)
	}
	if err := it.Validate(id); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", itemFile, err)
	}
	return &it, nil
}

func (s *Store) readItem(id string) (*Item, error) {
	if _, _, err := ParseID(id); err != nil {
		return nil, err
	}
	data, err := s.root.ReadFile(itemRel(id, itemFile))
	if err != nil {
		return nil, err
	}
	return decodeItem(id, data)
}

func (s *Store) writeItem(it *Item) error {
	data, err := yaml.Marshal(it)
	if err != nil {
		return err
	}
	return s.writeAtomic(itemRel(it.ID, itemFile), data)
}

// Stamp identifies a version of item.yaml on disk, used to detect changes
// made by other processes.
type Stamp struct {
	ModTime time.Time
	Size    int64
	Inode   uint64
}

// Stamp returns the current stamp of an item's item.yaml.
func (s *Store) Stamp(id string) (Stamp, error) {
	if _, _, err := ParseID(id); err != nil {
		return Stamp{}, err
	}
	fi, err := s.root.Stat(itemRel(id, itemFile))
	if err != nil {
		return Stamp{}, err
	}
	st := Stamp{ModTime: fi.ModTime(), Size: fi.Size()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.Inode = sys.Ino
	}
	return st, nil
}

// Loaded is an item together with its runs and stored raw alert.
type Loaded struct {
	Item     *Item
	Runs     []Run
	RawAlert json.RawMessage
	Stamp    Stamp
}

// Unreadable describes an item directory that could not be loaded.
type Unreadable struct {
	ID   string
	Path string
	Err  error
}

// List returns the IDs of all item directories (real directories with a
// valid item ID as name), sorted.
func (s *Store) List() ([]string, error) {
	d, err := s.root.Open(itemsDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if _, _, err := ParseID(e.Name()); err != nil {
			continue
		}
		ids = append(ids, e.Name())
	}
	slices.Sort(ids)
	return ids, nil
}

// Load reads one item, its runs and its raw alert from disk.
func (s *Store) Load(id string) (Loaded, error) {
	st, err := s.Stamp(id)
	if err != nil {
		return Loaded{}, err
	}
	it, err := s.readItem(id)
	if err != nil {
		return Loaded{}, err
	}
	runs, err := s.ReadRuns(id)
	if err != nil {
		return Loaded{}, err
	}
	raw, err := s.root.ReadFile(itemRel(id, alertFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Loaded{}, fmt.Errorf("read %s: %w", alertFile, err)
	}
	return Loaded{Item: it, Runs: runs, RawAlert: raw, Stamp: st}, nil
}

// LoadAll loads all items. Unreadable items are returned separately and are
// never modified.
func (s *Store) LoadAll() ([]Loaded, []Unreadable, error) {
	ids, err := s.List()
	if err != nil {
		return nil, nil, err
	}
	var loaded []Loaded
	var bad []Unreadable
	for _, id := range ids {
		l, err := s.Load(id)
		if err != nil {
			bad = append(bad, Unreadable{ID: id, Path: s.ItemDir(id), Err: err})
			continue
		}
		loaded = append(loaded, l)
	}
	return loaded, bad, nil
}

// Get reads the current item.yaml of an item.
func (s *Store) Get(id string) (*Item, error) {
	return s.readItem(id)
}

// Create creates a new item directory with item.yaml and (when raw is not
// nil) alert.json. The per-key counter is persisted before the directory is
// created, and an existing directory is never overwritten.
func (s *Store) Create(it *Item, raw json.RawMessage) error {
	key, n, err := ParseID(it.ID)
	if err != nil {
		return err
	}
	if err := it.Validate(it.ID); err != nil {
		return err
	}
	if err := s.ReserveCounter(key, n); err != nil {
		return err
	}
	if err := s.root.Mkdir(itemRel(it.ID), dirPerm); err != nil {
		return fmt.Errorf("create item directory: %w", err)
	}
	if err := s.root.Chmod(itemRel(it.ID), dirPerm); err != nil {
		return err
	}
	unlock, err := s.lockItem(it.ID)
	if err != nil {
		return err
	}
	defer unlock()
	if raw != nil {
		if err := s.writeAtomic(itemRel(it.ID, alertFile), raw); err != nil {
			return fmt.Errorf("write %s: %w", alertFile, err)
		}
	}
	if err := s.writeItem(it); err != nil {
		return fmt.Errorf("write %s: %w", itemFile, err)
	}
	return nil
}

// Update performs a locked read-modify-write of an item: it takes the item
// lock, reads the latest item.yaml from disk, applies fn and writes the result
// atomically.
func (s *Store) Update(id string, fn func(it *Item) error) (*Item, error) {
	if _, _, err := ParseID(id); err != nil {
		return nil, err
	}
	unlock, err := s.lockItem(id)
	if err != nil {
		return nil, err
	}
	defer unlock()
	it, err := s.readItem(id)
	if err != nil {
		return nil, err
	}
	if err := fn(it); err != nil {
		return nil, err
	}
	if err := it.Validate(id); err != nil {
		return nil, err
	}
	if err := s.writeItem(it); err != nil {
		return nil, err
	}
	return it, nil
}

// AppendAction records a user action on an item (see Item.ApplyAction).
func (s *Store) AppendAction(id string, action Action, run int, now time.Time) (*Item, error) {
	return s.Update(id, func(it *Item) error {
		return it.ApplyAction(action, run, now)
	})
}

// WriteRawAlert replaces alert.json of an existing item.
func (s *Store) WriteRawAlert(id string, raw json.RawMessage) error {
	if _, _, err := ParseID(id); err != nil {
		return err
	}
	unlock, err := s.lockItem(id)
	if err != nil {
		return err
	}
	defer unlock()
	return s.writeAtomic(itemRel(id, alertFile), raw)
}

// WriteRun writes runs/<n>/meta.yaml of an existing item.
func (s *Store) WriteRun(id string, r Run) error {
	if _, _, err := ParseID(id); err != nil {
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
	if err := s.mkdir(itemRel(id, runsDir)); err != nil {
		return err
	}
	if err := s.mkdir(itemRel(id, runsDir, strconv.Itoa(r.Number))); err != nil {
		return err
	}
	data, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	return s.writeAtomic(itemRel(id, runsDir, strconv.Itoa(r.Number), metaFile), data)
}

// ReadRuns reads all run metadata of an item, sorted by run number.
func (s *Store) ReadRuns(id string) ([]Run, error) {
	d, err := s.root.Open(itemRel(id, runsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil || n < 1 || strconv.Itoa(n) != e.Name() || !e.IsDir() {
			continue
		}
		data, err := s.root.ReadFile(itemRel(id, runsDir, e.Name(), metaFile))
		if err != nil {
			return nil, fmt.Errorf("read run %d: %w", n, err)
		}
		var r Run
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("decode run %d: %w", n, err)
		}
		if r.Number != n {
			return nil, fmt.Errorf("run %d: number does not match its directory", n)
		}
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("run %d: %w", n, err)
		}
		runs = append(runs, r)
	}
	slices.SortFunc(runs, func(a, b Run) int { return a.Number - b.Number })
	return runs, nil
}

// readCounters reads the durable per-key high-water marks. A missing file is
// an empty set; any other error is returned (never silently reset).
func (s *Store) readCounters() (map[string]int, error) {
	data, err := s.root.ReadFile(countersFile)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", countersFile, err)
	}
	counters := map[string]int{}
	if err := yaml.Unmarshal(data, &counters); err != nil {
		return nil, fmt.Errorf("decode %s: %w", countersFile, err)
	}
	if counters == nil {
		counters = map[string]int{}
	}
	return counters, nil
}

// ReserveCounter durably raises the high-water mark of key to at least n.
func (s *Store) ReserveCounter(key Key, n int) error {
	if !ValidKey(key) {
		return errors.New("invalid item key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	counters, err := s.readCounters()
	if err != nil {
		return err
	}
	if counters[key.String()] >= n {
		return nil
	}
	counters[key.String()] = n
	data, err := yaml.Marshal(counters)
	if err != nil {
		return err
	}
	return s.writeAtomic(countersFile, data)
}

// Counters returns, per key, the highest item number ever used: the maximum
// of the durable high-water marks and all item directory names (including
// unreadable ones).
func (s *Store) Counters() (map[Key]int, error) {
	s.mu.Lock()
	counters, err := s.readCounters()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := map[Key]int{}
	for k, n := range counters {
		src, fp, ok := cut(k)
		key := Key{Source: src, Fingerprint: fp}
		if !ok || !ValidKey(key) {
			return nil, fmt.Errorf("decode %s: invalid key", countersFile)
		}
		out[key] = max(out[key], n)
	}
	ids, err := s.List()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		key, n, _ := ParseID(id)
		out[key] = max(out[key], n)
	}
	return out, nil
}

func cut(k string) (string, string, bool) {
	i := len(k) - 1
	for i >= 0 && k[i] != '/' {
		i--
	}
	if i < 0 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

// PruneCandidate is a done item whose updated_at is older than the cutoff.
type PruneCandidate struct {
	ID        string
	Path      string
	UpdatedAt time.Time
}

// PruneCandidates lists valid done items with updated_at strictly before
// cutoff. Items that LoadAll would skip as unreadable are never candidates.
func (s *Store) PruneCandidates(cutoff time.Time) ([]PruneCandidate, error) {
	ids, err := s.List()
	if err != nil {
		return nil, err
	}
	var out []PruneCandidate
	for _, id := range ids {
		// Use the loader's readability rules: an item skipped as unreadable
		// (item.yaml, run metadata or alert.json) is never a candidate.
		l, err := s.Load(id)
		if err != nil {
			continue
		}
		if eligible(l.Item, cutoff) {
			out = append(out, PruneCandidate{ID: id, Path: s.ItemDir(id), UpdatedAt: l.Item.UpdatedAt})
		}
	}
	return out, nil
}

func eligible(it *Item, cutoff time.Time) bool {
	return it.State == StateDone && it.UpdatedAt.Before(cutoff)
}

// PruneItem deletes one item directory if, under the item lock, its current
// valid on-disk state is still done and older than cutoff. It reports whether
// the item was deleted. The caller must hold the instance lock. Identity
// counters are preserved.
func (s *Store) PruneItem(id string, cutoff time.Time) (bool, error) {
	key, n, err := ParseID(id)
	if err != nil {
		return false, err
	}
	rel := itemRel(id)
	fi, err := s.root.Lstat(rel)
	if err != nil {
		return false, err
	}
	if !fi.IsDir() || fi.Mode()&fs.ModeSymlink != 0 {
		return false, errors.New("not an item directory")
	}
	if err := s.checkContained(id); err != nil {
		return false, err
	}
	unlock, err := s.lockItem(id)
	if err != nil {
		return false, err
	}
	defer unlock()
	// Recheck against the current disk state with the same readability
	// rules as LoadAll; an unreadable item is never deleted.
	l, err := s.Load(id)
	if err != nil {
		return false, nil
	}
	if !eligible(l.Item, cutoff) {
		return false, nil
	}
	if err := s.ReserveCounter(key, n); err != nil {
		return false, err
	}
	if err := s.root.RemoveAll(rel); err != nil {
		return false, err
	}
	return true, nil
}

// checkContained verifies that the resolved item directory is a direct child
// of the resolved items directory.
func (s *Store) checkContained(id string) error {
	items, err := filepath.EvalSymlinks(s.ItemsDir())
	if err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(s.ItemDir(id))
	if err != nil {
		return err
	}
	if filepath.Dir(dir) != items || filepath.Base(dir) != id {
		return errors.New("item directory is outside the items directory")
	}
	return nil
}
