package swap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/engine"
	"github.com/JoyanLabs/cardo/internal/state"
)

// fakeEngine records lifecycle calls (including the handle it was asked to
// stop) and backs HEALTH with a server.
type fakeEngine struct {
	spec    *config.Engine
	pid     int
	actions []string
	stopped []engine.Running
	healthy atomic.Bool
	alive   atomic.Bool

	// startErr makes Start fail after it has already mutated state, the
	// shape of a `docker compose up -d` that creates half a project.
	startErr error
	// startDeadline records the deadline the caller put on the context, so
	// the rollback budget is observable from the outside.
	startDeadline time.Time
}

func (f *fakeEngine) Spec() *config.Engine { return f.spec }
func (f *fakeEngine) Start(ctx context.Context) (engine.Running, error) {
	f.actions = append(f.actions, "start "+f.spec.Name)
	f.alive.Store(true) // state is mutated before any failure is reported
	if d, ok := ctx.Deadline(); ok {
		f.startDeadline = d
	}
	if f.startErr != nil {
		return engine.Running{}, f.startErr
	}
	return engine.Running{PID: f.pid, Container: f.spec.Name}, nil
}
func (f *fakeEngine) Alive(_ context.Context, running engine.Running) bool {
	return f.alive.Load() && running.PID == f.pid
}
func (f *fakeEngine) Stop(_ context.Context, running engine.Running, _ time.Duration) error {
	f.actions = append(f.actions, fmt.Sprintf("stop %s pid=%d", f.spec.Name, running.PID))
	f.stopped = append(f.stopped, running)
	f.alive.Store(false)
	return nil
}

// stoppedPID returns the PID of the only Stop call, failing when Stop was
// never (or multiply) called.
func (f *fakeEngine) stoppedPID(t *testing.T) int {
	t.Helper()
	if len(f.stopped) != 1 {
		t.Fatalf("%s: want exactly one Stop, got %v", f.spec.Name, f.stopped)
	}
	return f.stopped[0].PID
}

func conf(t *testing.T, name string, extra ...string) *config.Engine {
	t.Helper()
	lines := append([]string{
		"NAME=" + name, "TYPE=command", "START=true",
		"HEALTH=http://127.0.0.1:1/health", "WAIT=1", "DRAIN=1",
	}, extra...)
	e, err := config.Parse(strings.NewReader(strings.Join(lines, "\n")), name+".conf")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// harness wires a registry of fake engines to a state dir.
type harness struct {
	t       *testing.T
	engines map[string]*fakeEngine
	sw      *Swapper
	out     bytes.Buffer
	store   *state.Store
}

func newHarness(t *testing.T, specs ...*config.Engine) *harness {
	t.Helper()
	h := &harness{t: t, engines: map[string]*fakeEngine{}, store: &state.Store{Dir: t.TempDir()}}
	reg := &config.Registry{Engines: map[string]*config.Engine{}}
	for i, s := range specs {
		fe := &fakeEngine{spec: s, pid: 4000 + i}
		fe.alive.Store(false)
		h.engines[s.Name] = fe
		reg.Engines[s.Name] = s
	}
	h.sw = &Swapper{
		Registry: reg,
		State:    h.store,
		Factory:  func(spec *config.Engine) (engine.Engine, error) { return h.engines[spec.Name], nil },
		Alive:    func(_ context.Context, _ *config.Engine, _ engine.Running) bool { return true },
		Out:      &h.out,
	}
	return h
}

func (h *harness) busyServer(initial int64) (url string, counter *atomic.Int64) {
	var n atomic.Int64
	n.Store(int64(initial))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fmt.Sprintf(`{"live":{"connections":%d}}`, n.Load())))
	}))
	h.t.Cleanup(srv.Close)
	return srv.URL + "#$.live.connections", &n
}

func (h *harness) healthServer(status func() int) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status())
	}))
	h.t.Cleanup(srv.Close)
	return srv.URL
}

// targetHealth answers 2xx only while the fake engine reports itself running,
// and refuses connections outright while it does not. `up` now treats any
// HTTP answer (not only a 2xx) as "the slot is occupied", so a happy-path
// test has to look like a cold port cardo itself is about to warm up: a
// server that answers 503 is an occupant, not an absence.
func (h *harness) targetHealth(name string) string {
	fe := h.engines[name]
	return h.gatedHealth(func() bool { return fe.alive.Load() })
}

// gatedListener refuses connections (a transport-level failure, which is what
// a free port really looks like) while closed, and hands them to the server
// once open.
type gatedListener struct {
	net.Listener
	open func() bool
}

func (g *gatedListener) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if g.open == nil || g.open() {
			return c, nil
		}
		c.Close() // nobody home: the client sees a transport failure
	}
}

// gatedHealth serves 200 on a port that refuses connections until open says
// otherwise.
func (h *harness) gatedHealth(open func() bool) string {
	h.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = &gatedListener{Listener: ln, open: open}
	srv.Start()
	h.t.Cleanup(srv.Close)
	return srv.URL
}

// useRealDriver routes the named engines through the real engine.New, so the
// ownership check inside commandEngine.Stop is the one under test.
func (h *harness) useRealDriver(names ...string) {
	h.t.Helper()
	real := map[string]bool{}
	for _, n := range names {
		real[n] = true
	}
	base := h.sw.Factory
	h.sw.Factory = func(spec *config.Engine) (engine.Engine, error) {
		if real[spec.Name] {
			return engine.New(spec, engine.Options{})
		}
		return base(spec)
	}
}

// startForeignShell starts a live process that is not any configured engine
// and returns its pid, waiting until /proc really shows the payload.
func startForeignShell(t *testing.T, payload string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", payload)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(b), payload) {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("foreign decoy %d never ran %q (cmdline %q)", pid, payload, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mutateStore runs a callback after every Save, so a test can change the
// world at the exact moment the swap layer writes its record.
type mutateStore struct {
	Store
	after func(state.Record)
}

func (m mutateStore) Save(r state.Record) error {
	if err := m.Store.Save(r); err != nil {
		return err
	}
	if m.after != nil {
		m.after(r)
	}
	return nil
}

// failSaveStore makes one Save fail: the record of a running target cannot be
// written, so the orphan guard has to stop what it just started.
type failSaveStore struct {
	Store
	failFor string
}

func (f failSaveStore) Save(r state.Record) error {
	if r.Active == f.failFor {
		return errors.New("state dir is on fire")
	}
	return f.Store.Save(r)
}

// unreachableProbe is a BUSY probe no process will ever answer.
func unreachableProbe() *config.Probe {
	return &config.Probe{URL: "http://127.0.0.1:1/busy", Selector: mustSelector("$.live.connections")}
}

func mustSelector(sel string) *config.Selector {
	s, err := config.ParseSelector(sel)
	if err != nil {
		panic(err)
	}
	return s
}

// deadResident makes every engine of the given name read as not running,
// which is what a stale record looks like to the swap layer.
func (h *harness) deadResident(names ...string) {
	h.sw.Alive = func(_ context.Context, spec *config.Engine, _ engine.Running) bool {
		for _, n := range names {
			if spec.Name == n {
				return false
			}
		}
		return true
	}
}

func TestUpWithoutStateStartsTarget(t *testing.T) {
	h := newHarness(t, conf(t, "target"))
	h.engines["target"].spec.Health = h.targetHealth("target")

	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "target") {
		t.Errorf("progress output should name the engine: %q", h.out.String())
	}
	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != "target" {
		t.Errorf("state active = %q", st.Active)
	}
}

