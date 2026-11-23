// Package engine starts and stops the engines cardo manages, per ADR-0001:
// one small interface, one implementation per declared TYPE. cardo only
// ever stops what it started (the swap layer owns ownership; this package
// only knows how to drive one engine spec).
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
)

// ErrGone means the engine cardo was asked to stop no longer exists
// (container removed, process exited). Callers treat it as stopped, with a
// warning: it is never an untracked-kill order, and a swap may proceed.
var ErrGone = errors.New("engine is gone")

// ErrNotOurs means a recorded pid is a live process whose command line does
// not prove it belongs to this engine: somebody else's process holds the slot.
// It is deliberately NOT ErrGone. The two outcomes of an unverifiable record
// are opposite: a record whose pid is no process at all is proceedable (the
// slot really is free), while a live unowned pid means the slot is busy, so
// the caller must abort rather than warn and start a second engine on it.
var ErrNotOurs = errors.New("recorded pid is alive but does not match this engine's START")

// aliveTimeout bounds one liveness probe (A: hooks got real budgets; the
// same discipline applies to liveness — a wedged docker daemon must not
// stall retire or health.Wait without bound).
const aliveTimeout = 5 * time.Second

// Running is the handle a Start returns: whichever identifier applies.
type Running struct {
	PID       int
	Container string
}

// Engine is the small boundary of ADR-0001: start, stop, liveness. Readiness
// is NOT this package's job; the health layer owns it.
type Engine interface {
	Spec() *config.Engine
	Start(ctx context.Context) (Running, error)
	// Alive reports liveness; an error means the driver cannot answer
	// (dockerd down, missing binary). Callers must never read an error as
	// "not running": that reads a wedged daemon as a stopped engine.
	Alive(ctx context.Context, running Running) (bool, error)
	Stop(ctx context.Context, running Running, grace time.Duration) error
}

// Options carries the external binaries (overridable for tests).
type Options struct {
	DockerBin string // default "docker"
}

// New builds the Engine for a validated spec.
func New(spec *config.Engine, opts Options) (Engine, error) {
	if opts.DockerBin == "" {
		opts.DockerBin = "docker"
	}
	switch spec.Type {
	case config.TypeDocker:
		return &dockerEngine{spec: spec, dockerBin: opts.DockerBin}, nil
	case config.TypeCompose:
		return &composeEngine{spec: spec, dockerBin: opts.DockerBin}, nil
	case config.TypeCommand:
		return &commandEngine{spec: spec}, nil
	default:
		return nil, fmt.Errorf("engine %s: unknown type %s", spec.Name, spec.Type)
	}
}

// runCmd executes a short lifecycle command and returns its error with
// stderr attached; lifecycle commands must stay quiet and quick.
func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, tail(string(out)))
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		s = s[len(s)-400:]
	}
	return s
}

// dockerEngine drives `docker start/stop` for a single named container.
type dockerEngine struct {
	spec      *config.Engine
	dockerBin string
}

func (d *dockerEngine) Spec() *config.Engine { return d.spec }

func (d *dockerEngine) Start(ctx context.Context) (Running, error) {
	if err := runCmd(ctx, d.dockerBin, "start", d.spec.Container); err != nil {
		return Running{}, err
	}
	return Running{Container: d.spec.Container}, nil
}

// Alive asks docker whether the container is running; an exited container
// is not alive, so health can classify it broken (ADR-0005). A driver that
// cannot answer (dockerd down, missing binary, deadline) is an ERROR, never
// a confident "false": a wedged daemon must abort the swap, not read as a
// stopped engine.
func (d *dockerEngine) Alive(ctx context.Context, running Running) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, aliveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.dockerBin, "inspect", "-f", "{{.State.Running}}", d.spec.Container)
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("docker inspect %s: %w", d.spec.Container, err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

func (d *dockerEngine) Stop(ctx context.Context, running Running, grace time.Duration) error {
	err := runCmd(ctx, d.dockerBin, "stop", "-t", fmt.Sprint(int(grace.Seconds())), d.spec.Container)
	if err == nil {
		return nil
	}
	// Locale-brittle in theory; docker's own error text here is stable and
	// the typed guard is `Alive` before any stop in the swap layer.
	if strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return ErrGone
	}
	return err
}

