// Package watch prints what is resident on the GPU slot right now, again and
// again, until the operator interrupts it.
//
// It is the read-only companion of `cardo status`: status reports the record
// (what cardo wrote down), watch measures (what the engines and the drivers
// say now). The picture is the same one status gives — who is resident, its
// pid, the slot, the rollback target — plus the live data an operator wants
// while a swap is being decided: is the resident process actually alive, and
// how many requests is BUSY reporting.
//
// The rendering is plain text with stable column widths and no terminal
// control: `cardo watch | tee`, `| grep` and piping into watch(1) all have to
// keep working, which is why nothing here reaches for a TUI library.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/engine"
	"github.com/JoyanLabs/cardo/internal/probe"
	"github.com/JoyanLabs/cardo/internal/state"
)

// Interval is the default cadence between snapshots. It is a variable rather
// than a constant so a test (or an environment) can move it; the CLI's
// --interval flag overrides it per run.
var Interval = 2 * time.Second

// checkTimeout bounds one live question, so a wedged endpoint delays a tick
// instead of stopping the loop.
var checkTimeout = 3 * time.Second

// The two states a measured column can be in that are not a measurement.
// Both are distinct from a real value, and "?"/"-" never read as "0 free".
const (
	// unknown is a question that could not be answered.
	unknown = "?"
	// unmeasured is a question that does not apply.
	unmeasured = "-"

	aliveYes = "yes"
	aliveNo  = "no"
)

// rowMinNameWidth keeps short names from shifting the columns of the labels.
const rowMinNameWidth = 8

// Store is the state surface watch reads; *state.Store satisfies it.
type Store interface {
	Load() (*state.Record, error)
}

// Row is one engine's line: the declaration (name, type, slot) plus whatever
// was measured about it this tick.
type Row struct {
	Name   string
	Type   string
	Slot   int
	Active bool
	// Alive is yes, no or "?" — "?" means the driver could not answer, which
	// is never the same as "not running" (a wedged dockerd is not an engine
	// that exited).
	Alive string
	// Busy is the resident's request count, "?" when the probe could not
	// answer, "-" when there was nothing to ask (the engine declares no BUSY,
	// or it is not the resident and its queue is not the one a swap drains).
	Busy string
	// Note carries the reason a column is "?", so an unmeasured value is
	// never unexplained.
	Note string
}

// Snapshot is one reading of the slot.
type Snapshot struct {
	Time        time.Time
	Slot        int
	Active      string
	ActivePID   int
	ActiveAlive string
	Previous    string
	// Resident says a record names a configured engine as holding the slot.
	// Without it, Active may still name what the record claims.
	Resident bool
	Rows     []Row
	Warnings []string
}

// Runner produces snapshots. Every input is injectable so the snapshot
// function is testable apart from the loop, and vice versa.
type Runner struct {
	// Registry is the loaded engines directory: watch shows every registered
	// engine, not only the resident one.
	Registry *config.Registry
	// State reads the ownership record.
	State Store
	// Factory builds an engine driver; nil asks engine.New with DockerBin.
	Factory func(spec *config.Engine) (engine.Engine, error)
	// DockerBin overrides the docker binary the default driver uses.
	DockerBin string
	// Busy asks one engine's BUSY probe; nil asks the probe itself.
	Busy func(ctx context.Context, spec *config.Engine) probe.Result
	// Now is the snapshot clock; nil is time.Now.
	Now func() time.Time
}

