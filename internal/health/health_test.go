package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoyanLabs/cardo/internal/probe"
)

// stallServer serves a HEALTH endpoint that answers non-200 with a JSON body
// containing progress; value is read on every request.
func stallServer(t *testing.T, progress func() (float64, bool)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		if p, ok := progress(); ok {
			fmt.Fprintf(w, `{"progress":%v}`, p)
		} else {
			w.Write([]byte(`{"message":"loading"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustProbe(t *testing.T, value string) *probe.Probe {
	t.Helper()
	p, err := probe.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestWaitHealthyImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	alive := func() bool { return true }
	start := time.Now()
	if err := Wait(context.Background(), srv.URL, nil, alive, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Errorf("healthy engine took %v to confirm", time.Since(start))
	}
}

// TestWaitAccepts2xxNotOnly200 pins "success is 2xx": a 204 (or any 2xx) is
// healthy, a 3xx is not.
func TestWaitAccepts2xxNotOnly200(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusNoContent, http.StatusPartialContent} {
		code := code
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			if err := Wait(context.Background(), srv.URL, nil, func() bool { return true }, 300*time.Millisecond); err != nil {
				t.Errorf("%d must count as healthy: %v", code, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPermanentRedirect)
	}))
	defer srv.Close()
	if err := Wait(context.Background(), srv.URL, nil, func() bool { return true }, 120*time.Millisecond); !errors.Is(err, ErrStalled) {
		t.Errorf("3xx must not count as healthy: %v", err)
	}
}

func TestWaitProgressChangeExtendsDeadline(t *testing.T) {
	// Progress advances every 60ms; the stall window is 150ms. Total load
	// takes ~5 changes = 300ms: a stall timeout would abort, progress must
	// carry it through.
	changes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		changes++
		if changes > 5 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"progress":%v}`, float64(changes)/10)
	}))
	defer srv.Close()
	alive := func() bool { return true }

	start := time.Now()
	if err := Wait(context.Background(), srv.URL, nil, alive, 150*time.Millisecond); err != nil {
		t.Fatalf("progress changes must extend the deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 151*time.Millisecond {
		t.Errorf("load finished in %v: deadline was not actually extended", elapsed)
	}
}

// TestWaitStatusChangeExtendsDeadline pins the status-change reset: an
// endpoint that keeps answering with a *different* status (503, 500, 503,
// 500, ...) is making observable progress, so the window must keep moving.
func TestWaitStatusChangeExtendsDeadline(t *testing.T) {
	const stall = 150 * time.Millisecond
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := n.Add(1)
		if i > 8 {
			w.WriteHeader(http.StatusOK) // only reachable by extending the window
			return
		}
		if i%2 == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		w.Write([]byte(`{"message":"loading"}`))
	}))
	defer srv.Close()

	start := time.Now()
	if err := Wait(context.Background(), srv.URL, nil, func() bool { return true }, stall); err != nil {
		t.Fatalf("changing statuses must extend the deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed < stall {
		t.Errorf("load finished in %v: the status change never reset the window", elapsed)
	}
}

