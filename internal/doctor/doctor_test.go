package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JoyanLabs/cardo/internal/state"
)

// harness is a doctor run against its own engines dir and its own state
// record, so no test can read a developer's real configuration.
type harness struct {
	t        *testing.T
	tmp      string
	confDir  string
	stateDir string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tmp := t.TempDir()
	h := &harness{
		t:        t,
		tmp:      tmp,
		confDir:  filepath.Join(tmp, "engines.d"),
		stateDir: filepath.Join(tmp, "state"),
	}
	if err := os.MkdirAll(h.confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) file(dir, name, body string) string {
	h.t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) conf(name, body string) { h.file(h.confDir, name, body) }

func (h *harness) state(body string) {
	h.t.Helper()
	if err := os.MkdirAll(h.stateDir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.stateDir, "state.json"), []byte(body), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) runner() *Runner {
	return &Runner{ConfDir: h.confDir, State: &state.Store{Dir: h.stateDir}}
}

// run is the whole verb: one shot, one report. A transport problem inside
// doctor must become a finding, never an error out of Run.
func (h *harness) run(t *testing.T) *Report {
	t.Helper()
	rep, err := h.runner().Run(context.Background())
	if err != nil {
		t.Fatalf("doctor.Run: %v", err)
	}
	return rep
}

// commandConf is a valid command engine with its endpoints supplied by the
// test, so a case only varies what it cares about.
func commandConf(name, health string, extra ...string) string {
	return commandConfStart(name, health, engineStart("4242"), extra...)
}

// commandConfStart is commandConf for the tests that also spawn the process:
// the recorded pid counts as a live resident only if its command line proves
// it runs this exact START (ADR-0004), so the declaration and the process the
// test starts have to agree to the byte.
func commandConfStart(name, health, start string, extra ...string) string {
	return fmt.Sprintf("NAME=%s\nTYPE=command\nSTART=%s\nHEALTH=%s\nDRAIN=0\nWAIT=5\nSLOT=0\n%s\n",
		name, start, health, strings.Join(extra, "\n"))
}

// engineStart is the START a command engine declares when the test also
// spawns the process that runs it. Two simple commands, so the shell stays as
// process group leader and its command line is exactly `sh -c <START>`, the
// shape the ownership proof accepts; a single command may be exec'd in place
// by the shell and then proves nothing.
func engineStart(marker string) string { return "sleep " + marker + " & sleep " + marker }

func dockerConf(name, health string, extra ...string) string {
	return fmt.Sprintf("NAME=%s\nTYPE=docker\nCONTAINER=%s\nHEALTH=%s\nDRAIN=0\nWAIT=5\nSLOT=0\n%s\n",
		name, name+"-ctr", health, strings.Join(extra, "\n"))
}

// closedPort is an address nothing listens on: the honest shape of "the
// engine is not running".
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

func hasFinding(rep *Report, level Level, substr string) bool {
	for _, f := range rep.Findings {
		if f.Level == level && strings.Contains(f.String(), substr) {
			return true
		}
	}
	return false
}

func allText(rep *Report) string {
	var b strings.Builder
	for _, f := range rep.Findings {
		b.WriteString(f.String())
		b.WriteString("\n")
	}
	return b.String()
}

// findingText returns the one finding line a case is about, so a test can
// argue about wording instead of about whether something matched at all.
func findingText(t *testing.T, rep *Report, substr string) string {
	t.Helper()
	for _, f := range rep.Findings {
		if strings.Contains(f.String(), substr) {
			return f.String()
		}
	}
	t.Fatalf("no finding contains %q:\n%s", substr, allText(rep))
	return ""
}

// jsonServer answers path with a JSON body; other paths 404.
func jsonServer(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, `{"error":"no such path"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDoctorWarnsWhenHealthIsNotReachable is rule (a): an engine whose health
// endpoint refuses connections is reported, and the run still finishes with
// a verdict. A doctor that dies on the first refused socket tells the
// operator nothing about the other engines.
func TestDoctorWarnsWhenHealthIsNotReachable(t *testing.T) {
	h := newHarness(t)
	h.conf("down.conf", commandConf("down", closedPort(t)))

	rep := h.run(t)
	if rep.OK() {
		t.Fatalf("an unreachable engine is a finding:\n%s", allText(rep))
	}
	if !hasFinding(rep, LevelWarn, "down: health not reachable") {
		t.Errorf("want an unreachable-health warning, got:\n%s", allText(rep))
	}
	if rep.Engines != 1 {
		t.Errorf("Engines = %d, want 1", rep.Engines)
	}
}

// TestDoctorClassifiesAnUnhealthyAnswer is rule (a) again: an endpoint that
// answers 503 is a different problem from a refused socket, and the operator
// needs the status code to tell "the process is up and unhappy" from
// "nothing is listening".
func TestDoctorClassifiesAnUnhealthyAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, `{"error":"loading"}`)
	}))
	defer srv.Close()
	h := newHarness(t)
	h.conf("warm.conf", commandConf("warm", srv.URL+"/health"))

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "answers but not healthy") {
		t.Errorf("a non-2xx health answer must be reported as unhealthy, got:\n%s", allText(rep))
	}
	if !strings.Contains(allText(rep), "503") {
		t.Errorf("the finding must carry the status code: %s", allText(rep))
	}
	if strings.Contains(allText(rep), "not reachable") {
		t.Errorf("an endpoint that answered is reachable: %s", allText(rep))
	}
}

// TestDoctorAcceptsAHealthyEngineAndReportsTheBusyCount is the clean case and
// rule (b): a 2xx health and a BUSY answer are not findings, and the count is
// still shown, because "0 in flight" is the thing the operator came to read.
func TestDoctorAcceptsAHealthyEngineAndReportsTheBusyCount(t *testing.T) {
	srv := jsonServer(t, map[string]string{
		"/health":  `{"status":"ok"}`,
		"/metrics": `{"live":{"connections":3}}`,
	})
	h := newHarness(t)
	h.conf("ok.conf", commandConf("ok", srv.URL+"/health", "BUSY="+srv.URL+"/metrics#$.live.connections"))

	rep := h.run(t)
	if !rep.OK() {
		t.Fatalf("a healthy engine with a busy count is not a finding:\n%s", allText(rep))
	}
	if !hasFinding(rep, LevelOK, "ok: health 200") {
		t.Errorf("the healthy engine must still get a line: %s", allText(rep))
	}
	if !hasFinding(rep, LevelOK, "BUSY 3") {
		t.Errorf("the BUSY count must be reported: %s", allText(rep))
	}
}

// TestDoctorWarnsWhenTheBusyProbeCannotAnswer is rule (b)'s fail-closed
// story: probe.Unknown is what makes `cardo up` refuse to swap blind, so
// doctor has to surface it, with the probe's own error, before a swap does.
func TestDoctorWarnsWhenTheBusyProbeCannotAnswer(t *testing.T) {
	srv := jsonServer(t, map[string]string{"/health": `{"status":"ok"}`})
	h := newHarness(t)
	h.conf("nope.conf", commandConf("nope", srv.URL+"/health", "BUSY="+srv.URL+"/metrics#$.live.connections"))

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "nope: BUSY probe could not answer") {
		t.Errorf("an unanswerable BUSY probe must be a warning: %s", allText(rep))
	}
	// The cause is the actionable part (ADR-0005: failures must be actionable).
	if !strings.Contains(allText(rep), "not found") && !strings.Contains(allText(rep), "not JSON") {
		t.Errorf("the warning must carry the probe error, got %s", allText(rep))
	}
}

// TestDoctorChecksTheProgressProbe is rule (c): PROGRESS is optional, so its
// absence is silent, its value is shown, and its failure is a warning.
func TestDoctorChecksTheProgressProbe(t *testing.T) {
	srv := jsonServer(t, map[string]string{
		"/health":  `{"status":"ok"}`,
		"/loading": `{"pct":0.4}`,
		"/broken":  `not json at all`,
	})
	h := newHarness(t)
	h.conf("good.conf", commandConf("good", srv.URL+"/health", "PROGRESS="+srv.URL+"/loading#$.pct"))
	rep := h.run(t)
	if !rep.OK() {
		t.Fatalf("a working PROGRESS probe is not a finding:\n%s", allText(rep))
	}
	if !hasFinding(rep, LevelOK, "progress 0.4") {
		t.Errorf("the progress value must be shown: %s", allText(rep))
	}

	h2 := newHarness(t)
	h2.conf("bad.conf", commandConf("bad", srv.URL+"/health", "PROGRESS="+srv.URL+"/broken#$.pct"))
	rep2 := h2.run(t)
	if !hasFinding(rep2, LevelWarn, "bad: PROGRESS probe could not answer") {
		t.Errorf("a failing PROGRESS probe must warn: %s", allText(rep2))
	}

	// No PROGRESS key at all: nothing to ask, nothing to say.
	h3 := newHarness(t)
	h3.conf("plain.conf", commandConf("plain", srv.URL+"/health"))
	if rep3 := h3.run(t); !rep3.OK() {
		t.Errorf("an engine without PROGRESS must not be faulted for it:\n%s", allText(rep3))
	}
}

// TestDoctorWarnsOnAMisnamedEngineFile is the config-level rule: a real
// engine whose file lost its .conf suffix vanishes from the registry in
// silence, so doctor has to name it. Documentation beside the engines is the
// case that must stay quiet.
func TestDoctorWarnsOnAMisnamedEngineFile(t *testing.T) {
	h := newHarness(t)
	h.conf("good.conf", commandConf("good", closedPort(t)))
	h.file(h.confDir, "tensorfold", dockerConf("tensorfold", "http://127.0.0.1:8888/health"))
	h.file(h.confDir, "README.md", "# examples\n\n```conf\nNAME=\"vllm-qwen\"\nTYPE=\"docker\"\n```\n")

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "tensorfold: not a .conf file, skipped") {
		t.Errorf("a misnamed engine file must be named in the output: %s", allText(rep))
	}
	if strings.Contains(allText(rep), "README.md") {
		t.Errorf("documentation beside the engines is not a finding: %s", allText(rep))
	}
	if strings.Contains(allText(rep), "tensorfold: health") {
		t.Errorf("a skipped file is not a registered engine, so its health is not checked: %s", allText(rep))
	}
}

