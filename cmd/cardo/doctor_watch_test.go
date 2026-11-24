package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/JoyanLabs/cardo/internal/watch"
)

// closedAddr is a port nothing listens on: the shape of "this engine is not
// running", which is the ordinary thing a doctor run reports.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

// TestUsageAdvertisesDoctorAndWatch keeps the help text honest about the two
// new verbs and watch's own flag.
func TestUsageAdvertisesDoctorAndWatch(t *testing.T) {
	newCLI(t, nil)
	code, _, out := capture(t, func() int { return run(nil) })
	if code != 2 {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	for _, want := range []string{"doctor", "watch", "--interval", "validate", "resident"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage must advertise %q:\n%s", want, out)
		}
	}
}

// TestDoctorExitsOneWhenItFindsSomething is the CLI contract for a doctor
// run: the verdict is in the exit code (so a cron job or a CI step can use
// it), the reasons are on stdout, and the run still finishes.
func TestDoctorExitsOneWhenItFindsSomething(t *testing.T) {
	newCLI(t, map[string]string{"demo.conf": strings.Replace(demoConf("71"), "http://127.0.0.1:1/health", closedAddr(t), 1)})

	code, out, errOut := capture(t, func() int { return run([]string{"doctor"}) })
	if code != 1 {
		t.Fatalf("doctor exit = %d, want 1 (stdout=%q stderr=%q)", code, out, errOut)
	}
	for _, want := range []string{"[warn] demo: health not reachable", "1 engines, 1 findings"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout must contain %q, got:\n%s", want, out)
		}
	}
}

// TestDoctorExitsZeroWhenEverythingAnswers is the control: without a clean
// pass the exit code says nothing.
func TestDoctorExitsZeroWhenEverythingAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		if r.URL.Path == "/metrics" {
			fmt.Fprintln(w, `{"connections":0}`)
			return
		}
		fmt.Fprintln(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	conf := strings.Replace(strings.Replace(demoConf("72"), "http://127.0.0.1:1/health", srv.URL+"/health", 1),
		"\nDRAIN=0", "\nBUSY="+srv.URL+"/metrics#$.connections\nDRAIN=0", 1)
	newCLI(t, map[string]string{"demo.conf": conf})

	code, out, errOut := capture(t, func() int { return run([]string{"doctor"}) })
	if code != 0 {
		t.Fatalf("a healthy setup must exit 0, got %d (stdout=%q stderr=%q)", code, out, errOut)
	}
	for _, want := range []string{"[ok] demo:", "BUSY 0", "0 findings"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout must contain %q, got:\n%s", want, out)
		}
	}
}

