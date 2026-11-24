package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxEngineFileBytes bounds the content peek that classifies a skipped file.
// An engine .conf is a dozen short lines; anything bigger is a log, a dump or
// somebody's data directory, and doctor must not read it just to guess. The
// bound is enforced twice: on the size the directory reports and on the bytes
// actually read, so neither a lying stat nor a file that grows under the read
// can turn the peek into an unbounded read.
const maxEngineFileBytes = 64 << 10

// errNotRegular is what openable reports for a directory entry that cardo may
// not open: a FIFO, a socket, a device node. Reading one of those blocks until
// something on the other end answers, and an engines directory is exactly the
// place a stray named pipe shows up (a launcher that was pointed at a file
// where an engine config was expected). Every verb goes through this scan, so
// one open here used to be a hang with no timeout and no message.
var errNotRegular = errors.New("not a regular file")

// SkippedFile is one entry of an engines directory that LoadDir never reads:
// documentation that lives beside engines, an editor backup, an engine whose
// file lost its .conf suffix, or a file cardo must not open at all.
type SkippedFile struct {
	// Name is the file name as it appears in the directory.
	Name string
	// EngineLike reports content that reads like an engine .conf, which is
	// how a misnamed `tensorfold` (no suffix, real keys inside) is told apart
	// from the README that is meant to sit next to the engines.
	EngineLike bool
	// Backup marks an editor backup (`name~`), which ADR-0006 ignores by
	// design: it is named so the operator can clean it up, never so it can be
	// renamed into the registry.
	Backup bool
	// Special marks an entry that is not a regular file at all — a FIFO, a
	// socket, a device node. cardo never opens one of these: reading a named
	// pipe blocks until somebody opens the other end, which would hand an
	// engines directory the power to wedge every verb. Its content is
	// therefore not inspected, and nothing may be claimed about it.
	Special bool
	// Mode is the file mode of a special entry, so the report line can say
	// what kind of thing it found.
	Mode os.FileMode
	// DeclaresName is the NAME an engine-like skipped file declares when a
	// registered engine already owns it. Renaming such a file registers
	// nothing (it collides), so its remedy cannot be a rename. It is empty for
	// every other skipped file, and is never set for a file cardo could not
	// read: nothing is claimed about content that was not read.
	DeclaresName string
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
	// Skipped is every file ScanDir did not read as an engine, in file order.
	// An entry cardo could not open is in here with Special set, never opened.
	Skipped []SkippedFile
}

// Err is the answer a verb that acts on engines has to respect: the first
// file it cannot trust. LoadDir is defined in terms of it, so a caller that
// reads the scan itself (list wants the skipped entries too) cannot disagree
// with LoadDir about whether the directory is usable.
func (s *DirScan) Err() error {
	if len(s.Problems) > 0 {
		return s.Problems[0]
	}
	return nil
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
			// The name is what makes a file an engine, but it is not what
			// makes it safe to open: a named pipe called broken.conf is a
			// hang waiting to happen, so it is a Problem instead of a read.
			path := filepath.Join(dir, name)
			info, err := openable(path)
			switch {
			case errors.Is(err, errNotRegular):
				scan.Problems = append(scan.Problems, notARegularFile(name, info.mode))
			case err != nil:
				scan.Problems = append(scan.Problems, err)
			default:
				confs = append(confs, name)
			}
		default:
			scan.Skipped = append(scan.Skipped, classifySkipped(dir, name))
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
	markNameCollisions(scan)
	return scan, nil
}

// classifySkipped decides what a non-.conf entry is, and only ever opens the
// ones it may open: the special files are named and left alone.
func classifySkipped(dir, name string) SkippedFile {
	sk := SkippedFile{Name: name, Backup: strings.HasSuffix(name, "~")}
	info, err := openable(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, errNotRegular) {
			sk.Special = true
			sk.Mode = info.mode
		}
		// An unreadable or vanished entry is still reported as skipped;
		// whether it looked like an engine is a detail doctor cannot act on,
		// and nothing here may claim content it could not read.
		return sk
	}
	peek := peekEngineContent(filepath.Join(dir, name), info)
	if peek == nil {
		return sk
	}
	sk.EngineLike = peek.engineLike()
	sk.DeclaresName = peek.name
	return sk
}

// entryInfo is what cardo may conclude about an engines-directory entry
// without opening it: the mode it reports (so a refusal can name the kind of
// file it found) and the size it reports (so a peek can bound itself before
// the first read).
type entryInfo struct {
	mode os.FileMode
	size int64
}

// openable stats path the way cardo has to: following a symlink to what it
// points at (a symlinked .conf is a normal way to share one engine file) and
// refusing everything that is not a regular file. Callers may open the path
// only when the error is nil; the returned info is valid for both the nil and
// the errNotRegular case.
func openable(path string) (entryInfo, error) {
	st, err := os.Stat(path)
	if err != nil {
		return entryInfo{}, err
	}
	info := entryInfo{mode: st.Mode(), size: st.Size()}
	if !st.Mode().IsRegular() {
		return info, errNotRegular
	}
	return info, nil
}

