package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// scanIn runs a scan that must never block, and fails the test instead of
// hanging the suite if it does. A named pipe in an engines directory is the
// case this guards: opening one for reading waits for a writer, and nothing
// in this file opens one.
func scanIn(t *testing.T, dir string) *DirScan {
	t.Helper()
	type result struct {
		scan *DirScan
		err  error
	}
	done := make(chan result, 1)
	go func() {
		s, err := ScanDir(dir)
		done <- result{s, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("ScanDir: %v", res.err)
		}
		return res.scan
	case <-time.After(5 * time.Second):
		t.Fatal("ScanDir blocked on an engines directory it should only have listed")
	}
	return nil
}

// mkfifo makes a named pipe in dir, the file shape that used to be opened
// just to guess what it was.
func mkfifo(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Skipf("mkfifo unavailable here: %v", err)
	}
	return p
}

// TestScanDirNeverOpensASpecialFile is the critical one: an operator (or a
// buggy launcher) leaves a named pipe in the engines directory, and the
// content peek opened it. Reading a FIFO blocks until somebody opens the
// other end, so `cardo list`, `cardo up` and `cardo doctor` all hung with no
// timeout and no message. A special file is now reported as skipped and is
// never opened.
func TestScanDirNeverOpensASpecialFile(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf": goodConf,
		"README.md": "# engines\n",
	})
	mkfifo(t, dir, "engine.pipe")

	scan := scanIn(t, dir)

	if _, ok := scan.Registry.Get("good"); !ok {
		t.Errorf("a stray fifo must not cost the operator the engines that load: %v", scan.Registry.Names())
	}
	var pipe, doc *SkippedFile
	for i := range scan.Skipped {
		switch scan.Skipped[i].Name {
		case "engine.pipe":
			pipe = &scan.Skipped[i]
		case "README.md":
			doc = &scan.Skipped[i]
		}
	}
	if pipe == nil {
		t.Fatalf("the named pipe must be reported as skipped: %+v", scan.Skipped)
	}
	if !pipe.Special {
		t.Errorf("a fifo must be classified as a special file, never opened: %+v", *pipe)
	}
	if pipe.EngineLike {
		t.Errorf("a file cardo never opened cannot be claimed to hold engine content: %+v", *pipe)
	}
	if pipe.Mode&os.ModeNamedPipe == 0 {
		t.Errorf("the report line needs the mode to say what it found: %+v", *pipe)
	}
	if doc == nil || doc.Special {
		t.Errorf("an ordinary document is skipped but not special: %+v", scan.Skipped)
	}

	// LoadDir goes through the same scan, so every verb is covered by it.
	done := make(chan error, 1)
	go func() {
		_, err := LoadDir(dir)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("LoadDir must load the engines beside a fifo, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadDir blocked on a named pipe in the engines directory")
	}
}

// TestScanDirRefusesToOpenASpecialFileNamedConf closes the other half of the
// same hole: the entry that gets opened without any classification is the one
// whose name ends in .conf, so a fifo named like an engine has to be refused
// as a problem rather than read.
func TestScanDirRefusesToOpenASpecialFileNamedConf(t *testing.T) {
	dir := dirWith(t, map[string]string{"good.conf": goodConf})
	mkfifo(t, dir, "broken.conf")

	scan := scanIn(t, dir)
	if len(scan.Problems) != 1 {
		t.Fatalf("the fifo masquerading as an engine must be one problem, got %v", scan.Problems)
	}
	if !strings.Contains(scan.Problems[0].Error(), "broken.conf") ||
		!strings.Contains(scan.Problems[0].Error(), "special file, not inspected") {
		t.Errorf("the problem must name the file and say it was not opened: %v", scan.Problems[0])
	}
	if _, ok := scan.Registry.Get("good"); !ok {
		t.Errorf("a broken neighbour must not hide the engine that loads: %v", scan.Registry.Names())
	}
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "broken.conf") {
		t.Errorf("LoadDir must still refuse a directory it cannot trust, got %v", err)
	}
}

// TestEngineLikeNeedsEngineKeysNotJustAnyKeys is the false-positive half of
// the misnamed-engine rule: a shell script beside the engines legitimately
// sets NAME= and SLOT= for its own purposes. Claiming engine content on one
// or two incidental keys is what makes doctor cry wolf, so it takes at least
// two distinct known keys and one of the keys only an engine writes.
func TestEngineLikeNeedsEngineKeysNotJustAnyKeys(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf": goodConf,
		// A launcher script: two known keys, neither of them an engine key.
		"start.sh": "#!/bin/sh\nNAME=worker\nSLOT=3\nexec ./serve\n",
		// One engine key alone is not enough either.
		"single":    "HEALTH=http://127.0.0.1:9/health\n",
		"onlytype":  "TYPE=command\n",
		"onlystart": "START=./run\n",
		// Two known keys, one of them an engine key: an engine that lost its
		// suffix.
		"tensorfold": "NAME=tensorfold\nSTART=./serve --port 8080\n",
		// An engine key inside a comment is not a declaration.
		"notes": "# HEALTH=http://127.0.0.1:9/health\nSLOT=0\nOWNER=me\n",
	})
	scan := scanIn(t, dir)

	for name, want := range map[string]bool{
		"start.sh":   false,
		"single":     false,
		"onlytype":   false,
		"onlystart":  false,
		"tensorfold": true,
		"notes":      false,
	} {
		got, ok := skippedNamed(t, scan, name)
		if !ok {
			continue
		}
		if got.EngineLike != want {
			t.Errorf("%s: EngineLike = %v, want %v", name, got.EngineLike, want)
		}
	}
}

