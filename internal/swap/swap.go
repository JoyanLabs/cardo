// Package swap orchestrates the whole swap sequence (ADR-0002..0005):
// drain the resident engine, stop it, run hooks, start the target, wait
// for health, and roll back to the previous engine when anything fails.
package swap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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

// ErrSlotOccupied means an engine cardo did not start already answers the
// HEALTH endpoint of the engine `up` wants to start. Putting a second engine
// on that slot would double-book the GPU (ADR-0001), so cardo refuses instead
// of warning and continuing.
var ErrSlotOccupied = errors.New("slot already occupied")

// ErrResidentUnconfigured means the state record names a resident engine
// that no longer has a config file: cardo cannot stop what it cannot
// describe, so it refuses to act destructively (ADR-0001/0004).
var ErrResidentUnconfigured = errors.New("resident engine has no config; refusing to swap over it")

// hookTimeout is the fixed hook budget of ADR-0003: a hook that needs more
// than a minute is wrong and must not hang a swap. A variable so tests can
// shrink it.
var hookTimeout = 60 * time.Second

// cleanupBudget bounds the detached rollback/cleanup context (ADR-0004:
// rollback is best effort and its own failure is reported loudly). It is a
// variable so a test can shrink it and see whether a rollback still has room
// to wait for health; the real budget a cleanup gets is cleanupBudgetFor.
var cleanupBudget = 2 * time.Minute

// cleanupBudgetMargin covers everything a rollback has to do besides
// previous's own health wait: starting it, the poll cadence, and saving the
// record.
const cleanupBudgetMargin = 15 * time.Second

// healthProbeTimeout bounds one divergence-guard request. It is short on
// purpose: the guard must not stall a swap behind a wedged endpoint, and a
// server that does not answer in time is not holding the slot in any way
// cardo can act on.
const healthProbeTimeout = 3 * time.Second

// client is the package HTTP client for cardo's own one-shot requests (the
// divergence warning). It is shared so no swap ever leaks a connection pool,
// and its timeout bounds a wedged endpoint.
var client = &http.Client{
	Timeout: 5 * time.Second,
	// One-shot requests only: keep-alive would hold a health server's conn
	// open past the swap, which is exactly what a test cleanup cannot
	// close. DisableKeepAlives makes every probe self-contained.
	Transport: &http.Transport{DisableKeepAlives: true},
}

// httpNewRequest builds a bounded GET for a cardo endpoint, rejecting a
// malformed URL rather than dialing nothing.
func httpNewRequest(ctx context.Context, url string) (*http.Request, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("no url")
	}
	return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
}

// Store is the state surface Swapper needs. *state.Store satisfies it; a test
// can wrap it to make one write fail.
type Store interface {
	Load() (*state.Record, error)
	Save(state.Record) error
	Clear() error
	DirPath() string
}