// TestUpRecordsStartedPID pins that the ownership record carries the handle
// cardo actually started, so a later `down` can stop it.
func TestUpRecordsStartedPID(t *testing.T) {
	h := newHarness(t, conf(t, "target"))
	h.engines["target"].spec.Health = h.targetHealth("target")

	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := h.engines["target"].pid; st.ActivePID != want {
		t.Errorf("state ActivePID = %d, want the started pid %d", st.ActivePID, want)
	}
}

// TestUpRefusesWhenTargetHealthAlreadyAnswers pins the slot guard, and since
// round 3 also its rollback: something cardo did not start already answers
// the target's HEALTH (any status code at all — a 404 from an nginx holds the
// port just as hard as a 200), so the swap is refused, the target never
// starts, and the engine this swap retired is brought back up and recorded as
// resident again. The refusal may not advise `down --forget` here: the record
// now names a live engine cardo owns, and forgetting it would orphan it.
func TestUpRefusesWhenTargetHealthAlreadyAnswers(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNotFound, http.StatusServiceUnavailable} {
		code := code
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			h := newHarness(t, conf(t, "cold"), conf(t, "hot"))
			h.engines["hot"].spec.Drain = 0
			// A stray process already serves the port cold is about to bind.
			h.engines["cold"].spec.Health = h.healthServer(func() int { return code })
			h.engines["hot"].spec.Health = h0health(t)
			if err := h.store.Save(state.Record{Version: 1, Active: "hot", ActivePID: h.engines["hot"].pid}); err != nil {
				t.Fatal(err)
			}

			err := h.sw.Up(context.Background(), "cold")
			if !errors.Is(err, ErrSlotOccupied) {
				t.Fatalf("want ErrSlotOccupied for a %d occupant, got %v", code, err)
			}
			if !errors.Is(err, ErrSwapFailed) {
				t.Errorf("a refusal that had to roll back is also a failed swap: %v", err)
			}
			if !strings.Contains(err.Error(), "already answers HEALTH of cold") {
				t.Errorf("the refusal must name the endpoint that answered: %v", err)
			}
			if !strings.Contains(err.Error(), "the previous engine was restored") {
				t.Errorf("the refusal must say the retired engine came back: %v", err)
			}
			if !strings.Contains(err.Error(), "investigate what answers HEALTH") {
				t.Errorf("the refusal must name the thing to go look at: %v", err)
			}
			if strings.Contains(err.Error(), "down --forget") {
				t.Errorf("the record now names a live cardo-owned engine; --forget would orphan it: %v", err)
			}
			if len(h.engines["cold"].actions) != 0 {
				t.Errorf("the target must never start on an occupied slot: %v", h.engines["cold"].actions)
			}
			if !containsAction(h.engines["hot"].actions, "start hot") {
				t.Errorf("the engine this swap retired must be started again: %v", h.engines["hot"].actions)
			}
			st, _ := h.store.Load()
			if st.Active != "hot" || st.ActivePID != h.engines["hot"].pid {
				t.Errorf("the restored engine must be recorded as resident again: %+v", st)
			}
			if st.Previous != "hot" {
				t.Errorf("the rollback target must survive: %+v", st)
			}
		})
	}
}

