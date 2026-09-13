package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/bnema/wlvision/internal/result"
)

// sessionsDirName is the subdirectory of the state root that holds records.
const sessionsDirName = "sessions"

// DefaultRetention is how long a session stays observable after its
// application, compositor, or container stopped. Cleanup happens on explicit
// close or when this deadline passes.
const DefaultRetention = 24 * time.Hour

// Store persists session records under a state root.
//
// The store contains only wlvision-owned state, never application data: a
// session's files live in its container, and everything wlvision keeps on the
// host is a record of what it created.
type Store struct {
	root string
}

// DefaultRoot returns the state root used when none is configured:
// $XDG_STATE_HOME/wlvision, or ~/.local/state/wlvision.
func DefaultRoot() (string, error) {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "wlvision"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("session: cannot locate the state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "wlvision"), nil
}

// NewStore opens the store, creating the root with owner-only permissions.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("session: a state root is required")
	}
	store := &Store{root: root}
	if err := os.MkdirAll(store.dir(), 0o700); err != nil {
		return nil, fmt.Errorf("session: cannot create %s: %w", store.dir(), err)
	}
	return store, nil
}

// Root returns the state root.
func (s *Store) Root() string { return s.root }

// dir returns the directory holding the records.
func (s *Store) dir() string { return filepath.Join(s.root, sessionsDirName) }

// path returns the record path for one session, refusing an identifier that is
// not a valid session id so no caller-supplied string can escape the store.
func (s *Store) path(id string) (string, error) {
	if !ValidSessionID(id) {
		return "", usageFailure("session.store", "invalid session identifier %q", id)
	}
	return filepath.Join(s.dir(), id+".json"), nil
}

// Save writes the record atomically: a temporary file in the same directory,
// synced, renamed over the target, then the directory synced. A crash therefore
// leaves either the previous record or the new one, never a partial file.
func (s *Store) Save(record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	target, err := s.path(record.Session)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return fmt.Errorf("session: cannot create %s: %w", s.dir(), err)
	}

	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("session: encoding the record of %s: %w", record.Session, err)
	}
	payload = append(payload, '\n')

	file, err := os.CreateTemp(s.dir(), ".record-*.tmp")
	if err != nil {
		return fmt.Errorf("session: cannot stage the record of %s: %w", record.Session, err)
	}
	staged := file.Name()

	discard := func(cause error) error {
		_ = file.Close()
		_ = os.Remove(staged)
		return cause
	}

	if _, err := file.Write(payload); err != nil {
		return discard(fmt.Errorf("session: cannot write the record of %s: %w", record.Session, err))
	}
	if err := file.Sync(); err != nil {
		return discard(fmt.Errorf("session: cannot sync the record of %s: %w", record.Session, err))
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("session: cannot close the record of %s: %w", record.Session, err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("session: cannot publish the record of %s: %w", record.Session, err)
	}
	return syncDir(s.dir())
}

// Load reads one record. An unknown session is reported as a session failure,
// not as a filesystem error, because that is what it means to the caller.
func (s *Store) Load(id string) (Record, error) {
	path, err := s.path(id)
	if err != nil {
		return Record{}, err
	}

	payload, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		failure := result.NewFailure(result.CodeSessionNotReady, "session.load", "unknown session %q", id)
		failure.Details = map[string]string{"session": id}
		return Record{}, failure
	case err != nil:
		return Record{}, fmt.Errorf("session: cannot read the record of %s: %w", id, err)
	}

	var record Record
	if err := json.Unmarshal(payload, &record); err != nil {
		return Record{}, result.NewFailure(result.CodeSessionNotReady, "session.load",
			"the record of session %q is unreadable: %v", id, err)
	}
	if err := record.Validate(); err != nil {
		return Record{}, err
	}
	return record, nil
}

// List returns every record, ordered by session identifier. A record that
// cannot be read is an error rather than a silent omission: an operator must
// never lose a container because its bookkeeping file went missing.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: cannot list %s: %w", s.dir(), err)
	}

	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		record, err := s.Load(name[:len(name)-len(".json")])
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	sort.Slice(records, func(i, j int) bool { return records[i].Session < records[j].Session })
	return records, nil
}

// Delete removes one record and syncs the directory. Removing a record that is
// already gone is not an error.
func (s *Store) Delete(id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: cannot remove the record of %s: %w", id, err)
	}
	return syncDir(s.dir())
}

// syncDir flushes a directory entry so a rename survives a crash.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("session: cannot open %s to sync it: %w", path, err)
	}
	defer func() { _ = dir.Close() }()

	if err := dir.Sync(); err != nil {
		return fmt.Errorf("session: cannot sync %s: %w", path, err)
	}
	return nil
}
