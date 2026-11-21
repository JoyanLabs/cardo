package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
)

// cli wires cardo's real environment: one config dir and one state dir per
// test, so run() never touches the developer's own directories.
type cli struct {
	t       *testing.T
	confDir string
}

func newCLI(t *testing.T, confs map[string]string) *cli {
	t.Helper()
	stateHome := t.TempDir()
	confHome := t.TempDir()
	t.Setenv("XDG_STATE_DIR", stateHome)
	t.Setenv("XDG_CONFIG_HOME", confHome)
	dir := filepath.Join(confHome, "cardo", "engines.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range confs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &cli{t: t, confDir: dir}
}

func (c *cli) stateDir() string { return filepath.Join(os.Getenv("XDG_STATE_DIR"), "cardo") }

func (c *cli) writeState(body string) {
	c.t.Helper()
	if err := os.MkdirAll(c.stateDir(), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.stateDir(), "state.json"), []byte(body), 0o644); err != nil {
		c.t.Fatal(err)
	}
}

func (c *cli) readState() string {
	c.t.Helper()
	b, err := os.ReadFile(filepath.Join(c.stateDir(), "state.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		c.t.Fatal(err)
	}
	return string(b)
}

// resident starts a real long-lived engine-shaped process (sh as process
// group leader, one background child) and records it as resident.
func (c *cli) resident(marker string) int {
	c.t.Helper()
	cmd := exec.Command("sh", "-c", "sleep "+marker+" & sleep "+marker)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	pid := cmd.Process.Pid
	// Wait for the exec to land: a pid captured straight after Start can
	// still show the parent's command line, and cardo's ownership check reads it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(b), "sleep "+marker) {
			break
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("decoy %d never exec'd (cmdline %q)", pid, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t := c.t
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	c.writeState(fmt.Sprintf(`{"version":1,"slot":0,"active":"demo","active_pid":%d}`, pid))
	return pid
}

func capture(t *testing.T, f func() int) (int, string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := f()
	os.Stdout, os.Stderr = oldOut, oldErr
	outW.Close()
	errW.Close()
	var o, e bytes.Buffer
	_, _ = io.Copy(&o, outR)
	_, _ = io.Copy(&e, errR)
	outR.Close()
	errR.Close()
	return code, o.String(), e.String()
}

// TestDownForgetClearsRecordWithoutStopping is the CLI half of the --forget
// escape hatch: the record is cleared, the process is left running, and the
// run says so.
func TestDownForgetClearsRecordWithoutStopping(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("91")})
	pid := c.resident("91")

	code, out, errOut := capture(t, func() int { return run([]string{"down", "--forget"}) })
	if code != 0 {
		t.Fatalf("down --forget exit = %d, stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, "forgetting demo without stopping") {
		t.Errorf("the run must say it is not stopping anything: %q", errOut)
	}
	if st := c.readState(); strings.Contains(st, `"active": "demo"`) || strings.Contains(st, fmt.Sprint(pid)) {
		t.Errorf("the record must be cleared, got %s", st)
	}
	if !pidAlive(pid) {
		t.Errorf("--forget killed pid %d; it must leave the engine alone", pid)
	}
}

// TestDownWithoutForgetStopsTheResident is the control: the same record,
// without --forget, stops the recorded process group.
func TestDownWithoutForgetStopsTheResident(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("92")})
	pid := c.resident("92")

	code, out, errOut := capture(t, func() int { return run([]string{"down"}) })
	if code != 0 {
		t.Fatalf("down exit = %d, stdout=%q stderr=%q", code, out, errOut)
	}
	if strings.Contains(errOut, "forgetting") {
		t.Errorf("a plain down must not claim to forget: %q", errOut)
	}
	if !waitForPIDDead(pid, 5*time.Second) {
		t.Errorf("pid %d survived `cardo down`", pid)
	}
}