// Swapper runs swaps against a registry and a state store. Out receives
// progress lines; tests capture it, the CLI prints it.
type Swapper struct {
	Registry *config.Registry
	State    Store
	Out      io.Writer

	// Factory builds the engine driver for a spec; nil means engine.New.
	Factory func(*config.Engine) (engine.Engine, error)

	// Alive reports whether a running handle is still alive; nil asks the
	// engine driver.
	Alive func(ctx context.Context, spec *config.Engine, running engine.Running) (bool, error)
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

// aliveClassified is the typed answer of a liveness question: true (running),
// false (confidently not running), or an error (the driver cannot answer —
// a wedged docker daemon must abort the swap, never read as a stopped
// engine).
type aliveAnswer struct {
	running bool
	err     error
}

func (s *Swapper) alive(ctx context.Context, spec *config.Engine, running engine.Running) aliveAnswer {
	if s.Alive != nil {
		running, err := s.Alive(ctx, spec, running)
		return aliveAnswer{running, err}
	}
	drv, err := s.driver(spec)
	if err != nil {
		return aliveAnswer{false, err}
	}
	return aliveAnswer2(drv.Alive(ctx, running))
}

func aliveAnswer2(b bool, err error) aliveAnswer {
	return aliveAnswer{b, err}
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

	// One record, one slot (B#4): a resident on slot 0 must never be
	// retired to make room for a target that declares slot 1 — that is a
	// silent cross-slot swap. cardo v1 manages one slot per record.
	if rec.Active != "" && rec.Slot != target.Slot {
		return fmt.Errorf("up %s: resident %s holds slot %d but %s declares slot %d; cardo v1 swaps one slot per record: free slot %d first", name, rec.Active, rec.Slot, target.Name, target.Slot, rec.Slot)
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
	if previous != "" {
		// The rollback target needs a timestamp of its own; a bare name cannot
		// be dated later.
		rec.PreviousAt = time.Now()
	} else {
		rec.PreviousAt = time.Time{}
	}
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

	// Divergence guard (examples/README lesson): if something already answers
	// the target's HEALTH, an untracked engine holds the slot and starting the
	// target on top of it would double-book the GPU (ADR-0001). PRE_UP has
	// already had its chance to free the endpoint. Nothing of the target has
	// started, so refusing costs nothing, and `previous` still names the
	// engine this swap stopped.
	//
	// The refusal happens after the resident was retired, so it must leave the
	// swap where it started: rolling back restores the engine this swap stopped
	// and records it resident again, instead of leaving it stopped behind an
	// empty record. The typed refusal is what the operator acts on, so it is
	// what is returned.
	remedy := occupiedEmptyRemedy
	if previous != "" {
		remedy = occupiedRestoredRemedy
	}
	if err := s.refuseOccupied(ctx, target, remedy); err != nil {
		if rbErr := s.rollback(ctx, rec, previous, target, nil, engine.Running{}, err); rbErr != nil {
			return rbErr
		}
		// Both identities survive: it is a failed swap, and the thing the
		// operator acts on is the occupied slot.
		return fmt.Errorf("%w: %w", ErrSwapFailed, err)
	}

	s.printf("starting %s", target.Name)
	drv, err := s.driver(target)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, nil, engine.Running{}, fmt.Errorf("driver: %w", err))
	}
	running, err := drv.Start(ctx)
	if err != nil {
		// Start failed after it may have created something (half a compose
		// project, a container that will not come up). The driver is the only
		// thing that knows how to take that back down, so it must be handed
		// over rather than dropped for lack of a running handle.
		return s.failUp(ctx, rec, previous, target, drv, engine.Running{}, fmt.Errorf("start: %w", err))
	}
	rec.Active = target.Name
	rec.ActiveAt = time.Now()
	rec.ActivePID = running.PID
	if err := s.State.Save(*rec); err != nil {
		// An engine that is running but not recorded as resident is invisible
		// to every later `status` and `down`, so no cardo run can stop it
		// again. Detach from the caller's context: the order to undo the start
		// must not die with the command that failed to write.
		stopCtx, cancel := context.WithTimeout(context.Background(), cleanupBudget)
		defer cancel()
		if sErr := drv.Stop(stopCtx, running, 10*time.Second); sErr != nil && !errors.Is(sErr, engine.ErrGone) {
			return fmt.Errorf("record started %s: %w; cardo could not stop the orphan either: %v", target.Name, err, sErr)
		}
		s.printf("warning: %s could not be recorded (%v) and was stopped again", target.Name, err)
		return fmt.Errorf("record started %s: %w", target.Name, err)
	}

	s.printf("waiting for health of %s (stall timeout %ds)", target.Name, target.Wait)
	err = health.Wait(ctx, target.Health, progressProbe(target),
		func() bool { return s.alive(ctx, target, running).running },
		time.Duration(target.Wait)*time.Second)
	if err != nil {
		return s.failUp(ctx, rec, previous, target, drv, running, fmt.Errorf("health: %w", err))
	}

	s.printf("%s is healthy and resident on slot %d", target.Name, target.Slot)
	return nil
}

// healthAlreadyUp reports whether the target's HEALTH endpoint already
// answers 2xx before cardo starts anything.
// occupancyAsk classifies one occupancy probe: an HTTP answer (any status)
// means somebody holds the port; a transport refusal means nobody is home;
// anything else (black-holing occupant, cancelled context) fails closed.
type occupancyAsk int

