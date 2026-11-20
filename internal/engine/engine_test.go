package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JoyanLabs/cardo/internal/config"
)

// shimDocker writes a fake docker that records its arguments, one
// invocation per line, and exits 0.
func shimDocker(t *testing.T) (bin string, log string) {
	t.Helper()
	bin, log = shimDockerReply(t, "", false)
	return bin, log
}

// shimDockerReply is a fake docker that records its arguments and answers
// every invocation with stdout (optionally failing with exit code 1).
func shimDockerReply(t *testing.T, reply string, fail bool) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "invocations")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
	if reply != "" {
		script += "printf '%s\\n' " + shellQuote(reply) + "\n"
	}
	if fail {
		script += "echo 'Error: No such object' >&2\nexit 1\n"
	}
	script += "exit 0\n"
	bin = filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\\''`) + "'"
}

// startDecoy starts a long-lived process in its own process group so a
// mistaken `kill(-pid)` cannot reach the test process. The caller must stop
// it via t.Cleanup.
func startDecoy(t *testing.T, args ...string) int {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd.Process.Pid
}

// pidAlive reports whether pid exists and is not an unreaped zombie.
func pidAlive(pid int) bool {
	if pid <= 0 || syscall.Kill(pid, syscall.Signal(0)) != nil {
		return false
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// pid (comm) State ...: comm may contain spaces, so read after the last ')'.
	rest := string(b)
	if i := strings.LastIndex(rest, ")"); i >= 0 {
		rest = strings.TrimSpace(rest[i+1:])
		return !strings.HasPrefix(rest, "Z")
	}
	return true
}

// waitForPIDDead polls until pid is gone, returning false on timeout.
func waitForPIDDead(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !pidAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func dockerSpec(t *testing.T, typ config.Type, key, val string) *config.Engine {
	body := "NAME=e\nTYPE=" + string(typ) + "\nHEALTH=http://x\n" + key + "=" + val + "\n"
	// The parser only reports KEY=value lines; keep the value on one line.
	e, err := config.Parse(strings.NewReader(body), "e.conf")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestDockerStartStop(t *testing.T) {
	bin, log := shimDocker(t)
	e := dockerSpec(t, config.TypeDocker, "CONTAINER", "c1")
	eng, err := New(e, Options{DockerBin: bin})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running, err := eng.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if running.Container != "c1" {
		t.Errorf("Running.Container = %q", running.Container)
	}
	if err := eng.Stop(ctx, running, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, log)
	want := []string{"start c1", "stop -t 5 c1"}
	if len(lines) != len(want) {
		t.Fatalf("invocations = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("invocation[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestComposeStartStop(t *testing.T) {
	bin, log := shimDocker(t)
	e := dockerSpec(t, config.TypeCompose, "COMPOSE_PROJECT", "proj")
	eng, err := New(e, Options{DockerBin: bin})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running, err := eng.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if running.Container != "proj" {
		t.Errorf("Running.Container = %q", running.Container)
	}
	if err := eng.Stop(ctx, running, 0); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, log)
	want := []string{"compose -p proj up -d", "compose -p proj down"}
	if len(lines) != len(want) {
		t.Fatalf("invocations = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("invocation[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestComposeUsesProjectDirectoryForADir pins ADR-0006's two shapes:
// COMPOSE_PROJECT naming a directory must select that tree with
// --project-directory, never -p (which would name a project instead).
func TestComposeUsesProjectDirectoryForADir(t *testing.T) {
	bin, log := shimDocker(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := dockerSpec(t, config.TypeCompose, "COMPOSE_PROJECT", dir)
	eng, err := New(e, Options{DockerBin: bin})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running, err := eng.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if running.Container != dir {
		t.Errorf("Running.Container = %q, want %q", running.Container, dir)
	}
	if err := eng.Stop(ctx, running, 0); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, log)
	want := []string{
		"compose --project-directory " + dir + " up -d",
		"compose --project-directory " + dir + " down",
	}
	if len(lines) != len(want) {
		t.Fatalf("invocations = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("invocation[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestDockerAlive pins that an exited container is not alive: health must be
// able to classify a dead engine as broken rather than stalling forever.
func TestDockerAlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		fail bool
		want bool
	}{
		{"running", "true", false, true},
		{"exited", "false", false, false},
		{"padded", "true\n", false, true},
		{"inspect fails", "true", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := shimDockerReply(t, tc.out, tc.fail)
			e := dockerSpec(t, config.TypeDocker, "CONTAINER", "c1")
			eng, err := New(e, Options{DockerBin: bin})
			if err != nil {
				t.Fatal(err)
			}
			if got := eng.Alive(context.Background(), Running{Container: "c1"}); got != tc.want {
				t.Errorf("Alive = %v, want %v (inspect said %q)", got, tc.want, tc.out)
			}
		})
	}
	// The liveness question must be asked of docker with an explicit state
	// format, not guessed from `ps`.
	bin, log := shimDockerReply(t, "true", false)
	e := dockerSpec(t, config.TypeDocker, "CONTAINER", "c1")
	eng, _ := New(e, Options{DockerBin: bin})
	if !eng.Alive(context.Background(), Running{Container: "c1"}) {
		t.Fatal("want alive")
	}
	lines := readLines(t, log)
	if len(lines) != 1 || lines[0] != `inspect -f {{.State.Running}} c1` {
		t.Errorf("invocations = %v", lines)
	}
}

// TestComposeAlive pins that an empty `compose ps -q` (no running container)
// is not alive.
func TestComposeAlive(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"one container", "9f2c1a4b7d3e", true},
		{"only whitespace", "   ", false},
		{"no containers", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, log := shimDockerReply(t, tc.out, false)
			e := dockerSpec(t, config.TypeCompose, "COMPOSE_PROJECT", "proj")
			eng, err := New(e, Options{DockerBin: bin})
			if err != nil {
				t.Fatal(err)
			}
			if got := eng.Alive(context.Background(), Running{Container: "proj"}); got != tc.want {
				t.Errorf("Alive = %v, want %v (ps -q said %q)", got, tc.want, tc.out)
			}
			lines := readLines(t, log)
			if len(lines) != 1 || lines[0] != "compose -p proj ps -q" {
				t.Errorf("invocations = %v, want one `compose -p proj ps -q`", lines)
			}
		})
	}
}

// TestCommandAlive pins the command liveness surfaces: a dead or absent pid
// is never alive, our own child is, a foreign live pid is.
func TestCommandAlive(t *testing.T) {
	ctx := context.Background()
	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", "true")}
	if eng.Alive(ctx, Running{}) {
		t.Error("pid 0 must not be alive")
	}
	if eng.Alive(ctx, Running{PID: 99999999}) {
		t.Error("a pid that never existed must not be alive")
	}

	// A foreign live pid (cardo did not start it) is still alive.
	foreign := startDecoy(t, "sleep", "30")
	if !eng.Alive(ctx, Running{PID: foreign}) {
		t.Errorf("live foreign pid %d reported dead", foreign)
	}

	// Our own child: alive while it runs, dead once the wait channel closes.
	live := dockerSpec(t, config.TypeCommand, "START", "sleep 30")
	own, err := New(live, Options{})
	if err != nil {
		t.Fatal(err)
	}
	running, err := own.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !own.Alive(ctx, running) {
		t.Errorf("own child pid %d reported dead while running", running.PID)
	}
	if err := own.Stop(ctx, running, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if own.Alive(ctx, running) {
		t.Error("stopped child must not report alive")
	}
}

// TestCommandStopRecordedPIDKillsGroup pins the crash-recovery path: a fresh
// cardo run (a brand new engine object that never started the process) must
// still stop the recorded pid the way it would stop its own child — the
// whole process group, not just the group leader.
func TestCommandStopRecordedPIDKillsGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	start := "sleep 71 & echo $! > " + pidFile + "; sleep 71"
	e := dockerSpec(t, config.TypeCommand, "START", start)
	ctx := context.Background()

	first, err := New(e, Options{})
	if err != nil {
		t.Fatal(err)
	}
	running, err := first.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if running.PID <= 0 {
		t.Fatalf("Running.PID = %d", running.PID)
	}
	child := waitForFilePID(t, pidFile, 5*time.Second)
	if !pidAlive(running.PID) || !pidAlive(child) {
		t.Fatalf("group leader %d alive=%v, child %d alive=%v", running.PID, pidAlive(running.PID), child, pidAlive(child))
	}

	// A fresh engine object: it has no idea this process exists.
	second, err := New(e, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Stop(ctx, running, 3*time.Second); err != nil {
		t.Fatalf("Stop of the recorded pid: %v", err)
	}
	if !waitForPIDDead(running.PID, 3*time.Second) {
		t.Errorf("group leader %d survived Stop of the recorded pid", running.PID)
	}
	if !waitForPIDDead(child, 3*time.Second) {
		t.Errorf("background child %d survived: Stop signalled the leader, not the group", child)
	}
}

// TestCommandStopForeignPIDIsErrGoneAndNeverSignalled pins the ADR-0004
// ownership guard: a recorded pid that does not look like this engine is
// reported gone, and cardo must not lay a hand on it.
func TestCommandStopForeignPIDIsErrGoneAndNeverSignalled(t *testing.T) {
	foreign := startDecoy(t, "sleep", "73")
	e := dockerSpec(t, config.TypeCommand, "START", "/nonexistent/cardo-engine-bin --serve")
	eng := &commandEngine{spec: e}

	err := eng.Stop(context.Background(), Running{PID: foreign}, 500*time.Millisecond)
	if !errors.Is(err, ErrGone) {
		t.Fatalf("want ErrGone for a pid cardo does not own, got %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(foreign)) {
		t.Errorf("error must name the offending pid: %v", err)
	}
	if !pidAlive(foreign) {
		t.Errorf("cardo signalled a process it does not own (pid %d is dead)", foreign)
	}
}

// TestStopGonePIDIsTypedGone: a recorded pid that no longer exists cannot be
// verified as ours, so it is typed ErrGone (the swap layer warns and moves
// on) rather than silently nil.
func TestStopGonePIDIsTypedGone(t *testing.T) {
	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", "true")}
	err := eng.Stop(context.Background(), Running{PID: 99999999}, time.Second)
	if !errors.Is(err, ErrGone) {
		t.Errorf("want ErrGone for a stale recorded pid, got %v", err)
	}
}

// waitForExec polls until the process image carries want in its command line.
// A pid captured straight after Start may still show the parent's cmdline,
// and the ownership check must not be judged on that instant.
func waitForExec(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never ran %q (cmdline %q)", pid, want, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCommandStopUnreapedPIDIsNotAHang pins the other failure mode of a
// recorded pid: its parent never reaps it, so after the group is killed the
// pid lingers as a zombie. A zombie holds no memory and answers nothing, so
// Stop must report success quickly instead of spinning until "did not die
// after SIGKILL".
func TestCommandStopUnreapedPIDIsNotAHang(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 74 & sleep 74")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	waitForExec(t, pid, "sleep 74")
	// Deliberately no Wait during the test: the zombie is the point.
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})

	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", "sh -c 'sleep 74 & sleep 74'")}
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		done <- result{eng.Stop(context.Background(), Running{PID: pid}, 500*time.Millisecond)}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Stop of an unreaped-but-dead pid must succeed, got %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop hung on a pid whose parent never reaps it")
	}
	if eng.Alive(context.Background(), Running{PID: pid}) {
		t.Errorf("pid %d is not a running process; Alive must be false", pid)
	}
}

func waitForFilePID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b, err := os.ReadFile(path); err == nil {
			n := strings.TrimSpace(string(b))
			if n != "" {
				pid := 0
				for _, c := range n {
					if c < '0' || c > '9' {
						t.Fatalf("pid file %s contains %q", path, n)
					}
					pid = pid*10 + int(c-'0')
				}
				if pid > 0 {
					return pid
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("hook never wrote %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCommandLifecycle(t *testing.T) {
	dir := t.TempDir()
	start := "/bin/sh -c 'sleep 30 & sleep 30'"
	body := "NAME=e\nTYPE=command\nHEALTH=http://x\nSTART=" + start + "\n"
	e, err := config.Parse(strings.NewReader(body), "e.conf")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(e, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	running, err := eng.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if running.PID <= 0 {
		t.Fatalf("Running.PID = %d", running.PID)
	}
	if err := syscall.Kill(running.PID, syscall.Signal(0)); err != nil {
		t.Fatalf("process should be alive after Start: %v", err)
	}
	if err := eng.Stop(ctx, running, 2*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := syscall.Kill(running.PID, syscall.Signal(0)); err == nil {
		t.Error("process still alive after Stop")
	}
	// Stop is idempotent for a handle this engine already stopped.
	if err := eng.Stop(ctx, running, time.Second); err != nil {
		t.Errorf("Stop after exit must be idempotent, got %v", err)
	}
	if eng.Alive(ctx, running) {
		t.Error("stopped engine must not report alive")
	}
	_ = dir
}

// TestDockerStopGoneIsTyped: docker's "no such container" is ErrGone, so the
// swap warns instead of failing the operation.
func TestDockerStopGoneIsTyped(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := "#!/bin/sh\necho 'Error: No such container: c1' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e := dockerSpec(t, config.TypeDocker, "CONTAINER", "c1")
	eng, err := New(e, Options{DockerBin: bin})
	if err != nil {
		t.Fatal(err)
	}
	err = eng.Stop(context.Background(), Running{Container: "c1"}, time.Second)
	if !errors.Is(err, ErrGone) {
		t.Fatalf("want ErrGone, got %v", err)
	}
}