func TestForgetIsRejectedOnVerbsThatDoNotSupportIt(t *testing.T) {
	newCLI(t, map[string]string{"demo.conf": demoConf("93")})
	for _, args := range [][]string{
		{"up", "--forget", "demo"},
		{"--forget", "list"},
		{"--forget", "status"},
	} {
		code, out, errOut := capture(t, func() int { return run(args) })
		if code != 2 {
			t.Errorf("%v exit = %d, want 2 (stdout=%q stderr=%q)", args, code, out, errOut)
		}
		if !strings.Contains(errOut, "--forget") || !strings.Contains(errOut, "down") {
			t.Errorf("%v must explain that --forget is for down only: %q", args, errOut)
		}
	}
}

func TestUsageAdvertisesForget(t *testing.T) {
	newCLI(t, nil)
	code, _, out := capture(t, func() int { return run(nil) })
	if code != 2 {
		t.Fatalf("no args exit = %d, want 2", code)
	}
	for _, want := range []string{"down --forget", "--forget", "up NAME", "list", "status"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage must advertise %q:\n%s", want, out)
		}
	}
}

func TestUnknownFlagIsReportedNotIgnored(t *testing.T) {
	newCLI(t, map[string]string{"demo.conf": demoConf("94")})
	code, _, errOut := capture(t, func() int { return run([]string{"list", "--froget"}) })
	if code != 2 {
		t.Fatalf("a misspelled flag exit = %d, want 2 (stderr=%q)", code, errOut)
	}
	if !strings.Contains(errOut, "unknown flag") {
		t.Errorf("want an unknown-flag error, got %q", errOut)
	}
}

func TestStatusAndListStillWork(t *testing.T) {
	newCLI(t, map[string]string{"demo.conf": demoConf("95")})
	code, out, errOut := capture(t, func() int { return run([]string{"list"}) })
	if code != 0 {
		t.Fatalf("list exit = %d (stdout=%q stderr=%q)", code, out, errOut)
	}
	if !strings.Contains(out, "demo") || !strings.Contains(out, "command") {
		t.Errorf("list output = %q", out)
	}
	code, out, errOut = capture(t, func() int { return run([]string{"status"}) })
	if code != 0 {
		t.Fatalf("status exit = %d (stdout=%q stderr=%q)", code, out, errOut)
	}
	if !strings.Contains(out, "nothing resident") {
		t.Errorf("status output = %q", out)
	}
}

func demoConf(marker string) string {
	// START is the exact string cardo hands to `sh -c`, which is what
	// resident() reproduces so cardo's ownership check sees its own engine.
	return "NAME=demo\nTYPE=command\nSTART=sleep " + marker + " & sleep " + marker + "\n" +
		"HEALTH=http://127.0.0.1:1/health\nDRAIN=0\nWAIT=5\nSLOT=0\n"
}