// TestDoctorTellsTheOperatorToInspectNotJustRename is the wording rule both
// halves of the misnamed-engine story need. A rename is not always the right
// move: an editor backup is ignored on purpose (ADR-0006), and a copy of a
// file that is already loaded would collide with it. What doctor can always
// say is "look at this file".
func TestDoctorTellsTheOperatorToInspectNotJustRename(t *testing.T) {
	h := newHarness(t)
	h.conf("good.conf", commandConf("good", closedPort(t)))
	h.file(h.confDir, "tensorfold", dockerConf("tensorfold", "http://127.0.0.1:8888/health"))
	h.file(h.confDir, "good.conf.bak", commandConf("good", closedPort(t)))
	h.file(h.confDir, "good.conf~", commandConf("good", closedPort(t)))
	h.file(h.confDir, "start.sh", "#!/bin/sh\nNAME=worker\nSLOT=3\nexec ./serve\n")

	rep := h.run(t)

	lost := findingText(t, rep, "tensorfold: not a .conf file, skipped")
	if !strings.Contains(lost, "inspect") {
		t.Errorf("the remedy must start with looking at the file: %q", lost)
	}
	if !strings.Contains(lost, "only if it is an engine") {
		t.Errorf("a rename may only be suggested conditionally: %q", lost)
	}

	backup := findingText(t, rep, "good.conf~")
	if !strings.Contains(backup, "backup") {
		t.Errorf("a ~ file must be named as the editor backup it is: %q", backup)
	}
	if strings.Contains(backup, "renam") {
		t.Errorf("ADR-0006 ignores ~ backups, so renaming one is not the advice: %q", backup)
	}

	dup := findingText(t, rep, "good.conf.bak")
	if !strings.Contains(dup, "inspect") {
		t.Errorf("a colliding file must be inspected, not renamed: %q", dup)
	}
	if !strings.Contains(dup, `"good"`) {
		t.Errorf("the finding must name the NAME it collides with: %q", dup)
	}
	if strings.Contains(dup, "to register it") {
		t.Errorf("renaming it would not register anything: %q", dup)
	}

	if strings.Contains(allText(rep), "start.sh") {
		t.Errorf("a script that sets NAME= and SLOT= is not engine content: %s", allText(rep))
	}
}

