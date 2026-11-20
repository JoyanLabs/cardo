package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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

func TestWaitHealthyImmediately(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	alive := func() bool { return true }
	start := time.Now()
	if err := Wait(context.Background(), srv.URL, "", alive, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Errorf("healthy engine took %v to confirm", time.Since(start))
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
	if err := Wait(context.Background(), srv.URL, "", alive, 150*time.Millisecond); err != nil {
		t.Fatalf("progress changes must extend the deadline: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 151*time.Millisecond {
		t.Errorf("load finished in %v: deadline was not actually extended", elapsed)
	}
}

func TestWaitStuckProgressTimesOut(t *testing.T) {
	// Same progress value forever: growing messages would be an anti-pattern
	// (a stuck load must stall out). No progress field at all here.
	srv := stallServer(t, func() (float64, bool) { return 0, false })
	alive := func() bool { return true }

	err := Wait(context.Background(), srv.URL+"/health", "", alive, 120*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
}

func TestWaitMessageOnlyChangeDoesNotExtend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"message":"eta %d"}`, time.Now().UnixNano())
	}))
	defer srv.Close()

	alive := func() bool { return true }
	err := Wait(context.Background(), srv.URL, "", alive, 120*time.Millisecond)
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
	err := Wait(context.Background(), health.URL, progress.URL, alive, 150*time.Millisecond)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("want ErrStalled once progress stops, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 151*time.Millisecond {
		t.Errorf("stalled after %v: progress probe did not extend the deadline", elapsed)
	}
}

func TestWaitDeadEngineIsBroken(t *testing.T) {
	alive := func() bool { return false }
	err := Wait(context.Background(), "http://127.0.0.1:1/health", "", alive, time.Second)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("want ErrBroken, got %v", err)
	}
}

func TestWaitDeadEngineAfterProgressIsBroken(t *testing.T) {
	// The engine made progress, then died: broken, not stalled.
	changes := 0
	srv := stallServer(t, func() (float64, bool) {
		changes++
		return float64(changes) / 10, true
	})
	alive := func() bool { return changes < 4 }
	err := Wait(context.Background(), srv.URL, "", alive, 150*time.Millisecond)
	if !errors.Is(err, ErrBroken) {
		t.Fatalf("want ErrBroken, got %v", err)
	}
}