// foreignDecoy starts a process that only looks like a cardo engine: argv[0]
// names some other program, and the payload is not any configured START. It
// is a group leader so a signal would really take it down, which is exactly
// the damage the ownership check has to prevent.
func foreignDecoy(t *testing.T, argv0, payload string) int {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Args = []string{argv0, "-c", payload}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	for {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if strings.Contains(string(b), payload) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("decoy %d never exec'd (cmdline %q)", pid, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return pid
}

// TestDownRefusesToStopAForeignProcess is the CLI half of the ownership
// check, and since round 3 of the review the two outcomes are distinct: a
// recorded pid that is a live process cardo cannot prove is its own is not
// "already gone". The swap aborts loudly, the process is left running, and
// the record stays for the operator to look at.
func TestDownRefusesToStopAForeignProcess(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("97")})
	const payload = "sleep 9077 & sleep 9077"
	pid := foreignDecoy(t, "/usr/bin/fish", payload)
	c.writeState(fmt.Sprintf(`{"version":1,"slot":0,"active":"demo","active_pid":%d}`, pid))

	code, out, errOut := capture(t, func() int { return run([]string{"down"}) })
	if code == 0 {
		t.Fatalf("down over a live foreign pid must fail, stdout=%q stderr=%q", out, errOut)
	}
	if !pidAlive(pid) {
		t.Errorf("`cardo down` killed a process it does not own (pid %d)", pid)
	}
	if strings.Contains(errOut, "already gone") {
		t.Errorf("a live process must not be reported as gone, which is the proceedable outcome: %q", errOut)
	}
	if !strings.Contains(errOut, "will not signal it") || !strings.Contains(errOut, fmt.Sprint(pid)) {
		t.Errorf("cardo must say the recorded pid is alive but not its engine: stdout=%q stderr=%q", out, errOut)
	}
	if got := c.activeName(); got != "demo" {
		t.Errorf("a down that could not stop the resident must leave the record for the operator, got %s", c.readState())
	}

	// The escape hatch is the only way out, and it still does not kill.
	code, out, errOut = capture(t, func() int { return run([]string{"down", "--forget"}) })
	if code != 0 {
		t.Fatalf("down --forget must clear the record: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := c.activeName(); got != "" {
		t.Errorf("the record must be cleared by --forget, got %s", c.readState())
	}
	if !pidAlive(pid) {
		t.Errorf("--forget killed pid %d; it must leave a foreign process alone", pid)
	}
}

// TestDownStopsADeadRecordedPIDWithoutComplaint is the other half of the
// split the review asked for: a record whose pid is no longer a process at
// all is the proceedable case. `down` warns, clears the record and succeeds.
func TestDownStopsADeadRecordedPIDWithoutComplaint(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("96")})
	// A pid that is not a process: `cardo up` wrote the record, something else
	// reaped the engine, and the record is simply stale.
	c.writeState(`{"version":1,"slot":0,"active":"demo","active_pid":99999999}`)

	code, out, errOut := capture(t, func() int { return run([]string{"down"}) })
	if code != 0 {
		t.Fatalf("a stale record of a dead pid must not block down: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := c.activeName(); got != "" {
		t.Errorf("the stale record must be cleared, got %s", c.readState())
	}
	if strings.Contains(errOut, "will not signal it") {
		t.Errorf("nothing here belongs to somebody else: %q", errOut)
	}
	_ = out
}

// gatedHealth answers on a port that refuses connections until running() says
// something is home. cardo's divergence guard now reads any HTTP answer,
// including an error status, as an occupant, so an end-to-end `up` test has
// to look like a cold port that the engine itself warms up.
func gatedHealth(t *testing.T, running func() bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Listener = &gateListener{Listener: ln, open: running}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

type gateListener struct {
	net.Listener
	open func() bool
}

func (g *gateListener) Accept() (net.Conn, error) {
	for {
		c, err := g.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if g.open == nil || g.open() {
			return c, nil
		}
		c.Close() // nobody home: the client sees a transport failure
	}
}

// processRunningWith reports whether some live process carries want in its
// command line (argv is \0-separated in /proc, so it is joined to compare).
func processRunningWith(want string) bool {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		argv := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		if strings.Contains(strings.Join(argv, " "), want) {
			return true
		}
	}
	return false
}

// activeName reads the engine cardo records as resident ("" when nothing is).
func (c *cli) activeName() string {
	c.t.Helper()
	var rec struct {
		Active string `json:"active"`
	}
	_ = json.Unmarshal([]byte(c.readState()), &rec)
	return rec.Active
}

// residentPID reads the pid cardo recorded as resident.
func (c *cli) residentPID() int {
	c.t.Helper()
	var rec struct {
		ActivePID int `json:"active_pid"`
	}
	if err := json.Unmarshal([]byte(c.readState()), &rec); err != nil {
		c.t.Fatalf("state is not readable: %v (%s)", err, c.readState())
	}
	if rec.ActivePID <= 0 {
		c.t.Fatalf("state records no pid: %s", c.readState())
	}
	return rec.ActivePID
}

// commandConf is a command engine whose HEALTH only answers once a process
// carrying payload is running, which is the honest shape of "cardo starts it".
func commandConf(name, start, payload string, wait int) string {
	return "NAME=" + name + "\nTYPE=command\nSTART=" + start + "\n" +
		"HEALTH=__HEALTH__\nDRAIN=0\nWAIT=" + fmt.Sprint(wait) + "\nSLOT=0\n"
}

