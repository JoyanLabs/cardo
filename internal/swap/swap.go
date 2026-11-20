// Package swap orchestrates the whole swap sequence (ADR-0002..0005):
// drain the resident engine, stop it, run hooks, start the target, wait
// for health, and roll back to the previous engine when anything fails.
package swap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/engine"
	"github.com/JoyanLabs/cardo/internal/health"
	"github.com/JoyanLabs/cardo/internal/probe"
	"github.com/JoyanLabs/cardo/internal/state"
)

// ErrSwapFailed wraps every failed `up`: the cause names the stage
// (drain, hook, start, health) and rollback has already been attempted.
var ErrSwapFailed = errors.New("swap failed")

// ErrNothingResident means there is nothing recorded as resident: `down`
// is a no-op and `up` has no rollback target.
var ErrNothingResident = errors.New("nothing is resident: no engine to stop")

// ErrResidentUnconfigured means the state record names a resident engine
// that no longer has a config file: cardo cannot stop what it cannot
// describe, so it refuses to act destructively (ADR-0001/0004).
var ErrResidentUnconfigured = errors.New("resident engine has no config; refusing to swap over it")

// hookTimeout is the fixed hook budget of ADR-0003: a hook that needs more
// than a minute is wrong and must not hang a swap. A variable so tests can
// shrink it.
var hookTimeout = 60 * time.Second

// cleanupBudget bounds the detached rollback/cleanup context (ADR-0004:
// rollback is best effort and its own failure is reported loudly).
const cleanupBudget = 2 * time.Minute

// client is the package HTTP client for cardo's own one-shot requests (the
// divergence warning). It is shared so no swap ever leaks a connection pool,
// and its timeout bounds a wedged endpoint.
var client = &http.Client{Timeout: 5 * time.Second}

// httpNewRequest builds a bounded GET for a cardo endpoint, rejecting a
// malformed URL rather than dialing nothing.
func httpNewRequest(ctx context.Context, url string) (*http.Request, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("no url")
	}
	return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
}

// Swapper runs swaps against a registry and a state store. Out receives
// progress lines; tests capture it, the CLI prints it.
type Swapper struct {
	Registry *config.Registry
	State    *state.Store
	Out      io.Writer

	// Factory builds the engine driver for a spec; nil means engine.New.
	Factory func(*config.Engine) (engine.Engine, error)

	// Alive reports whether a running handle is still alive; nil asks the
	// engine driver.
	Alive func(ctx context.Context, spec *config.Engine, running engine.Running) bool
}

func (s *Swapper) printf(format string, args ...any) {
	if s.Out != nil {
		fmt.Fprintf(s.Out, format+"\n", args...)
	}
}

func (s *Swapper) driver(spec *config.Engine) (engine.Engine, error) {
	if s.Factory != nil {
		return s.Factory(spec)
	}
	return engine.New(spec, engine.Options{})
}

func (s *Swapper) alive(ctx context.Context, spec *config.Engine, running engine.Running) bool {
	if s.Alive != nil {
		return s.Alive(ctx, spec, running)
	}
	drv, err := s.driver(spec)
	if err != nil {
		return false
	}
	return drv.Alive(ctx, running)
}

// Up swaps the resident engine for the named one, with full rollback.
func (s *Swapper) Up(ctx context.Context, name string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	target, ok := s.Registry.Get(name)
	if !ok {
		return fmt.Errorf("up %s: %w", name, config.ErrNotFound)
	}

	rec, err := s.State.Load()
	if errors.Is(err, state.ErrNoState) {
		rec = &state.Record{Slot: target.Slot}
	} else if err != nil {
		return err
	}

	// ADR-0004: `previous` is always the engine stopped by THIS swap; a
	// stale value never survives a new run.
	var previous string
	if rec.Active != "" {
		if err := s.retire(ctx, rec.Active, rec.ActivePID); err != nil {
			return err
		}
		previous = rec.Active
	}
	rec.Previous = previous
	rec.Active = ""
	rec.ActiveAt = time.Time{}
	rec.ActivePID = 0
	if err := s.State.Save(*rec); err != nil {
		return err
	}

	// PRE_UP of the target runs before anything of the target exists: a
	// failure here restores the previous engine (which is already stopped,
	// so only its start is needed).
	if target.PreUp != "" {
		s.printf("running PRE_UP for %s", target.Name)
		if err := runHook(ctx, target.PreUp); err != nil {
			return s.failUp(ctx, rec, previous, target, nil, engine.Running{}, fmt.Errorf("PRE_UP: %w", err))
		}
	}

	// Divergence warning (examples/README lesson): if something already
	// answers the target's HEALTH, an untracked engine may hold the slot.
	if s.healthAlreadyUp(ctx, target) {
		s.printf("warning: something already answers HEALTH of %s; an untracked engine may hold the slot", target.Name)
	}

	s.printf("starting %s", target.Name)
	drv, err := s.driver(target)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, nil, engine.Running{}, fmt.Errorf("driver: %w", err))
	}
	running, err := drv.Start(ctx)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, nil, engine.Running{}, fmt.Errorf("start: %w", err))
	}
	rec.Active = target.Name
	rec.ActiveAt = time.Now()
	rec.ActivePID = running.PID
	if err := s.State.Save(*rec); err != nil {
		return err
	}

	s.printf("waiting for health of %s (stall timeout %ds)", target.Name, target.Wait)
	err = health.Wait(ctx, target.Health, progressProbe(target),
		func() bool { return s.alive(ctx, target, running) },
		time.Duration(target.Wait)*time.Second)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, drv, running, fmt.Errorf("health: %w", err))
	}

	s.printf("%s is healthy and resident on slot %d", target.Name, target.Slot)
	return nil
}

