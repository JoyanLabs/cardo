// Package doctor validates the engines directory and then asks every engine
// the questions a swap would ask it: is HEALTH reachable, can BUSY answer,
// can PROGRESS answer, and does the state record agree with what the driver
// sees. It is the one-shot check an operator runs before trusting `cardo up`
// with a swap, so the same fail-closed rules apply: a probe that cannot
// answer is reported, never read as "free", and a driver that cannot answer
// is never read as "not running".
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/engine"
	"github.com/JoyanLabs/cardo/internal/probe"
	"github.com/JoyanLabs/cardo/internal/state"
)

// CheckTimeout bounds one network question. It is a variable so a test can
// prove a wedged endpoint becomes a finding instead of a hung doctor.
var CheckTimeout = 3 * time.Second

// maxHealthBodyBytes bounds what doctor reads of a health answer. It reports
// the status code, not the payload.
const maxHealthBodyBytes = 4 << 10

// Level is the severity of one report line, and the prefix an operator (or a
// grep) reads.
type Level int

// The three severities a report line can carry.
const (
	// LevelErr is a config problem: cardo cannot trust what it is being told
	// to run, so no engine check can be believed until it is fixed.
	LevelErr Level = iota
	// LevelWarn is a runtime problem: the setup is usable, but something the
	// operator asked about is not what it should be.
	LevelWarn
	// LevelOK is the line an engine gets when nothing about it is wrong, so a
	// clean run still says what it looked at.
	LevelOK
)

// String is the line prefix.
func (l Level) String() string {
	switch l {
	case LevelErr:
		return "[err]"
	case LevelWarn:
		return "[warn]"
	default:
		return "[ok]"
	}
}

// Finding is one line of the report.
type Finding struct {
	Level Level
	// Subject is the engine name, or the subsystem a finding is not about an
	// engine of ("config", "state").
	Subject string
	Message string
}

// String renders the line.
func (f Finding) String() string {
	return fmt.Sprintf("%s %s: %s", f.Level, f.Subject, f.Message)
}

// Report is one doctor pass.
type Report struct {
	// Engines is how many engines the registry holds, so "0 engines, 0
	// findings" cannot be mistaken for a checked-and-clean directory.
	Engines int
	// Findings is every line, already in report order: config first, then
	// engines sorted by name, then the state record.
	Findings []Finding
}

// add appends one line.
func (r *Report) add(l Level, subject, message string) {
	r.Findings = append(r.Findings, Finding{Level: l, Subject: subject, Message: message})
}

// Count is how many findings need attention, which is the M in the summary
// line and the reason for the exit code. The [ok] lines are not findings.
func (r *Report) Count() int {
	n := 0
	for _, f := range r.Findings {
		if f.Level != LevelOK {
			n++
		}
	}
	return n
}

// OK reports a run with nothing to fix.
func (r *Report) OK() bool { return r.Count() == 0 }

// StateLoader is the state surface doctor reads; *state.Store satisfies it.
type StateLoader interface {
	Load() (*state.Record, error)
}

// Runner is one doctor pass over one engines directory.
type Runner struct {
	// ConfDir is the engines directory, read with config.ScanDir.
	ConfDir string
	// State reads the ownership record. A nil State reports no divergence
	// rather than inventing one.
	State StateLoader
	// Factory builds an engine driver; nil asks engine.New with DockerBin.
	// Tests use it to hand over a docker shim.
	Factory func(spec *config.Engine) (engine.Engine, error)
	// DockerBin overrides the docker binary the default driver uses.
	DockerBin string
}

// Run makes one pass. It returns an error only when the pass itself was
// impossible (an unreadable directory that is not simply missing): every
// engine-level failure is a Finding, because a doctor that dies on the first
// refused socket says nothing about the engines after it.
func (r *Runner) Run(ctx context.Context) (*Report, error) {
	rep := &Report{}
	scan, err := config.ScanDir(r.ConfDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			rep.add(LevelErr, "config", fmt.Sprintf("%s does not exist yet: there are no engines to validate", r.ConfDir))
			return rep, nil
		}
		return nil, err
	}
	for _, p := range scan.Problems {
		rep.add(LevelErr, "config", p.Error())
	}
	for _, sk := range scan.Skipped {
		if !sk.EngineLike {
			continue
		}
		// The whole point of this line: an engine whose file lost its suffix
		// is not registered, and nothing else in cardo ever mentions it.
		rep.add(LevelWarn, sk.Name, "not a .conf file, skipped: it holds engine configuration, so rename it to "+sk.Name+".conf to register it")
	}

	rec, stateErr := r.loadState(rep)
	names := scan.Registry.Names()
	for _, name := range names {
		e, ok := scan.Registry.Get(name)
		if !ok {
			continue
		}
		r.checkEngine(ctx, rep, e, rec)
	}
	r.checkRecord(rep, scan.Registry, rec, stateErr)

	rep.Engines = len(scan.Registry.Engines)
	return rep, nil
}