// Snapshot takes one reading. It reports rather than fails: a loop that
// stopped on the first unreachable engine would leave the operator staring at
// a frozen screen, which is worse than a stale one.
func (r *Runner) Snapshot(ctx context.Context) *Snapshot {
	s := &Snapshot{Time: r.now(), ActiveAlive: unmeasured, Slot: 0}
	rec, _ := r.load(s)

	if rec != nil && rec.Active != "" {
		s.Active, s.ActivePID, s.Previous, s.Slot = rec.Active, rec.ActivePID, rec.Previous, rec.Slot
		if _, ok := r.spec(rec.Active); !ok {
			// The same verdict doctor gives: no cardo command can act on this
			// record, so watch does not pretend to be measuring it.
			s.Warnings = append(s.Warnings, fmt.Sprintf(
				"stale record (use down --forget): %q is recorded resident but has no config file", rec.Active))
		} else {
			s.Resident = true
		}
	}

	// Only the resident is measured. Its process is the one the record claims
	// is alive and its queue is the one a swap would have to drain; asking
	// every engine would put a network round trip per row in a two-second
	// loop for numbers nobody acts on.
	var busy, note string
	if s.Resident {
		spec, _ := r.spec(s.Active)
		var busyNote string
		s.ActiveAlive, note = r.aliveOf(ctx, spec, s.ActivePID)
		busy, busyNote = r.busyOf(ctx, spec)
		note = joinNotes(note, busyNote)
	}

	for _, name := range r.Registry.Names() {
		spec, ok := r.spec(name)
		if !ok {
			continue
		}
		row := Row{Name: name, Type: string(spec.Type), Slot: spec.Slot, Alive: unmeasured, Busy: unmeasured}
		if s.Resident && name == s.Active {
			row.Active = true
			row.Alive, row.Busy, row.Note = s.ActiveAlive, busy, note
		}
		s.Rows = append(s.Rows, row)
	}
	return s
}

