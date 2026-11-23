package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxEngineFileBytes bounds the content peek that classifies a skipped file.
// An engine .conf is a dozen short lines; anything bigger is a log, a dump or
// somebody's data directory, and doctor must not read it just to guess.
const maxEngineFileBytes = 64 << 10

// SkippedFile is one entry of an engines directory that LoadDir never reads:
// documentation that lives beside engines, an editor backup, or an engine
// whose file lost its .conf suffix.
type SkippedFile struct {
	// Name is the file name as it appears in the directory.
	Name string
	// EngineLike reports content that reads like an engine .conf, which is
	// how a misnamed `tensorfold` (no suffix, real keys inside) is told apart
	// from the README that is meant to sit next to the engines.
	EngineLike bool
}

// DirScan is one classified engines directory. LoadDir is the caller that
// only wants to act; doctor wants the whole picture, including the files that
// are quietly not engines, so both read come from here and cannot drift.
type DirScan struct {
	// Registry holds the engines that parsed and validated.
	Registry *Registry
	// Problems is one error per .conf file that did not, in file order. Each
	// error already names its own file, so a broken neighbour never hides the
	// next one.
	Problems []error
	// Skipped is every regular file LoadDir did not read, in file order.
	Skipped []SkippedFile
}

// ScanDir reads dir once and classifies every entry. A missing or unreadable
// directory is returned as the plain os error (callers branch on
// os.ErrNotExist); a bad file is a Problem, not a failure of the scan.
func ScanDir(dir string) (*DirScan, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	scan := &DirScan{Registry: &Registry{Engines: map[string]*Engine{}}}
	var confs []string
	// os.ReadDir sorts by name, so both lists come out in file order and a
	// report built from them is stable between runs.
	for _, ent := range entries {
		name := ent.Name()
		switch {
		case ent.IsDir() || strings.HasPrefix(name, "."):
			// A directory (model data beside the engines) and a dotfile
			// (.editorconfig, shell history leftovers) are not engine
			// candidates and are not worth a warning either.
			continue
		case strings.HasSuffix(name, ".conf"):
			confs = append(confs, name)
		default:
			sk := SkippedFile{Name: name}
			// An unreadable file is still reported as skipped; whether it
			// looked like an engine is a detail doctor cannot act on.
			if like, lerr := engineLike(filepath.Join(dir, name)); lerr == nil {
				sk.EngineLike = like
			}
			scan.Skipped = append(scan.Skipped, sk)
		}
	}
	for _, name := range confs {
		e, err := ParseFile(filepath.Join(dir, name))
		if err != nil {
			scan.Problems = append(scan.Problems, err)
			continue
		}
		if err := e.Validate(); err != nil {
			scan.Problems = append(scan.Problems, err)
			continue
		}
		if _, dup := scan.Registry.Engines[e.Name]; dup {
			scan.Problems = append(scan.Problems, fmt.Errorf("%s: NAME %q already declared in another file", e.File, e.Name))
			continue
		}
		scan.Registry.Engines[e.Name] = e
	}
	return scan, nil
}

// docNames and docExts are the documentation that is meant to live beside
// engines (examples/ ships a README next to its two .conf files). They are
// never reported as a lost engine, however many KEY=value lines a code sample
// inside them quotes.
var (
	docNames = []string{"readme", "license", "licence", "copying", "notice", "changelog", "contributing"}
	docExts  = map[string]bool{
		".md": true, ".markdown": true, ".txt": true, ".rst": true,
		".adoc": true, ".org": true, ".html": true, ".json": true,
		".yaml": true, ".yml": true, ".toml": true,
	}
)

// isDoc reports whether a skipped file is documentation rather than an engine
// that lost its suffix.
func isDoc(name string) bool {
	lower := strings.ToLower(name)
	for _, d := range docNames {
		if strings.HasPrefix(lower, d) {
			return true
		}
	}
	return docExts[strings.ToLower(filepath.Ext(lower))]
}

// engineLike reports whether the file at path holds engine content: a KEY
// this format knows, at the start of a line, with a value after the '='. It
// deliberately does not parse the file (a misnamed engine is often also
// broken, and the point is only to say "you have a file here that looks like
// an engine and cardo cannot load it").
func engineLike(path string) (bool, error) {
	if isDoc(filepath.Base(path)) {
		return false, nil
	}
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if st.IsDir() || st.Size() > maxEngineFileBytes {
		return false, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		if knownKeys[strings.TrimSpace(line[:eq])] {
			return true, nil
		}
	}
	return false, nil
}