// healthAlreadyUp reports whether the target's HEALTH endpoint already
// answers 2xx before cardo starts anything.
func (s *Swapper) healthAlreadyUp(ctx context.Context, spec *config.Engine) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := httpNewRequest(ctx, spec.Health)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode/100 == 2
}

// failUp performs the rollback best effort on a detached context (a Ctrl-C
// must not orphan the target or skip the restore) and wraps the cause.
func (s *Swapper) failUp(ctx context.Context, rec *state.Record, previous string, target *config.Engine, drv engine.Engine, running engine.Running, cause error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
	defer cancel()

	if drv != nil {
		if err := drv.Stop(cleanup, running, 10*time.Second); err != nil && !errors.Is(err, engine.ErrGone) {
			cause = fmt.Errorf("%v (also failed stopping the target: %v)", cause, err)
		}
		if target.PostDown != "" {
			if err := runHook(cleanup, target.PostDown); err != nil {
				s.printf("warning: POST_DOWN of %s failed: %v", target.Name, err)
			}
		}
	}
	rec.Active = ""
	rec.ActivePID = 0
	if previous != "" {
		s.printf("rolling back to %s", previous)
		if rrunning, err := s.bringUp(cleanup, previous); err != nil {
			// Rollback is best effort and never recurs (ADR-0004). The
			// state leaves nothing resident so the next `up` starts clean.
			if err := s.State.Save(*rec); err != nil {
				return err
			}
			return fmt.Errorf("%w: %v; rollback to %s also failed: %v", ErrSwapFailed, cause, previous, err)
		} else {
			rec.Active = previous
			rec.ActiveAt = time.Now()
			rec.ActivePID = rrunning.PID
		}
	}
	if err := s.State.Save(*rec); err != nil {
		return err
	}
	return fmt.Errorf("%w: %v", ErrSwapFailed, cause)
}

// bringUp starts an engine and waits for plain health, without hooks,
// returning the running handle so the ownership record stays complete.
func (s *Swapper) bringUp(ctx context.Context, name string) (engine.Running, error) {
	spec, ok := s.Registry.Get(name)
	if !ok {
		return engine.Running{}, fmt.Errorf("engine %s is no longer configured", name)
	}
	drv, err := s.driver(spec)
	if err != nil {
		return engine.Running{}, err
	}
	running, err := drv.Start(ctx)
	if err != nil {
		return engine.Running{}, err
	}
	if err := health.Wait(ctx, spec.Health, progressProbe(spec),
		func() bool { return s.alive(ctx, spec, running) },
		time.Duration(spec.Wait)*time.Second); err != nil {
		return running, err
	}
	return running, nil
}

// Down stops the resident engine, draining first. forget=true skips the
// stop and only clears the record (the ADR-0004 escape hatch for divergence).
func (s *Swapper) Down(ctx context.Context, forget bool) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	rec, err := s.State.Load()
	if errors.Is(err, state.ErrNoState) || (err == nil && rec.Active == "") {
		return ErrNothingResident
	}
	if err != nil {
		return err
	}
	if !forget {
		if err := s.retire(ctx, rec.Active, rec.ActivePID); err != nil {
			return err
		}
	} else {
		s.printf("forgetting %s without stopping it", rec.Active)
	}
	rec.Previous = rec.Active // the engine stopped by this operation
	rec.Active = ""
	rec.ActivePID = 0
	if err := s.State.Save(*rec); err != nil {
		return err
	}
	return nil
}

// retire drains and stops the named resident engine (by its recorded PID),
// running its POST_DOWN.
func (s *Swapper) retire(ctx context.Context, name string, pid int) error {
	spec, ok := s.Registry.Get(name)
	if !ok {
		// The recorded engine is no longer configured: refuse to act
		// destructively — starting a new engine over an unverifiable
		// resident would double-book the slot (ADR-0001/0004).
		return fmt.Errorf("%w: %q is recorded as resident but has no config file; start nothing over it, run `cardo down --forget` to clear the record", ErrResidentUnconfigured, name)
	}
	if err := s.drain(ctx, spec); err != nil {
		return err
	}
	s.printf("stopping %s", name)
	drv, err := s.driver(spec)
	if err != nil {
		return err
	}
	running := engine.Running{PID: pid, Container: spec.Container}
	if err := drv.Stop(ctx, running, 30*time.Second); err != nil && !errors.Is(err, engine.ErrGone) {
		return err
	} else if errors.Is(err, engine.ErrGone) {
		s.printf("warning: %s was already gone", name)
	}
	if spec.PostDown != "" {
		s.printf("running POST_DOWN for %s", name)
		if err := runHook(ctx, spec.PostDown); err != nil {
			// The engine is already stopped; a failing POST_DOWN must not
			// resurrect it. Warn and continue.
			s.printf("warning: POST_DOWN failed: %v", err)
		}
	}
	return nil
}

