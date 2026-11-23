package watch

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/engine"
	"github.com/JoyanLabs/cardo/internal/probe"
	"github.com/JoyanLabs/cardo/internal/state"
)

// fixedTime keeps the rendered timestamp deterministic.
var fixedTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fixture is a runner over its own registry and its own state record.
type fixture struct {
	t        *testing.T
	registry *config.Registry
	dir      string
}

func newFixture(t *testing.T, confs map[string]string) *fixture {
	t.Helper()
	dir := t.TempDir()
	reg := &config.Registry{Engines: map[string]*config.Engine{}}
	for name, body := range confs {
		e, err := config.Parse(strings.NewReader(body), name)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("validate %s: %v", name, err)
		}
		reg.Engines[e.Name] = e
	}
	return &fixture{t: t, registry: reg, dir: dir}
}

func (f *fixture) state(body string) *state.Store {
	f.t.Helper()
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "state.json"), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return &state.Store{Dir: f.dir}
}

func (f *fixture) emptyState() *state.Store { return &state.Store{Dir: filepath.Join(f.dir, "none")} }

func (f *fixture) runner(st Store) *Runner {
	return &Runner{Registry: f.registry, State: st, Now: func() time.Time { return fixedTime }}
}

// commandConf and dockerConf build a valid engine .conf; the extra lines are
// appended verbatim, so a test that names its own SLOT does not collide with
// the default one.
func commandConf(name, health string, extra ...string) string {
	return engineConf("command", name, health, extra...)
}

func dockerConf(name, health string, extra ...string) string {
	return engineConf("docker", name, health, extra...)
}

func engineConf(kind, name, health string, extra ...string) string {
	lines := []string{"NAME=" + name, "TYPE=" + kind, "HEALTH=" + health, "DRAIN=0", "WAIT=5"}
	if kind == "command" {
		lines = append(lines, "START=sleep 4343")
	} else {
		lines = append(lines, "CONTAINER="+name+"-ctr")
	}
	if !anyHasPrefix(extra, "SLOT=") {
		lines = append(lines, "SLOT=0")
	}
	return strings.Join(append(lines, extra...), "\n") + "\n"
}

func anyHasPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func rowOf(s *Snapshot, name string) (Row, bool) {
	for _, r := range s.Rows {
		if r.Name == name {
			return r, true
		}
	}
	return Row{}, false
}

// TestSnapshotShowsTheStatusPicturePlusLiveData is the watch contract: the
// status picture (who is resident, its pid, the slot, the rollback target)
// and per-engine rows, with the active engine marked.
func TestSnapshotShowsTheStatusPicturePlusLiveData(t *testing.T) {
	f := newFixture(t, map[string]string{
		"on.conf":  commandConf("on", "http://127.0.0.1:1/health"),
		"off.conf": commandConf("off", "http://127.0.0.1:2/health", "SLOT=1"),
	})
	live := spawnEngine(t, "4441")
	st := f.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"on","active_pid":%d,"previous":"off"}`, live))

	s := f.runner(st).Snapshot(context.Background())
	if s.Active != "on" || s.ActivePID != live || s.Slot != 0 || s.Previous != "off" {
		t.Errorf("status picture = %+v", s)
	}
	if s.ActiveAlive != "yes" {
		t.Errorf("ActiveAlive = %q, want yes for pid %d", s.ActiveAlive, live)
	}
	on, ok := rowOf(s, "on")
	if !ok {
		t.Fatalf("rows = %+v", s.Rows)
	}
	if !on.Active || on.Alive != "yes" {
		t.Errorf("the resident row must be marked and measured: %+v", on)
	}
	if on.Slot != 0 || on.Type != "command" {
		t.Errorf("row must carry the declared slot and type: %+v", on)
	}
	off, ok := rowOf(s, "off")
	if !ok {
		t.Fatalf("rows = %+v", s.Rows)
	}
	if off.Active || off.Alive != "-" || off.Busy != "-" {
		t.Errorf("a non-resident engine has nothing measured about it: %+v", off)
	}
	if off.Slot != 1 {
		t.Errorf("off row slot = %d, want 1", off.Slot)
	}
}

// TestSnapshotOrdersRowsByName pins the stable ordering a refreshing display
// needs: rows that jump between ticks are unreadable.
func TestSnapshotOrdersRowsByName(t *testing.T) {
	f := newFixture(t, map[string]string{
		"zeta.conf":  commandConf("zeta", "http://127.0.0.1:1/health"),
		"alpha.conf": commandConf("alpha", "http://127.0.0.1:1/health"),
		"mid.conf":   commandConf("mid", "http://127.0.0.1:1/health"),
	})
	s := f.runner(f.emptyState()).Snapshot(context.Background())
	var got []string
	for _, r := range s.Rows {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "alpha,mid,zeta" {
		t.Errorf("Rows = %v, want sorted by name", got)
	}
}

// TestSnapshotCountsBusyOnlyForTheResident is rule: BUSY is asked of the
// resident, whose queue is the one a swap would have to drain; asking every
// engine would put a network round trip per row in a 2-second loop.
func TestSnapshotCountsBusyOnlyForTheResident(t *testing.T) {
	f := newFixture(t, map[string]string{
		"on.conf":  commandConf("on", "http://127.0.0.1:1/health", "BUSY=http://127.0.0.1:9/metrics#$.count"),
		"off.conf": commandConf("off", "http://127.0.0.1:2/health", "BUSY=http://127.0.0.1:9/metrics#$.count"),
	})
	live := spawnEngine(t, "4442")
	st := f.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"on","active_pid":%d}`, live))

	var asked []string
	r := f.runner(st)
	r.Busy = func(_ context.Context, e *config.Engine) probe.Result {
		asked = append(asked, e.Name)
		return probe.Result{Count: 7, State: probe.Busy}
	}
	s := r.Snapshot(context.Background())
	if strings.Join(asked, ",") != "on" {
		t.Errorf("BUSY asked of %v, want only the resident", asked)
	}
	on, _ := rowOf(s, "on")
	if on.Busy != "7" {
		t.Errorf("Busy = %q, want 7", on.Busy)
	}
}

