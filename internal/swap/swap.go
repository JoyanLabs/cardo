// Package swap orchestrates the whole swap sequence (ADR-0002..0005):
// drain the resident engine, stop it, run hooks, start the target, wait
// for health, and roll back to the previous engine when anything fails.
package swap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/engine"
	"github.com/JoyanLabs/cardo/internal/health"
	"github.com/JoyanLabs/cardo/internal/probe"
	"github.com/JoyanLabs/cardo/internal/state"
	"os/exec"
)

// ErrSwapFailed wraps every failed `up`: the cause names the stage
// (drain, hook, start, health) and rollback has already been attempted.
var ErrSwapFailed = errors.New("swap failed")

// ErrNothingResident means there is nothing recorded as resident: `down`
// is a no-op and `up` has no rollback target.
var ErrNothingResident = errors.New("nothing is resident: no engine to stop")

// hookTimeout is the fixed hook budget of ADR-0003: a hook that needs more
// than a minute is wrong and must not hang a swap.
const hookTimeout = 60 * time.Second

// Swapper runs swaps against a registry and a state store. Out receives
// progress lines; tests capture it, the CLI prints it.
type Swapper struct {
	Registry *config.Registry
	State    *state.Store
	Out      io.Writer

	// Factory builds the engine driver for a spec; nil means engine.New.
	Factory func(*config.Engine) (engine.Engine, error)

	// Alive reports whether a running handle is still alive; nil means
	// engine.Running.Alive.
	Alive func(running engine.Running) bool
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

func (s *Swapper) alive(running engine.Running) bool {
	if s.Alive != nil {
		return s.Alive(running)
	}
	return running.Alive()
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
	previous := rec.Previous
	if rec.Active != "" {
		if err := s.retire(ctx, rec.Active); err != nil {
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
			return s.failUp(ctx, rec, previous, target, nil, fmt.Errorf("PRE_UP: %w", err))
		}
	}

	s.printf("starting %s", target.Name)
	drv, err := s.driver(target)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, nil, fmt.Errorf("driver: %w", err))
	}
	running, err := drv.Start(ctx)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, nil, fmt.Errorf("start: %w", err))
	}
	rec.Active = target.Name
	rec.ActiveAt = time.Now()
	rec.ActivePID = running.PID
	if err := s.State.Save(*rec); err != nil {
		return err
	}

	s.printf("waiting for health of %s (stall timeout %ds)", target.Name, target.Wait)
	err = health.Wait(ctx, target.Health, progressURL(target), func() bool { return s.alive(running) }, time.Duration(target.Wait)*time.Second)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, drv, fmt.Errorf("health: %w", err))
	}

	s.printf("%s is healthy and resident on slot %d", target.Name, target.Slot)
	return nil
}

// failUp performs the rollback best effort and wraps the cause. running is
// the target's handle when it had been started.
func (s *Swapper) failUp(ctx context.Context, rec *state.Record, previous string, target *config.Engine, drv engine.Engine, cause error) error {
	if drv != nil {
		running := engine.Running{PID: rec.ActivePID, Container: target.Container}
		if err := drv.Stop(ctx, running, 10*time.Second); err != nil && !errors.Is(err, engine.ErrGone) {
			cause = fmt.Errorf("%v (also failed stopping the target: %v)", cause, err)
		}
		if target.PostDown != "" {
			if err := runHook(ctx, target.PostDown); err != nil {
				s.printf("warning: POST_DOWN of %s failed: %v", target.Name, err)
			}
		}
	}
	rec.Active = ""
	rec.ActivePID = 0
	if previous != "" {
		s.printf("rolling back to %s", previous)
		if err := s.bringUp(ctx, previous); err != nil {
			// Rollback is best effort and never recurs (ADR-0004). The
			// state leaves nothing resident so the next `up` starts clean.
			if err := s.State.Save(*rec); err != nil {
				return err
			}
			return fmt.Errorf("%w: %v; rollback to %s also failed: %v", ErrSwapFailed, cause, previous, err)
		}
		rec.Active = previous
		rec.ActiveAt = time.Now()
	}
	if err := s.State.Save(*rec); err != nil {
		return err
	}
	return fmt.Errorf("%w: %v", ErrSwapFailed, cause)
}