// Run prints a snapshot every interval until ctx is cancelled (the CLI binds
// it to SIGINT, so Ctrl-C is this loop's normal ending) or ticks snapshots
// have been printed (ticks <= 0 means forever).
//
// interval <= 0 takes the package default, which is what makes the cadence
// injectable without every caller naming it.
func Run(ctx context.Context, r *Runner, interval time.Duration, ticks int, w io.Writer) error {
	if r == nil {
		return errors.New("watch: no runner to snapshot")
	}
	if w == nil {
		w = io.Discard
	}
	if interval <= 0 {
		interval = Interval
	}
	if interval <= 0 {
		return errors.New("watch: interval must be positive")
	}
	for i := 0; ; i++ {
		if ctx.Err() != nil {
			// An operator's Ctrl-C is not an error.
			return nil
		}
		if err := Render(w, r.Snapshot(ctx)); err != nil {
			return err
		}
		if ticks > 0 && i+1 >= ticks {
			return nil
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// Render writes one snapshot. The shape is fixed: a stamped header, the
// status picture, one line per engine at a stable width, then any warning.
func Render(w io.Writer, s *Snapshot) error {
	if s == nil {
		return errors.New("watch: nothing to render")
	}
	slot := strconv.Itoa(s.Slot)
	if !s.Resident {
		slot = unmeasured
	}
	lines := []string{
		fmt.Sprintf("watch %s  slot %s", s.Time.Format("2006-01-02 15:04:05"), slot),
		"> " + picture(s),
	}
	lines = append(lines, rows(s)...)
	for _, warn := range s.Warnings {
		lines = append(lines, "  ! "+warn)
	}
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	return err
}

// picture is the status line: the same facts `cardo status` reports, in one
// line a scanning eye can diff between ticks.
func picture(s *Snapshot) string {
	resident := s.Active
	if resident == "" {
		resident = "none"
	}
	parts := []string{"resident " + resident, "alive " + s.ActiveAlive}
	if s.ActivePID > 0 {
		parts = append(parts, "pid "+strconv.Itoa(s.ActivePID))
	}
	if s.Previous != "" {
		parts = append(parts, "previous "+s.Previous)
	}
	return strings.Join(parts, "  ")
}

// rows renders the per-engine lines at a width that does not move with the
// name lengths, so consecutive ticks line up.
func rows(s *Snapshot) []string {
	width := rowMinNameWidth
	for _, r := range s.Rows {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	out := make([]string, 0, len(s.Rows))
	for _, r := range s.Rows {
		mark := " "
		if r.Active {
			mark = "*"
		}
		line := fmt.Sprintf("%s %-*s %-8s slot %-3d alive %-3s busy %-3s",
			mark, width, r.Name, r.Type, r.Slot, r.Alive, r.Busy)
		if r.Note != "" {
			line += "  " + r.Note
		}
		out = append(out, line)
	}
	return out
}

// aliveOf asks the driver whether the engine is running: docker and compose
// ask docker, a command engine is its recorded pid. An error is "?" with the
// reason, never "no".
func (r *Runner) aliveOf(ctx context.Context, spec *config.Engine, pid int) (string, string) {
	drv, err := r.driver(spec)
	if err != nil {
		return unknown, cannotVerify(err)
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	alive, err := drv.Alive(cctx, engine.Running{PID: pid, Container: spec.Container})
	if err != nil {
		return unknown, cannotVerify(err)
	}
	if !alive {
		return aliveNo, ""
	}
	return aliveYes, ""
}

// busyOf asks the resident's BUSY probe. Unknown is "?": a 0 in a monitoring
// loop reads as "nothing to drain, safe to swap", and that is not what an
// unanswerable probe means.
func (r *Runner) busyOf(ctx context.Context, spec *config.Engine) (string, string) {
	if spec.Busy == nil {
		return unmeasured, ""
	}
	res := r.askBusy(ctx, spec)
	switch res.State {
	case probe.Free:
		return "0", ""
	case probe.Busy:
		return strconv.Itoa(res.Count), ""
	default:
		return unknown, trim(res.Err)
	}
}

// askBusy runs the BUSY question, through the injected hook when there is one.
func (r *Runner) askBusy(ctx context.Context, spec *config.Engine) probe.Result {
	if r.Busy != nil {
		return r.Busy(ctx, spec)
	}
	p := newProbe(spec.Busy)
	if p == nil {
		return probe.Result{State: probe.Unknown,
			Err: fmt.Errorf("BUSY %s#%s is not a usable probe", spec.Busy.URL, spec.Busy.Selector)}
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	return p.Check(cctx)
}

// load reads the record once, recording an unreadable one as a warning. A
// corrupt record must not be shown as an idle slot: the operator would read
// "nothing resident" where the truth is "cardo cannot tell".
func (r *Runner) load(s *Snapshot) (*state.Record, error) {
	if r.State == nil {
		return nil, nil
	}
	rec, err := r.State.Load()
	if errors.Is(err, state.ErrNoState) {
		return nil, nil
	}
	if err != nil {
		s.Warnings = append(s.Warnings, fmt.Sprintf("state: %v", err))
		return nil, err
	}
	return rec, nil
}

// spec looks up an engine by name.
func (r *Runner) spec(name string) (*config.Engine, bool) {
	if r.Registry == nil {
		return nil, false
	}
	spec, ok := r.Registry.Get(name)
	return spec, ok && spec != nil
}

// driver builds the engine driver for one spec.
func (r *Runner) driver(spec *config.Engine) (engine.Engine, error) {
	if r.Factory != nil {
		return r.Factory(spec)
	}
	return engine.New(spec, engine.Options{DockerBin: r.DockerBin})
}

// now is the snapshot clock.
func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// cannotVerify is the divergence wording shared with doctor: an unanswered
// question is its own finding, and never a death certificate.
func cannotVerify(err error) string {
	return fmt.Sprintf("cannot verify (driver error: %v)", err)
}

// trim makes an error fit one column of a line without swallowing its point.
func trim(err error) string {
	if err == nil {
		return "probe could not answer"
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return msg
}

// joinNotes merges the reasons a row's columns are unknown.
func joinNotes(notes ...string) string {
	var kept []string
	for _, n := range notes {
		if n != "" {
			kept = append(kept, n)
		}
	}
	return strings.Join(kept, "; ")
}

// newProbe renders a parsed config probe as a live probe, so watch asks a
// BUSY endpoint exactly the way the drain loop does.
func newProbe(p *config.Probe) *probe.Probe {
	if p == nil {
		return nil
	}
	live, err := probe.Parse(p.URL + "#" + p.Selector.String())
	if err != nil {
		return nil
	}
	return live
}
