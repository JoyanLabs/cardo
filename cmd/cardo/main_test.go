package main

import (
	"bytes"
	"fmt"
	"io"
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
	return "NAME=demo\nTYPE=command\nSTART=sh -c 'sleep " + marker + " & sleep " + marker + "'\n" +
		"HEALTH=http://127.0.0.1:1/health\nDRAIN=0\nWAIT=5\nSLOT=0\n"
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
