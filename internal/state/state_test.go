package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	want := Record{
		Version:  1,
		Slot:     0,
		Active:   "red-snow-tabby",
		Previous: "tensorfold-qwen",
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Active != want.Active || got.Previous != want.Previous || got.Version != want.Version {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

func TestLoadMissingIsErrNoState(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	_, err := s.Load()
	if !errors.Is(err, ErrNoState) {
		t.Fatalf("want ErrNoState, got %v", err)
	}
}

func TestSaveIsAtomicAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	if err := s.Save(Record{Version: 1, Active: "a"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("leftover file %q after Save", e.Name())
		}
	}
}

func TestClear(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if err := s.Save(Record{Version: 1, Active: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); !errors.Is(err, ErrNoState) {
		t.Fatalf("after Clear want ErrNoState, got %v", err)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear of missing file must be idempotent, got %v", err)
	}
}

// TestLoadRejectsUnknownVersion pins the schema guard: a record from a future
// (or otherwise unknown) cardo must be refused loudly, never half-read as if
// its fields were understood. Version 0 (a hand-written record with no
// version) and 1 (what Save writes) are the accepted shapes.
func TestLoadRejectsUnknownVersion(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"version 1", `{"version":1,"active":"a"}`, true},
		{"no version field", `{"active":"a"}`, true},
		{"explicit zero", `{"version":0,"active":"a"}`, true},
		{"version 2", `{"version":2,"active":"a"}`, false},
		{"version 99", `{"version":99,"active":"a"}`, false},
		{"negative version", `{"version":-1,"active":"a"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			s := Store{Dir: dir}
			got, err := s.Load()
			if tc.ok {
				if err != nil {
					t.Fatalf("want it accepted, got %v", err)
				}
				if got.Active != "a" {
					t.Errorf("Load = %+v", got)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s must be rejected, got %+v", tc.name, got)
			}
			if errors.Is(err, ErrNoState) {
				t.Errorf("an unreadable schema must not look like an empty record: %v", err)
			}
			if !strings.Contains(err.Error(), "version") || !strings.Contains(err.Error(), "state.json") {
				t.Errorf("the error must name the version and the file: %v", err)
			}
		})
	}
}

// TestSaveSurvivesReloadWithEveryField exercises the whole durable path
// (temp file, fsync, rename, directory fsync) and pins that the ownership
// record comes back intact, PID included.
func TestSaveSurvivesReloadWithEveryField(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	at := time.Date(2025, 11, 20, 9, 13, 0, 0, time.UTC)
	want := Record{Version: 1, Slot: 2, Active: "red-snow-tabby", ActiveAt: at, ActivePID: 4242, Previous: "tensorfold-qwen"}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("saving over an existing record: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.Slot != want.Slot || got.Active != want.Active ||
		got.ActivePID != want.ActivePID || got.Previous != want.Previous || !got.ActiveAt.Equal(at) {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
	// The saved document is the version cardo writes, on disk.
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"version": 1`) {
		t.Errorf("state.json must record the schema version: %s", b)
	}
}

// TestSaveRoundTripsPreviousAt pins that the rollback timestamp is part of
// the durable record, not a field swap sets and loses: `status` and a later
// rollback decision both need to know when the previous engine was resident.
func TestSaveRoundTripsPreviousAt(t *testing.T) {
	dir := t.TempDir()
	s := Store{Dir: dir}
	at := time.Date(2025, 11, 20, 9, 13, 0, 0, time.UTC)
	want := Record{Version: 1, Active: "red-snow-tabby", ActiveAt: at,
		Previous: "tensorfold-qwen", PreviousAt: at.Add(-time.Hour)}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.PreviousAt.Equal(want.PreviousAt) {
		t.Errorf("PreviousAt = %v, want %v", got.PreviousAt, want.PreviousAt)
	}
	if !got.ActiveAt.Equal(at) {
		t.Errorf("ActiveAt = %v, want %v", got.ActiveAt, at)
	}
}

// TestSaveReportsAnUnsyncableDirectory pins that the durability step is real:
// a rename is only durable once the directory entry is flushed, so a Save
// that cannot fsync the directory must say so instead of reporting success.
func TestSaveReportsAnUnsyncableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	sub := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: sub}
	// Writable and traversable, but not readable: creating and renaming in
	// the directory work, opening it to flush it does not.
	if err := os.Chmod(sub, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	if err := s.Save(Record{Active: "a"}); err == nil {
		t.Fatal("Save must report a directory it cannot fsync, not claim the record is durable")
	}
}

func TestLoadCorruptIsTyped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir}
	_, err := s.Load()
	if err == nil || errors.Is(err, ErrNoState) {
		t.Fatalf("corrupt state must be a distinct error, got %v", err)
	}
}