// drain waits until the engine's BUSY count reaches zero (ADR-0002).
// DRAIN=0 with a BUSY probe still asks once: an unanswerable or busy
// engine aborts instead of being swapped blindly. No BUSY at all degrades
// to a grace wait with a warning. The drain always fails closed.
func (s *Swapper) drain(ctx context.Context, spec *config.Engine) error {
	if spec.Busy == nil {
		if spec.Drain <= 0 {
			return nil
		}
		s.printf("warning: %s has no BUSY probe; grace-waiting %ds instead of draining", spec.Name, spec.Drain)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(spec.Drain) * time.Second):
		}
		return nil
	}
	p := progressProbeFrom(spec.Busy.URL, spec.Busy.Selector)

	if spec.Drain <= 0 {
		s.printf("warning: %s sets DRAIN=0; asking BUSY once anyway", spec.Name)
		res := p.Check(ctx)
		if res.State != probe.Free {
			return fmt.Errorf("drain %s: busy (%s) and DRAIN=0 forbids swapping; raise DRAIN", spec.Name, probeCause(res))
		}
		return nil
	}

	cadence := drainCadence(spec.Drain)
	deadline := time.Now().Add(time.Duration(spec.Drain) * time.Second)
	var lastErr error
	for {
		// Each attempt is bounded by the remaining budget, so a slow probe
		// cannot stretch the drain past DRAIN.
		budget := time.Until(deadline)
		if budget <= 0 {
			return drainTimeout(spec, lastErr)
		}
		if budget > 5*time.Second {
			budget = 5 * time.Second
		}
		attemptCtx, cancel := context.WithTimeout(ctx, budget)
		res := p.Check(attemptCtx)
		cancel()
		if res.State == probe.Free && time.Now().Before(deadline) {
			return nil
		}
		if res.State == probe.Unknown {
			lastErr = res.Err
		}
		if time.Now().After(deadline) {
			return drainTimeout(spec, lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cadence):
		}
	}
}

func drainTimeout(spec *config.Engine, lastErr error) error {
	if lastErr != nil {
		return fmt.Errorf("drain %s: probe could not answer within %ds (%v); not swapping blindly", spec.Name, spec.Drain, lastErr)
	}
	return fmt.Errorf("drain %s: still busy after %ds", spec.Name, spec.Drain)
}

func probeCause(res probe.Result) string {
	if res.Err != nil {
		return fmt.Sprintf("probe could not answer: %v", res.Err)
	}
	return fmt.Sprintf("count %d", res.Count)
}

func drainCadence(drainSeconds int) time.Duration {
	p := time.Duration(drainSeconds) * time.Second / 5
	if p < 50*time.Millisecond {
		return 50 * time.Millisecond
	}
	if p > time.Second {
		return time.Second
	}
	return p
}

// progressProbe renders the optional PROGRESS key as a parsed probe.
func progressProbe(spec *config.Engine) *probe.Probe {
	if spec.Progress == nil {
		return nil
	}
	return progressProbeFrom(spec.Progress.URL, spec.Progress.Selector)
}

func progressProbeFrom(url string, sel *config.Selector) *probe.Probe {
	p, err := probe.Parse(url + "#" + sel.String())
	if err != nil {
		return nil
	}
	return p
}

// tail keeps hook output out of the operator's way: the last part of stderr
// is what names the failing command.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}

// runHook executes one hook through sh -c with the fixed ADR-0003 timeout:
// the context is honoured (CommandContext) and the whole process group is
// killed on timeout, so a wedged hook can never hang a swap.
func runHook(ctx context.Context, cmd string) error {
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The hook's children inherit its output pipes, so killing only the shell
	// would leave CombinedOutput blocked on a wedged grandchild holding the
	// write end open. Kill the whole group at the timeout: that is what makes
	// the budget a real deadline.
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			// Make sure the group is really gone even if the shell spawned
			// children.
			if c.Process != nil {
				_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
			}
			return fmt.Errorf("hook timed out after %s", hookTimeout)
		}
		return fmt.Errorf("%v: %s", err, tail(string(out)))
	}
	return nil
}

// lock takes the state-dir lockfile so two swappers cannot run (ADR-0004).
// A lock held by a dead process is broken and taken over.
func (s *Swapper) lock() (func(), error) {
	if err := os.MkdirAll(s.State.Dir, 0o755); err != nil {
		return nil, err
	}
	path := s.State.Dir + "/cardo.lock"
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("lock: %v", err)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && syscall.Kill(pid, syscall.Signal(0)) == nil {
			return nil, fmt.Errorf("another cardo run holds the lock (pid %d)", pid)
		}
		// Stale lock: remove it and retry the create once.
		os.Remove(path)
	}
	return nil, fmt.Errorf("lock: could not acquire %s", path)
}