// TestDoctorWarnsOnAnEngineSavedAsYaml is the silent-engine fix: the
// documentation list used to swallow every .yaml, .json, .toml and .html in
// the engines directory, so an engine saved with the wrong extension was
// skipped and never mentioned.
func TestDoctorWarnsOnAnEngineSavedAsYaml(t *testing.T) {
	h := newHarness(t)
	h.conf("good.conf", commandConf("good", closedPort(t)))
	h.file(h.confDir, "engine.yaml", dockerConf("tensorfold", "http://127.0.0.1:8888/health"))
	h.file(h.confDir, "compose.yaml", "services:\n  demo:\n    image: busybox\n")
	h.file(h.confDir, "package.json", `{"name": "cardo", "version": "0.1.0"}`)

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "engine.yaml: not a .conf file, skipped") {
		t.Errorf("an engine saved as .yaml must be named, not swallowed by the doc list: %s", allText(rep))
	}
	for _, quiet := range []string{"compose.yaml", "package.json"} {
		if strings.Contains(allText(rep), quiet) {
			t.Errorf("%s holds no engine keys and must stay quiet: %s", quiet, allText(rep))
		}
	}
}

// TestDoctorReportsASpecialFileWithoutOpeningIt is the hang fix: the content
// peek used to open every non-.conf entry, and opening a named pipe for
// reading waits for a writer that never comes. The pipe has to be reported as
// skipped and never opened, with the run finishing on its own.
func TestDoctorReportsASpecialFileWithoutOpeningIt(t *testing.T) {
	h := newHarness(t)
	h.conf("good.conf", commandConf("good", closedPort(t)))
	if err := syscall.Mkfifo(filepath.Join(h.confDir, "engine.pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable here: %v", err)
	}

	type done struct {
		rep *Report
		err error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch := make(chan done, 1)
	go func() {
		rep, err := h.runner().Run(ctx)
		ch <- done{rep, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("Run: %v", got.err)
		}
		line := findingText(t, got.rep, "engine.pipe: special file, not inspected")
		if !strings.Contains(line, "named pipe") {
			t.Errorf("the line must say what kind of file it found: %q", line)
		}
		if strings.Contains(allText(got.rep), "engine.pipe: health") {
			t.Errorf("an unopened file is not an engine to check: %s", allText(got.rep))
		}
		if !hasFinding(got.rep, LevelWarn, "good: health not reachable") {
			t.Errorf("the engines beside it must still be checked: %s", allText(got.rep))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("doctor hung on a named pipe in the engines directory")
	}
}

// TestDoctorReturnsTheContextErrorInsteadOfVerdicts is the interrupt rule: a
// pass whose context is already done must not walk the engines with a dead
// context and call every unreachable endpoint "broken".
func TestDoctorReturnsTheContextErrorInsteadOfVerdicts(t *testing.T) {
	h := newHarness(t)
	h.conf("a.conf", commandConf("a", closedPort(t)))
	h.conf("b.conf", commandConf("b", closedPort(t)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := h.runner().Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("an interrupted pass must return the context error, got %v", err)
	}
	if rep != nil && len(rep.Findings) != 0 {
		t.Errorf("a cancelled pass must render nothing, got:\n%s", allText(rep))
	}
}

// TestDoctorReportsEveryBrokenConfigFile is the config-level rule again:
// doctor validates the whole directory, not up to the first syntax error.
func TestDoctorReportsEveryBrokenConfigFile(t *testing.T) {
	h := newHarness(t)
	h.conf("aaa.conf", "NAME=a\nTYPE=command\nSTART=x\nHEALTH=http://a\nTYPO=1\n")
	h.conf("zzz.conf", "NAME=z\nTYPE=command\nSTART=z\nHEALTH=http://z\nWAIT=0\n")

	rep := h.run(t)
	if rep.OK() {
		t.Fatal("broken configs are findings")
	}
	if !hasFinding(rep, LevelErr, "aaa.conf") || !hasFinding(rep, LevelErr, "TYPO") {
		t.Errorf("the first bad file must be reported with its key: %s", allText(rep))
	}
	if !hasFinding(rep, LevelErr, "zzz.conf") || !hasFinding(rep, LevelErr, "WAIT") {
		t.Errorf("the second bad file must not be hidden by the first: %s", allText(rep))
	}
}

// TestDoctorFlagsARecordedResidentThatIsNotRunning is rule (d): the record
// says resident, the driver says dead. The control (a pid that is really
// there) must stay quiet, or the warning is worthless.
func TestDoctorFlagsARecordedResidentThatIsNotRunning(t *testing.T) {
	srv := jsonServer(t, map[string]string{"/health": `{"status":"ok"}`})
	h := newHarness(t)
	start := engineStart("4451")
	h.conf("demo.conf", commandConfStart("demo", srv.URL+"/health", start))
	h.state(`{"version":1,"slot":0,"active":"demo","active_pid":99999999}`)

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "demo: recorded resident but not running") {
		t.Errorf("a dead resident must be reported: %s", allText(rep))
	}

	live := spawnEngine(t, start)
	h.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"demo","active_pid":%d}`, live))
	liveRep := h.run(t)
	if hasFinding(liveRep, LevelWarn, "recorded resident but not running") {
		t.Errorf("pid %d is running; doctor must not question it:\n%s", live, allText(liveRep))
	}
	if !hasFinding(liveRep, LevelOK, "resident pid") {
		t.Errorf("the live resident must be shown as resident: %s", allText(liveRep))
	}
}

// TestDoctorWarnsWhenTheResidentPidIsNotProvablyOurs is ADR-0004 in the
// read-only verbs. `cardo down` already refuses to signal a recorded pid it
// cannot prove is its own, so doctor must not print that same pid as a
// resident that is alive: an unrelated live process holding the GPU reads as
// "all is well" otherwise. The START here names the very command the foreign
// process runs, and it still proves nothing — cardo starts `sh -c <START>`.
func TestDoctorWarnsWhenTheResidentPidIsNotProvablyOurs(t *testing.T) {
	srv := jsonServer(t, map[string]string{"/health": `{"status":"ok"}`})
	h := newHarness(t)
	h.conf("demo.conf", commandConfStart("demo", srv.URL+"/health", "sleep 402"))
	pid := spawnForeign(t, "402")
	h.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"demo","active_pid":%d}`, pid))

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "demo: cannot verify ownership") {
		t.Errorf("an unprovable live pid must be its own finding: %s", allText(rep))
	}
	if strings.Contains(allText(rep), "recorded resident but not running") {
		t.Errorf("the process is alive; this record is not stale: %s", allText(rep))
	}
	if hasFinding(rep, LevelOK, "resident pid") {
		t.Errorf("a pid cardo cannot prove is not a resident it measured: %s", allText(rep))
	}

	// The matching case is unchanged: the process the engine really started
	// keeps its plain resident line.
	h2 := newHarness(t)
	start := engineStart("403")
	h2.conf("demo.conf", commandConfStart("demo", srv.URL+"/health", start))
	live := spawnEngine(t, start)
	h2.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"demo","active_pid":%d}`, live))
	rep2 := h2.run(t)
	if !hasFinding(rep2, LevelOK, fmt.Sprintf("resident pid %d alive", live)) {
		t.Errorf("a provably own pid stays a live resident: %s", allText(rep2))
	}
	if strings.Contains(allText(rep2), "cannot verify ownership") {
		t.Errorf("the matching case must not be doubted: %s", allText(rep2))
	}
}

// TestDoctorKeepsTheResidentVerdictWhenTheEngineHasOtherFindings is the
// report rule: whether the GPU is held is the question a swap turns on, so an
// unrelated warning about the same engine must not swallow the liveness line.
func TestDoctorKeepsTheResidentVerdictWhenTheEngineHasOtherFindings(t *testing.T) {
	h := newHarness(t)
	start := engineStart("404")
	h.conf("demo.conf", commandConfStart("demo", closedPort(t), start))
	live := spawnEngine(t, start)
	h.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"demo","active_pid":%d}`, live))

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "demo: health not reachable") {
		t.Fatalf("the broken endpoint is still a finding: %s", allText(rep))
	}
	if !hasFinding(rep, LevelOK, fmt.Sprintf("demo: resident pid %d alive", live)) {
		t.Errorf("the liveness line must survive the engine's other findings:\n%s", allText(rep))
	}
	if n := countFindings(rep, LevelOK, "resident pid"); n != 1 {
		t.Errorf("the resident verdict must be printed once, got %d:\n%s", n, allText(rep))
	}
}

