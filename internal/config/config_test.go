package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseExampleTensorfold(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "tensorfold.conf", `# TensorFold serving the NVIDIA Qwen3.8-Flash-Next FP4 checkpoint.
NAME="tensorfold-qwen"
DESCRIPTION="Qwen3.8-Flash-Next FP4 via TensorFold v1.1.4 (docker)"
TYPE="docker"
CONTAINER="qwen38-flash-next-tf"
HEALTH="http://localhost:8888/health"
# TensorFold reports live connections in its health JSON: $.live.connections
BUSY="http://localhost:8888/health#$.live.connections"
DRAIN="0"
WAIT="60"
SLOT="0"
`)

	e, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "tensorfold-qwen" {
		t.Errorf("Name = %q", e.Name)
	}
	if e.Type != TypeDocker {
		t.Errorf("Type = %q", e.Type)
	}
	if e.Container != "qwen38-flash-next-tf" {
		t.Errorf("Container = %q", e.Container)
	}
	if e.Health != "http://localhost:8888/health" {
		t.Errorf("Health = %q", e.Health)
	}
	if e.Busy == nil || e.Busy.URL != "http://localhost:8888/health" {
		t.Errorf("Busy = %+v", e.Busy)
	} else if got, want := e.Busy.Selector.String(), "$.live.connections"; got != want {
		t.Errorf("Busy.Selector = %q, want %q", got, want)
	}
	if e.Drain != 0 || e.Wait != 60 || e.Slot != 0 {
		t.Errorf("Drain/Wait/Slot = %d/%d/%d", e.Drain, e.Wait, e.Slot)
	}
	if e.PreUp != "" || e.PostDown != "" {
		t.Errorf("hooks should default empty: %q %q", e.PreUp, e.PostDown)
	}
}