const (
	occupiedNo occupancyAsk = iota // refused connection: the slot is free
	occupiedYes                     // any HTTP answer holds the port
	occupiedUnknown                 // timeout or other error: fail closed
)

func (s *Swapper) healthAlreadyUp(ctx context.Context, spec *config.Engine) (occupancyAsk, error) {
	ctx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()
	req, err := httpNewRequest(ctx, spec.Health)
	if err != nil {
		return occupiedUnknown, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			// The caller (or the operator) gave up: never read that as "free".
			return occupiedUnknown, fmt.Errorf("occupancy check of %s cancelled: %w", spec.Health, ctx.Err())
		}
		if isRefused(err) {
			// Nothing is listening: the slot is free, which is exactly what it
			// looks like before cardo starts the engine that will answer it.
			return occupiedNo, nil
		}
		// A black-holing occupant (SYN dropped, no RST) answers nothing but
		// holds the port just as hard as a 200; fail closed.
		return occupiedUnknown, fmt.Errorf("occupancy check of %s failed: %w", spec.Health, err)
	}
	defer resp.Body.Close()
	// Any answer at all means somebody holds the port. A 404 from an nginx or
	// a 503 from a half-dead upstream blocks the slot just as hard as a 200,
	// and starting a second engine underneath it is the double-booking this
	// guard exists to prevent; the status only says how alive they are.
	return occupiedYes, nil
}

// isRefused reports whether the transport error means "nobody is listening"
// (connection refused / no route / no such host) rather than "nobody answered
// in time".
func isRefused(err error) bool {
	var oe *net.OpError
	if errors.As(err, &oe) {
		if oe.Op == "dial" {
			if errors.Is(oe.Err, syscall.ECONNREFUSED) || errors.Is(oe.Err, os.ErrNotExist) {
				return true
			}
			var se syscall.Errno
			if errors.As(oe.Err, &se) {
				return se == syscall.ECONNREFUSED || se == syscall.EHOSTUNREACH || se == syscall.ENETUNREACH
			}
		}
	}
	return false
}

// The remedy in a refusal depends on what the operator can actually do about
// it, which differs by where in the swap the guard fired.
const (
	// occupiedStaleRemedy applies where cardo never had a live handle: the
	// record is the only thing naming a resident, and --forget is how a stale
	// one is cleared.
	occupiedStaleRemedy = "whatever holds it is not recorded as resident, so inspect it and clear the record with `cardo down --forget` if it is stale"
	// occupiedRestoredRemedy applies where this swap stopped an engine and the
	// rollback has just brought it back. Advising `down --forget` there would
	// have the operator throw away the record of a live engine cardo does own.
	occupiedRestoredRemedy = "the previous engine was restored as resident by this rollback, so there is nothing to forget: investigate what answers HEALTH there before anything else goes on the slot"
	// occupiedEmptyRemedy applies where nothing was resident before the swap:
	// there is no record to clear and nothing was restored.
	occupiedEmptyRemedy = "nothing was resident before this swap, so there is no record to clear: investigate what answers HEALTH there before anything else goes on the slot"
)

// refuseOccupied turns "the HEALTH endpoint already answers" into the typed
// refusal that stops a swap before a second engine is put on the same slot.
func (s *Swapper) refuseOccupied(ctx context.Context, spec *config.Engine, remedy string) error {
	ask, err := s.healthAlreadyUp(ctx, spec)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSlotOccupied, err)
	}
	if ask != occupiedYes {
		return nil
	}
	return fmt.Errorf("%w: something already answers HEALTH of %s, so cardo will not start a second engine on that slot; %s", ErrSlotOccupied, spec.Name, remedy)
}

