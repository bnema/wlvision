package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bnema/wlvision/internal/result"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	store := newTestStore(t)
	record := testRecord("demo")
	record.Payloads = []Payload{{Digest: "sha256:abc", Path: "/run/wlvision/payload/app", Bytes: 4096, ReceivedAt: testTime}}
	record.Process = &ProcessExit{Code: 3, At: testTime, Command: []string{"/payload/app"}}

	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := store.Load("demo")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Session != record.Session || loaded.State != record.State || loaded.Revision != record.Revision {
		t.Errorf("loaded = %+v, want the saved record", loaded)
	}
	if loaded.Engine != record.Engine || loaded.Image != record.Image || loaded.Weston != record.Weston {
		t.Errorf("loaded engine metadata = %+v, want the saved record", loaded.Engine)
	}
	if loaded.Limits != record.Limits {
		t.Errorf("loaded limits = %+v, want %+v", loaded.Limits, record.Limits)
	}
	if len(loaded.Payloads) != 1 || loaded.Payloads[0].Digest != "sha256:abc" {
		t.Errorf("loaded payloads = %+v, want the saved payload", loaded.Payloads)
	}
	if loaded.Process == nil || loaded.Process.Code != 3 {
		t.Errorf("loaded process = %+v, want the saved exit record", loaded.Process)
	}
	if !loaded.CreatedAt.Equal(record.CreatedAt) {
		t.Errorf("CreatedAt = %s, want %s", loaded.CreatedAt, record.CreatedAt)
	}
}

func TestStoreWritesOwnerOnlyFiles(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(testRecord("demo")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dirInfo, err := os.Stat(store.dir())
	if err != nil {
		t.Fatalf("stat %s: %v", store.dir(), err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("record directory mode = %o, want 700", perm)
	}

	fileInfo, err := os.Stat(filepath.Join(store.dir(), "demo.json"))
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %o, want 600", perm)
	}
}

func TestStoreReplacesRecordAtomicallyAndDeterministically(t *testing.T) {
	store := newTestStore(t)
	record := testRecord("demo")

	if err := store.Save(record); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(store.dir(), "demo.json"))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}

	// Rewriting an unchanged record must produce the same bytes, so a record
	// can be compared and diffed between runs.
	if err := store.Save(record); err != nil {
		t.Fatalf("Save again: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(store.dir(), "demo.json"))
	if err != nil {
		t.Fatalf("read record again: %v", err)
	}
	if string(first) != string(second) {
		t.Error("rewriting an unchanged record changed its bytes")
	}

	entries, err := os.ReadDir(store.dir())
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			t.Errorf("staging file %q survived the write", entry.Name())
		}
	}
}

func TestStoreIgnoresStagingFilesLeftByACrash(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(testRecord("demo")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A crash between creating and renaming the staged file leaves it behind.
	staged := filepath.Join(store.dir(), ".record-123.tmp")
	if err := os.WriteFile(staged, []byte("{ truncated"), 0o600); err != nil {
		t.Fatalf("stage a leftover file: %v", err)
	}

	records, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].Session != "demo" {
		t.Errorf("List = %+v, want only the published record", records)
	}
}

func TestStoreListIsOrderedAndEmptyByDefault(t *testing.T) {
	store := newTestStore(t)

	records, err := store.List()
	if err != nil {
		t.Fatalf("List on an empty store: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("List = %+v, want no records", records)
	}

	for _, id := range []string{"zulu", "alpha", "mike"} {
		if err := store.Save(testRecord(id)); err != nil {
			t.Fatalf("Save %s: %v", id, err)
		}
	}

	records, err = store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, 0, len(records))
	for _, record := range records {
		got = append(got, record.Session)
	}
	if len(got) != 3 || got[0] != "alpha" || got[1] != "mike" || got[2] != "zulu" {
		t.Errorf("List order = %v, want [alpha mike zulu]", got)
	}
}

func TestStoreReportsAnUnknownSession(t *testing.T) {
	store := newTestStore(t)

	_, err := store.Load("ghost")
	if err == nil {
		t.Fatal("loading an unknown session succeeded")
	}

	var failure *result.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error %v (%T) is not a *result.Failure", err, err)
	}
	if failure.Code != result.CodeSessionNotReady {
		t.Errorf("code = %s, want %s", failure.Code, result.CodeSessionNotReady)
	}
	if failure.Details["session"] != "ghost" {
		t.Errorf("details = %v, want the session name", failure.Details)
	}
}

func TestStoreRefusesToLeaveItsRoot(t *testing.T) {
	root := t.TempDir()
	store, err := NewStore(root)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	escape := testRecord("demo")
	escape.Session = "../../escape"
	if err := store.Save(escape); err == nil {
		t.Error("a record with a traversing session identifier was written")
	}
	if _, err := store.Load("../../escape"); err == nil {
		t.Error("a traversing session identifier was read")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.json")); err == nil {
		t.Error("a record escaped the state root")
	}
}

func TestStoreDelete(t *testing.T) {
	store := newTestStore(t)
	if err := store.Save(testRecord("demo")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := store.Delete("demo"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Load("demo"); err == nil {
		t.Error("the record survived deletion")
	}
	if err := store.Delete("demo"); err != nil {
		t.Errorf("deleting a removed record: %v", err)
	}
}

func TestStoreRejectsAnIncompleteRecord(t *testing.T) {
	store := newTestStore(t)

	record := testRecord("demo")
	record.State = State("paused")
	if err := store.Save(record); err == nil {
		t.Fatal("a record with an unknown state was written")
	}

	entries, err := os.ReadDir(store.dir())
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the store contains %d entries after a refused write", len(entries))
	}
}
