// Package config parses cardo's flat per-engine .conf files and validates
// them per ADR-0006: one file per engine, KEY="value" lines, strict key set.
package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Type is the engine type declared in a .conf file. It is an explicit enum
// (ADR-0006): unknown values fail validation, there is no catch-all.
type Type string

const (
	TypeDocker  Type = "docker"
	TypeCompose Type = "compose"
	TypeCommand Type = "command"
)

// KeyError reports a specific config problem with its file, line and key.
type KeyError struct {
	File   string
	Line   int
	Key    string
	Reason string
}

func (e *KeyError) Error() string {
	loc := e.File
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", e.File, e.Line)
	}
	return fmt.Sprintf("%s: %s: %s", loc, e.Key, e.Reason)
}

// Selector is a parsed BUSY/PROGRESS value selector (ADR-0002): a root "$"
// plus a dot path of field names and integer array indices, with ".length"
// accepted on arrays.
type Selector struct {
	path []string
}

// ParseSelector parses the selector part of a BUSY/PROGRESS value. A selector
// without the "$." prefix is treated as a top-level field name.
func ParseSelector(s string) (*Selector, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty selector")
	}
	if !strings.HasPrefix(s, "$.") {
		s = "$." + s
	}
	parts := strings.Split(strings.TrimPrefix(s, "$."), ".")
	for _, p := range parts {
		if p == "" || strings.ContainsAny(p, " \t\r\n") {
			return nil, fmt.Errorf("invalid selector path %q", s)
		}
	}
	return &Selector{path: parts}, nil
}

// String returns the canonical form, always prefixed with "$.".
func (s *Selector) String() string {
	return "$." + strings.Join(s.path, ".")
}

// Path returns the path tokens after the "$." root.
func (s *Selector) Path() []string {
	return append([]string(nil), s.path...)
}

// Engine is one parsed engine configuration.
type Engine struct {
	File           string
	Name           string
	Description    string
	Type           Type
	Container      string
	ComposeProject string
	Start          string
	Health         string
	Busy           *Probe
	Progress       *Probe
	Drain          int
	Wait           int
	Slot           int
	PreUp          string
	PostDown       string

	numErrs [][2]string
}

// Probe is a URL plus a JSON field selector, the shape of BUSY and PROGRESS.
type Probe struct {
	URL      string
	Selector *Selector
}

const (
	defaultDrain = 60
	defaultWait  = 120
)

var knownKeys = map[string]bool{
	"NAME": true, "DESCRIPTION": true, "TYPE": true,
	"CONTAINER": true, "COMPOSE_PROJECT": true, "START": true,
	"HEALTH": true, "BUSY": true, "PROGRESS": true,
	"DRAIN": true, "WAIT": true, "SLOT": true,
	"PRE_UP": true, "POST_DOWN": true,
}