// TestUpRefusalWithNoPreviousKeepsTheRecordEmpty is the other half of the
// refusal message: nothing was resident, so nothing was restored, and the
// record must stay empty. Advising `down --forget` there is noise about a
// record that names nothing.
func TestUpRefusalWithNoPreviousKeepsTheRecordEmpty(t *testing.T) {
	h := newHarness(t, conf(t, "cold"))
	h.engines["cold"].spec.Health = h.healthServer(func() int { return http.StatusTeapot })

	err := h.sw.Up(context.Background(), "cold")
	if !errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("want ErrSlotOccupied, got %v", err)
	}
	if strings.Contains(err.Error(), "down --forget") {
		t.Errorf("nothing is recorded, so --forget is not the remedy: %v", err)
	}
	if strings.Contains(err.Error(), "the previous engine was restored") {
		t.Errorf("there was no previous engine: %v", err)
	}
	if len(h.engines["cold"].actions) != 0 {
		t.Errorf("the target must never start: %v", h.engines["cold"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "" || st.ActivePID != 0 || st.Previous != "" {
		t.Errorf("the record must stay empty: %+v", st)
	}
}

// TestUpRefusalKeepsTheRecordOfADeadResident is the dead-resident variant:
// the process cardo recorded is gone but something still answers its HEALTH.
// The refusal happens inside retire, before the record is touched, so the
// record must still name the resident it could not stop — and only there is
// `down --forget` the right advice, because that record really is stale.
func TestUpRefusalKeepsTheRecordOfADeadResident(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0
	target := conf(t, "target")
	h := newHarness(t, cur, target)
	h.engines["current"].spec.Health = h.healthServer(func() int { return http.StatusOK })
	h.engines["target"].spec.Health = h.targetHealth("target")
	h.deadResident("current")
	const recordedPID = 616161
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: recordedPID}); err != nil {
		t.Fatal(err)
	}

	err := h.sw.Up(context.Background(), "target")
	if !errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("want ErrSlotOccupied, got %v", err)
	}
	if !strings.Contains(err.Error(), "already answers HEALTH of current") {
		t.Errorf("the refusal must name the held slot: %v", err)
	}
	if !strings.Contains(err.Error(), "down --forget") {
		t.Errorf("a stale record is exactly what --forget is for: %v", err)
	}
	if len(h.engines["current"].actions) != 0 || len(h.engines["target"].actions) != 0 {
		t.Errorf("nothing may move: current=%v target=%v", h.engines["current"].actions, h.engines["target"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "current" || st.ActivePID != recordedPID {
		t.Errorf("the refusal must leave the record it could not verify intact: %+v", st)
	}
}

// TestUpAbortsWhenTheRecordedResidentIsAliveButNotOurs is the double-booking
// guard the reviewers asked for at the swap layer: the recorded pid is a live
// process whose command line is not this engine's START. That is not "the
// resident was already gone" — the slot is held — so the retire aborts, the
// record survives, and nothing is started over it.
func TestUpAbortsWhenTheRecordedResidentIsAliveButNotOurs(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0
	cur.Start = "sleep 42 & sleep 42" // not what the live decoy runs
	target := conf(t, "target")
	h := newHarness(t, cur, target)
	h.useRealDriver("current")
	h.engines["target"].spec.Health = h.targetHealth("target")
	foreign := startForeignShell(t, "sleep 79 & sleep 79")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: foreign}); err != nil {
		t.Fatal(err)
	}

	err := h.sw.Up(context.Background(), "target")
	if !errors.Is(err, engine.ErrNotOurs) {
		t.Fatalf("want engine.ErrNotOurs, got %v", err)
	}
	if errors.Is(err, engine.ErrGone) {
		t.Errorf("a live process must never take the warn-and-proceed path: %v", err)
	}
	if strings.Contains(h.out.String(), "was already gone") {
		t.Errorf("the swap must not call a live foreign process gone: %q", h.out.String())
	}
	if len(h.engines["target"].actions) != 0 {
		t.Errorf("nothing may start on a slot a live process holds: %v", h.engines["target"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "current" || st.ActivePID != foreign {
		t.Errorf("the record must survive for the operator: %+v", st)
	}
	if !pidAlive(foreign) {
		t.Errorf("cardo killed a process it could not prove was its own (pid %d)", foreign)
	}
}

// TestDownAbortsWhenTheRecordedResidentIsAliveButNotOurs: `down` has the same
// double-booking stake as `up`, and a record cardo cannot honour must not be
// cleared either.
func TestDownAbortsWhenTheRecordedResidentIsAliveButNotOurs(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0
	cur.Start = "sleep 42 & sleep 42" // not what the live decoy runs
	h := newHarness(t, cur)
	h.useRealDriver("current")
	foreign := startForeignShell(t, "sleep 78 & sleep 78")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: foreign}); err != nil {
		t.Fatal(err)
	}

	err := h.sw.Down(context.Background(), false)
	if !errors.Is(err, engine.ErrNotOurs) {
		t.Fatalf("want engine.ErrNotOurs, got %v", err)
	}
	st, _ := h.store.Load()
	if st.Active != "current" || st.ActivePID != foreign {
		t.Errorf("a down that could not stop the resident must not clear the record: %+v", st)
	}
	if !pidAlive(foreign) {
		t.Errorf("cardo killed a process it does not own (pid %d)", foreign)
	}
	// The escape hatch still works, and it is the only way out.
	if err := h.sw.Down(context.Background(), true); err != nil {
		t.Fatalf("down --forget must stay reachable: %v", err)
	}
}

// TestUpStopsTheTargetWhenStartFails pins the partially-created engine: a
// Start that fails after creating something (half a compose project, a
// container that will not start) must still be handed to Stop, and its
// POST_DOWN must run, instead of being dropped because there is no handle.
func TestUpStopsTheTargetWhenStartFails(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "postdown.ran")
	target := conf(t, "target", "POST_DOWN=touch "+marker)
	h := newHarness(t, target)
	h.engines["target"].spec.Health = h.targetHealth("target")
	h.engines["target"].startErr = errors.New("compose created half a project and gave up")

	err := h.sw.Up(context.Background(), "target")
	if !errors.Is(err, ErrSwapFailed) {
		t.Fatalf("want ErrSwapFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "half a project") {
		t.Errorf("the cause must survive: %v", err)
	}
	if !containsAction(h.engines["target"].actions, "stop target") {
		t.Errorf("the driver must be asked to stop what a failed Start may have created: %v", h.engines["target"].actions)
	}
	if _, serr := os.Stat(marker); serr != nil {
		t.Errorf("the target's POST_DOWN must run after a failed Start (its teardown is how a half-created project is cleaned up): %v", serr)
	}
	st, _ := h.store.Load()
	if st.Active != "" || st.ActivePID != 0 {
		t.Errorf("nothing is resident: %+v", st)
	}
}

// TestRollbackCleanupContextOutlivesTheBaseCleanupBudget pins F3: examples
// set WAIT=300, so a rollback that restores the previous engine needs its own
// health wait to fit inside the detached cleanup context. The base 2m budget
// silently truncates a 300s load.
func TestRollbackCleanupContextOutlivesTheBaseCleanupBudget(t *testing.T) {
	prev := conf(t, "prev")
	prev.Drain = 0
	prev.Wait = 45
	prev.Health = h0health(t)
	target := conf(t, "target", "PRE_UP=exit 3")
	h := newHarness(t, prev, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "prev", ActivePID: h.engines["prev"].pid}); err != nil {
		t.Fatal(err)
	}

	if err := h.sw.Up(context.Background(), "target"); !errors.Is(err, ErrSwapFailed) {
		t.Fatalf("want ErrSwapFailed, got %v", err)
	}
	if h.engines["prev"].startDeadline.IsZero() {
		t.Fatal("the rollback never started the previous engine")
	}
	if got := time.Until(h.engines["prev"].startDeadline); got <= cleanupBudget {
		t.Errorf("the rollback health wait got %v of cleanup budget; with previous=%q (WAIT=%ds) it must outlast the base %v",
			got, "prev", prev.Wait, cleanupBudget)
	}
	st, _ := h.store.Load()
	if st.Active != "prev" {
		t.Errorf("the rollback must still restore the record: %+v", st)
	}
}

// TestCleanupBudgetForFallsBackToTheBaseBudget pins the budget arithmetic
// itself, including the two cases that must not be extended.
func TestCleanupBudgetForFallsBackToTheBaseBudget(t *testing.T) {
	prev := conf(t, "prev")
	prev.Wait = 300
	h := newHarness(t, prev, conf(t, "target"))

	if got := h.sw.cleanupBudgetFor(""); got != cleanupBudget {
		t.Errorf("cleanupBudgetFor(\"\") = %v, want the base %v", got, cleanupBudget)
	}
	if got := h.sw.cleanupBudgetFor("no-such-engine"); got != cleanupBudget {
		t.Errorf("a registry miss = %v, want the base %v", got, cleanupBudget)
	}
	got := h.sw.cleanupBudgetFor("prev")
	if got <= cleanupBudget+300*time.Second {
		t.Errorf("cleanupBudgetFor(prev) = %v, want the base %v plus the engine's 300s WAIT and a margin", got, cleanupBudget)
	}
}