// TestUpDownStopsAnExecPrefixedEngine is smoke (a) of the ownership review,
// end to end: START="exec sleep N" is the documented way to keep a command
// engine in the foreground, and dash then replaces the shell image, so
// /proc shows `sleep N` rather than `sh -c exec sleep N`. `cardo up` must
// bring it up and `cardo down` must still recognise its own engine and stop
// it, rather than calling it gone and leaving it running.
func TestUpDownStopsAnExecPrefixedEngine(t *testing.T) {
	const start = "exec sleep 9341"
	c := newCLI(t, map[string]string{"demo.conf": strings.Replace(
		commandConf("demo", start, "sleep 9341", 10), "__HEALTH__", gatedHealth(t, func() bool {
			return processRunningWith("sleep 9341")
		}), 1)})

	code, out, errOut := capture(t, func() int { return run([]string{"up", "demo"}) })
	if code != 0 {
		t.Fatalf("up must bring up an exec-prefixed engine: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	pid := c.residentPID()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if !pidAlive(pid) {
		t.Fatalf("engine %d is not running after up", pid)
	}

	code, out, errOut = capture(t, func() int { return run([]string{"down"}) })
	if code != 0 {
		t.Fatalf("down exit = %d, stdout=%q stderr=%q", code, out, errOut)
	}
	if !waitForPIDDead(pid, 5*time.Second) {
		t.Errorf("pid %d survived `cardo down`: an exec-prefixed START must still prove ownership", pid)
	}
	if got := c.activeName(); got != "" {
		t.Errorf("the record must be cleared after a clean down, got %s", c.readState())
	}
}

// TestDownRefusesAnEditedExecPrefixedStart is smoke (b): the record was
// written when START was `exec sleep 9343`, and the config has since been
// edited to a different payload. The live process is no longer provably
// cardo's, so `down` must refuse, leave it running, and keep the record;
// --forget remains the operator's way out.
func TestDownRefusesAnEditedExecPrefixedStart(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": strings.Replace(
		commandConf("demo", "exec sleep 9343", "sleep 9343", 10), "__HEALTH__", gatedHealth(t, func() bool {
			return processRunningWith("sleep 9343")
		}), 1)})

	code, out, errOut := capture(t, func() int { return run([]string{"up", "demo"}) })
	if code != 0 {
		t.Fatalf("up failed: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	pid := c.residentPID()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })

	// The operator edits START after the engine came up.
	body := strings.Replace(commandConf("demo", "exec sleep 9344", "sleep 9344", 10), "__HEALTH__", "http://127.0.0.1:1/health", 1)
	if err := os.WriteFile(filepath.Join(c.confDir, "demo.conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut = capture(t, func() int { return run([]string{"down"}) })
	if code == 0 {
		t.Fatalf("down must refuse a payload its START no longer describes: stdout=%q stderr=%q", out, errOut)
	}
	if !pidAlive(pid) {
		t.Errorf("cardo killed pid %d on an ownership proof it could not make", pid)
	}
	if !strings.Contains(errOut, "will not signal it") {
		t.Errorf("the refusal must say why it will not touch the pid: %q", errOut)
	}
	if got := c.activeName(); got != "demo" {
		t.Errorf("the record of the live engine must survive the refusal, got %s", c.readState())
	}

	code, out, errOut = capture(t, func() int { return run([]string{"down", "--forget"}) })
	if code != 0 {
		t.Fatalf("--forget must still work: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if !pidAlive(pid) {
		t.Errorf("--forget must not kill anything (pid %d)", pid)
	}
}

// TestDownForgetClearsACorruptRecord is the escape hatch the state loader
// itself points at: it has to work precisely when the record cannot be read.
func TestDownForgetClearsACorruptRecord(t *testing.T) {
	for _, body := range []string{"{not json at all", `{"version":99,"active":"demo"}`} {
		c := newCLI(t, map[string]string{"demo.conf": demoConf("99")})
		c.writeState(body)

		code, _, errOut := capture(t, func() int { return run([]string{"down"}) })
		if code == 0 {
			t.Fatalf("%q: a record cardo cannot read must not be ignored", body)
		}
		if !strings.Contains(errOut, "state") {
			t.Errorf("%q: the failure must name the record: %q", body, errOut)
		}

		code, out, errOut := capture(t, func() int { return run([]string{"down", "--forget"}) })
		if code != 0 {
			t.Fatalf("--forget must clear an unreadable record: exit=%d stdout=%q stderr=%q", code, out, errOut)
		}
		if st := c.readState(); st != "" {
			t.Errorf("the unreadable record must be gone, got %s", st)
		}
		if !strings.Contains(errOut, "state record cleared") {
			t.Errorf("--forget must say what it did: %q", errOut)
		}
	}
}

// TestUpRefusesWhenTheSlotAlreadyAnswers is the CLI half of the slot guard:
// an engine cardo never started answers the target's HEALTH, so `up` refuses
// instead of double-booking the slot, and nothing is started or recorded.
func TestUpRefusesWhenTheSlotAlreadyAnswers(t *testing.T) {
	hot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer hot.Close()
	conf := strings.Replace(demoConf("98"), "http://127.0.0.1:1/health", hot.URL, 1)
	c := newCLI(t, map[string]string{"demo.conf": conf})

	code, out, errOut := capture(t, func() int { return run([]string{"up", "demo"}) })
	if code == 0 {
		t.Fatalf("`up` over an occupied slot must fail, stdout=%q stderr=%q", out, errOut)
	}
	for _, want := range []string{"already answers HEALTH of demo", "investigate what answers HEALTH"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr must contain %q, got %q", want, errOut)
		}
	}
	// Nothing was resident before this swap and nothing was restored, so the
	// record is empty: telling the operator to `down --forget` an empty record
	// sends them chasing a thing that is not there.
	if strings.Contains(errOut, "down --forget") {
		t.Errorf("there is no record to forget here, got %q", errOut)
	}
	if strings.Contains(errOut, "was restored") {
		t.Errorf("nothing was restored (nothing had been stopped): %q", errOut)
	}
	if st := c.readState(); strings.Contains(st, `"active": "demo"`) {
		t.Errorf("cardo must not claim an engine it never started, got %s", st)
	}
}

// TestStatusFlagsADeadResident pins the record hygiene of `status`: a pid in
// the record is not a running engine, and reading "active" as "running" sends
// the operator to the wrong machine.
func TestStatusFlagsADeadResident(t *testing.T) {
	c := newCLI(t, map[string]string{"demo.conf": demoConf("96")})
	c.writeState(`{"version":1,"slot":0,"active":"demo","active_at":"2026-01-02T03:04:05Z","active_pid":99999999}`)

	code, out, errOut := capture(t, func() int { return run([]string{"status"}) })
	if code != 0 {
		t.Fatalf("status exit = %d (stdout=%q stderr=%q)", code, out, errOut)
	}
	if !strings.Contains(out, "(not running?)") {
		t.Errorf("status must question a resident whose pid is gone: %q", out)
	}

	// Control: a pid that is really there is reported without the warning.
	live := c.resident("96")
	code, out, errOut = capture(t, func() int { return run([]string{"status"}) })
	if code != 0 {
		t.Fatalf("status exit = %d (stdout=%q stderr=%q)", code, out, errOut)
	}
	if !strings.Contains(out, "active:   demo") {
		t.Errorf("the live resident must be reported: %q", out)
	}
	if strings.Contains(out, "(not running?)") {
		t.Errorf("pid %d is running; status must not question it: %q", live, out)
	}
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
	rest := string(b)
	if i := strings.LastIndex(rest, ")"); i >= 0 {
		rest = strings.TrimSpace(rest[i+1:])
		return !strings.HasPrefix(rest, "Z")
	}
	return true
}

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