// Parse reads one .conf body and returns the engine it declares. Syntax
// errors (unknown or duplicated keys) fail immediately; semantic validation
// (types, enums, required keys) is Validate.
func Parse(r io.Reader, file string) (*Engine, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	// asWritten keeps each value before comment stripping. It is what lets a
	// probe tell "no value at all" from "a value the comment rule ate".
	asWritten := map[string]string{}
	order := []string{}
	for i, line := range strings.Split(string(raw), "\n") {
		n := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			return nil, &KeyError{File: file, Line: n, Reason: "not a KEY=value line"}
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if !knownKeys[key] {
			return nil, &KeyError{File: file, Line: n, Key: key, Reason: "unknown key"}
		}
		if _, dup := kv[key]; dup {
			return nil, &KeyError{File: file, Line: n, Key: key, Reason: "duplicate key"}
		}
		// A quoted value runs to its closing quote and nothing after that
		// quote is part of the value, so `KEY="v" # comment` cannot leak the
		// comment into the value. An unquoted value keeps its glued '#'
		// (the BUSY/PROGRESS url#selector separator) and loses only a
		// whitespace-preceded comment (ADR-0006).
		val = parseValue(val)
		kv[key] = val
		asWritten[key] = strings.TrimSpace(line[eq+1:])
		order = append(order, key)
	}
	e := &Engine{File: file, Drain: defaultDrain, Wait: defaultWait, Slot: 0}
	get := func(k string) string { return kv[k] }
	if v, ok := kv["NAME"]; ok {
		e.Name = v
	}
	e.Description = get("DESCRIPTION")
	if v, ok := kv["TYPE"]; ok {
		e.Type = Type(v)
	}
	e.Container = get("CONTAINER")
	e.ComposeProject = get("COMPOSE_PROJECT")
	e.Start = get("START")
	e.Health = get("HEALTH")
	e.PreUp = get("PRE_UP")
	e.PostDown = get("POST_DOWN")
	if v, ok := kv["DRAIN"]; ok {
		e.deferNum(&e.Drain, "DRAIN", v)
	}
	if v, ok := kv["WAIT"]; ok {
		e.deferNum(&e.Wait, "WAIT", v)
	}
	if v, ok := kv["SLOT"]; ok {
		e.deferNum(&e.Slot, "SLOT", v)
	}
	parseProbe := func(key string, dst **Probe) error {
		v, ok := kv[key]
		if !ok {
			return nil
		}
		if v == "" {
			// Two very different lines end up here: `BUSY=` (the config says
			// there is no probe, which is fine) and `BUSY=#$.count`, where the
			// '#' that separates url from selector opens what the comment rule
			// reads as a comment, so the whole value disappears. Dropping the
			// second one silently is the dangerous outcome: an absent BUSY
			// probe lets `cardo up` swap over an engine that is still serving
			// requests. Only the text as written tells them apart.
			if sel, bare := bareSelectorValue(asWritten[key]); bare {
				return missingProbeURL(file, key, asWritten[key], sel)
			}
			return nil
		}
		u, sel, found := strings.Cut(v, "#")
		if !found {
			return &KeyError{File: file, Key: key, Reason: "missing #selector part"}
		}
		if strings.TrimSpace(u) == "" {
			return missingProbeURL(file, key, v, sel)
		}
		s, err := ParseSelector(sel)
		if err != nil {
			return &KeyError{File: file, Key: key, Reason: err.Error()}
		}
		*dst = &Probe{URL: u, Selector: s}
		return nil
	}
	if err := parseProbe("BUSY", &e.Busy); err != nil {
		return nil, err
	}
	if err := parseProbe("PROGRESS", &e.Progress); err != nil {
		return nil, err
	}
	return e, nil
}

// bareSelectorValue reports whether a value as written is nothing but a
// selector: an empty part before the '#'. An empty or absent value is not one
// (that is "no probe"), and neither is a value with a URL in front.
func bareSelectorValue(raw string) (string, bool) {
	u, sel, found := strings.Cut(strings.TrimSpace(raw), "#")
	if !found || strings.TrimSpace(u) != "" {
		return "", false
	}
	return sel, true
}

// missingProbeURL is the rejection of a BUSY/PROGRESS value with no URL in
// front of the selector. It names the URL that is missing rather than letting
// the key quietly disappear from the parsed engine.
func missingProbeURL(file, key, value, sel string) error {
	return &KeyError{File: file, Key: key, Reason: fmt.Sprintf(
		"missing URL before the '#': %q selects %q but says nothing to ask; write http://host:port/path#%s",
		value, sel, sel)}
}

// ParseFile reads and parses the .conf file at path.
func ParseFile(path string) (*Engine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, filepath.Base(path))
}

// Validate applies the semantic rules that parsing deliberately defers:
// enum TYPE, required-by-type keys, URL shape, non-negative numbers. It
// accumulates every finding into one error so doctor can show them all.
func (e *Engine) Validate() error {
	var errs []error
	kerr := func(key, reason string) error {
		return &KeyError{File: e.File, Key: key, Reason: reason}
	}
	errs = append(errs, e.validateNums(kerr)...)
	errs = append(errs, e.validateRanges(kerr)...)
	switch e.Type {
	case TypeDocker:
		if e.Container == "" {
			return kerr("CONTAINER", "required for TYPE=docker")
		}
	case TypeCompose:
		if e.ComposeProject == "" {
			return kerr("COMPOSE_PROJECT", "required for TYPE=compose")
		}
	case TypeCommand:
		if e.Start == "" {
			return kerr("START", "required for TYPE=command")
		}
	default:
		return kerr("TYPE", fmt.Sprintf("unknown TYPE %q (want docker, compose or command)", e.Type))
	}
	if e.Name == "" {
		errs = append(errs, kerr("NAME", "required"))
	}
	if _, err := url.ParseRequestURI(e.Health); err != nil || !strings.Contains(e.Health, "://") {
		errs = append(errs, kerr("HEALTH", "not a URL: "+e.Health))
	}
	if len(errs) == 1 {
		return errs[0]
	}
	if len(errs) > 1 {
		return errors.Join(errs...)
	}
	return nil
}