// TestSilentWhenHealthIsFreeBeforeStart: the refusal is only for an endpoint
// that answers, not for a port nothing is listening on.
func TestUpSilentWhenHealthIsFreeBeforeStart(t *testing.T) {
	h := newHarness(t, conf(t, "target"))
	h.engines["target"].spec.Health = h.targetHealth("target")
	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.out.String(), "already answers HEALTH") {
		t.Errorf("a cold engine must not be refused: %q", h.out.String())
	}
}

func TestUpDrainsCurrentBeforeStop(t *testing.T) {
	cur := conf(t, "current")
	target := conf(t, "target")
	busyURL, n := h0(t)
	cur.Busy = mustProbeConfig(t, busyURL)
	target.Health = "http://127.0.0.1:1/health"

	h := newHarness(t, cur, target)
	_ = n
	// current busy count starts at 1 and drops after the first drain poll;
	// the target's health is healthy.
	n.Store(1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		n.Store(0)
	}()
	if err := h.store.Save(state.Record{Version: 1, Active: "current"}); err != nil {
		t.Fatal(err)
	}
	h.engines["target"].spec.Health = h.targetHealth("target")

	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	curFe := h.engines["current"]
	tgtFe := h.engines["target"]
	if len(curFe.actions) == 0 || !strings.HasPrefix(curFe.actions[0], "stop current") {
		t.Errorf("current actions = %v", curFe.actions)
	}
	if len(tgtFe.actions) == 0 || tgtFe.actions[0] != "start target" {
		t.Errorf("target actions = %v", tgtFe.actions)
	}
	st, _ := h.store.Load()
	if st.Previous != "current" || st.Active != "target" {
		t.Errorf("state = %+v", st)
	}
}

// TestUpStopsResidentByRecordedPID pins the ownership half of ADR-0004: the
// engine cardo retires is stopped by the PID it recorded when it started it,
// not by a handle invented at stop time.
func TestUpStopsResidentByRecordedPID(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0 // no BUSY probe: retire immediately, this test is about the handle
	target := conf(t, "target")
	h := newHarness(t, cur, target)
	h.engines["target"].spec.Health = h.targetHealth("target")

	const recordedPID = 424242
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: recordedPID}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	if got := h.engines["current"].stoppedPID(t); got != recordedPID {
		t.Errorf("retire stopped pid %d, want the recorded pid %d", got, recordedPID)
	}
}

// TestPreviousIsTheEngineThisSwapStopped pins ADR-0004's rollback target:
// `previous` names the engine this run stopped, and a stale value from an
// earlier run never survives it.
func TestPreviousIsTheEngineThisSwapStopped(t *testing.T) {
	// Nothing resident but a stale rollback target in the record.
	h := newHarness(t, conf(t, "target"))
	h.engines["target"].spec.Health = h.targetHealth("target")
	if err := h.store.Save(state.Record{Version: 1, Active: "", Previous: "long-gone"}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	st, _ := h.store.Load()
	if st.Previous != "" {
		t.Errorf("stale previous %q survived a swap that stopped nothing", st.Previous)
	}

	// A real stop: previous is the engine that was resident.
	h2 := newHarness(t, conf(t, "old"), conf(t, "target"))
	h2.engines["target"].spec.Health = h2.targetHealth("target")
	if err := h2.store.Save(state.Record{Version: 1, Active: "old", ActivePID: h2.engines["old"].pid}); err != nil {
		t.Fatal(err)
	}
	if err := h2.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	st2, _ := h2.store.Load()
	if st2.Previous != "old" {
		t.Errorf("previous = %q, want the engine this swap stopped", st2.Previous)
	}
}

func h0(t *testing.T) (string, *atomic.Int64) {
	var n atomic.Int64
	n.Store(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(fmt.Sprintf(`{"live":{"connections":%d}}`, n.Load())))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "#$.live.connections", &n
}

func mustProbeConfig(t *testing.T, value string) *config.Probe {
	t.Helper()
	u, sel, _ := strings.Cut(value, "#")
	s, err := config.ParseSelector(sel)
	if err != nil {
		t.Fatal(err)
	}
	return &config.Probe{URL: u, Selector: s}
}

func TestDrainTimeoutLeavesEngineUntouched(t *testing.T) {
	cur := conf(t, "current")
	busyURL, _ := h0(t)
	cur.Busy = mustProbeConfig(t, busyURL) // stuck at 1
	cur.Drain = 1                          // 1s budget
	target := conf(t, "target")

	h := newHarness(t, cur, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "current"}); err != nil {
		t.Fatal(err)
	}
	err := h.sw.Up(context.Background(), "target")
	if err == nil || !strings.Contains(err.Error(), "drain") {
		t.Fatalf("want drain timeout error, got %v", err)
	}
	if len(h.engines["current"].actions) != 0 {
		t.Errorf("busy engine must not be stopped: %v", h.engines["current"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "current" {
		t.Errorf("state must be untouched, got %+v", st)
	}
}

// TestDrainLateFreeAfterDeadlineStillTimesOut pins that DRAIN is a hard
// budget: a BUSY answer that only arrives after the deadline is rejected,
// even though it says "free". A slow probe must never stretch the drain.
func TestDrainLateFreeAfterDeadlineStillTimesOut(t *testing.T) {
	cur := conf(t, "current")
	target := conf(t, "target")
	started := time.Now()
	var first atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if first.CompareAndSwap(false, true) {
			// The one and only answer: Free, but late.
			if d := 1300 * time.Millisecond; time.Since(started) < d {
				time.Sleep(d - time.Since(started))
			}
		}
		w.Write([]byte(`{"live":{"connections":0}}`))
	}))
	defer srv.Close()
	cur.Busy = mustProbeConfig(t, srv.URL+"#$.live.connections")
	cur.Drain = 1

	h := newHarness(t, cur, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "current"}); err != nil {
		t.Fatal(err)
	}
	err := h.sw.Up(context.Background(), "target")
	if err == nil || !strings.Contains(err.Error(), "drain") {
		t.Fatalf("a Free that arrives after DRAIN must still time out, got %v", err)
	}
	if len(h.engines["current"].actions) != 0 {
		t.Errorf("engine drained past its deadline must not be stopped: %v", h.engines["current"].actions)
	}
	if len(h.engines["target"].actions) != 0 {
		t.Errorf("target must not start after a late drain: %v", h.engines["target"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "current" {
		t.Errorf("state must be untouched, got %+v", st)
	}
}

// TestDrainZeroWithBusyAborts pins that DRAIN=0 is not "swap without
// checking": one BUSY round is still asked, and busy aborts the swap.
func TestDrainZeroWithBusyAborts(t *testing.T) {
	cur := conf(t, "current")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"live":{"connections":3}}`))
	}))
	defer srv.Close()
	cur.Busy = mustProbeConfig(t, srv.URL+"#$.live.connections")
	cur.Drain = 0
	target := conf(t, "target")

	h := newHarness(t, cur, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "current"}); err != nil {
		t.Fatal(err)
	}
	err := h.sw.Up(context.Background(), "target")
	if err == nil || !strings.Contains(err.Error(), "drain") {
		t.Fatalf("DRAIN=0 with a busy engine must abort, got %v", err)
	}
	if !strings.Contains(err.Error(), "DRAIN=0") {
		t.Errorf("the failure must name the knob to raise: %v", err)
	}
	if len(h.engines["current"].actions) != 0 || len(h.engines["target"].actions) != 0 {
		t.Errorf("nothing may move: current=%v target=%v", h.engines["current"].actions, h.engines["target"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "current" {
		t.Errorf("state must be untouched, got %+v", st)
	}
}

// TestDrainZeroWithFreeProceeds: the single BUSY round may pass.
func TestDrainZeroWithFreeProceeds(t *testing.T) {
	cur := conf(t, "current")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"live":{"connections":0}}`))
	}))
	defer srv.Close()
	cur.Busy = mustProbeConfig(t, srv.URL+"#$.live.connections")
	cur.Drain = 0
	target := conf(t, "target")

	h := newHarness(t, cur, target)
	h.engines["target"].spec.Health = h.targetHealth("target")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: h.engines["current"].pid}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(h.out.String()), "drain=0") &&
		!strings.Contains(strings.ToLower(h.out.String()), "asking busy once") {
		t.Errorf("the single-probe round must be announced: %q", h.out.String())
	}
	st, _ := h.store.Load()
	if st.Active != "target" {
		t.Errorf("state = %+v", st)
	}
}