// composeEngine drives `docker compose up -d / down` for a project or
// directory (ADR-0006 allows COMPOSE_PROJECT to name either).
type composeEngine struct {
	spec      *config.Engine
	dockerBin string
}

// projectArgs returns the compose selector for the configured value: a
// directory gets --project-directory (compose reads that tree's files); a
// bare name gets -p.
func (c *composeEngine) projectArgs() []string {
	if st, err := os.Stat(c.spec.ComposeProject); err == nil && st.IsDir() {
		return []string{"--project-directory", c.spec.ComposeProject}
	}
	return []string{"-p", c.spec.ComposeProject}
}

func (c *composeEngine) Spec() *config.Engine { return c.spec }

func (c *composeEngine) Start(ctx context.Context) (Running, error) {
	args := append([]string{"compose"}, c.projectArgs()...)
	args = append(args, "up", "-d")
	if err := runCmd(ctx, c.dockerBin, args...); err != nil {
		return Running{}, err
	}
	return Running{Container: c.spec.ComposeProject}, nil
}

// Alive reports whether the project has any running container. Driver
// errors are errors, never "not running".
func (c *composeEngine) Alive(ctx context.Context, running Running) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, aliveTimeout)
	defer cancel()
	args := append([]string{"compose"}, c.projectArgs()...)
	args = append(args, "ps", "-q")
	out, err := exec.CommandContext(ctx, c.dockerBin, args...).Output()
	if err != nil {
		return false, fmt.Errorf("docker compose ps %s: %w", c.spec.ComposeProject, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

func (c *composeEngine) Stop(ctx context.Context, running Running, grace time.Duration) error {
	// compose down has its own grace handling via stop_grace_period in the
	// project file; cardo does not override it per call.
	args := append([]string{"compose"}, c.projectArgs()...)
	args = append(args, "down")
	return runCmd(ctx, c.dockerBin, args...)
}

// commandEngine runs the START command in the foreground as a child process
// and owns its lifetime: stop is SIGTERM to the process group, wait the
// grace timeout, then SIGKILL the group — driven directly, never via context
// cancellation, which would silently cap the graceful phase (llama-swap's
// "v219 bug").
type commandEngine struct {
	spec *config.Engine

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func (c *commandEngine) Spec() *config.Engine { return c.spec }

func (c *commandEngine) Start(ctx context.Context) (Running, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		return Running{}, fmt.Errorf("engine %s already started", c.spec.Name)
	}
	cmd := exec.Command("sh", "-c", c.spec.Start)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return Running{}, err
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	c.cmd = cmd
	c.done = done
	return Running{PID: cmd.Process.Pid}, nil
}

func (c *commandEngine) Alive(ctx context.Context, running Running) (bool, error) {
	if running.PID <= 0 {
		return false, nil
	}
	c.mu.Lock()
	mine := c.cmd != nil && c.cmd.Process.Pid == running.PID
	c.mu.Unlock()
	if mine {
		// Our own child: the authoritative signal is the wait channel.
		select {
		case <-c.done:
			return false, nil
		default:
			return true, nil
		}
	}
	return processAlive(running.PID), nil
}

// Stop stops a running handle: either the child this engine object started,
// or a recorded PID from the state record (a fresh process; the recorded
// process group gets the same SIGTERM→grace→SIGKILL sequence). Stop is
// idempotent for handles that are already gone.
func (c *commandEngine) Stop(ctx context.Context, running Running, grace time.Duration) error {
	if running.PID <= 0 {
		return nil
	}
	pid := running.PID
	c.mu.Lock()
	cmd, done := c.cmd, c.done
	c.mu.Unlock()

	if cmd == nil || cmd.Process.Pid != pid {
		// A recorded PID from an earlier run. Its two failure modes mean
		// opposite things to the caller (ADR-0004 ownership check), so they
		// get two distinct outcomes: nothing there at all is ErrGone and the
		// swap may proceed; a live process cardo cannot prove is its own is
		// ErrNotOurs, which must abort the swap. Either way it is never a
		// signal.
		if !processAlive(pid) {
			return fmt.Errorf("%w: recorded pid %d of engine %s is no longer a process", ErrGone, pid, c.spec.Name)
		}
		if !looksLikeEngine(pid, c.spec.Start) {
			return fmt.Errorf("%w: pid %d is alive but its command line is not `sh -c %s`, so cardo will not signal it (engine %s)",
				ErrNotOurs, pid, c.spec.Start, c.spec.Name)
		}
		return stopGroup(pid, grace)
	}

	// Our own child: authoritative liveness is the wait channel; SIGTERM,
	// wait the grace timeout on it, then SIGKILL the group.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(grace):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return fmt.Errorf("engine %s did not die after SIGKILL", c.spec.Name)
	}
}

// stopGroup runs the SIGTERM → grace → SIGKILL sequence against a process
// group and returns once the group leader is gone.
func stopGroup(pid int, grace time.Duration) error {
	// Graceful phase: SIGTERM to the whole group.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Forceful phase: SIGKILL the whole group.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	forceDeadline := time.Now().Add(5 * time.Second)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(forceDeadline) {
			return fmt.Errorf("pid %d did not die after SIGKILL", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// processAlive reports whether pid is a live process. A zombie is not alive:
// the process cardo owned has finished, it holds no memory and answers no
// requests, and a parent that has not got round to reaping it yet must not
// turn a successful stop into "did not die after SIGKILL". (The in-instance
// case is covered by the wait channel, not by this check.)
func processAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, syscall.Signal(0)) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false // it went away between the two calls
	}
	// "pid (comm) state ...": comm may contain spaces and parentheses, so
	// the state is the word after the last ')'.
	rest := string(b)
	if i := strings.LastIndexByte(rest, ')'); i >= 0 {
		rest = strings.TrimSpace(rest[i+1:])
	} else {
		return true // unreadable shape: assume the signal 0 answer
	}
	return !strings.HasPrefix(rest, "Z")
}

