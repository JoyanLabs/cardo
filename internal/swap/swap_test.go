package swap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
}

func (f *fakeEngine) Spec() *config.Engine { return f.spec }
func (f *fakeEngine) Start(_ context.Context) (engine.Running, error) {
	f.actions = append(f.actions, "start "+f.spec.Name)
	f.alive.Store(true)
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

func TestUpWithoutStateStartsTarget(t *testing.T) {
	h := newHarness(t, conf(t, "target"))
	health := h.healthServer(func() int { return http.StatusOK })
	spec := h.engines["target"].spec
	spec.Health = health

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
	h.engines["target"].spec.Health = h.healthServer(func() int { return http.StatusOK })

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

// TestUpWarnsWhenHealthAlreadyAnswers pins the divergence warning: something
// already answers the target's HEALTH before cardo starts it, which usually
// means an untracked engine holds the slot.
func TestUpWarnsWhenHealthAlreadyAnswers(t *testing.T) {
	h := newHarness(t, conf(t, "cold"), conf(t, "hot"))
	// A stray process already serves the port cold is about to bind.
	h.engines["cold"].spec.Health = h.healthServer(func() int { return http.StatusOK })
	if err := h.store.Save(state.Record{Version: 1, Active: "hot", ActivePID: h.engines["hot"].pid}); err != nil {
		t.Fatal(err)
	}

	if err := h.sw.Up(context.Background(), "cold"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "already answers HEALTH") {
		t.Errorf("expected a divergence warning, got %q", h.out.String())
	}
}

// TestUpSilentWhenHealthIsFreeBeforeStart: the warning is only for an
// endpoint that already answers, not for a cold port that cardo itself is
// about to bring up.
func TestUpSilentWhenHealthIsFreeBeforeStart(t *testing.T) {
	h := newHarness(t, conf(t, "target"))
	h.engines["target"].spec.Health = h.healthServer(func() int {
		if h.engines["target"].alive.Load() {
			return http.StatusOK // only cardo's own start makes it answer
		}
		return http.StatusServiceUnavailable
	})
	if err := h.sw.Up(context.Background(), "target"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.out.String(), "already answers HEALTH") {
		t.Errorf("a cold engine must not be warned about: %q", h.out.String())
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
	hts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hts.Close()
	h.engines["target"].spec.Health = hts.URL

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
	h.engines["target"].spec.Health = h.healthServer(func() int { return http.StatusOK })

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
	h.engines["target"].spec.Health = h.healthServer(func() int { return http.StatusOK })
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
	h2.engines["target"].spec.Health = h2.healthServer(func() int { return http.StatusOK })
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
	h.engines["target"].spec.Health = h.healthServer(func() int { return http.StatusOK })
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
	h.engines["target"].spec.Health = h.healthServer(func() int { return http.StatusOK })
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
	hts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hts.Close()
	h.engines["target"].spec.Health = hts.URL

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
	hts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hts.Close()
	h.engines["a"].spec.Health = hts.URL
	if err := h.sw.Up(context.Background(), "a"); err != nil {
		t.Fatalf("stale lock must be broken, got %v", err)
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