// skippedNamed looks one skipped file up, failing the test when the scan did
// not report it at all.
func skippedNamed(t *testing.T, scan *DirScan, name string) (*SkippedFile, bool) {
	t.Helper()
	for i := range scan.Skipped {
		if scan.Skipped[i].Name == name {
			return &scan.Skipped[i], true
		}
	}
	t.Errorf("%s must be reported as skipped: %+v", name, scan.Skipped)
	return nil, false
}

// TestScanMarksEditorBackupsWithoutRenamingThem pins the shape doctor needs:
// a `name~` backup is an editor's leftover that ADR-0006 ignores on purpose,
// so it is classified as a backup whether or not its content looks like an
// engine (it usually does — it is a copy of one).
func TestScanMarksEditorBackupsWithoutRenamingThem(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf":   goodConf,
		"good.conf~":  misnamedEngine,
		"scratch~":    "just a note\n",
		"tabby.conf~": misnamedEngine,
	})
	scan := scanIn(t, dir)

	for _, name := range []string{"good.conf~", "scratch~", "tabby.conf~"} {
		sk, ok := skippedNamed(t, scan, name)
		if !ok {
			continue
		}
		if !sk.Backup {
			t.Errorf("%s is an editor backup and must be named as one: %+v", name, *sk)
		}
	}
	if sk, ok := skippedNamed(t, scan, "good.conf~"); ok && !sk.EngineLike {
		t.Errorf("a backup of an engine still holds engine content: %+v", *sk)
	}
}

// TestScanFlagsASkippedEngineThatDeclaresARegisteredName gives doctor the
// fact it needs to stop telling the operator to rename a file that would only
// collide with the engine already loaded.
func TestScanFlagsASkippedEngineThatDeclaresARegisteredName(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf":     goodConf, // NAME=good
		"good.conf.bak": goodConf,
		"tensorfold":    misnamedEngine,
	})
	scan := scanIn(t, dir)

	if sk, ok := skippedNamed(t, scan, "good.conf.bak"); ok {
		if !sk.EngineLike {
			t.Errorf("a copy of a valid engine is engine content: %+v", *sk)
		}
		if sk.DeclaresName != "good" {
			t.Errorf("the collision must be recorded so the remedy is not a rename: %+v", *sk)
		}
	}
	if sk, ok := skippedNamed(t, scan, "tensorfold"); ok && sk.DeclaresName != "" {
		t.Errorf("tensorfold declares a free NAME, so a rename really is its remedy: %+v", *sk)
	}
}

// TestDocExtsLeaveConfigShapesToTheEngineCheck is the silent-engine fix:
// .json/.yaml/.yml/.toml/.html/.adoc were on the documentation list, so an
// engine saved as engine.yaml was skipped with nothing said about it. Those
// extensions now fall through to the content check, while real documentation
// stays quiet however many KEY=value lines a code sample quotes.
func TestDocExtsLeaveConfigShapesToTheEngineCheck(t *testing.T) {
	dir := dirWith(t, map[string]string{
		"good.conf":   goodConf,
		"engine.yaml": misnamedEngine,
		"engine.yml":  misnamedEngine,
		"engine.json": misnamedEngine,
		"engine.toml": misnamedEngine,
		"engine.html": misnamedEngine,
		"engine.adoc": misnamedEngine,
		// Real documentation, and foreign config shapes that hold no engine
		// keys: neither must be reported as a lost engine.
		"README.md":    "# engines\n\n```\nNAME=\"vllm\"\nTYPE=\"docker\"\n```\n",
		"notes.txt":    "Remember NAME=good is the resident.\n",
		"guide.rst":    "SLOT=0 is the first GPU.\n",
		"package.json": `{"name": "cardo", "version": "0.1.0"}`,
		"compose.yaml": "services:\n  demo:\n    image: busybox\n",
	})
	scan := scanIn(t, dir)

	for _, name := range []string{"engine.yaml", "engine.yml", "engine.json", "engine.toml", "engine.html", "engine.adoc"} {
		sk, ok := skippedNamed(t, scan, name)
		if !ok {
			continue
		}
		if !sk.EngineLike {
			t.Errorf("%s holds engine keys and must be named, not silently swallowed by the doc list: %+v", name, *sk)
		}
	}
	for _, name := range []string{"README.md", "notes.txt", "guide.rst", "package.json", "compose.yaml"} {
		sk, ok := skippedNamed(t, scan, name)
		if !ok {
			continue
		}
		if sk.EngineLike {
			t.Errorf("%s is not an engine and must stay quiet: %+v", name, *sk)
		}
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