// looksLikeEngine verifies a recorded PID before cardo signals it, reading
// the command line the kernel reports for it.
func looksLikeEngine(pid int, start string) bool {
	if start == "" {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(b) == 0 {
		return false
	}
	return provesOwnership(strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00"), start)
}

// provesOwnership is the whole ownership rule, as a pure function of argv and
// START so every shape is testable (a shell that has exec'd its payload is
// gone again before any test could look at it).
//
// cardo starts command engines as `sh -c <START>` (see commandEngine.Start),
// so the process is ours only if its command line is one of exactly two
// things:
//
//  1. argv[0] whose last path element is "sh" (NOT "fish", NOT "bash": the
//     old suffix match killed a foreign `fish -c ...`), argv[1] == "-c", and
//     argv[2] this engine's own START — or that START with a leading `exec `
//     (a shell that was handed the payload exec-prefixed and has not replaced
//     itself yet);
//  2. the payload of a START that begins with `exec `, which is what dash
//     leaves behind when it replaces the shell image in place: `/proc` then
//     shows the program itself (`sleep 297`), not the shell. An exec'd image
//     is only ever provable for a START that asked for exec, so a plain START
//     can never claim a non-shell process.
//
// Anything else is somebody else's process and is left running. A payload
// whose quoting the shell had to reshape (it arrives as fewer argv words than
// Fields produces) is not provable either: cardo refuses rather than guessing
// at a kill, and `cardo down --forget` remains the escape hatch.
func provesOwnership(argv []string, start string) bool {
	if start == "" || len(argv) == 0 {
		return false
	}
	if len(argv) >= 3 && path.Base(argv[0]) == "sh" && argv[1] == "-c" {
		return argv[2] == start || argv[2] == "exec "+start
	}
	payload, ok := execPayload(start)
	if !ok {
		return false
	}
	if len(argv) != len(payload) || path.Base(argv[0]) != path.Base(payload[0]) {
		return false
	}
	for i := 1; i < len(argv); i++ {
		if argv[i] != payload[i] {
			return false
		}
	}
	return true
}

// execPayload splits the payload of a START whose first word is the `exec`
// builtin, the shape that lets a command engine run in the foreground without
// a shell parent holding the process group.
func execPayload(start string) ([]string, bool) {
	fields := strings.Fields(start)
	if len(fields) < 2 || fields[0] != "exec" {
		return nil, false
	}
	return fields[1:], true
}
