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
	"testing"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/engine"
	"github.com/JoyanLabs/cardo/internal/state"
)

// fakeEngine records lifecycle calls and backs HEALTH with a server.
type fakeEngine struct {
	spec    *config.Engine
	actions []string
	healthy atomic.Bool
	alive   atomic.Bool
}

func (f *fakeEngine) Spec() *config.Engine { return f.spec }
func (f *fakeEngine) Start(_ context.Context) (engine.Running, error) {
	f.actions = append(f.actions, "start "+f.spec.Name)
	f.alive.Store(true)
	return engine.Running{PID: 1000 + int(time.Now().UnixNano()%1000), Container: f.spec.Name}, nil
}
func (f *fakeEngine) Stop(_ context.Context, _ engine.Running, _ time.Duration) error {
	f.actions = append(f.actions, "stop "+f.spec.Name)
	f.alive.Store(false)
	return nil
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
	for _, s := range specs {
		fe := &fakeEngine{spec: s}
		fe.alive.Store(false)
		h.engines[s.Name] = fe
		reg.Engines[s.Name] = s
	}
	h.sw = &Swapper{
		Registry: reg,
		State:    h.store,
		Factory:  func(spec *config.Engine) (engine.Engine, error) { return h.engines[spec.Name], nil },
		Alive:    func(engine.Running) bool { return true },
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
	if len(curFe.actions) == 0 || curFe.actions[0] != "stop current" {
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
	if !contains(tgt.actions, "start target") || !contains(tgt.actions, "stop target") {
		t.Errorf("target actions = %v", tgt.actions)
	}
	if !contains(h.engines["prev"].actions, "start prev") {
		t.Errorf("previous must be restarted: %v", h.engines["prev"].actions)
	}
	st, _ := h.store.Load()
	if st.Active != "prev" {
		t.Errorf("state must be restored, got %+v", st)
	}
}

func TestDownWithoutStateIsTypedNoOp(t *testing.T) {
	h := newHarness(t, conf(t, "a"))
	err := h.sw.Down(context.Background())
	if !errors.Is(err, ErrNothingResident) {
		t.Fatalf("want ErrNothingResident, got %v", err)
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
