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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
)

// ErrGone means the engine cardo was asked to stop no longer exists
// (container removed, process exited). Callers treat it as stopped, with a
// warning: it is never an untracked-kill order.
var ErrGone = errors.New("engine is gone")

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
	Alive(ctx context.Context, running Running) bool
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
// is not alive, so health can classify it broken (ADR-0005).
func (d *dockerEngine) Alive(ctx context.Context, running Running) bool {
	cmd := exec.CommandContext(ctx, d.dockerBin, "inspect", "-f", "{{.State.Running}}", d.spec.Container)
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
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

// Alive reports whether the project has any running container.
func (c *composeEngine) Alive(ctx context.Context, running Running) bool {
	args := append([]string{"compose"}, c.projectArgs()...)
	args = append(args, "ps", "-q")
	out, err := exec.CommandContext(ctx, c.dockerBin, args...).Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
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

func (c *commandEngine) Alive(ctx context.Context, running Running) bool {
	if running.PID <= 0 {
		return false
	}
	c.mu.Lock()
	mine := c.cmd != nil && c.cmd.Process.Pid == running.PID
	c.mu.Unlock()
	if mine {
		// Our own child: the authoritative signal is the wait channel.
		select {
		case <-c.done:
			return false
		default:
			return true
		}
	}
	return processAlive(running.PID)
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
		// A recorded PID from an earlier run: verify it still looks like this
		// engine before signalling (ADR-0004 ownership check).
		if !looksLikeEngine(pid, c.spec.Start) {
			return fmt.Errorf("%w: recorded pid %d does not match engine %s", ErrGone, pid, c.spec.Name)
		}
		if !processAlive(pid) {
			return nil
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

// looksLikeEngine verifies a recorded PID before cardo signals it: the
// command engines cardo starts are `sh -c <START>`, so the process cmdline
// must name sh (or the shell that replaced itself) — anything else is not
// ours and must not be signalled.
func looksLikeEngine(pid int, start string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(b) == 0 {
		return false
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) == 0 {
		return false
	}
	if strings.HasSuffix(parts[0], "sh") {
		return true
	}
	// A START whose first token replaced the shell entirely.
	if len(start) > 0 && strings.Contains(string(b), strings.Fields(start)[0]) {
		return true
	}
	return false
}
