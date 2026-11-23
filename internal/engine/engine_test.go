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

// startDecoyArgv starts path with a caller-supplied argv (argv[0] included),
// so a test can pin the exact command line a process reports in
// /proc/<pid>/cmdline. It runs in its own process group so a mistaken
// kill(-pid) cannot reach the test process.
func startDecoyArgv(t *testing.T, path string, argv ...string) int {
	t.Helper()
	cmd := exec.Command(path)
	cmd.Args = argv
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
		name    string
		out     string
		fail    bool
		want    bool
		wantErr bool
	}{
		{"running", "true", false, true, false},
		{"exited", "false", false, false, false},
		{"padded", "true\n", false, true, false},
		{"inspect fails", "true", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := shimDockerReply(t, tc.out, tc.fail)
			e := dockerSpec(t, config.TypeDocker, "CONTAINER", "c1")
			eng, err := New(e, Options{DockerBin: bin})
			if err != nil {
				t.Fatal(err)
			}
			got, aerr := eng.Alive(context.Background(), Running{Container: "c1"})
			if tc.wantErr {
				if aerr == nil {
					t.Errorf("Alive = %v, want a driver error (a wedged daemon is never 'not running')", got)
				}
				return
			}
			if aerr != nil || got != tc.want {
				t.Errorf("Alive = %v, want %v (inspect said %q)", got, tc.want, tc.out)
			}
		})
	}
	// The liveness question must be asked of docker with an explicit state
	// format, not guessed from `ps`.
	bin, log := shimDockerReply(t, "true", false)
	e := dockerSpec(t, config.TypeDocker, "CONTAINER", "c1")
	eng, _ := New(e, Options{DockerBin: bin})
	if b, aerr := eng.Alive(context.Background(), Running{Container: "c1"}); aerr != nil || !b {
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
			if got, aerr := eng.Alive(context.Background(), Running{Container: "proj"}); aerr != nil || got != tc.want {
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
	if b, _ := eng.Alive(ctx, Running{}); b {
		t.Error("pid 0 must not be alive")
	}
	if b, _ := eng.Alive(ctx, Running{PID: 99999999}); b {
		t.Error("a pid that never existed must not be alive")
	}

	// A foreign live pid (cardo did not start it) is still alive.
	foreign := startDecoy(t, "sleep", "30")
	if b, aerr := eng.Alive(ctx, Running{PID: foreign}); aerr != nil || !b {
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
	if b, _ := own.Alive(ctx, running); !b {
		t.Errorf("own child pid %d reported dead while running", running.PID)
	}
	if err := own.Stop(ctx, running, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if b, _ := own.Alive(ctx, running); b {
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

// TestCommandStopForeignPIDIsErrNotOursAndNeverSignalled pins the ADR-0004
// ownership guard, split in two (Round-3 review, F1): a recorded pid that is
// a live process but whose command line does not prove it is this engine's is
// ErrNotOurs, not ErrGone. The slot is really held, so the caller must abort
// rather than warn and start something over it. cardo never signals it.
func TestCommandStopForeignPIDIsErrNotOursAndNeverSignalled(t *testing.T) {
	foreign := startDecoy(t, "sleep", "73")
	e := dockerSpec(t, config.TypeCommand, "START", "/nonexistent/cardo-engine-bin --serve")
	eng := &commandEngine{spec: e}

	err := eng.Stop(context.Background(), Running{PID: foreign}, 500*time.Millisecond)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("want ErrNotOurs for a live pid cardo does not own, got %v", err)
	}
	if errors.Is(err, ErrGone) {
		t.Errorf("a live process is not gone; the two outcomes must stay distinct: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(foreign)) {
		t.Errorf("error must name the offending pid: %v", err)
	}
	if !pidAlive(foreign) {
		t.Errorf("cardo signalled a process it does not own (pid %d is dead)", foreign)
	}
}

// TestCommandStopForeignShellCDeliverableIsErrNotOurs reproduces the defect
// the old guard allowed: a foreign `fish -c ...` (whose argv[0] also ends in
// "sh") was killed by `cardo down` because the guard accepted any shell-ish
// argv0, or any cmdline containing START's first token. cardo starts engines
// as `sh -c <START>`, so anything whose argv[2] is not exactly START is not
// ours and must be left running, reported as ErrNotOurs.
func TestCommandStopForeignShellCDeliverableIsErrNotOurs(t *testing.T) {
	const foreignPayload = "sleep 79 & sleep 79"
	foreign := startDecoyArgv(t, "sh", "/usr/bin/fish", "-c", foreignPayload)
	// The ownership check must not be judged on the instant before the child
	// finishes execing, when /proc still shows the parent's command line.
	waitForExec(t, foreign, foreignPayload)

	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", "sleep 42 & sleep 42")}
	err := eng.Stop(context.Background(), Running{PID: foreign}, 500*time.Millisecond)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("a foreign shell one-liner is not cardo's engine, got %v", err)
	}
	if errors.Is(err, ErrGone) {
		t.Errorf("pid %d is alive; ErrGone would let the swap start over it: %v", foreign, err)
	}
	if !pidAlive(foreign) {
		t.Errorf("cardo signalled a foreign process (pid %d is dead)", foreign)
	}
}

// TestCommandStopShellWithoutDashCIsErrNotOurs: a shell that was not started
// with -c cannot be carrying cardo's START, so it is not ours either.
func TestCommandStopShellWithoutDashCIsErrNotOurs(t *testing.T) {
	script := filepath.Join(t.TempDir(), "someone-elses-loop.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 78\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := startDecoyArgv(t, "sh", "/bin/sh", script)
	waitForExec(t, foreign, script)

	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", "sleep 42 & sleep 42")}
	err := eng.Stop(context.Background(), Running{PID: foreign}, 500*time.Millisecond)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("a shell without -c is not cardo's engine, got %v", err)
	}
	if !pidAlive(foreign) {
		t.Errorf("cardo signalled a foreign process (pid %d is dead)", foreign)
	}
}

// TestStopRecordedPIDNeedsTheExactStart pins the reuse case: the same live
// pid is cardo's only under the START that launched it. A different START
// (a recycled pid, or a START edited in place) must be ErrGone with the
// process left running, while the matching START still stops its group.
func TestStopRecordedPIDNeedsTheExactStart(t *testing.T) {
	const startA = "sleep 81 & sleep 81"
	const startB = "sleep 82 & sleep 82"
	ctx := context.Background()

	first, err := New(dockerSpec(t, config.TypeCommand, "START", startA), Options{})
	if err != nil {
		t.Fatal(err)
	}
	running, err := first.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-running.PID, syscall.SIGKILL) })
	waitForExec(t, running.PID, startA)

	other, err := New(dockerSpec(t, config.TypeCommand, "START", startB), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Stop(ctx, running, 300*time.Millisecond); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("an engine whose START differs must refuse the live pid, got %v", err)
	}
	if errors.Is(err, ErrGone) {
		t.Errorf("the process is alive: ErrGone would let `up` start over it: %v", err)
	}
	if !pidAlive(running.PID) {
		t.Fatalf("the refused Stop killed a process the engine does not own (pid %d)", running.PID)
	}

	same, err := New(dockerSpec(t, config.TypeCommand, "START", startA), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := same.Stop(ctx, running, 3*time.Second); err != nil {
		t.Fatalf("the matching engine must stop the recorded pid: %v", err)
	}
	if !waitForPIDDead(running.PID, 3*time.Second) {
		t.Errorf("group leader %d survived Stop under its own START", running.PID)
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
	if errors.Is(err, ErrNotOurs) {
		t.Errorf("a pid that is no process at all must be proceedable ErrGone, not ErrNotOurs: %v", err)
	}
}

// TestStopExecPrefixedStartStopsTheRecordedPID is the smoke case the reviewers
// reproduced: START="exec sleep N" is the documented way to keep a command
// engine in the foreground without a shell parent. dash replaces the shell
// image with the payload, so /proc no longer shows `sh -c ...` at all: the
// ownership proof has to recognise its own exec'd payload and stop it, instead
// of reporting the engine as somebody else's.
func TestStopExecPrefixedStartStopsTheRecordedPID(t *testing.T) {
	const start = "exec sleep 297"
	pid := startDecoy(t, "sh", "-c", start)
	waitForExecedImage(t, pid, "sleep 297")

	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", start)}
	if err := eng.Stop(context.Background(), Running{PID: pid}, 3*time.Second); err != nil {
		t.Fatalf("Stop of an exec'd START must succeed, got %v", err)
	}
	if !waitForPIDDead(pid, 3*time.Second) {
		t.Errorf("pid %d survived Stop of its own exec'd START", pid)
	}
}

// TestStopExecPrefixedStartNeedsTheSamePayload is the other half of the smoke:
// the record says `exec sleep 296`, the config has since been edited to
// `exec sleep 299`. The live process is not ours: ErrNotOurs, left running.
func TestStopExecPrefixedStartNeedsTheSamePayload(t *testing.T) {
	pid := startDecoy(t, "sh", "-c", "exec sleep 296")
	waitForExecedImage(t, pid, "sleep 296")

	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", "exec sleep 299")}
	err := eng.Stop(context.Background(), Running{PID: pid}, 500*time.Millisecond)
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("an edited START must not prove ownership of a live pid, got %v", err)
	}
	if !pidAlive(pid) {
		t.Errorf("cardo signalled a process it could not prove is its own (pid %d)", pid)
	}
}

// TestStopRecordedPIDAcceptsAnyShPath pins argv[0] of the proof: cardo runs
// `sh -c <START>`, and the shell it finds may be /bin/sh, /usr/bin/sh or a
// busybox link. What matters is the last element of the path, which is also
// what excludes fish and bash.
func TestStopRecordedPIDAcceptsAnyShPath(t *testing.T) {
	const start = "sleep 295 & sleep 295"
	pid := startDecoyArgv(t, "sh", "/bin/sh", "-c", start)
	waitForExec(t, pid, start)

	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", start)}
	if err := eng.Stop(context.Background(), Running{PID: pid}, 3*time.Second); err != nil {
		t.Fatalf("Stop of a pid launched by /bin/sh: %v", err)
	}
	if !waitForPIDDead(pid, 3*time.Second) {
		t.Errorf("pid %d survived Stop under its own START", pid)
	}
}

// TestProvesOwnershipChecksTheLiveProcess is the exported half of the proof.
// The table above pins the rule; `watch` and `doctor` ask it about a pid they
// did not start, so the wrapper has to read the command line the kernel
// reports and answer the same way Stop would.
func TestProvesOwnershipChecksTheLiveProcess(t *testing.T) {
	const start = "sleep 411 & sleep 411"
	pid := startDecoy(t, "sh", "-c", start)
	waitForExec(t, pid, start)
	if !ProvesOwnership(pid, start) {
		t.Errorf("the process this START started must prove itself (pid %d)", pid)
	}
	if ProvesOwnership(pid, "sleep 999 & sleep 999") {
		t.Errorf("another engine's START must not claim this pid")
	}
	if ProvesOwnership(pid, "") {
		t.Errorf("an engine with no START can prove nothing")
	}

	// A bare `sleep 412` is somebody else's process: cardo starts
	// `sh -c <START>`, and a plain START never proves an exec'd image.
	foreign := startDecoy(t, "sleep", "412")
	waitForExec(t, foreign, "412")
	if !pidAlive(foreign) {
		t.Fatalf("decoy %d is not alive", foreign)
	}
	if ProvesOwnership(foreign, "sleep 412") {
		t.Errorf("a bare sleep is not `sh -c sleep 412`, however the words match")
	}

	// A pid that is no process at all proves nothing either — which is the
	// caller's "unknown", never an implied "alive".
	if ProvesOwnership(99999999, start) {
		t.Errorf("a pid that is not a process cannot be anybody's engine")
	}
}

// TestProvesOwnership exercises the ownership proof itself, argv shape by
// argv shape. The real-process tests above cannot reach every shape (a shell
// that execs its payload is gone before the test can look), so the table is
// the spec for what may be signalled.
func TestProvesOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		argv  []string
		start string
		want  bool
	}{
		{"exact start", []string{"sh", "-c", "sleep 1 & sleep 1"}, "sleep 1 & sleep 1", true},
		{"sh by path", []string{"/usr/bin/sh", "-c", "sleep 1"}, "sleep 1", true},
		{"exec prefixed payload", []string{"sh", "-c", "exec sleep 1"}, "sleep 1", true},
		{"start already carries exec", []string{"sh", "-c", "exec sleep 1"}, "exec sleep 1", true},
		{"exec'd payload of an exec start", []string{"sleep", "297"}, "exec sleep 297", true},
		{"exec'd payload by absolute path", []string{"/usr/bin/sleep", "297"}, "exec /usr/bin/sleep 297", true},
		{"fish is not sh", []string{"/usr/bin/fish", "-c", "sleep 1"}, "sleep 1", false},
		{"bash is not sh", []string{"/bin/bash", "-c", "sleep 1"}, "sleep 1", false},
		{"argv0 ending in sh is not enough", []string{"/usr/local/bin/fish", "-c", "sleep 1"}, "sleep 1", false},
		{"no -c", []string{"sh", "loop.sh"}, "loop.sh", false},
		{"different start", []string{"sh", "-c", "sleep 2"}, "sleep 1", false},
		{"plain start cannot prove an exec'd image", []string{"sleep", "297"}, "sleep 297", false},
		{"exec'd payload of another engine", []string{"sleep", "999"}, "exec sleep 297", false},
		{"exec'd payload with extra args", []string{"sleep", "297", "x"}, "exec sleep 297", false},
		{"too few argv", []string{"sh", "-c"}, "sleep 1", false},
		{"empty start", []string{"sh", "-c", ""}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := provesOwnership(tc.argv, tc.start); got != tc.want {
				t.Errorf("provesOwnership(%q, %q) = %v, want %v", tc.argv, tc.start, got, tc.want)
			}
		})
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

// waitForExecedImage polls until the pid's command line is exactly want
// (\0-separated argv), i.e. the shell has really replaced itself with the
// payload: a substring check would also match `sh -c exec <payload>`.
func waitForExecedImage(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		if !hasArgv(argv, "-c") && strings.Join(argv, " ") == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became %q (cmdline %q)", pid, want, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hasArgv(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
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

	// The recorded pid was launched as `sh -c <START>`, so its START is the
	// payload itself: the ownership check compares argv[2] against it exactly.
	eng := &commandEngine{spec: dockerSpec(t, config.TypeCommand, "START", "sleep 74 & sleep 74")}
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
	if b, _ := eng.Alive(context.Background(), Running{PID: pid}); b {
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
	if b, _ := eng.Alive(ctx, running); b {
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