// TestWaitIdenticalStatusForeverStalls pins the other side of the same rule:
// one status served forever is NOT progress, however many times it answers.
func TestWaitIdenticalStatusForeverStalls(t *testing.T) {
	const stall = 150 * time.Millisecond
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"message":"loading"}`)) // identical body and status
	}))
	defer srv.Close()

	start := time.Now()
	err := Wait(context.Background(), srv.URL, nil, func() bool { return true }, stall)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("an identical status forever must stall, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < stall {
		t.Errorf("stalled after %v, before one full window", elapsed)
	} else if elapsed > 4*stall {
		t.Errorf("stalled after %v: an identical status kept resetting the window", elapsed)
	}
}

func TestWaitStuckProgressTimesOut(t *testing.T) {
	// Same progress value forever: growing messages would be an anti-pattern
	// (a stuck load must stall out). No progress field at all here.
	srv := stallServer(t, func() (float64, bool) { return 0, false })
	alive := func() bool { return true }

	err := Wait(context.Background(), srv.URL+"/health", nil, alive, 120*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
}

// TestWaitRepeatedIdenticalProgressStalls pins that a numeric progress field
// that never changes is not progress either.
func TestWaitRepeatedIdenticalProgressStalls(t *testing.T) {
	const stall = 150 * time.Millisecond
	srv := stallServer(t, func() (float64, bool) { return 0.5, true }) // stuck at 0.5
	start := time.Now()
	err := Wait(context.Background(), srv.URL, nil, func() bool { return true }, stall)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("an unchanging numeric progress must stall, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*stall {
		t.Errorf("stalled after %v: an identical progress kept resetting the window", elapsed)
	}
}

func TestWaitMessageOnlyChangeDoesNotExtend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"message":"eta %d"}`, time.Now().UnixNano())
	}))
	defer srv.Close()

	alive := func() bool { return true }
	err := Wait(context.Background(), srv.URL, nil, alive, 120*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("message-only changes must not extend: %v", err)
	}
}

func TestWaitProgressProbeResetsDeadline(t *testing.T) {
	// The numeric progress lives on a separate PROGRESS endpoint (ADR-0005);
	// HEALTH itself answers non-JSON forever. After the progress finishes the
	// endpoint still never turns 200, so the wait must end in a stall — but
	// only after the five progress changes extended the deadline.
	changes := 0
	health := stallServer(t, func() (float64, bool) { return 0, false })
	progress := stallServer(t, func() (float64, bool) {
		changes++
		if changes > 10 {
			return 1.0, true // load completed; no further progress changes
		}
		return float64(changes) / 10, true
	})

	alive := func() bool { return true }
	start := time.Now()
	err := Wait(context.Background(), health.URL, mustProbe(t, progress.URL+"#$.progress"), alive, 150*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled once progress stops, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 151*time.Millisecond {
		t.Errorf("stalled after %v: progress probe did not extend the deadline", elapsed)
	}
}

// TestWaitProgressProbeIsLoadBearing is the mutation guard for the PROGRESS
// reset: the HEALTH endpoint answers 503 with no progress field of its own
// and only turns 200 long after one stall window, while the PROGRESS endpoint
// reports a ramp that then stops. Without the probe reset the wait gives up
// before the load finishes; with it, the wait succeeds. The nil-probe run is
// the negative control that proves the same servers stall out on their own.
func TestWaitProgressProbeIsLoadBearing(t *testing.T) {
	const stall = 300 * time.Millisecond
	const progressRounds = 12 // polls that still report a *new* value

	type load struct {
		health   *httptest.Server
		progress *httptest.Server
		polls    *atomic.Int64 // PROGRESS polls
		healthy  *atomic.Bool  // the HEALTH endpoint ever served a 2xx
	}

	newLoad := func() load {
		var l load
		l.polls = new(atomic.Int64)
		l.healthy = new(atomic.Bool)
		l.progress = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := l.polls.Add(1)
			if n > progressRounds {
				n = progressRounds // the load is stuck at its last value
			}
			fmt.Fprintf(w, `{"progress":%v}`, float64(n)/progressRounds)
		}))
		t.Cleanup(l.progress.Close)
		l.health = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if l.polls.Load() > progressRounds {
				l.healthy.Store(true)
				w.WriteHeader(http.StatusOK) // answers only once the probe got there
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"message":"loading"}`))
		}))
		t.Cleanup(l.health.Close)
		return l
	}

	t.Run("with probe succeeds past one window", func(t *testing.T) {
		l := newLoad()
		start := time.Now()
		if err := Wait(context.Background(), l.health.URL, mustProbe(t, l.progress.URL+"#$.progress"),
			func() bool { return true }, stall); err != nil {
			t.Fatalf("the PROGRESS probe must carry the wait to health: %v", err)
		}
		if elapsed := time.Since(start); elapsed < 2*stall {
			t.Errorf("healthy after %v, want >= %v: the probe never extended the window", elapsed, 2*stall)
		}
		if l.polls.Load() <= progressRounds {
			t.Errorf("succeeded after only %d progress polls: health was reached without the probe", l.polls.Load())
		}
		if !l.healthy.Load() {
			t.Error("success must come from a 2xx HEALTH answer")
		}
	})

	t.Run("without probe stalls", func(t *testing.T) {
		l := newLoad()
		err := Wait(context.Background(), l.health.URL, nil, func() bool { return true }, stall)
		if !errors.Is(err, ErrStalled) {
			t.Fatalf("negative control: without the PROGRESS probe the wait must stall, got %v", err)
		}
		if l.healthy.Load() {
			t.Error("negative control: the engine reached health without the probe, so the probe is not load-bearing")
		}
	})
}

// rampServer answers non-200 with a changing numeric progress until request
// `healthyFrom`, then serves 200: a load that only finishes late.
func rampServer(t *testing.T, healthyFrom int64) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var reqs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := reqs.Add(1)
		if i >= healthyFrom {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"progress":%v}`, float64(i)/100)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