// checkEngine asks one engine its questions and writes its lines. An engine
// with nothing wrong gets a single [ok] line carrying the live data, so the
// operator sees what was looked at.
func (r *Runner) checkEngine(ctx context.Context, rep *Report, e *config.Engine, rec *state.Record) {
	var shown []string
	var findings []Finding

	summary, f := r.checkHealth(ctx, e)
	if f != nil {
		findings = append(findings, *f)
	} else {
		shown = append(shown, summary)
	}
	summary, f = r.checkBusy(ctx, e)
	if f != nil {
		findings = append(findings, *f)
	} else if summary != "" {
		shown = append(shown, summary)
	}
	summary, f = r.checkProgress(ctx, e)
	if f != nil {
		findings = append(findings, *f)
	} else if summary != "" {
		shown = append(shown, summary)
	}
	if rec != nil && rec.Active == e.Name {
		summary, fs := r.checkResident(ctx, e, rec)
		findings = append(findings, fs...)
		if len(fs) == 0 && summary != "" {
			shown = append(shown, summary)
		}
	}

	if len(findings) > 0 {
		rep.Findings = append(rep.Findings, findings...)
		return
	}
	line := "nothing to check"
	if len(shown) > 0 {
		line = strings.Join(shown, "; ")
	}
	rep.add(LevelOK, e.Name, line)
}