// TestSnapshotRendersUnknownBusyAsQuestion is the ADR-0002 rule in the
// display: an unanswerable BUSY probe is "?", never 0, because a 0 in a
// monitoring loop reads as "safe to swap" when the truth is unknown.
func TestSnapshotRendersUnknownBusyAsQuestion(t *testing.T) {
	f := newFixture(t, map[string]string{"on.conf": commandConf("on", "http://127.0.0.1:1/health", "BUSY=http://127.0.0.1:9/metrics#$.count")})
	live := spawnEngine(t, "4443")
	st := f.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"on","active_pid":%d}`, live))

	r := f.runner(st)
	r.Busy = func(_ context.Context, _ *config.Engine) probe.Result {
		return probe.Result{State: probe.Unknown, Err: fmt.Errorf("probe %s: not JSON", "http://x/metrics")}
	}
	s := r.Snapshot(context.Background())
	on, _ := rowOf(s, "on")
	if on.Busy != "?" {
		t.Errorf("Busy = %q, want \"?\" for an unanswerable probe", on.Busy)
	}
	if !strings.Contains(on.Note, "not JSON") {
		t.Errorf("the row must carry the probe cause: %+v", on)
	}
}

// TestSnapshotUsesTheDriverForContainersAndThePidForCommands is the liveness
// rule: docker/compose ask their driver, a command engine is a pid. A driver
// that cannot answer is "?", never "no" (a wedged dockerd is not an engine
// that exited).
func TestSnapshotUsesTheDriverForContainersAndThePidForCommands(t *testing.T) {
	f := newFixture(t, map[string]string{
		"cmd.conf":  commandConf("cmd", "http://127.0.0.1:1/health"),
		"dock.conf": dockerConf("dock", "http://127.0.0.1:2/health"),
		"dead.conf": commandConf("dead", "http://127.0.0.1:3/health"),
	})
	live := spawnEngine(t, "4444")
	shim := filepath.Join(t.TempDir(), "docker-shim")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho \"dockerd is unreachable\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	st := f.state(`{"version":1,"slot":0,"active":"dock","active_pid":0}`)
	r := f.runner(st)
	r.DockerBin = shim
	s := r.Snapshot(context.Background())
	dock, _ := rowOf(s, "dock")
	if dock.Alive != "?" {
		t.Errorf("docker liveness with a failing driver = %q, want ?", dock.Alive)
	}
	// The reason travels with the row: an unknown column must say what it
	// asked and could not hear back.
	if !strings.Contains(dock.Note, "cannot verify (driver error:") || !strings.Contains(dock.Note, "dock-ctr") {
		t.Errorf("the row must say why liveness is unknown: %+v", dock)
	}
	if s.ActiveAlive != "?" {
		t.Errorf("ActiveAlive = %q, want ?", s.ActiveAlive)
	}

	// A driver that answers yes is trusted even with no pid at all: a
	// container's life is docker's to report, and a pid probe would say no.
	r = f.runner(st)
	r.Factory = func(_ *config.Engine) (engine.Engine, error) { return fakeDriver{alive: true}, nil }
	if s := r.Snapshot(context.Background()); s.ActiveAlive != "yes" {
		t.Errorf("ActiveAlive = %q, want yes: the driver, not the pid, decides for a container", s.ActiveAlive)
	}

	// A command engine is measured by its recorded pid.
	st = f.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"cmd","active_pid":%d}`, live))
	if s := f.runner(st).Snapshot(context.Background()); s.ActiveAlive != "yes" {
		t.Errorf("ActiveAlive = %q, want yes for the live pid %d", s.ActiveAlive, live)
	}
	st = f.state(`{"version":1,"slot":0,"active":"dead","active_pid":99999999}`)
	if s := f.runner(st).Snapshot(context.Background()); s.ActiveAlive != "no" {
		t.Errorf("ActiveAlive = %q, want no for a pid that is gone", s.ActiveAlive)
	}
}