// TestDoctorNamesAMisnamedEngineFileAndAStaleRecord is the operator-facing
// half of the two config-level rules, through the real verb.
func TestDoctorNamesAMisnamedEngineFileAndAStaleRecord(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": strings.Replace(demoConf("73"), "http://127.0.0.1:1/health", closedAddr(t), 1)})
	// An engine whose file lost its .conf suffix, and a record that names an
	// engine nobody configures.
	if err := os.WriteFile(filepath.Join(c.confDir, "tensorfold"),
		[]byte("NAME=tensorfold\nTYPE=docker\nCONTAINER=tensorfold\nHEALTH=http://127.0.0.1:8888/health\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.writeState(`{"version":1,"slot":0,"active":"ghost","active_pid":123}`)

	code, out, errOut := capture(t, func() int { return run([]string{"doctor"}) })
	if code != 1 {
		t.Fatalf("doctor exit = %d, want 1 (stdout=%q stderr=%q)", code, out, errOut)
	}
	for _, want := range []string{
		"[warn] tensorfold: not a .conf file, skipped",
		"stale record (use down --forget)",
		"ghost",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout must contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "tensorfold: health") {
		t.Errorf("a skipped file is not a registered engine: %s", out)
	}
}

// TestDoctorAndWatchHonourConfDir pins that the new verbs read the directory
// the operator named, not the default one.
func TestDoctorAndWatchHonourConfDir(t *testing.T) {
	newCLI(t, nil) // the default engines.d stays empty
	dir := t.TempDir()
	conf := strings.Replace(demoConf("74"), "http://127.0.0.1:1/health", closedAddr(t), 1)
	if err := os.WriteFile(filepath.Join(dir, "named.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := capture(t, func() int {
		return run([]string{"--conf-dir", dir, "doctor"})
	})
	if code != 1 {
		t.Fatalf("doctor exit = %d, want 1 (stdout=%q stderr=%q)", code, out, errOut)
	}
	if !strings.Contains(out, "demo") || !strings.Contains(out, "1 engines") {
		t.Errorf("--conf-dir must decide which engines doctor reads:\n%s", out)
	}

	// Control: without the flag, the default (empty) directory is used.
	code, out, errOut = capture(t, func() int { return run([]string{"doctor"}) })
	if code != 0 {
		t.Fatalf("an empty default dir is not a finding, exit = %d (stdout=%q stderr=%q)", code, out, errOut)
	}
	if !strings.Contains(out, "0 engines, 0 findings") {
		t.Errorf("the default directory must be the empty one: %s", out)
	}
}

// TestIntervalIsForWatchOnly mirrors the --forget rule: a flag that belongs
// to one verb must not silently apply to another.
func TestIntervalIsForWatchOnly(t *testing.T) {
	newCLI(t, map[string]string{"demo.conf": demoConf("75")})
	for _, args := range [][]string{
		{"--interval", "1", "list"},
		{"status", "--interval", "1"},
		{"up", "--interval", "1", "demo"},
		{"doctor", "--interval", "1"},
	} {
		code, out, errOut := capture(t, func() int { return run(args) })
		if code != 2 {
			t.Errorf("%v exit = %d, want 2 (stdout=%q stderr=%q)", args, code, out, errOut)
		}
		if !strings.Contains(errOut, "--interval") || !strings.Contains(errOut, "watch") {
			t.Errorf("%v must explain that --interval is for watch only: %q", args, errOut)
		}
	}

	// A value that is not a number of seconds is the flag parser's problem.
	code, _, errOut := capture(t, func() int { return run([]string{"watch", "--interval", "soon"}) })
	if code != 2 {
		t.Fatalf("a garbage --interval exit = %d, want 2 (stderr=%q)", code, errOut)
	}
	if !strings.Contains(errOut, "interval") {
		t.Errorf("the failure must name the flag: %q", errOut)
	}
}

// TestIntervalRejectsValuesThatAreNotAnInterval is the usage-error rule: the
// flag parser accepts "nan" and "inf" as numbers, and a 1-microsecond tick is
// not a cadence but a probe flood against the resident engine. Both are
// operator mistakes, so they exit 2 like any other bad flag — before the verb
// runs, which is why every case here points at a missing engines directory:
// a rejected value must be rejected even when the run itself could not have
// started.
func TestIntervalRejectsValuesThatAreNotAnInterval(t *testing.T) {
	newCLI(t, map[string]string{"demo.conf": demoConf("78")})
	missing := filepath.Join(t.TempDir(), "nope")

	for _, bad := range []string{"nan", "-nan", "inf", "-inf", "0.02", "0.1", "0.249", "0.000001", "-1", "-0.5", "86401", "1e99"} {
		code, out, errOut := capture(t, func() int {
			return run([]string{"--conf-dir", missing, "watch", "--interval", bad})
		})
		if code != 2 {
			t.Errorf("--interval %s exit = %d, want 2 (stdout=%q stderr=%q)", bad, code, out, errOut)
		}
		if !strings.Contains(errOut, "--interval") {
			t.Errorf("--interval %s must be named in the usage error: %q", bad, errOut)
		}
	}

	// The values an operator may really use: the floor, the default, and
	// 0 meaning "whatever the watch package defaults to". A missing engines
	// directory stops the run before the loop, which is how the test tells
	// "the flag was accepted" from "the flag was rejected" without watching
	// forever.
	for _, good := range []string{"0", "0.25", "1", "2", "86400"} {
		code, out, errOut := capture(t, func() int {
			return run([]string{"--conf-dir", missing, "watch", "--interval", good})
		})
		if code != 1 {
			t.Errorf("--interval %s is a legal cadence, exit = %d (stdout=%q stderr=%q)", good, code, out, errOut)
		}
		if strings.Contains(errOut, "--interval") {
			t.Errorf("--interval %s must not be refused: %q", good, errOut)
		}
	}
}

// TestListOfASpecialFileComesBackAtOnce is the hang fix at the verb an
// operator types first: `cardo list` used to open every non-.conf entry for a
// content peek, so a named pipe in the engines directory wedged it with no
// timeout and no message. It has to come back at once, still list what it can
// read, and say which file it refused to open — a file that was skipped
// without being read is otherwise invisible until doctor happens to run.
func TestListOfASpecialFileComesBackAtOnce(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("77")})
	if err := syscall.Mkfifo(filepath.Join(c.confDir, "engine.pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unavailable here: %v", err)
	}

	// A file rather than a pipe for the capture: a test that blocks reading
	// its own capture would be indistinguishable from the hang it is testing.
	path := filepath.Join(t.TempDir(), "list.out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = f, f
	ch := make(chan int, 1)
	go func() { ch <- run([]string{"--conf-dir", c.confDir, "list"}) }()
	var code int
	select {
	case code = <-ch:
	case <-time.After(30 * time.Second):
		os.Stdout, os.Stderr = oldOut, oldErr
		f.Close()
		t.Fatal("`cardo list` did not come back: a named pipe in the engines directory must not be able to wedge it")
	}
	os.Stdout, os.Stderr = oldOut, oldErr
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	text := string(mustRead(t, path))
	if code != 0 {
		t.Errorf("list exit = %d, want 0: a file cardo refuses to open is not a failure to list the others\n%s", code, text)
	}
	if !strings.Contains(text, "engine.pipe: special file, not inspected") {
		t.Errorf("list must name the file it refused to open: %q", text)
	}
	if !strings.Contains(text, "named pipe") {
		t.Errorf("the line must say what kind of file it found: %q", text)
	}
	if !strings.Contains(text, "demo") {
		t.Errorf("the engines that can be read must still be listed: %q", text)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestWatchReportsAnUnprovableResidentAsUnknown is the operator-facing half
// of the ownership rule: the record points at somebody else's live process,
// so the refreshing screen must not read "alive yes".
func TestWatchReportsAnUnprovableResidentAsUnknown(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("79")})
	// demoConf's START is `sleep 79 & sleep 79`; a bare `sleep 402` is alive
	// but is not the engine the record claims.
	pid := plainSleep(t, "402")
	c.writeState(fmt.Sprintf(`{"version":1,"slot":0,"active":"demo","active_pid":%d}`, pid))

	out := runWatchAndInterrupt(t, "--interval", "0.3")
	if !strings.Contains(out, "alive unknown (pid does not match START)") {
		t.Errorf("watch must say the pid is not provably the engine's:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprint(pid)) {
		t.Errorf("the unknown verdict must name the pid it could not claim:\n%s", out)
	}
	if strings.Contains(out, "alive yes") {
		t.Errorf("a process cardo cannot own must never be shown as a live resident:\n%s", out)
	}
}

// plainSleep starts a bare `sleep <marker>` (no shell in front of it, so no
// START can prove it) in its own process group, and cleans it up.
func plainSleep(t *testing.T, marker string) int {
	t.Helper()
	cmd := exec.Command("sleep", marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return pid
}

// TestWatchNeedsConfiguredEngines: watching a directory that does not exist
// is not an empty view that ticks forever.
func TestWatchNeedsConfiguredEngines(t *testing.T) {
	newCLI(t, nil)
	missing := filepath.Join(t.TempDir(), "nope")
	code, out, errOut := capture(t, func() int { return run([]string{"--conf-dir", missing, "watch"}) })
	if code != 1 {
		t.Fatalf("watch over a missing dir exit = %d, want 1 (stdout=%q stderr=%q)", code, out, errOut)
	}
	if !strings.Contains(errOut, "nope") {
		t.Errorf("the failure must name the directory: %q", errOut)
	}
}

// TestWatchStopsOnSIGINT is the loop's other exit: the operator's Ctrl-C.
// The signal goes to this process, where cardo's own handler catches it, so
// the test proves the wiring rather than a context the test made itself.
func TestWatchStopsOnSIGINT(t *testing.T) {
	newCLI(t, map[string]string{"demo.conf": strings.Replace(demoConf("76"), "http://127.0.0.1:1/health", closedAddr(t), 1)})
	orig := watch.Interval
	watch.Interval = 20 * time.Millisecond
	defer func() { watch.Interval = orig }()

	// The cadence comes from the package default, not --interval: the CLI
	// refuses a tick faster than 250ms, and this test is about the interrupt,
	// not about the floor (TestIntervalRejectsValuesThatAreNotAnInterval
	// covers the flag).
	out := runWatchAndInterrupt(t)
	if !strings.Contains(out, "watch ") {
		t.Fatalf("watch must print snapshots:\n%s", out)
	}
	if !strings.Contains(out, "demo") || !strings.Contains(out, "slot 0") {
		t.Errorf("the snapshot must show the registered engine:\n%s", out)
	}
	if n := strings.Count(out, "watch "); n < 2 {
		t.Errorf("a 20ms interval must print more than one snapshot before the interrupt, got %d:\n%s", n, out)
	}
}

// runWatchAndInterrupt runs the watch verb in the CLI's own goroutine, sends
// this process the SIGINT an operator sends, and returns what watch printed
// plus its exit code.
func runWatchAndInterrupt(t *testing.T, args ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watch.out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = f
	code := make(chan int, 1)
	go func() { code <- run(append([]string{"watch"}, args...)) }()
	// Wait for the loop to be running before interrupting it: sending SIGINT
	// to this process with no handler installed would take the test binary
	// down, and that is a worse failure than the one being tested.
	if !waitForCount(path, "watch ", 2, 5*time.Second) {
		os.Stdout = old
		f.Close()
		t.Fatalf("watch did not keep printing snapshots; refusing to interrupt a process that is not looping")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		os.Stdout = old
		f.Close()
		t.Fatalf("signalling ourselves: %v", err)
	}
	got := <-code
	os.Stdout = old
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("watch exit = %d after SIGINT, want 0", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// waitForCount reports whether path holds at least n occurrences of want by
// the deadline. Interrupting watch is only meaningful once it has proved it
// is looping, and sending SIGINT with no handler installed would take the
// test binary down instead of failing this test.
func waitForCount(path, want string, n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(path)
		if strings.Count(string(b), want) >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestExistingVerbsAreUntouchedByTheNewFlags is the PRESERVE half of the
// shared argument parser: --interval joins --conf-dir and --forget in
// splitArgs, and list, status, up and down all go through that parser. Their
// old shapes have to keep working exactly as they did.
func TestExistingVerbsAreUntouchedByTheNewFlags(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("77")})

	code, out, errOut := capture(t, func() int { return run([]string{"list"}) })
	if code != 0 || !strings.Contains(out, "demo") || !strings.Contains(out, "command") {
		t.Errorf("list exit=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = capture(t, func() int { return run([]string{"--conf-dir", c.confDir, "list"}) })
	if code != 0 || !strings.Contains(out, "*") && !strings.Contains(out, "demo") {
		t.Errorf("--conf-dir list exit=%d out=%q err=%q", code, out, errOut)
	}
	code, out, errOut = capture(t, func() int { return run([]string{"status"}) })
	if code != 0 || !strings.Contains(out, "nothing resident") {
		t.Errorf("status exit=%d out=%q err=%q", code, out, errOut)
	}
	// up still takes exactly one name, and an unknown verb is still a usage
	// error rather than a crash.
	if code, _, errOut := capture(t, func() int { return run([]string{"up", "demo", "extra"}) }); code != 2 {
		t.Errorf("up with two names exit=%d err=%q", code, errOut)
	}
	if code, _, errOut := capture(t, func() int { return run([]string{"wat"}) }); code != 2 {
		t.Errorf("unknown verb exit=%d err=%q", code, errOut)
	}
	// A flag that is not cardo's is still reported as unknown.
	if code, _, errOut := capture(t, func() int { return run([]string{"status", "--nope"}) }); code != 2 {
		t.Errorf("unknown flag exit=%d err=%q", code, errOut)
	} else if !strings.Contains(errOut, "unknown flag") {
		t.Errorf("want an unknown-flag error, got %q", errOut)
	}
}