// deferNum records a numeric conversion to be validated later, so one bad
// key does not hide the rest.
func (e *Engine) deferNum(dst *int, key, v string) {
	n, err := strconv.Atoi(v)
	if err != nil {
		e.numErrs = append(e.numErrs, [2]string{key, err.Error()})
		return
	}
	*dst = n
}

func (e *Engine) validateNums(kerr func(string, string) error) []error {
	var out []error
	for _, ne := range e.numErrs {
		out = append(out, kerr(ne[0], "not a number: "+ne[1]))
	}
	return out
}

// validateRanges applies the value ranges a successful conversion does not
// cover. DRAIN may be 0 (ask BUSY once and refuse to swap while busy) but
// never negative; WAIT bounds the stall window so it must leave a positive
// budget; SLOT is a non-negative GPU index.
func (e *Engine) validateRanges(kerr func(string, string) error) []error {
	var out []error
	if e.Drain < 0 {
		out = append(out, kerr("DRAIN", "must be >= 0 (0 asks BUSY once and refuses to swap while busy)"))
	}
	if e.Wait <= 0 {
		out = append(out, kerr("WAIT", "must be > 0 (the stall timeout in seconds)"))
	}
	if e.Slot < 0 {
		out = append(out, kerr("SLOT", "must be >= 0"))
	}
	return out
}

// Registry is a loaded engines directory: engines by name.
type Registry struct {
	Engines map[string]*Engine
}

// Get returns the named engine.
func (r *Registry) Get(name string) (*Engine, bool) {
	e, ok := r.Engines[name]
	return e, ok
}

// Names returns engine names sorted.
func (r *Registry) Names() []string {
	ns := make([]string, 0, len(r.Engines))
	for n := range r.Engines {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

// LoadDir loads every engine .conf in dir, skipping dotfiles, editor
// backups and non-.conf files (documentation may live alongside engines),
// and fails on duplicate NAME across files. An empty directory is a valid,
// empty registry.
func LoadDir(dir string) (*Registry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	reg := &Registry{Engines: map[string]*Engine{}}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		e, err := ParseFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if err := e.Validate(); err != nil {
			return nil, err
		}
		if _, dup := reg.Engines[e.Name]; dup {
			return nil, fmt.Errorf("%s: NAME %q already declared in another file", e.File, e.Name)
		}
		reg.Engines[e.Name] = e
	}
	return reg, nil
}

// ErrNotFound is returned when no engine with the requested name exists.
var ErrNotFound = errors.New("engine not found")

// parseValue resolves the right-hand side of a KEY=value line. A value that
// opens with a quote runs to the next quote and everything after it is noise
// (a trailing comment, stray whitespace, whatever); an unquoted value keeps
// its glued '#' because that is the url#selector separator, and loses only a
// comment introduced by whitespace.
func parseValue(v string) string {
	if strings.HasPrefix(v, `"`) {
		if end := strings.Index(v[1:], `"`); end >= 0 {
			return v[1 : end+1]
		}
		// An unterminated quote: take what is there rather than eating the
		// rest of the line as a comment.
		return strings.TrimSpace(strings.TrimPrefix(v, `"`))
	}
	v = stripComment(v)
	v = strings.TrimSpace(v)
	return strings.Trim(v, `"`)
}

// stripComment removes a trailing comment from an unquoted value. Quoting is
// optional (ADR-0006), and an unquoted BUSY/PROGRESS legitimately contains
// '#': the url#selector separator. So only a '#' that opens a comment (at the
// start, or preceded by whitespace) starts one; a '#' glued between characters
// is part of the value.
func stripComment(v string) string {
	if strings.HasPrefix(v, `"`) {
		return v
	}
	for i := 0; i < len(v); i++ {
		if v[i] != '#' {
			continue
		}
		if i == 0 || v[i-1] == ' ' || v[i-1] == '\t' {
			return v[:i]
		}
	}
	return v
}