// TestSnapshotFlagsAStaleRecord keeps watch and doctor telling the same story
// about a record no command can act on.
func TestSnapshotFlagsAStaleRecord(t *testing.T) {
	f := newFixture(t, map[string]string{"on.conf": commandConf("on", "http://127.0.0.1:1/health")})
	st := f.state(`{"version":1,"slot":0,"active":"ghost","active_pid":7}`)

	s := f.runner(st).Snapshot(context.Background())
	if len(s.Warnings) == 0 {
		t.Fatalf("a record naming an unconfigured engine must warn")
	}
	joined := strings.Join(s.Warnings, "\n")
	if !strings.Contains(joined, "stale record (use down --forget)") || !strings.Contains(joined, "ghost") {
		t.Errorf("warning must name the stale record and its remedy: %q", joined)
	}
	if s.Resident {
		t.Errorf("an unconfigured resident is not a resident row: %+v", s)
	}
}

// TestSnapshotWithoutARecord is the idle slot: nothing resident, nothing
// measured, and no crash.
func TestSnapshotWithoutARecord(t *testing.T) {
	f := newFixture(t, map[string]string{"on.conf": commandConf("on", "http://127.0.0.1:1/health")})
	s := f.runner(f.emptyState()).Snapshot(context.Background())
	if s.Resident || s.Active != "" || s.ActiveAlive != "-" {
		t.Errorf("no record must mean nothing resident: %+v", s)
	}
	on, ok := rowOf(s, "on")
	if !ok || on.Alive != "-" || on.Busy != "-" {
		t.Errorf("rows = %+v", s.Rows)
	}
	if len(s.Warnings) != 0 {
		t.Errorf("an idle slot is not a warning: %v", s.Warnings)
	}
}

// TestSnapshotReportsAnUnreadableRecord: watch must keep refreshing and say
// the record is unreadable, rather than showing an idle slot.
func TestSnapshotReportsAnUnreadableRecord(t *testing.T) {
	f := newFixture(t, map[string]string{"on.conf": commandConf("on", "http://127.0.0.1:1/health")})
	st := f.state("{not json")
	s := f.runner(st).Snapshot(context.Background())
	if !strings.Contains(strings.Join(s.Warnings, "\n"), "state") {
		t.Errorf("an unreadable record must be a warning line: %v", s.Warnings)
	}
	if len(s.Rows) != 1 {
		t.Errorf("the registry must still be shown: %+v", s.Rows)
	}
}

// TestRenderIsStableWidthAndANSIFree pins the piping contract: `cardo watch |
// ...` and watch(1) need plain text and columns that do not shift with the
// name lengths.
func TestRenderIsStableWidthAndANSIFree(t *testing.T) {
	f := newFixture(t, map[string]string{
		"tiny.conf":               commandConf("tiny", "http://127.0.0.1:1/health"),
		"a-much-longer-name.conf": commandConf("a-much-longer-name", "http://127.0.0.1:2/health"),
	})
	s := f.runner(f.emptyState()).Snapshot(context.Background())
	var b bytes.Buffer
	if err := Render(&b, s); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("watch must stay ANSI-free for piping: %q", out)
	}
	var rowLines []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, "*") || strings.HasPrefix(line, " ") {
			rowLines = append(rowLines, line)
		}
	}
	if len(rowLines) != 2 {
		t.Fatalf("want one row per engine, got %q", out)
	}
	if len(rowLines[0]) != len(rowLines[1]) {
		t.Errorf("rows must be the same width regardless of name length:\n%q\n%q", rowLines[0], rowLines[1])
	}
	if !strings.Contains(out, fixedTime.Format("2006-01-02 15:04:05")) {
		t.Errorf("each snapshot must be stamped: %q", out)
	}
	if !strings.Contains(out, "resident none") {
		t.Errorf("an idle slot must say so: %q", out)
	}
}