// TestDoctorReportsADriverErrorAsUnverifiable is rule (d)'s sharp edge: an
// engine driver that cannot answer (a wedged or missing docker) is NOT a dead
// engine. Reading it as dead would tell the operator to start a second engine
// on a slot that is still held (ADR-0002/0004 fail-closed discipline).
func TestDoctorReportsADriverErrorAsUnverifiable(t *testing.T) {
	h := newHarness(t)
	shim := h.file(h.tmp, "docker-shim", "#!/bin/sh\necho \"Cannot connect to the Docker daemon\" >&2\nexit 1\n")
	if err := os.Chmod(shim, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := jsonServer(t, map[string]string{"/health": `{"status":"ok"}`})
	h.conf("dock.conf", dockerConf("dock", srv.URL+"/health"))
	h.state(`{"version":1,"slot":0,"active":"dock","active_pid":4242}`)

	r := h.runner()
	r.DockerBin = shim
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasFinding(rep, LevelWarn, "dock: cannot verify (driver error:") {
		t.Errorf("a driver that cannot answer must be its own finding: %s", allText(rep))
	}
	if strings.Contains(allText(rep), "not running") {
		t.Errorf("a driver error must never be reported as a dead engine: %s", allText(rep))
	}
}

// TestDoctorFlagsAStaleRecordIs rule (e): the record names an engine that has
// no config file, so no cardo command can stop it. The remedy is named too.
func TestDoctorFlagsAStaleRecord(t *testing.T) {
	h := newHarness(t)
	h.conf("demo.conf", commandConf("demo", closedPort(t)))
	h.state(`{"version":1,"slot":0,"active":"ghost","active_pid":123}`)

	rep := h.run(t)
	if !hasFinding(rep, LevelWarn, "stale record (use down --forget)") {
		t.Errorf("a record naming an unconfigured engine must be reported with its remedy: %s", allText(rep))
	}
	if !strings.Contains(allText(rep), "ghost") {
		t.Errorf("the finding must name the engine: %s", allText(rep))
	}
}

// TestDoctorReportsAnUnreadableRecord: a corrupt or future-schema state.json
// is not "nothing resident"; the operator must not be told the slot is free.
func TestDoctorReportsAnUnreadableRecord(t *testing.T) {
	h := newHarness(t)
	h.conf("demo.conf", commandConf("demo", closedPort(t)))
	h.state("{not json at all")

	rep := h.run(t)
	if rep.OK() {
		t.Fatal("an unreadable record is a finding")
	}
	if !hasFinding(rep, LevelErr, "state:") {
		t.Errorf("the report must name the record as the problem: %s", allText(rep))
	}
}

// TestDoctorMissingConfDir: a directory that does not exist yet is reported,
// not an empty pass.
func TestDoctorMissingConfDir(t *testing.T) {
	h := newHarness(t)
	r := &Runner{ConfDir: filepath.Join(h.tmp, "nowhere"), State: &state.Store{Dir: h.stateDir}}
	rep, err := r.Run(context.Background())
	if err != nil {
		t.Fatalf("a missing dir is a finding, not an error out of Run: %v", err)
	}
	if rep.OK() || !hasFinding(rep, LevelErr, "nowhere") {
		t.Errorf("want an [err] naming the missing dir, got ok=%v:\n%s", rep.OK(), allText(rep))
	}
}

// TestDoctorEmptyDirIsNotAFinding: zero engines is a valid registry (the
// install-time state), and doctor must say so with a verdict line.
func TestDoctorEmptyDirIsNotAFinding(t *testing.T) {
	h := newHarness(t)
	rep := h.run(t)
	if !rep.OK() {
		t.Errorf("an empty engines dir is not a problem: %s", allText(rep))
	}
	if rep.Engines != 0 {
		t.Errorf("Engines = %d, want 0", rep.Engines)
	}
}

// TestDoctorNeverHangsOnAWedgedEndpoint is the other half of rule (a): a
// server that accepts and never answers must become an unreachable finding
// within the check budget, and the rest of the run must still be reported.
func TestDoctorNeverHangsOnAWedgedEndpoint(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-time.After(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer hang.Close()
	h := newHarness(t)
	h.conf("hang.conf", commandConf("hang", hang.URL+"/health"))
	h.conf("late.conf", commandConf("late", hang.URL+"/health"))

	orig := CheckTimeout
	CheckTimeout = 120 * time.Millisecond
	defer func() { CheckTimeout = orig }()

	done := make(chan *Report, 1)
	go func() {
		rep, err := h.runner().Run(context.Background())
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		done <- rep
	}()
	select {
	case rep := <-done:
		if rep.OK() {
			t.Errorf("a wedged endpoint must be a finding:\n%s", allText(rep))
		}
		if n := countFindings(rep, LevelWarn, "not reachable"); n != 2 {
			t.Errorf("both engines must be reported, got %d:\n%s", n, allText(rep))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("doctor hung behind a wedged endpoint")
	}
}

// spawnEngine starts a long-lived process shaped like a cardo command engine
// (`sh -c <start>` as process group leader) and returns its pid; the group is
// killed when the test ends. The caller passes the very START its engine
// declares, which is what makes the ownership proof accept the pid.
func spawnEngine(t *testing.T, start string) int {
	t.Helper()
	return spawnProc(t, "sh", "-c", start)
}

// spawnForeign starts a live process that is not a cardo engine at all — a
// bare `sleep <marker>` with no shell in front of it — and returns its pid.
// No configured START can prove it, which is the case the read-only verbs
// must refuse to call alive.
func spawnForeign(t *testing.T, marker string) int {
	t.Helper()
	return spawnProc(t, "sleep", marker)
}

func spawnProc(t *testing.T, argv ...string) int {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Wait for the exec to land: a pid captured straight after Start can
	// still show the parent's command line.
	want := strings.Join(argv, " ")
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(b), argv[len(argv)-1]) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d never ran %q (cmdline %q)", pid, want, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return pid
}

func countFindings(rep *Report, level Level, substr string) int {
	n := 0
	for _, f := range rep.Findings {
		if f.Level == level && strings.Contains(f.String(), substr) {
			n++
		}
	}
	return n
}

// TestRenderIsOneLinePerFindingWithPrefixesAndASummary pins the output
// contract: parseable prefixes, engines sorted, and the final verdict line.
func TestRenderIsOneLinePerFindingWithPrefixesAndASummary(t *testing.T) {
	rep := &Report{Engines: 2}
	rep.add(LevelErr, "config", "aaa.conf: TYPO: unknown key")
	rep.add(LevelWarn, "bbb", "health not reachable: connection refused")
	rep.add(LevelOK, "aaa", "health 200")

	var b strings.Builder
	if err := Render(&b, rep); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want one line per finding plus the summary, got:\n%s", got)
	}
	for i, want := range []string{"[err]", "[warn]", "[ok]"} {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("line %d = %q, want prefix %s", i, lines[i], want)
		}
	}
	if lines[3] != "2 engines, 2 findings" {
		t.Errorf("summary = %q, want \"2 engines, 2 findings\"", lines[3])
	}
}

// TestRenderSortsEngines pins the sorted order: a stable report is one an
// operator can diff between runs.
func TestRenderSortsEngines(t *testing.T) {
	h := newHarness(t)
	h.conf("zeta.conf", commandConf("zeta", closedPort(t)))
	h.conf("alpha.conf", commandConf("alpha", closedPort(t)))
	h.conf("mid.conf", commandConf("mid", closedPort(t)))

	rep := h.run(t)
	var engines []string
	for _, f := range rep.Findings {
		if f.Level != LevelOK && f.Subject != "config" && f.Subject != "state" {
			engines = append(engines, f.Subject)
		}
	}
	want := []string{"alpha", "mid", "zeta"}
	if strings.Join(engines, ",") != strings.Join(want, ",") {
		t.Errorf("findings must be grouped by sorted engine name, got %v:\n%s", engines, allText(rep))
	}
}