// TestWaitToleratesOneTransientDeadVerdict pins A#7: one flaky liveness
// verdict (a container inspect that answers "not running" once) must not
// classify a loading engine broken. Only a run of consecutive dead verdicts
// is evidence of a dead engine.
func TestWaitToleratesOneTransientDeadVerdict(t *testing.T) {
	const stall = 150 * time.Millisecond
	srv, _ := rampServer(t, 8)
	var verdicts atomic.Int64
	alive := func() bool { return verdicts.Add(1) != 3 } // exactly one blip

	if err := Wait(context.Background(), srv.URL, nil, alive, stall); err != nil {
		t.Fatalf("a single transient dead verdict must not break the wait: %v", err)
	}
	if got := verdicts.Load(); got < 5 {
		t.Errorf("the wait took %d liveness verdicts: it never got past the blip", got)
	}
}

// TestWaitSustainedDeadVerdictsAreBroken is the other side of the tolerance:
// a run of consecutive dead verdicts is still ErrBroken, and it must take the
// whole run (one verdict alone must not condemn the engine).
func TestWaitSustainedDeadVerdictsAreBroken(t *testing.T) {
	restore := DeadWindow
	DeadWindow = 300 * time.Millisecond
	t.Cleanup(func() { DeadWindow = restore })

	const stall = 2 * time.Second
	srv, _ := rampServer(t, 1000) // never reaches health within the test
	var verdicts atomic.Int64
	alive := func() bool { return verdicts.Add(1) < 3 } // dead from the 3rd on

	err := Wait(context.Background(), srv.URL, nil, alive, stall)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("a sustained dead verdict must be ErrBroken, got %v", err)
	}
	if got := verdicts.Load(); got < 5 {
		t.Errorf("ErrBroken after %d liveness verdicts: only %d were dead, so the wait gave up before a run of three", got, got-2)
	}
}

func TestWaitDeadEngineIsBroken(t *testing.T) {
	restore := DeadWindow
	DeadWindow = 100 * time.Millisecond
	t.Cleanup(func() { DeadWindow = restore })

	alive := func() bool { return false }
	err := Wait(context.Background(), "http://127.0.0.1:1/health", nil, alive, time.Second)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("want ErrBroken, got %v", err)
	}
}

func TestWaitDeadEngineAfterProgressIsBroken(t *testing.T) {
	restore := DeadWindow
	DeadWindow = 100 * time.Millisecond
	t.Cleanup(func() { DeadWindow = restore })

	// The engine made progress, then died: broken, not stalled.
	changes := 0
	srv := stallServer(t, func() (float64, bool) {
		changes++
		return float64(changes) / 10, true
	})
	alive := func() bool { return changes < 4 }
	err := Wait(context.Background(), srv.URL, nil, alive, 150*time.Millisecond)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("want ErrBroken, got %v", err)
	}
}