// TestRenderMarksTheResident checks the marker an operator scans for.
func TestRenderMarksTheResident(t *testing.T) {
	f := newFixture(t, map[string]string{
		"on.conf":  commandConf("on", "http://127.0.0.1:1/health"),
		"off.conf": commandConf("off", "http://127.0.0.1:2/health"),
	})
	live := spawnEngine(t, "4445")
	st := f.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"on","active_pid":%d,"previous":"off"}`, live))
	var b bytes.Buffer
	if err := Render(&b, f.runner(st).Snapshot(context.Background())); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "* on") {
		t.Errorf("the resident row must be marked:\n%s", out)
	}
	if strings.Contains(out, "* off") {
		t.Errorf("only the resident is marked:\n%s", out)
	}
	if !strings.Contains(out, "previous off") {
		t.Errorf("the rollback target belongs to the status picture:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("pid %d", live)) {
		t.Errorf("the recorded pid must be shown:\n%s", out)
	}
}

// TestRunPrintsOneSnapshotPerTick is the loop contract: two ticks, and the
// live data actually changes between them (a loop that sampled once would
// pass every rendering test above).
func TestRunPrintsOneSnapshotPerTick(t *testing.T) {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		n := atomic.AddInt64(&count, 1)
		fmt.Fprintf(w, `{"connections":%d}`, n)
	}))
	defer srv.Close()

	f := newFixture(t, map[string]string{"on.conf": commandConf("on", srv.URL+"/health", "BUSY="+srv.URL+"/metrics#$.connections")})
	live := spawnEngine(t, "4446")
	st := f.state(fmt.Sprintf(`{"version":1,"slot":0,"active":"on","active_pid":%d}`, live))

	var b bytes.Buffer
	if err := Run(context.Background(), f.runner(st), 10*time.Millisecond, 2, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if got := strings.Count(out, "watch "); got != 2 {
		t.Fatalf("want two snapshots, got %d:\n%s", got, out)
	}
	// Snapshot 1 asks BUSY once (count 1), snapshot 2 asks again (count 2).
	if !strings.Contains(out, "busy 1") || !strings.Contains(out, "busy 2") {
		t.Errorf("each tick must re-measure BUSY:\n%s", out)
	}
}

// TestRunUsesTheIntervalVar pins the injection point: with no interval given
// the loop runs at the package default, which a test (or an operator's
// environment) can move.
func TestRunUsesTheIntervalVar(t *testing.T) {
	f := newFixture(t, map[string]string{"on.conf": commandConf("on", "http://127.0.0.1:1/health")})
	orig := Interval
	Interval = 5 * time.Millisecond
	defer func() { Interval = orig }()

	done := make(chan error, 1)
	go func() {
		var b bytes.Buffer
		done <- Run(context.Background(), f.runner(f.emptyState()), 0, 3, &b)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the loop ignored the injected interval")
	}
}

// TestRunStopsWhenTheContextIsCancelled is the SIGINT contract: the loop is
// driven by a context the CLI wires to SIGINT, so cancelling it must return
// promptly and cleanly.
func TestRunStopsWhenTheContextIsCancelled(t *testing.T) {
	f := newFixture(t, map[string]string{"on.conf": commandConf("on", "http://127.0.0.1:1/health")})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	orig := Interval
	Interval = 5 * time.Millisecond
	defer func() { Interval = orig }()

	done := make(chan error, 1)
	go func() {
		var b bytes.Buffer
		done <- Run(ctx, f.runner(f.emptyState()), 0, 0, &b)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an operator's Ctrl-C is not an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop when the context was cancelled")
	}
}

// fakeDriver answers liveness from the test, so a case can be "the driver
// cannot answer" or "the driver says alive" without depending on a docker
// shim's exit code.
type fakeDriver struct {
	alive bool
	err   error
}

func (f fakeDriver) Spec() *config.Engine { return nil }
func (f fakeDriver) Start(context.Context) (engine.Running, error) {
	return engine.Running{}, f.err
}
func (f fakeDriver) Alive(context.Context, engine.Running) (bool, error) { return f.alive, f.err }
func (f fakeDriver) Stop(context.Context, engine.Running, time.Duration) error {
	return f.err
}

// spawnEngine starts a long-lived process shaped like a cardo command engine
// (sh as process group leader running the configured START) and returns its
// pid; the group is killed when the test ends.
func spawnEngine(t *testing.T, marker string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep "+marker+" & sleep "+marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(b), "sleep "+marker) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine %d never exec'd (cmdline %q)", pid, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return pid
}