// rollback performs the best-effort undo of a failed `up` on a detached
// context (a Ctrl-C must not orphan the target or skip the restore) and
// returns nil when the previous engine is resident again. Any error it
// returns already names `cause` and is the caller's answer.
func (s *Swapper) rollback(ctx context.Context, rec *state.Record, previous string, target *config.Engine, drv engine.Engine, running engine.Running, cause error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupBudgetFor(previous))
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
	rec.ActiveAt = time.Time{}
	var rrunning engine.Running
	if previous != "" {
		s.printf("rolling back to %s", previous)
		prevSpec, haveSpec := s.Registry.Get(previous)
		rrunning, rerr := s.bringUp(cleanup, previous)
		if rerr != nil {
			// A rollback target that is running but unverified is still a live
			// process cardo has just started. A record that says nothing is
			// resident makes it unkillable by any later `cardo down`, so the
			// ownership stays recorded and the failure is said out loud.
			if rrunning.PID > 0 && haveSpec {
				if ans := s.alive(cleanup, prevSpec, rrunning); ans.running {
					rec.Active = previous
					rec.ActiveAt = time.Now()
					rec.ActivePID = rrunning.PID
					if serr := s.State.Save(*rec); serr != nil {
						return fmt.Errorf("%w: %v; rollback to %s is running (pid %d) but NOT verified healthy (%v), and recording it also failed: %v", ErrSwapFailed, cause, previous, rrunning.PID, rerr, serr)
					}
					return fmt.Errorf("%w: %v; rollback to %s is running (pid %d) but NOT verified healthy; it stays running and recorded, so check it and then `cardo down` (rollback health wait: %v)", ErrSwapFailed, cause, previous, rrunning.PID, rerr)
				}
			}
			// It died or never came up: nothing of it may be left behind, and
			// the record must not name a process that is not there.
			if rrunning.PID > 0 && haveSpec {
				if prevDrv, derr := s.driver(prevSpec); derr == nil {
					if serr := prevDrv.Stop(cleanup, rrunning, 10*time.Second); serr != nil && !errors.Is(serr, engine.ErrGone) {
						s.printf("warning: could not stop unverified rollback %s (pid %d): %v", previous, rrunning.PID, serr)
					}
				}
			}
			// Rollback is best effort and never recurs (ADR-0004). The
			// state leaves nothing resident so the next `up` starts clean.
			if err := s.State.Save(*rec); err != nil {
				return err
			}
			return fmt.Errorf("%w: %v; rollback to %s also failed: %v", ErrSwapFailed, cause, previous, rerr)
		}
		rec.Active = previous
		rec.ActiveAt = time.Now()
		rec.ActivePID = rrunning.PID
	}
	if serr := s.State.Save(*rec); serr != nil {
		// B#2: the restored engine is running but the record says nothing is
		// resident — the exact orphan the record exists to prevent. The safe
		// default is to stop what we just restored and report the whole
		// story; the next `up` starts clean.
		if previous != "" && rrunning.PID > 0 {
			if prevSpec, haveSpec := s.Registry.Get(previous); haveSpec {
				if prevDrv, derr := s.driver(prevSpec); derr == nil {
					if serr2 := prevDrv.Stop(cleanup, rrunning, 10*time.Second); serr2 != nil {
						return fmt.Errorf("%w: %v; the restored %s (pid %d) could not be stopped after the record save failed (%v, then %v) and is running unrecorded", ErrSwapFailed, cause, previous, rrunning.PID, serr, serr2)
					}
				}
			}
		}
		return fmt.Errorf("%w: %v; the record could not be saved after rollback (%v), so nothing is resident", ErrSwapFailed, cause, serr)
	}
	return nil
}

// failUp performs the rollback and wraps the cause, for the failures where
// "the swap failed" is the whole story.
func (s *Swapper) failUp(ctx context.Context, rec *state.Record, previous string, target *config.Engine, drv engine.Engine, running engine.Running, cause error) error {
	if err := s.rollback(ctx, rec, previous, target, drv, running, cause); err != nil {
		return err
	}
	return fmt.Errorf("%w: %v", ErrSwapFailed, cause)
}

