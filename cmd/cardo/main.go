// Command cardo swaps which engine holds one GPU slot: drain first, roll
// back if the new one does not answer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
	"github.com/JoyanLabs/cardo/internal/doctor"
	"github.com/JoyanLabs/cardo/internal/state"
	"github.com/JoyanLabs/cardo/internal/swap"
	"github.com/JoyanLabs/cardo/internal/watch"
)

// defaultConfDir is $XDG_CONFIG_HOME/cardo/engines.d (default
// ~/.config/cardo/engines.d): one file per engine, no hardcoded paths.
func defaultConfDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "cardo", "engines.d")
}

const usage = `cardo — one model per GPU. Swap which engine holds the slot.

Usage:
  cardo [--conf-dir DIR] list          registered engines and which one is up
  cardo [--conf-dir DIR] status        what the state record says
  cardo [--conf-dir DIR] doctor        validate every .conf, then check each engine's health and probes
  cardo [--conf-dir DIR] watch         what is resident on the slot right now, refreshed (Ctrl-C stops it)
  cardo [--conf-dir DIR] up NAME      drain, stop, start, verify, roll back on failure
  cardo [--conf-dir DIR] down         stop the resident engine, draining first
  cardo [--conf-dir DIR] down --forget clear the record without stopping anything
                                       (escape hatch when the record diverged)

Flags:
  --conf-dir DIR     directory holding one .conf per engine (default %s)
  --forget           with down only: forget the resident engine, leave it running
  --interval SECONDS with watch only: seconds between snapshots (%s, default 2)

doctor exits 0 when it finds nothing and 1 when it does; each line is
prefixed [err], [warn] or [ok] and the last line reads "N engines, M findings".

Engines are declared in small .conf files, one per engine, under --conf-dir.`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("cardo", flag.ContinueOnError)
	confDir := fs.String("conf-dir", "", "directory holding one .conf per engine")
	forget := fs.Bool("forget", false, "down only: clear the state record without stopping the engine")
	interval := fs.Float64("interval", 0, "watch only: seconds between snapshots (0.25 to 86400, default 2)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), usage+"\n", defaultConfDir(), intervalBounds())
	}
	// People type `cardo down --forget`; the stdlib flag package stops at the
	// first positional, so the accepted flags are moved ahead of the verb.
	flags, positional := splitArgs(args)
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if len(positional) == 0 {
		fs.Usage()
		return 2
	}
	for _, a := range positional {
		if strings.HasPrefix(a, "-") {
			fmt.Fprintf(os.Stderr, "cardo: unknown flag %q (see cardo --help)\n", a)
			return 2
		}
	}
	verb := positional[0]
	if *forget && verb != "down" {
		fmt.Fprintln(os.Stderr, "cardo: --forget applies to `down` only")
		return 2
	}
	if *interval != 0 && verb != "watch" {
		fmt.Fprintln(os.Stderr, "cardo: --interval applies to `watch` only")
		return 2
	}
	// The cadence is validated with the rest of the flags, before any verb
	// runs: a bad number is an operator mistake, and it is answered the way a
	// bad flag is answered (usage exit), not by starting a run that cannot be
	// a status screen.
	tick, tickErr := intervalValue(*interval)
	if tickErr != nil {
		fmt.Fprintln(os.Stderr, "cardo:", tickErr)
		return 2
	}
	if *confDir == "" {
		*confDir = defaultConfDir()
	}
	st := &state.Store{Dir: state.ResolveDir("")}
	var err error

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// status speaks only to the state record; doctor reads the directory
	// itself, because a directory it cannot read is one of its findings. The
	// other verbs need engines.
	var reg *config.Registry
	var scan *config.DirScan
	switch verb {
	case "status":
		return doStatus(st)
	case "doctor":
		return doDoctor(ctx, *confDir, st)
	case "list", "up", "down", "watch":
		// The scan rather than LoadDir's registry alone, because `list` also
		// has to mention the entries cardo would not open: a file that was
		// skipped without being read is invisible until doctor is run, and a
		// named pipe left where an engine file belongs is exactly the entry an
		// operator cannot find by looking. scan.Err() is what LoadDir would
		// have returned (it is defined in terms of it), so the verb that acts
		// and the verb that reports cannot disagree about a broken directory.
		s, err := config.ScanDir(*confDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && verb == "list" {
				fmt.Fprintf(os.Stderr, "cardo: no engines: %s does not exist yet\n", *confDir)
				return 0
			}
			fmt.Fprintln(os.Stderr, "cardo:", err)
			return 1
		}
		if err := s.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "cardo:", err)
			return 1
		}
		reg, scan = s.Registry, s
	default:
		fmt.Fprintf(os.Stderr, "cardo: unknown verb %q (want list, status, doctor, watch, up or down)\n", verb)
		return 2
	}

	sw := &swap.Swapper{Registry: reg, State: st, Out: os.Stderr}

	switch verb {
	case "list":
		return doList(reg, st, scan)
	case "watch":
		return doWatch(ctx, reg, st, tick)
	case "up":
		if len(positional) != 2 {
			fmt.Fprintln(os.Stderr, "cardo: up takes exactly one engine name")
			return 2
		}
		err = sw.Up(ctx, positional[1])
	case "down":
		err = sw.Down(ctx, *forget)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	return 0
}