// notARegularFile is the Problem line for an entry whose name says engine and
// whose inode says otherwise.
func notARegularFile(name string, mode os.FileMode) error {
	return fmt.Errorf("%s: %s: cardo will not open it", name, SpecialFileDescription(mode))
}

// SpecialFileDescription is the operator-facing line for an entry cardo would
// not open. It is exported because every verb that lists the engines directory
// has to say the same thing about it: a file that was skipped without being
// read is invisible unless the tool that skipped it says so.
func SpecialFileDescription(mode os.FileMode) string {
	return fmt.Sprintf("special file, not inspected: it is a %s", specialKind(mode))
}

// specialKind names the file shapes cardo must never open, in the words an
// operator can act on.
func specialKind(m os.FileMode) string {
	switch {
	case m&os.ModeNamedPipe != 0:
		return "named pipe (fifo)"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeCharDevice != 0:
		return "character device"
	case m&os.ModeDevice != 0:
		return "block device"
	default:
		return "special file"
	}
}

// markNameCollisions keeps, on each engine-like skipped file, only the NAME
// that a loaded engine already owns. It is the fact that turns "rename it"
// into the wrong advice: a copy of a file that is already registered collides
// instead of registering, so naming the collision is the whole report.
func markNameCollisions(scan *DirScan) {
	for i := range scan.Skipped {
		sk := &scan.Skipped[i]
		if sk.EngineLike && scan.Registry.Engines[sk.DeclaresName] == nil {
			sk.DeclaresName = ""
		}
	}
}

// docNames and docExts are the documentation that is meant to live beside
// engines (examples/ ships a README next to its two .conf files). They are
// never reported as a lost engine, however many KEY=value lines a code sample
// inside them quotes. Prose formats only: the spec names .md, .txt and .rst,
// and .markdown/.org are the same kind of file under other names.
//
// Config formats are deliberately NOT on this list. An `engine.yaml` is an
// engine that lost its extension far more often than it is documentation, and
// a doc list that swallowed .json/.yaml/.yml/.toml/.html/.adoc silently
// dropped the engines written in them; now they fall through to the content
// check, where an engine among them is named and a foreign config file stays
// quiet on its own merits (it declares none of our keys).
var (
	docNames = []string{"readme", "license", "licence", "copying", "notice", "changelog", "contributing"}
	docExts  = map[string]bool{
		".md": true, ".markdown": true, ".txt": true, ".rst": true, ".org": true,
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

// essentialKeys are the keys only an engine writes. One of them, plus a second
// known key, is what separates "this file configures an engine" from "this
// file happens to assign a variable that shares a name with one of ours".
var essentialKeys = map[string]bool{"HEALTH": true, "TYPE": true, "START": true}

// contentPeek is the bounded look at a skipped file's opening bytes: the
// distinct keys this format knows, whether an engine-only key is among them,
// and the NAME it declares.
type contentPeek struct {
	keys      map[string]bool
	essential bool
	name      string
}

// engineLike is the claim doctor prints. It takes two distinct known keys and
// at least one key only an engine writes, because a launcher script that sets
// NAME= and SLOT= for its own purposes is not an engine, and neither is a lone
// stray key in somebody's notes. A false here is cheap (a lost engine goes
// unmentioned); a true sends the operator hunting for a file to act on.
func (p *contentPeek) engineLike() bool {
	return len(p.keys) >= 2 && p.essential
}

// peekEngineContent reads at most maxEngineFileBytes of a file cardo has
// already established is a regular file, and reports what its lines declare.
// A documentation file, a file too big to be an engine config, and a file that
// cannot be read all yield no peek: nothing is claimed about them.
//
// It deliberately does not parse the file (a misnamed engine is often also
// broken, and the point is only to say "you have a file here that looks like
// an engine and cardo cannot load it").
func peekEngineContent(path string, info entryInfo) *contentPeek {
	if isDoc(filepath.Base(path)) {
		return nil
	}
	if info.size > maxEngineFileBytes {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	// The LimitReader is the bound that holds even if the stat lied (a file
	// that grew in between, or one on a filesystem that reports 0): the peek
	// reads this many bytes and no more.
	b, err := io.ReadAll(io.LimitReader(f, maxEngineFileBytes))
	if err != nil {
		return nil
	}
	peek := &contentPeek{keys: map[string]bool{}}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if !knownKeys[key] {
			continue
		}
		peek.keys[key] = true
		if essentialKeys[key] {
			peek.essential = true
		}
		if key == "NAME" {
			// Normalised exactly the way Parse does, so the NAME a skipped
			// file declares means the same NAME the registry holds.
			peek.name = parseValue(strings.TrimSpace(line[eq+1:]))
		}
	}
	return peek
}
