package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodConf is a valid engine, used wherever a directory needs at least one
// engine that loads.
const goodConf = "NAME=good\nTYPE=command\nSTART=/bin/serve\nHEALTH=http://127.0.0.1:8080/health\n"

// misnamedEngine is what an operator writes when the editor loses the
// suffix: real engine content in a file LoadDir cannot see.
const misnamedEngine = "NAME=tensorfold\nTYPE=docker\nCONTAINER=tensorfold\nHEALTH=http://127.0.0.1:8888/health\n"

// dirWith returns a directory holding every named file with the given body.
func dirWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestScanDirReportsEveryBadFileSeparately is the doctor contract: one broken
// .conf must not hide the next one. An operator fixing a config dir wants the
// whole list in one pass, and each finding has to name its own file.
func TestScanDirReportsEveryBadFileSeparately(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf": goodConf,
		"aaa.conf":  "NAME=a\nTYPE=command\nSTART=x\nHEALTH=http://a\nTYPO=1\n",
		"zzz.conf":  "NAME=z\nTYPE=command\nSTART=z\nHEALTH=http://z\nWAIT=0\n",
	})
	scan, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Problems) != 2 {
		t.Fatalf("want one problem per broken file, got %d: %v", len(scan.Problems), scan.Problems)
	}
	if _, ok := scan.Registry.Get("good"); !ok {
		t.Errorf("a broken neighbour must not cost the operator the engines that load: %v", scan.Registry.Names())
	}
	joined := ""
	for _, p := range scan.Problems {
		joined += p.Error() + "\n"
	}
	for _, want := range []string{"aaa.conf", "TYPO", "zzz.conf", "WAIT"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems must name %q, got:\n%s", want, joined)
		}
	}
}

// TestScanDirNamesFilesThatVanishSilently is the misnamed-engine half of
// doctor: a file with real engine content and no .conf suffix is skipped by
// LoadDir with no trace, so the engine simply never exists. ScanDir has to
// surface it, and must not cry wolf on the documentation that is meant to
// live beside engines.
func TestScanDirNamesFilesThatVanishSilently(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf":      goodConf,
		"tensorfold":     misnamedEngine,
		"tabby.conf.bak": misnamedEngine,
		"README.md":      "# Example engines\n\n```conf\nNAME=\"vllm-qwen\"\nTYPE=\"docker\"\nCONTAINER=\"vllm-qwen\"\n```\n",
		"notes.txt":      "Remember to drain before swapping; ask BUSY first.\n",
		".hidden":        misnamedEngine,
		"edit.conf~":     misnamedEngine,
		"subdir/ignore":  misnamedEngine,
		"LICENSE":        "MIT\n",
	})
	scan, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, sk := range scan.Skipped {
		got[sk.Name] = sk.EngineLike
	}
	for name, wantLike := range map[string]bool{
		"tensorfold":     true,
		"tabby.conf.bak": true,
		"edit.conf~":     true,
		"README.md":      false,
		"notes.txt":      false,
		"LICENSE":        false,
	} {
		like, ok := got[name]
		if !ok {
			t.Errorf("%s must be reported as skipped, got %v", name, got)
			continue
		}
		if like != wantLike {
			t.Errorf("%s: EngineLike = %v, want %v", name, like, wantLike)
		}
	}
	// Dotfiles and directories are not engine candidates at all: an
	// .editorconfig or a data directory must never look like a lost engine.
	for _, absent := range []string{".hidden", "subdir"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s must not be reported as skipped at all: %v", absent, got)
		}
	}
}

// TestLoadDirStillStopsAtTheFirstProblem is the PRESERVE half: doctor got a
// new lens on the directory, but every existing caller of LoadDir must see
// exactly the behaviour it saw before, including which error comes back.
func TestLoadDirStillStopsAtTheFirstProblem(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf": goodConf,
		"aaa.conf":  "NAME=a\nTYPE=command\nSTART=x\nHEALTH=http://a\nTYPO=1\n",
		"zzz.conf":  "NAME=z\nTYPE=command\nSTART=z\nHEALTH=http://z\nWAIT=0\n",
	})
	_, err := LoadDir(dir)
	if err == nil {
		t.Fatal("a broken file must still fail LoadDir")
	}
	if !strings.Contains(err.Error(), "aaa.conf") || !strings.Contains(err.Error(), "TYPO") {
		t.Errorf("LoadDir must still return the first problem in file order, got %v", err)
	}
	if strings.Contains(err.Error(), "zzz.conf") {
		t.Errorf("LoadDir must return one error, not the joined list: %v", err)
	}

	// Docs and backups still load as the ignorable things they are.
	ok := dirWith(t, map[string]string{
		"good.conf":  goodConf,
		"README.md":  "# docs\nNAME=\"not-an-engine\"\n",
		"edit.conf~": misnamedEngine,
		".hidden":    misnamedEngine,
	})
	reg, err := LoadDir(ok)
	if err != nil {
		t.Fatalf("documentation must still load as nothing: %v", err)
	}
	if got := reg.Names(); len(got) != 1 || got[0] != "good" {
		t.Errorf("Names = %v, want only good", got)
	}

	// The duplicate-NAME refusal survives the refactor.
	dup := dirWith(t, map[string]string{"a.conf": goodConf, "b.conf": goodConf})
	if _, err := LoadDir(dup); err == nil || !strings.Contains(err.Error(), "already declared") {
		t.Errorf("duplicate NAME must still be refused, got %v", err)
	}

	// A missing directory is still the typed os.ErrNotExist callers branch on.
	if _, err := LoadDir(filepath.Join(t.TempDir(), "nope")); !os.IsNotExist(err) {
		t.Errorf("a missing dir must still report os.ErrNotExist, got %v", err)
	}
	if _, err := ScanDir(filepath.Join(t.TempDir(), "nope")); !os.IsNotExist(err) {
		t.Errorf("ScanDir must pass the directory error through unchanged, got %v", err)
	}
}

// TestEngineLikeRefusesToSniffUnboundedFiles bounds the content peek: an
// engines dir is small, but doctor must not read a megabyte of some stray
// file (or a device-shaped surprise) just to guess.
func TestEngineLikeRefusesToSniffUnboundedFiles(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"huge": strings.Repeat("NAME=x\n", 200000), // 1.6 MB of engine-looking noise
	})
	scan, err := ScanDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, sk := range scan.Skipped {
		if sk.Name == "huge" && sk.EngineLike {
			t.Errorf("a file past the sniff bound must not be read as an engine: %v", sk)
		}
	}
}