// splitArgs moves cardo's own flags ahead of the verb and returns the
// positional words separately. Anything it does not recognise stays with the
// verb, where run reports it as an unknown flag rather than treating it as
// another argument.
func splitArgs(args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-conf-dir" || a == "--conf-dir":
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case strings.HasPrefix(a, "-conf-dir=") || strings.HasPrefix(a, "--conf-dir="):
			flags = append(flags, a)
		case a == "-forget" || a == "--forget" || a == "-forget=true" || a == "--forget=true" ||
			a == "-forget=false" || a == "--forget=false":
			flags = append(flags, a)
		case a == "-interval" || a == "--interval":
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case strings.HasPrefix(a, "-interval=") || strings.HasPrefix(a, "--interval="):
			flags = append(flags, a)
		case a == "-h" || a == "--help" || a == "-help":
			flags = append(flags, a)
		default:
			positional = append(positional, a)
		}
	}
	return flags, positional
}

// doList prints the registry with the resident marked. scan is the read that
// produced the registry, used for the one skipped entry an operator has to
// hear about from every verb that lists the directory: an entry cardo would
// not open. Documentation and editor backups stay quiet here — reviewing those
// is doctor's job, and a listing that comments on the README is a listing
// nobody reads.
func doList(reg *config.Registry, st *state.Store, scan *config.DirScan) int {
	if scan != nil {
		for _, sk := range scan.Skipped {
			if sk.Special {
				fmt.Fprintf(os.Stderr, "cardo: skipped %s: %s\n", sk.Name, config.SpecialFileDescription(sk.Mode))
			}
		}
	}
	rec, err := st.Load()
	active := ""
	if err == nil {
		active = rec.Active
	} else if !errors.Is(err, state.ErrNoState) {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	names := reg.Names()
	if len(names) == 0 {
		fmt.Println("no engines registered under the config dir")
		return 0
	}
	for _, n := range names {
		e, _ := reg.Get(n)
		mark := " "
		if n == active {
			mark = "*"
		}
		desc := ""
		if e.Description != "" {
			desc = "  — " + e.Description
		}
		fmt.Printf("%s %-24s %-8s slot %d%s\n", mark, n, e.Type, e.Slot, desc)
	}
	return 0
}

func doStatus(st *state.Store) int {
	rec, err := st.Load()
	if errors.Is(err, state.ErrNoState) {
		fmt.Println("nothing resident (no state record)")
		return 0
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	if rec.Active == "" {
		fmt.Println("nothing resident")
	} else {
		suffix := ""
		// The record claims an engine by pid; if that pid is not a process,
		// the operator must not read "active" as "running". The check is a
		// signal-0 probe, so status never needs an engine driver.
		if rec.ActivePID > 0 && syscall.Kill(rec.ActivePID, syscall.Signal(0)) != nil {
			suffix = " (not running?)"
		}
		fmt.Printf("active:   %s (since %s)%s\n", rec.Active, rec.ActiveAt.Format("2006-01-02 15:04:05"), suffix)
	}
	if rec.Previous != "" {
		fmt.Printf("previous: %s (rollback target)\n", rec.Previous)
	}
	return 0
}

// doDoctor runs one validation-and-connectivity pass. The findings are the
// output, so they go to stdout; the exit code is the verdict a script can
// branch on.
func doDoctor(ctx context.Context, confDir string, st *state.Store) int {
	r := &doctor.Runner{ConfDir: confDir, State: st}
	rep, err := r.Run(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	if rerr := doctor.Render(os.Stdout, rep); rerr != nil {
		fmt.Fprintln(os.Stderr, "cardo:", rerr)
		return 1
	}
	if !rep.OK() {
		return 1
	}
	return 0
}

// The bounds on `cardo watch --interval`, and the reason each end exists.
//
// flag's float parser takes any number, and both extremes are mistakes
// rather than intentions. NaN would arm a timer that never fires (the screen
// freezes and looks like a hung engine); +Inf is the same freeze typed
// differently. At the other end, every tick asks the resident engine its
// HEALTH and BUSY questions, so a microsecond cadence is not a faster status
// screen but a probe flood against the process serving a model — and 0.02s,
// the value a test might reach for, is 50 requests a second.
const (
	minWatchInterval = 250 * time.Millisecond
	maxWatchInterval = 24 * time.Hour
)

// intervalBounds is the help text's statement of what --interval accepts,
// written from the constants above so the help cannot drift from the check.
func intervalBounds() string {
	return fmt.Sprintf("%g to %g seconds", minWatchInterval.Seconds(), maxWatchInterval.Seconds())
}

// intervalValue turns the --interval flag into a tick, or says in one line why
// that number is not a cadence. 0 means "the operator did not name one", and
// is handed to watch.Run, which falls back to watch.Interval.
func intervalValue(seconds float64) (time.Duration, error) {
	switch {
	case seconds == 0:
		return 0, nil
	case math.IsNaN(seconds):
		return 0, fmt.Errorf("--interval nan is not a cadence: it never ticks again; omit the flag for the default %s", intervalBounds())
	case math.IsInf(seconds, 1):
		return 0, fmt.Errorf("--interval +Inf is not a cadence: watch has to tick again eventually; use a number within %s", intervalBounds())
	case math.IsInf(seconds, -1) || seconds < 0:
		return 0, fmt.Errorf("--interval %g is not a cadence: time does not run backwards; use a number within %s", seconds, intervalBounds())
	}
	d := time.Duration(seconds * float64(time.Second))
	if d < minWatchInterval {
		return 0, fmt.Errorf("--interval %g ticks every %s: each tick asks the resident engine its questions, so the fastest cadence is %s", seconds, d, minWatchInterval)
	}
	if d > maxWatchInterval {
		return 0, fmt.Errorf("--interval %g is longer than %s: watch is a status screen, not a scheduler", seconds, maxWatchInterval)
	}
	return d, nil
}

// doWatch prints the slot until the operator interrupts it. interval 0 means
// "whatever the watch package defaults to".
func doWatch(ctx context.Context, reg *config.Registry, st *state.Store, interval time.Duration) int {
	r := &watch.Runner{Registry: reg, State: st}
	if err := watch.Run(ctx, r, interval, 0, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cardo:", err)
		return 1
	}
	return 0
}