// checkHealth is the connectivity half of the verb: 2xx is healthy, any other
// HTTP answer is "answers but not healthy", and a transport failure is "not
// reachable". All three are verdicts about this engine; none of them ends the
// run.
func (r *Runner) checkHealth(ctx context.Context, e *config.Engine) (string, *Finding) {
	warn := func(format string, args ...any) (string, *Finding) {
		return "", &Finding{Level: LevelWarn, Subject: e.Name, Message: fmt.Sprintf(format, args...)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.Health, nil)
	if err != nil {
		return warn("health not reachable: HEALTH %q is not a usable URL: %v", e.Health, err)
	}
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	req = req.WithContext(cctx)
	client := &http.Client{
		Timeout: CheckTimeout,
		// One-shot requests: a keep-alive connection outliving the report is
		// the kind of thing a wedged health server can hold open.
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	resp, err := client.Do(req)
	if err != nil {
		return warn("health not reachable: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHealthBodyBytes))
	if resp.StatusCode/100 == 2 {
		return fmt.Sprintf("health %d", resp.StatusCode), nil
	}
	return warn("health answers but not healthy: HTTP %d from %s", resp.StatusCode, e.Health)
}

// checkBusy is the ADR-0002 story surfaced before a swap needs it: Free and
// Busy are both honest answers (and the count is what the operator came to
// read), Unknown is a warning with the probe's own error.
func (r *Runner) checkBusy(ctx context.Context, e *config.Engine) (string, *Finding) {
	if e.Busy == nil {
		return "", nil
	}
	p := newProbe(e.Busy)
	if p == nil {
		return "", &Finding{Level: LevelWarn, Subject: e.Name,
			Message: fmt.Sprintf("BUSY probe could not answer: %s#%s is not a usable probe", e.Busy.URL, e.Busy.Selector)}
	}
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	res := p.Check(cctx)
	switch res.State {
	case probe.Free, probe.Busy:
		// Both are honest answers, and the count is what the operator came to
		// read: BUSY 0 is the number that says "a swap can drain nothing".
		return fmt.Sprintf("BUSY %d in flight", res.Count), nil
	default:
		return "", &Finding{Level: LevelWarn, Subject: e.Name,
			Message: fmt.Sprintf("BUSY probe could not answer: %v", res.Err)}
	}
}

// checkProgress is ADR-0005's optional PROGRESS key: no key is nothing to
// say, a value is shown, and a failing endpoint is a warning.
func (r *Runner) checkProgress(ctx context.Context, e *config.Engine) (string, *Finding) {
	if e.Progress == nil {
		return "", nil
	}
	p := newProbe(e.Progress)
	if p == nil {
		return "", &Finding{Level: LevelWarn, Subject: e.Name,
			Message: fmt.Sprintf("PROGRESS probe could not answer: %s#%s is not a usable probe", e.Progress.URL, e.Progress.Selector)}
	}
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	defer cancel()
	v, err := p.ProgressValue(cctx)
	if err != nil {
		return "", &Finding{Level: LevelWarn, Subject: e.Name,
			Message: fmt.Sprintf("PROGRESS probe could not answer: %v", err)}
	}
	if v == nil {
		// The endpoint answered and simply carries no numeric progress: not
		// a fault, and not a value to show either.
		return "", nil
	}
	return fmt.Sprintf("progress %g", *v), nil
}

// checkResident is the divergence check (ADR-0004): the record claims this
// engine is resident, so the driver is asked whether it really is. A driver
// that cannot answer is its own finding and never a death certificate;
// reading a wedged dockerd as "exited" is how a second engine ends up on a
// held slot.
func (r *Runner) checkResident(ctx context.Context, e *config.Engine, rec *state.Record) (string, []Finding) {
	drv, err := r.driver(e)
	if err != nil {
		return "", []Finding{{Level: LevelWarn, Subject: e.Name,
			Message: fmt.Sprintf("cannot verify (driver error: %v)", err)}}
	}
	alive, err := drv.Alive(ctx, engine.Running{PID: rec.ActivePID, Container: e.Container})
	if err != nil {
		return "", []Finding{{Level: LevelWarn, Subject: e.Name,
			Message: fmt.Sprintf("cannot verify (driver error: %v)", err)}}
	}
	if !alive {
		return "", []Finding{{Level: LevelWarn, Subject: e.Name,
			Message: fmt.Sprintf("recorded resident but not running: the record claims pid %d; if the engine is really gone, `cardo down --forget` clears the record", rec.ActivePID)}}
	}
	return fmt.Sprintf("resident pid %d alive", rec.ActivePID), nil
}

// loadState reads the record once. An unreadable record is a finding in its
// own right; it is never the same as "nothing resident", which would tell the
// operator the slot is free.
func (r *Runner) loadState(rep *Report) (*state.Record, error) {
	if r.State == nil {
		return nil, nil
	}
	rec, err := r.State.Load()
	if errors.Is(err, state.ErrNoState) {
		return nil, nil
	}
	if err != nil {
		rep.add(LevelErr, "state", err.Error())
		return nil, err
	}
	return rec, nil
}

// checkRecord reports a record that no cardo command can act on: it names an
// engine that has no config file, so neither `down` nor `up` can stop it.
func (r *Runner) checkRecord(rep *Report, reg *config.Registry, rec *state.Record, stateErr error) {
	if stateErr != nil || rec == nil || rec.Active == "" {
		return
	}
	if _, ok := reg.Get(rec.Active); ok {
		return
	}
	rep.add(LevelWarn, "state", fmt.Sprintf("stale record (use down --forget): %q is recorded resident but has no config file, so no cardo command can stop it", rec.Active))
}

// driver builds the engine driver for one spec.
func (r *Runner) driver(spec *config.Engine) (engine.Engine, error) {
	if r.Factory != nil {
		return r.Factory(spec)
	}
	return engine.New(spec, engine.Options{DockerBin: r.DockerBin})
}

// newProbe renders a parsed config probe as a live probe. probe.Parse is the
// only constructor, so doctor asks a BUSY/PROGRESS pair exactly the way the
// drain loop does.
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

// Render writes the report: one line per finding with its prefix, engines in
// sorted order, and the verdict line last so a script can read the tail.
func Render(w io.Writer, rep *Report) error {
	for _, f := range rep.Findings {
		if _, err := fmt.Fprintln(w, f.String()); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%d engines, %d findings\n", rep.Engines, rep.Count())
	return err
}