// cleanupBudgetFor is how long a detached cleanup may take when it has to
// bring `previous` back up. The base budget covers an ordinary stop; a
// rollback additionally has to wait out previous's own HEALTH, and the
// shipped examples set WAIT=300, which a flat 2m would silently truncate and
// then report a perfectly restorable engine as "rollback also failed".
// Anything cardo cannot price (no previous, an engine that left the registry)
// keeps the base budget.
func (s *Swapper) cleanupBudgetFor(previous string) time.Duration {
	if previous == "" {
		return cleanupBudget
	}
	spec, ok := s.Registry.Get(previous)
	if !ok || spec.Wait <= 0 {
		return cleanupBudget
	}
	return cleanupBudget + time.Duration(spec.Wait)*time.Second + cleanupBudgetMargin
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
		func() bool { return s.alive(ctx, spec, running).running },
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
	if errors.Is(err, state.ErrNoState) {
		return ErrNothingResident
	}
	if err != nil {
		// The escape hatch must not depend on reading the record it exists to
		// replace: a corrupt or future-schema state.json is exactly when
		// `down --forget` is the only way forward, and it is the remedy the
		// loader itself names. Without --forget cardo still refuses to guess.
		if forget {
			if err := s.State.Clear(); err != nil {
				return err
			}
			s.printf("state record cleared")
			return nil
		}
		return err
	}
	if rec.Active == "" {
		return ErrNothingResident
	}
	if !forget {
		if err := s.retire(ctx, rec.Active, rec.ActivePID); err != nil {
			return err
		}
	} else {
		s.printf("forgetting %s without stopping it", rec.Active)
	}
	rec.Previous = rec.Active // the engine stopped by this operation
	rec.PreviousAt = time.Now()
	rec.Active = ""
	rec.ActivePID = 0
	rec.ActiveAt = time.Time{}
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
	drv, err := s.driver(spec)
	if err != nil {
		return err
	}
	running := engine.Running{PID: pid, Container: spec.Container}

	// Ask liveness before asking the resident anything at all. Draining puts
	// a question to the process, and a process that is not running answers
	// nothing: the BUSY probe would burn the whole DRAIN budget and abort the
	// swap over an engine that is already gone.
	if ans := s.alive(ctx, spec, running); ans.err != nil {
		return fmt.Errorf("cannot verify liveness of %s: %w", name, ans.err)
	} else if !ans.running {
		s.printf("warning: %s is not running; skipping drain and stop", name)
		// The record diverged from reality, and whatever holds the slot is not
		// what cardo started. Do not stack another engine on it.
		return s.refuseOccupied(ctx, spec, occupiedStaleRemedy)
	}

	if err := s.drain(ctx, spec); err != nil {
		return err
	}
	s.printf("stopping %s", name)
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
// Only a lock whose owner is provably dead is broken and taken over.
func (s *Swapper) lock() (func(), error) {
	dir := s.State.DirPath()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := dir + "/cardo.lock"
	ours := strconv.Itoa(os.Getpid())
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%s\n", ours)
			f.Close()
			// Unlock is ours-only: if anything replaced the file in the
			// meantime, deleting it would release somebody else's lock.
			return func() { s.unlock(path, ours) }, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("lock: %v", err)
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			// An unreadable lockfile may name a swapper that is very much
			// alive: crossing it is not an option, and neither is deleting it.
			return nil, fmt.Errorf("lock held: %s exists but cannot be read (%v); remove it only if no cardo is running", path, readErr)
		}
		pidText := strings.TrimSpace(string(b))
		pid, convErr := strconv.Atoi(pidText)
		if convErr != nil || pid <= 0 {
			// Empty or garbage: it names no owner, but it is still somebody's
			// claim, most likely a swapper that has just created the file and
			// not yet written its pid. Taking it away would let two swappers
			// both believe they hold the lock, and each would unlock the other's.
			return nil, fmt.Errorf("lock held: %s does not name a pid (%q); remove it only if no cardo is running", path, pidText)
		}
		if syscall.Kill(pid, syscall.Signal(0)) == nil {
			return nil, fmt.Errorf("another cardo run holds the lock (pid %d)", pid)
		}
		// The owner is provably dead: break the stale lock and retry the
		// create once.
		os.Remove(path)
	}
	return nil, fmt.Errorf("lock: could not acquire %s", path)
}

// unlock releases the lockfile, but only while it still names our pid. If the
// content is not ours, the file belongs to somebody else now and stays put.
func (s *Swapper) unlock(path, ours string) {
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != ours {
		return
	}
	os.Remove(path)
}
