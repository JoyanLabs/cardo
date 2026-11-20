package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