func TestParseQuotingAndComments(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.conf", `
# full-line comment
NAME=plain-name      # trailing comment
DESCRIPTION="quoted value # not a comment"
`)
	e, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "plain-name" {
		t.Errorf("Name = %q", e.Name)
	}
	if e.Description != "quoted value # not a comment" {
		t.Errorf("Description = %q", e.Description)
	}
}

func TestParseUnknownKeyFails(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.conf", "NAME=a\nTYPE=command\nSTART=x\nHEALTH=http://x\nPREUP=typo\n")
	_, err := ParseFile(p)
	if err == nil {
		t.Fatal("want error for unknown key PREUP")
	}
	if !strings.Contains(err.Error(), "PREUP") || !strings.Contains(err.Error(), "a.conf") {
		t.Errorf("error must name the key and the file: %v", err)
	}
	var keyErr *KeyError
	if !errors.As(err, &keyErr) {
		t.Errorf("want *KeyError, got %T: %v", err, err)
	}
}

func TestParseDuplicateKeyFails(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.conf", "NAME=a\nNAME=b\n")
	_, err := ParseFile(p)
	if err == nil || !strings.Contains(err.Error(), "NAME") {
		t.Fatalf("want duplicate NAME error, got %v", err)
	}
}

func TestValidateRequiredByType(t *testing.T) {
	base := func(typ string) string {
		return "NAME=a\nTYPE=" + typ + "\nHEALTH=http://x\n"
	}
	cases := []struct {
		typ, extra, wantErr string
	}{
		{"docker", "", "CONTAINER"},
		{"docker", "CONTAINER=c\n", ""},
		{"command", "", "START"},
		{"command", "START=/bin/x\n", ""},
		{"compose", "", "COMPOSE_PROJECT"},
		{"compose", "COMPOSE_PROJECT=p\n", ""},
		{"bad", "", "TYPE"},
	}
	for _, c := range cases {
		e, err := Parse(strings.NewReader(base(c.typ)+c.extra), "a.conf")
		if err != nil {
			t.Fatalf("parse %q: %v", c.typ, err)
		}
		err = e.Validate()
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.typ, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: want error naming %s, got %v", c.typ, c.wantErr, err)
		}
	}
}

func TestValidateNumbersAndURLs(t *testing.T) {
	e, err := Parse(strings.NewReader("NAME=a\nTYPE=command\nSTART=x\nHEALTH=nonsense\nWAIT=abc\n"), "a.conf")
	if err != nil {
		t.Fatal(err)
	}
	err = e.Validate()
	if err == nil || !strings.Contains(err.Error(), "HEALTH") {
		t.Fatalf("want HEALTH URL error, got %v", err)
	}
	if !strings.Contains(err.Error(), "WAIT") {
		t.Errorf("error should also name WAIT: %v", err)
	}
}

// TestParseUnquotedValueKeepsEmbeddedHash pins ADR-0006's optional quoting:
// an unquoted BUSY/PROGRESS value must survive its url#selector separator.
// Only a '#' that starts a comment (preceded by whitespace) is a comment.
func TestParseUnquotedValueKeepsEmbeddedHash(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.conf", `
NAME=plain
TYPE=command
START=/bin/serve --port 8080
HEALTH=http://127.0.0.1:8080/health
BUSY=http://127.0.0.1:8080/metrics#$.queue.pending.length   # live jobs
PROGRESS=http://127.0.0.1:8080/loading#progress # one word comment
`)
	e, err := ParseFile(p)
	if err != nil {
		t.Fatalf("unquoted url#selector must parse: %v", err)
	}
	if e.Busy == nil {
		t.Fatal("Busy is nil: the '#' separator was eaten as a comment")
	}
	if e.Busy.URL != "http://127.0.0.1:8080/metrics" {
		t.Errorf("Busy.URL = %q", e.Busy.URL)
	}
	if got := e.Busy.Selector.String(); got != "$.queue.pending.length" {
		t.Errorf("Busy.Selector = %q", got)
	}
	if e.Progress == nil {
		t.Fatal("Progress is nil: the '#' separator was eaten as a comment")
	}
	if e.Progress.URL != "http://127.0.0.1:8080/loading" {
		t.Errorf("Progress.URL = %q", e.Progress.URL)
	}
	if got := e.Progress.Selector.String(); got != "$.progress" {
		t.Errorf("Progress.Selector = %q", got)
	}
	// The real comment still has to go.
	if e.Start != "/bin/serve --port 8080" {
		t.Errorf("Start = %q", e.Start)
	}
}

// TestParseTrailingCommentStillStripped: the other side of the rule — a
// whitespace-preceded '#' is still a comment, and a value with no '#' at all
// is untouched.
func TestParseTrailingCommentStillStripped(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.conf", "NAME=a\nTYPE=command\nSTART=x\nHEALTH=http://x\nDESCRIPTION=plain value\t\t# tab-prefixed comment\n")
	e, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Description != "plain value" {
		t.Errorf("Description = %q, want the comment stripped", e.Description)
	}
}

// TestParseQuotedValueIgnoresTrailingComment pins the quoted half of
// ADR-0006's optional quoting: once a value opens with a quote, its content
// runs to the next quote and everything after it is noise. The old rule kept
// `# the port is fixed by the slot` inside START, which then ran as arguments.
func TestParseQuotedValueIgnoresTrailingComment(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.conf", `
NAME=a
TYPE=command
START="/bin/serve --port 8080" # the port is fixed by the slot
HEALTH=http://127.0.0.1:8080/health
DESCRIPTION="quoted # keeps the hash"   	# and this one too
`)
	e, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Start != "/bin/serve --port 8080" {
		t.Errorf("Start = %q, want the quoted content only", e.Start)
	}
	if e.Description != "quoted # keeps the hash" {
		t.Errorf("Description = %q, want the quoted content with its own '#' kept", e.Description)
	}
}

// TestParseUnquotedValueStillStripsWhitespaceComment is the other side of the
// same rule and must not regress: an unquoted value keeps its glued '#'
// (the url#selector separator) and loses only a whitespace-preceded comment.
func TestParseUnquotedValueStillStripsWhitespaceComment(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "a.conf", "NAME=a\nTYPE=command\nSTART=/bin/serve # run it\nHEALTH=http://127.0.0.1:8080/health\nBUSY=http://127.0.0.1:8080/metrics#$.live.connections\n")
	e, err := ParseFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if e.Start != "/bin/serve" {
		t.Errorf("Start = %q, want the comment stripped", e.Start)
	}
	if e.Busy == nil || e.Busy.URL != "http://127.0.0.1:8080/metrics" {
		t.Errorf("Busy = %+v, want the glued '#' separator kept", e.Busy)
	}
}

// TestValidateNumberRanges pins the ADR-0006 range rules that a successful
// number conversion does not cover: DRAIN may be 0 (probe once and refuse),
// WAIT must leave a positive stall window, SLOT is a non-negative index.
func TestValidateNumberRanges(t *testing.T) {
	base := "NAME=a\nTYPE=command\nSTART=/bin/x\nHEALTH=http://x\n"
	cases := []struct {
		name    string
		line    string
		wantKey string
	}{
		{"drain negative", "DRAIN=-1\n", "DRAIN"},
		{"wait zero", "WAIT=0\n", "WAIT"},
		{"wait negative", "WAIT=-5\n", "WAIT"},
		{"slot negative", "SLOT=-1\n", "SLOT"},
		{"drain zero allowed", "DRAIN=0\n", ""},
		{"slot zero allowed", "SLOT=0\n", ""},
		{"wait positive allowed", "WAIT=1\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, err := Parse(strings.NewReader(base+c.line), "a.conf")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			err = e.Validate()
			if c.wantKey == "" {
				if err != nil {
					t.Errorf("%s must be accepted, got %v", c.line, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantKey) {
				t.Fatalf("want a %s range error, got %v", c.wantKey, err)
			}
			var keyErr *KeyError
			if !errors.As(err, &keyErr) {
				t.Errorf("range findings must be *KeyError, got %T: %v", err, err)
			}
		})
	}
}

// TestLoadDirRejectsOutOfRangeNumbers: the range rules apply to real files,
// not only to Validate called directly.
func TestLoadDirRejectsOutOfRangeNumbers(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.conf", "NAME=a\nTYPE=command\nSTART=/bin/a\nHEALTH=http://a\nWAIT=0\n")
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "WAIT") {
		t.Fatalf("WAIT=0 must be rejected on load, got %v", err)
	}
}

func TestLoadDirNamespacesAndIgnores(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.conf", "NAME=a\nTYPE=command\nSTART=/bin/a\nHEALTH=http://a\n")
	write(t, dir, ".hidden.conf", "NAME=h\n")
	write(t, dir, "edit.conf~", "NAME=b\n")
	write(t, dir, "README.md", "# docs may live alongside engine confs\n")
	if _, err := LoadDir(dir); err != nil {
		t.Fatal(err)
	}

	write(t, dir, "dup.conf", "NAME=a\nTYPE=command\nSTART=/bin/b\nHEALTH=http://b\n")
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "a") {
		t.Fatalf("want duplicate NAME error, got %v", err)
	}
}

func TestSelectorGrammar(t *testing.T) {
	cases := []struct {
		in  string
		ok  bool
		str string
	}{
		{"$.live.connections", true, "$.live.connections"},
		{"$.queue.0.length", true, "$.queue.0.length"},
		{"generations_active", true, "$.generations_active"},
		{"$.a b", false, ""},
		{"$.a..b", false, ""},
		{"$.", false, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		s, err := ParseSelector(c.in)
		if c.ok != (err == nil) {
			t.Errorf("ParseSelector(%q) err = %v", c.in, err)
			continue
		}
		if c.ok && s.String() != c.str {
			t.Errorf("ParseSelector(%q).String() = %q, want %q", c.in, s.String(), c.str)
		}
	}
}