// TestWaitProbeFailureNeverSucceeds pins that a PROGRESS endpoint that cannot
// answer never buys the wait extra time beyond one window.
func TestWaitProbeFailureNeverSucceeds(t *testing.T) {
	const stall = 150 * time.Millisecond
	health := stallServer(t, func() (float64, bool) { return 0, false })
	// A dead PROGRESS endpoint: the probe errors on every call.
	start := time.Now()
	err := Wait(context.Background(), health.URL, mustProbe(t, "http://127.0.0.1:1/progress#$.progress"),
		func() bool { return true }, stall)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("a failing progress probe must not extend the wait, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*stall {
		t.Errorf("waited %v on a probe that can never answer", elapsed)
	}
}

// TestWaitStalledNamesTheLastFetchFailure pins F7: "health stalled" alone
// tells an operator nothing about why. The last fetch error (connection
// refused, a wedged dial, a truncated body) is the difference between an
// engine that never bound its port and one whose endpoint is wedged, so it
// has to survive into the message.
func TestWaitStalledNamesTheLastFetchFailure(t *testing.T) {
	err := Wait(context.Background(), "http://127.0.0.1:1/health", nil, func() bool { return true }, 120*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the stall message must carry the last fetch error, got %q", err)
	}
}

// TestWaitBrokenNamesTheLastFetchFailure: the same for ErrBroken, where the
// operator's next question is whether the process died or the endpoint did.
func TestWaitBrokenNamesTheLastFetchFailure(t *testing.T) {
	restore := DeadWindow
	DeadWindow = 50 * time.Millisecond
	t.Cleanup(func() { DeadWindow = restore })

	err := Wait(context.Background(), "http://127.0.0.1:1/health", nil, func() bool { return false }, time.Second)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("want ErrBroken, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("the broken message must carry the last fetch error, got %q", err)
	}
}

// TestWaitDeadVerdictsMustSpanTheDeadWindow pins the wall-clock half of the
// dead rule: three dead verdicts are not enough on their own, they must also
// span the window, so a burst of flaky liveness answers cannot condemn an
// engine that is still loading. The endpoint alternates status codes, which
// counts as progress and keeps the stall timeout out of the decision.
func TestWaitDeadVerdictsMustSpanTheDeadWindow(t *testing.T) {
	restore := DeadWindow
	DeadWindow = 1 * time.Second
	t.Cleanup(func() { DeadWindow = restore })

	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	err := Wait(context.Background(), srv.URL, nil, func() bool { return false }, 200*time.Millisecond)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("a sustained dead verdict must still be ErrBroken, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < DeadWindow {
		t.Errorf("ErrBroken after %v, shorter than the %v dead window it must span", elapsed, DeadWindow)
	}
}

// TestWaitShortDeadRunStallsInsteadOfBreaking is the same rule from the other
// side: with a dead window nothing inside the test can span, a run of dead
// verdicts is treated as a flaky inspector and the wait reports the stall.
func TestWaitShortDeadRunStallsInsteadOfBreaking(t *testing.T) {
	restore := DeadWindow
	DeadWindow = time.Hour
	t.Cleanup(func() { DeadWindow = restore })

	srv := stallServer(t, func() (float64, bool) { return 0, false }) // never healthy, never progressing
	var verdicts atomic.Int64
	err := Wait(context.Background(), srv.URL, nil, func() bool { verdicts.Add(1); return false }, 300*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("a dead run shorter than the window must stall, not break: %v", err)
	}
	if got := verdicts.Load(); got < 3 {
		t.Errorf("the wait gave up after %d liveness verdicts, before a run of %d", got, deadVerdicts)
	}
}

// TestDeadVerdictRulesAreTheDocumentedOnes pins the production values: three
// consecutive dead verdicts, over at least five seconds of wall time.
func TestDeadVerdictRulesAreTheDocumentedOnes(t *testing.T) {
	if deadVerdicts < 3 {
		t.Errorf("deadVerdicts = %d, want at least 3 (one flaky verdict must not condemn an engine)", deadVerdicts)
	}
	if DeadWindow < 5*time.Second {
		t.Errorf("DeadWindow = %v, want at least 5s of dead verdicts before ErrBroken", DeadWindow)
	}
}