// bringUp starts an engine and waits for plain health, without hooks.
func (s *Swapper) bringUp(ctx context.Context, name string) error {
	spec, ok := s.Registry.Get(name)
	if !ok {
		return fmt.Errorf("engine %s is no longer configured", name)
	}
	drv, err := s.driver(spec)
	if err != nil {
		return err
	}
	running, err := drv.Start(ctx)
	if err != nil {
		return err
	}
	return health.Wait(ctx, spec.Health, progressURL(spec), func() bool { return s.alive(running) }, time.Duration(spec.Wait)*time.Second)
}

// Down stops the resident engine, draining first.
func (s *Swapper) Down(ctx context.Context) error {
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
	if err := s.retire(ctx, rec.Active); err != nil {
		return err
	}
	rec.Active = ""
	rec.ActivePID = 0
	if err := s.State.Save(*rec); err != nil {
		return err
	}
	return nil
}

// retire drains and stops the named resident engine, running its POST_DOWN.
func (s *Swapper) retire(ctx context.Context, name string) error {
	spec, ok := s.Registry.Get(name)
	if !ok {
		// The recorded engine is no longer configured: warn, refuse to stop
		// something cardo can no longer describe (ADR-0001 ownership rule).
		s.printf("warning: %s is recorded as resident but has no config; not stopping it", name)
		return nil
	}
	if err := s.drain(ctx, spec); err != nil {
		return err
	}
	s.printf("stopping %s", name)
	drv, err := s.driver(spec)
	if err != nil {
		return err
	}
	running := engine.Running{PID: 0, Container: spec.Container}
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
// A missing BUSY degrades to a grace wait with a warning; a probe that
// cannot answer is busy, never free: the drain fails closed.
func (s *Swapper) drain(ctx context.Context, spec *config.Engine) error {
	if spec.Drain == 0 {
		return nil
	}
	if spec.Busy == nil {
		s.printf("warning: %s has no BUSY probe; grace-waiting %ds instead of draining", spec.Name, spec.Drain)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(spec.Drain) * time.Second):
		}
		return nil
	}
	p, err := probe.Parse(spec.Busy.URL + "#" + spec.Busy.Selector.String())
	if err != nil {
		return fmt.Errorf("drain %s: %v", spec.Name, err)
	}
	cadence := drainCadence(spec.Drain)
	deadline := time.Now().Add(time.Duration(spec.Drain) * time.Second)
	for {
		res := p.Check(ctx)
		if res.State == probe.Free {
			return nil
		}
		if time.Now().After(deadline) {
			if res.State == probe.Unknown {
				return fmt.Errorf("drain %s: probe could not answer within %ds; not swapping blindly", spec.Name, spec.Drain)
			}
			return fmt.Errorf("drain %s: still busy (%d) after %ds", spec.Name, res.Count, spec.Drain)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cadence):
		}
	}
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

// progressURL renders the optional PROGRESS key for health.Wait.
func progressURL(spec *config.Engine) string {
	if spec.Progress == nil {
		return ""
	}
	return spec.Progress.URL + "#" + spec.Progress.Selector.String()
}

// runHook executes one hook through sh -c with the fixed ADR-0003 timeout.
func runHook(ctx context.Context, cmd string) error {
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	out, err := exec2(ctx, cmd)
	if err != nil {
		return fmt.Errorf("%v: %s", err, tail(out))
	}
	return nil
}

// lock takes the state-dir lockfile so two swappers cannot run (ADR-0004).
// A lock held by a dead process is broken and taken over.
func (s *Swapper) lock() (func(), error) {
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

// exec2 runs a shell command and returns its combined output.
func exec2(ctx context.Context, cmd string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	b, err := c.CombinedOutput()
	return string(b), err
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}
