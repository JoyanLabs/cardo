// Package engine starts and stops the engines cardo manages, per ADR-0001:
// one small interface, one implementation per declared TYPE. cardo only
// ever stops what it started (the swap layer owns ownership; this package
// only knows how to drive one engine spec).
package engine

import (
	"context"
	"errors"
	"fmt"
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

// Alive reports whether this running handle still refers to something.
func (r Running) Alive() bool {
	if r.PID > 0 {
		return syscall.Kill(r.PID, syscall.Signal(0)) == nil
	}
	if r.Container != "" {
		return true
	}
	return false
}

// Engine is the small boundary of ADR-0001: start, stop, that is all.
// Readiness is NOT this package's job; the health layer owns it.
type Engine interface {
	Spec() *config.Engine
	Start(ctx context.Context) (Running, error)
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

func (d *dockerEngine) Stop(ctx context.Context, running Running, grace time.Duration) error {
	err := runCmd(ctx, d.dockerBin, "stop", "-t", fmt.Sprint(int(grace.Seconds())), d.spec.Container)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "No such container") {
		return ErrGone
	}
	return err
}

// composeEngine drives `docker compose -p <project> up -d / down`.
type composeEngine struct {
	spec      *config.Engine
	dockerBin string
}

func (c *composeEngine) Spec() *config.Engine { return c.spec }

func (c *composeEngine) Start(ctx context.Context) (Running, error) {
	if err := runCmd(ctx, c.dockerBin, "compose", "-p", c.spec.ComposeProject, "up", "-d"); err != nil {
		return Running{}, err
	}
	return Running{Container: c.spec.ComposeProject}, nil
}

func (c *composeEngine) Stop(ctx context.Context, running Running, grace time.Duration) error {
	// compose down has its own grace handling via stop_grace_period in the
	// project file; cardo does not override it per call.
	return runCmd(ctx, c.dockerBin, "compose", "-p", c.spec.ComposeProject, "down")
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

func (c *commandEngine) Stop(ctx context.Context, running Running, grace time.Duration) error {
	c.mu.Lock()
	cmd, done := c.cmd, c.done
	c.mu.Unlock()
	if cmd == nil || running.PID <= 0 {
		return nil
	}
	pid := cmd.Process.Pid
	if running.PID != pid {
		// A different run than the one this engine object started: the
		// recorded process is not ours to signal. Treat as gone.
		return ErrGone
	}
	// Gone already? SIGTERM to a dead pid just errors; treat as done.
	if syscall.Kill(-pid, syscall.Signal(0)) != nil &&
		syscall.Kill(pid, syscall.Signal(0)) != nil {
		return nil
	}
	// Graceful phase: SIGTERM to the whole group.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
		return nil
	case <-time.After(grace):
		// Forceful phase: SIGKILL the whole group.
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
}