// TestDrainZeroWithoutBusyProceedsImmediately: DRAIN=0 means no wait, so an
// engine with no BUSY probe must not be held for a grace window.
func TestDrainZeroWithoutBusyProceedsImmediately(t *testing.T) {
	cur := conf(t, "current") // no BUSY
	cur.Drain = 0
	target := conf(t, "target")

	h := newHarness(t, cur, target)
	h.engines["target"].spec.Health = h.targetHealth("target")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: h.engines["current"].pid}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 750*time.Millisecond {
		t.Errorf("DRAIN=0 without a BUSY probe waited %v; it must not grace-wait", elapsed)
	}
	st, _ := h.store.Load()
	if st.Active != "target" {
		t.Errorf("state = %+v", st)
	}
}

func TestUnknownProbeAnswerIsBusyNotFree(t *testing.T) {
	cur := conf(t, "current")
	// A 503 with an unrelated JSON body: the probe cannot answer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"starting"}`))
	}))
	defer srv.Close()
	cur.Busy = mustProbeConfig(t, srv.URL+"#$.live.connections")
	cur.Drain = 1
	target := conf(t, "target")

	h := newHarness(t, cur, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "current"}); err != nil {
		t.Fatal(err)
	}
	err := h.sw.Up(context.Background(), "target")
	if err == nil || !strings.Contains(err.Error(), "drain") {
		t.Fatalf("unknown probe answers must fail closed, got %v", err)
	}
	if len(h.engines["current"].actions) != 0 {
		t.Errorf("current must not be stopped on unknown busy: %v", h.engines["current"].actions)
	}
	// ADR-0005: the failure must name the cause, so the operator can tell a
	// broken probe from a busy engine.
	if !strings.Contains(err.Error(), "probe could not answer") ||
		!strings.Contains(err.Error(), srv.URL) {
		t.Errorf("drain failure must carry the last probe error: %v", err)
	}
}

func TestMissingBusyDegradesToGraceWait(t *testing.T) {
	cur := conf(t, "current") // no BUSY
	cur.Drain = 1
	target := conf(t, "target")
	h := newHarness(t, cur, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "current"}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	h.engines["target"].spec.Health = h.targetHealth("target")

	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Errorf("grace wait skipped: elapsed %v", time.Since(start))
	}
	if !strings.Contains(strings.ToLower(h.out.String()), "busy") {
		t.Errorf("degraded drain must warn: %q", h.out.String())
	}
}

func TestPreUpFailureRollsBack(t *testing.T) {
	prev := conf(t, "prev")
	hts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hts.Close()
	prev.Health = hts.URL
	target := conf(t, "target", "PRE_UP=exit 3")
	h := newHarness(t, prev, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "prev", Previous: "old"}); err != nil {
		t.Fatal(err)
	}
	err := h.sw.Up(context.Background(), "target")
	if err == nil {
		t.Fatal("PRE_UP failure must fail the swap")
	}
	if len(h.engines["target"].actions) != 0 {
		t.Errorf("target must never start: %v", h.engines["target"].actions)
	}
	if !contains(h.engines["prev"].actions, "start prev") {
		t.Errorf("previous engine must be restored: %v", h.engines["prev"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "prev" {
		t.Errorf("state must be restored to prev, got %+v", st)
	}
}

// TestRollbackRecordsRestoredPID pins that the restore path keeps the
// ownership record complete: the engine brought back by a rollback is
// resident under the PID cardo just started, not without one.
func TestRollbackRecordsRestoredPID(t *testing.T) {
	prev := conf(t, "prev")
	prev.Drain = 0
	prev.Health = h0health(t)
	target := conf(t, "target", "PRE_UP=exit 3")
	h := newHarness(t, prev, target)
	const retiredPID = 515151
	if err := h.store.Save(state.Record{Version: 1, Active: "prev", ActivePID: retiredPID}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Up(context.Background(), "target"); !errors.Is(err, ErrSwapFailed) {
		t.Fatalf("want ErrSwapFailed, got %v", err)
	}
	st, err := h.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Active != "prev" {
		t.Fatalf("state must be restored, got %+v", st)
	}
	if want := h.engines["prev"].pid; st.ActivePID != want {
		t.Errorf("rollback recorded ActivePID %d, want the restarted pid %d", st.ActivePID, want)
	}
}

// h0health is a health endpoint that always answers 200.
func h0health(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHealthFailureRollsBackToPrevious(t *testing.T) {
	prev := conf(t, "prev")
	target := conf(t, "target")
	target.Health = "http://127.0.0.1:1/health" // dead: broken engine
	target.Wait = 1
	h := newHarness(t, prev, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "prev"}); err != nil {
		t.Fatal(err)
	}

	// prev comes back healthy on rollback.
	hts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hts.Close()
	prev.Health = hts.URL

	err := h.sw.Up(context.Background(), "target")
	if err == nil || !errors.Is(err, ErrSwapFailed) {
		t.Fatalf("want ErrSwapFailed, got %v", err)
	}
	tgt := h.engines["target"]
	started, stopped := false, false
	for _, a := range tgt.actions {
		switch {
		case strings.HasPrefix(a, "start"):
			started = true
		case strings.HasPrefix(a, "stop"):
			stopped = true
		}
	}
	if !started || !stopped {
		t.Errorf("target actions = %v", tgt.actions)
	}
	if !containsAction(h.engines["prev"].actions, "start prev") {
		t.Errorf("previous must be restarted: %v", h.engines["prev"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "prev" {
		t.Errorf("state must be restored, got %+v", st)
	}
}

// TestUpAbortsOnUnconfiguredResident pins the destructive-action guard: when
// the record names an engine that has no config file, cardo cannot describe
// (and therefore cannot safely stop) the resident, so it refuses to start
// anything over it.
func TestUpAbortsOnUnconfiguredResident(t *testing.T) {
	target := conf(t, "target")
	h := newHarness(t, target)
	h.engines["target"].spec.Health = h.healthServer(func() int { return http.StatusOK })
	if err := h.store.Save(state.Record{Version: 1, Active: "ghost", ActivePID: 777}); err != nil {
		t.Fatal(err)
	}

	err := h.sw.Up(context.Background(), "target")
	if !errors.Is(err, ErrResidentUnconfigured) {
		t.Fatalf("want ErrResidentUnconfigured, got %v", err)
	}
	if len(h.engines["target"].actions) != 0 {
		t.Errorf("nothing may start over an unverifiable resident: %v", h.engines["target"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "ghost" || st.ActivePID != 777 {
		t.Errorf("the record must survive for the operator, got %+v", st)
	}
	if !strings.Contains(err.Error(), "down --forget") {
		t.Errorf("the failure must name the escape hatch: %v", err)
	}
}

// TestHookTimeoutKillsTheWholeGroup pins ADR-0003's fixed hook budget: a
// wedged hook is killed (with its children) at the timeout instead of
// hanging the swap.
func TestHookTimeoutKillsTheWholeGroup(t *testing.T) {
	const hookBudget = 150 * time.Millisecond
	restore := hookTimeout
	hookTimeout = hookBudget
	t.Cleanup(func() { hookTimeout = restore })

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "hook-child.pid")
	target := conf(t, "target", fmt.Sprintf("PRE_UP=sleep 21 & echo $! > %s; wait", pidFile))
	h := newHarness(t, target)

	start := time.Now()
	err := h.sw.Up(context.Background(), "target")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSwapFailed) {
		t.Fatalf("a wedged hook must fail the swap, got %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("the failure must name the hook timeout: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("hook ran %v past its %s budget", elapsed, hookBudget)
	}
	child := waitForFilePID(t, pidFile, 2*time.Second)
	if !waitForPIDDead(child, 2*time.Second) {
		t.Errorf("hook child pid %d outlived the hook timeout: the group was not killed", child)
	}
}

func TestDownWithoutStateIsTypedNoOp(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	err := h.sw.Down(context.Background(), false)
	if !errors.Is(err, ErrNothingResident) {
		t.Fatalf("want ErrNothingResident, got %v", err)
	}
}

// TestDownStopsResidentByRecordedPID pins that `down` actually stops the
// resident, using the PID recorded when cardo started it.
func TestDownStopsResidentByRecordedPID(t *testing.T) {
	a := conf(t, "a")
	a.Drain = 0
	h := newHarness(t, a)
	const recordedPID = 606060
	if err := h.store.Save(state.Record{Version: 1, Active: "a", ActivePID: recordedPID}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Down(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := h.engines["a"].stoppedPID(t); got != recordedPID {
		t.Errorf("down stopped pid %d, want the recorded pid %d", got, recordedPID)
	}
	st, _ := h.store.Load()
	if st.Active != "" || st.ActivePID != 0 {
		t.Errorf("record must be cleared, got %+v", st)
	}
	if st.Previous != "a" {
		t.Errorf("previous = %q, want the engine this run stopped", st.Previous)
	}
}

// TestDownForgetConfiguredResident: --forget clears the record and leaves the
// process alone (the escape hatch for a divergent record).
func TestDownForgetConfiguredResident(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	if err := h.store.Save(state.Record{Version: 1, Active: "a", ActivePID: 6161}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Down(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(h.engines["a"].actions) != 0 {
		t.Errorf("--forget must not stop anything: %v", h.engines["a"].actions)
	}
	if !strings.Contains(h.out.String(), "forgetting a without stopping") {
		t.Errorf("--forget must say what it did: %q", h.out.String())
	}
	st, _ := h.store.Load()
	if st.Active != "" || st.ActivePID != 0 {
		t.Errorf("record must be cleared, got %+v", st)
	}
}

// TestDownForgetWorksForUnconfiguredResident: the escape hatch must work in
// exactly the case the destructive guard refuses.
func TestDownForgetWorksForUnconfiguredResident(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	if err := h.store.Save(state.Record{Version: 1, Active: "ghost", ActivePID: 777}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Down(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	st, _ := h.store.Load()
	if st.Active != "" {
		t.Errorf("record must be cleared, got %+v", st)
	}
}

// TestDownWithoutForgetRefusesUnconfiguredResident: the same record without
// --forget is refused, not silently cleared.
func TestDownWithoutForgetRefusesUnconfiguredResident(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	if err := h.store.Save(state.Record{Version: 1, Active: "ghost", ActivePID: 777}); err != nil {
		t.Fatal(err)
	}
	if err := h.sw.Down(context.Background(), false); !errors.Is(err, ErrResidentUnconfigured) {
		t.Fatalf("want ErrResidentUnconfigured, got %v", err)
	}
	st, _ := h.store.Load()
	if st.Active != "ghost" {
		t.Errorf("a refused down must leave the record alone, got %+v", st)
	}
}

// TestRollbackHealthFailureRecordsUnverifiedPrevious pins the orphan the
// rollback path used to create: the previous engine came back up as a live
// process, its health wait then failed, and the record said nothing was
// resident. A running process that cardo does not record is unkillable.
func TestRollbackHealthFailureRecordsUnverifiedPrevious(t *testing.T) {
	prev := conf(t, "prev")
	prev.Drain = 0
	prev.Health = "http://127.0.0.1:1/health" // comes up but never answers
	prev.Wait = 1
	target := conf(t, "target", "PRE_UP=exit 3")
	h := newHarness(t, prev, target)
	if err := h.store.Save(state.Record{Version: 1, Active: "prev", ActivePID: 515151}); err != nil {
		t.Fatal(err)
	}

	err := h.sw.Up(context.Background(), "target")
	if !errors.Is(err, ErrSwapFailed) {
		t.Fatalf("want ErrSwapFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "NOT verified healthy") {
		t.Errorf("the failure must say the rollback is up but unverified: %v", err)
	}
	st, lerr := h.store.Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if st.Active != "prev" {
		t.Errorf("the restored engine must stay recorded, got %+v", st)
	}
	if want := h.engines["prev"].pid; st.ActivePID != want {
		t.Errorf("ActivePID = %d, want the pid of the process that is running (%d)", st.ActivePID, want)
	}
}

// TestDownForgetClearsCorruptOrUnsupportedState is the escape hatch the state
// loader itself points at. It must work in exactly the case the record cannot
// be read at all; a plain `down` still refuses to guess.
func TestDownForgetClearsCorruptOrUnsupportedState(t *testing.T) {
	for _, body := range []string{"{not json at all", `{"version":99,"active":"a"}`} {
		t.Run(body, func(t *testing.T) {
			h := newHarness(t, conf(t, "a"))
			path := filepath.Join(h.store.Dir, "state.json")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := h.sw.Down(context.Background(), false); err == nil {
				t.Fatal("a record cardo cannot read must not be quietly ignored")
			}
			if len(h.engines["a"].actions) != 0 {
				t.Errorf("an unreadable record must not stop anything: %v", h.engines["a"].actions)
			}
			if err := h.sw.Down(context.Background(), true); err != nil {
				t.Fatalf("down --forget is the escape hatch for an unreadable record, got %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("the unreadable record must be gone, stat = %v", err)
			}
			if !strings.Contains(h.out.String(), "state record cleared") {
				t.Errorf("--forget must say what it did: %q", h.out.String())
			}
		})
	}
}

// TestUpRefusesWhenADeadResidentStillAnswersHealth pins the divergent record:
// the process cardo recorded is gone, yet something still answers its HEALTH,
// so the slot is held by an engine cardo never started. Draining a corpse and
// stacking a second engine on the real occupant is the failure to prevent.
func TestUpRefusesWhenADeadResidentStillAnswersHealth(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0
	target := conf(t, "target")
	h := newHarness(t, cur, target)
	h.engines["current"].spec.Health = h.healthServer(func() int { return http.StatusOK })
	h.engines["target"].spec.Health = h.targetHealth("target")
	h.deadResident("current")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: h.engines["current"].pid}); err != nil {
		t.Fatal(err)
	}

	err := h.sw.Up(context.Background(), "target")
	if !errors.Is(err, ErrSlotOccupied) {
		t.Fatalf("want ErrSlotOccupied, got %v", err)
	}
	if !strings.Contains(err.Error(), "already answers HEALTH of current") {
		t.Errorf("the refusal must name the engine whose slot is held: %v", err)
	}
	if len(h.engines["current"].actions) != 0 || len(h.engines["target"].actions) != 0 {
		t.Errorf("nothing may move: current=%v target=%v", h.engines["current"].actions, h.engines["target"].actions)
	}
}

// TestUpSkipsDrainForADeadResident pins the dead-end the reviewers reproduced:
// the recorded resident is not running, so there is nothing to drain, yet
// asking its BUSY probe burned the whole DRAIN budget and aborted the swap
// forever. Liveness is asked first.
func TestUpSkipsDrainForADeadResident(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0
	cur.Busy = unreachableProbe() // nobody will ever answer
	target := conf(t, "target")
	h := newHarness(t, cur, target)
	h.engines["target"].spec.Health = h.targetHealth("target")
	h.deadResident("current")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: h.engines["current"].pid}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatalf("a swap away from a dead resident must proceed: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the dead resident still cost %v: its drain was not skipped", elapsed)
	}
	if len(h.engines["current"].actions) != 0 {
		t.Errorf("a dead resident must not be drained or stopped: %v", h.engines["current"].actions)
	}
	if !strings.Contains(h.out.String(), "current is not running; skipping drain and stop") {
		t.Errorf("the skip must be said out loud: %q", h.out.String())
	}
	st, _ := h.store.Load()
	if st.Active != "target" || st.Previous != "current" {
		t.Errorf("state = %+v", st)
	}
}

// TestDownSkipsDrainForADeadResident: the same dead-end hit `down`, which
// aborted on an unreachable BUSY probe of a process that was already gone.
func TestDownSkipsDrainForADeadResident(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0
	cur.Busy = unreachableProbe()
	h := newHarness(t, cur)
	h.deadResident("current")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: 616161}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := h.sw.Down(context.Background(), false); err != nil {
		t.Fatalf("down must be able to clear a dead resident: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("down spent %v draining a process that is not running", elapsed)
	}
	if len(h.engines["current"].actions) != 0 {
		t.Errorf("a dead resident must not be drained or stopped: %v", h.engines["current"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "" || st.ActivePID != 0 || st.Previous != "current" {
		t.Errorf("the record must be cleared with previous kept: %+v", st)
	}
}

// TestUpStillAbortsWhenAnAliveResidentCannotAnswerBusy is the control: the
// skip is about liveness, not about a probe cardo does not like. A resident
// that IS running still fails closed when BUSY cannot answer.
func TestUpStillAbortsWhenAnAliveResidentCannotAnswerBusy(t *testing.T) {
	cur := conf(t, "current")
	cur.Drain = 0
	cur.Busy = unreachableProbe()
	target := conf(t, "target")
	h := newHarness(t, cur, target)
	h.engines["target"].spec.Health = h.targetHealth("target")
	if err := h.store.Save(state.Record{Version: 1, Active: "current", ActivePID: h.engines["current"].pid}); err != nil {
		t.Fatal(err)
	}

	err := h.sw.Up(context.Background(), "target")
	if err == nil || !strings.Contains(err.Error(), "drain") {
		t.Fatalf("an alive resident with an unanswerable BUSY must abort the swap, got %v", err)
	}
	if len(h.engines["current"].actions) != 0 || len(h.engines["target"].actions) != 0 {
		t.Errorf("nothing may move: current=%v target=%v", h.engines["current"].actions, h.engines["target"].actions)
	}
}

// TestRecordTimestampsStayInStepWithTheirFields pins record hygiene: a
// `previous` without a timestamp is a rollback target nobody can date, and an
// ActiveAt outliving the Active it describes is a lie about what is resident.
func TestRecordTimestampsStayInStepWithTheirFields(t *testing.T) {
	h := newHarness(t, conf(t, "a"), conf(t, "b"))
	h.engines["a"].spec.Drain = 0
	h.engines["b"].spec.Drain = 0
	h.engines["b"].spec.Health = h.targetHealth("b")
	if err := h.sw.Up(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	st, _ := h.store.Load()
	if st.Active != "b" || st.ActiveAt.IsZero() {
		t.Fatalf("up must stamp when the engine became resident: %+v", st)
	}
	if err := h.sw.Down(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	st, _ = h.store.Load()
	if !st.ActiveAt.IsZero() {
		t.Errorf("down cleared Active but left ActiveAt = %v", st.ActiveAt)
	}
	if st.Previous != "b" || st.PreviousAt.IsZero() {
		t.Errorf("down named a previous engine without stamping when: %+v", st)
	}

	h2 := newHarness(t, conf(t, "a"), conf(t, "b"))
	h2.engines["a"].spec.Drain = 0
	h2.engines["b"].spec.Drain = 0
	h2.engines["a"].spec.Health = h2.targetHealth("a")
	h2.engines["b"].spec.Health = h2.targetHealth("b")
	if err := h2.sw.Up(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if err := h2.sw.Up(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	st2, _ := h2.store.Load()
	if st2.Previous != "b" || st2.PreviousAt.IsZero() {
		t.Errorf("the rollback target of this swap must be stamped: %+v", st2)
	}
	if st2.Active != "a" || st2.ActiveAt.IsZero() {
		t.Errorf("the resident must be stamped: %+v", st2)
	}
}

// TestUpStopsTargetWhenTheRecordCannotBeSaved pins the orphan guard: an engine
// that is running but not recorded as resident cannot be stopped by any later
// cardo run, so a record that cannot be written must not leave it running.
func TestUpStopsTargetWhenTheRecordCannotBeSaved(t *testing.T) {
	h := newHarness(t, conf(t, "target"))
	h.engines["target"].spec.Health = h.targetHealth("target")
	h.sw.State = failSaveStore{Store: h.store, failFor: "target"}

	err := h.sw.Up(context.Background(), "target")
	if err == nil {
		t.Fatal("a record that cannot be written must fail the up")
	}
	if !strings.Contains(err.Error(), "on fire") {
		t.Errorf("the failure must carry the state error: %v", err)
	}
	if got := h.engines["target"].stoppedPID(t); got != h.engines["target"].pid {
		t.Errorf("the unrecordable target must be stopped, stopped %+v", h.engines["target"].stopped)
	}
	st, _ := h.store.Load()
	if st.Active != "" || st.ActivePID != 0 {
		t.Errorf("the record must not claim an engine cardo could not record: %+v", st)
	}
}

func TestUpUnknownNameIsTypedNotFound(t *testing.T) {
	h := newHarness(t)
	err := h.sw.Up(context.Background(), "nope")
	if !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("want config.ErrNotFound, got %v", err)
	}
}

func TestLockBlocksConcurrentSwapper(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	// Simulate another live swapper holding the lock: write our own pid.
	if err := os.MkdirAll(h.store.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.store.Dir, "cardo.lock"), []byte(fmt.Sprint(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	err := h.sw.Up(context.Background(), "a")
	if err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatalf("want lock error, got %v", err)
	}
	// A lock held by a dead process is broken and taken over.
	if err := os.WriteFile(filepath.Join(h.store.Dir, "cardo.lock"), []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.engines["a"].spec.Health = h.targetHealth("a")
	if err := h.sw.Up(context.Background(), "a"); err != nil {
		t.Fatalf("stale lock must be broken, got %v", err)
	}
}

// TestLockTreatsAnEmptyLockfileAsHeld pins the race two swappers used to lose:
// a freshly created lockfile reads back as empty (pid 0), which the old code
// called stale and deleted, so both runs believed they held the lock and each
// unlocked the other. An empty lockfile is somebody's claim: refuse, and
// leave it exactly where it is.
func TestLockTreatsAnEmptyLockfileAsHeld(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	h.engines["a"].spec.Health = h.targetHealth("a")
	path := filepath.Join(h.store.Dir, "cardo.lock")
	if err := os.MkdirAll(h.store.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "   \n", "not-a-pid"} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		err := h.sw.Up(context.Background(), "a")
		if err == nil || !strings.Contains(err.Error(), "lock held") {
			t.Fatalf("lockfile %q must be treated as held, got %v", body, err)
		}
		if b, rerr := os.ReadFile(path); rerr != nil || string(b) != body {
			t.Errorf("the swapper must not delete a lockfile it does not own: content %q, read err %v", b, rerr)
		}
		if len(h.engines["a"].actions) != 0 {
			t.Errorf("nothing may start while the lock is held: %v", h.engines["a"].actions)
		}
	}
}

// TestUnlockLeavesAForeignLockfileAlone pins the other half of the same race:
// unlocking may only remove the file while it still names our pid.
func TestUnlockLeavesAForeignLockfileAlone(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	h.engines["a"].spec.Health = h.targetHealth("a")
	path := filepath.Join(h.store.Dir, "cardo.lock")

	unlock, err := h.sw.lock()
	if err != nil {
		t.Fatal(err)
	}
	if b, rerr := os.ReadFile(path); rerr != nil || strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("the lock must name our pid, got %q (%v)", b, rerr)
	}
	// Another swapper takes the file over while we are running.
	if err := os.WriteFile(path, []byte("808080"), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock()
	if b, rerr := os.ReadFile(path); rerr != nil || string(b) != "808080" {
		t.Errorf("unlock deleted a lockfile it no longer owns: content %q, read err %v", b, rerr)
	}

	// And it does remove its own.
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("unlock left our own lockfile behind: %v", statErr)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAction(list []string, prefix string) bool {
	for _, v := range list {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// pidAlive reports whether pid exists and is not an unreaped zombie.
func pidAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, syscall.Signal(0)) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	rest := string(b)
	if i := strings.LastIndex(rest, ")"); i >= 0 {
		rest = strings.TrimSpace(rest[i+1:])
		return !strings.HasPrefix(rest, "Z")
	}
	return true
}

func waitForPIDDead(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !pidAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForFilePID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil {
			n := strings.TrimSpace(string(b))
			if n != "" {
				pid := 0
				for _, c := range n {
					if c < '0' || c > '9' {
						t.Fatalf("pid file %s contains %q", path, n)
					}
					pid = pid*10 + int(c-'0')
				}
				if pid > 0 {
					return pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the hook never wrote %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
