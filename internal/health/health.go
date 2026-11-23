// Package health waits for an engine to become healthy with a *stall*
// timeout (ADR-0005): WAIT bounds how long health may make no observable
// progress, not the total wait. The deadline resets when numeric loading
// progress changes or when the health answer itself changes; a message
// alone never counts (a growing ETA can change while a load is stuck).
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/JoyanLabs/cardo/internal/probe"
)

// maxBody bounds one health response.
const maxBody = 64 << 10

// ErrStalled means health made no progress for the stall window: the load is
// stuck or the endpoint is wedged.
var ErrStalled = errors.New("health stalled: no progress within the WAIT window")

// ErrBroken means the engine is dead or impossible: process/container
// exited, or the wait was cancelled.
var ErrBroken = errors.New("engine broken")

// deadVerdicts is how many consecutive !alive() answers it takes to call an
// engine broken. One flaky verdict (a container inspect that answers once
// while docker is busy) must not condemn an engine that is still loading;
// only a sustained run of them is evidence the process or container exited.
const deadVerdicts = 3

// deadWindow is how long a run of dead verdicts must already have been going
// on before it is treated as an exited process. Counting verdicts alone is
// not enough: a liveness check that is merely flaky can hand over three
// "dead" answers inside a few hundred milliseconds, while an engine that
// really exited stays dead for as long as anyone looks. A variable so tests
// can shrink it.
// DeadWindow is package-level so the swap tests can shrink it: the
// orchestration layer pins drain, rollback and refusal semantics, not the
// liveness window itself.
var DeadWindow = 5 * time.Second

// maxPoll bounds the probe cadence; it is clamped against the stall window
// so a short WAIT still polls fast enough to notice progress.
const maxPoll = 1 * time.Second

// pollInterval returns the probe cadence for a stall window.
func pollInterval(stall time.Duration) time.Duration {
	p := stall / 4
	if p < 20*time.Millisecond {
		return 20 * time.Millisecond
	}
	if p > maxPoll {
		return maxPoll
	}
	return p
}

// Wait polls the health URL until it answers 2xx, extending the stall
// deadline on observable progress. progress is the optional PROGRESS probe
// (ADR-0005, passed parsed so the selector cannot be mangled); alive reports
// whether the engine process/container is still alive. alive answering dead
// deadVerdicts times in a row, over at least DeadWindow of wall time, is
// ErrBroken (a short run of flaky verdicts is not). The last transport
// failure is carried into both ErrStalled and ErrBroken.
func Wait(ctx context.Context, healthURL string, progress *probe.Probe, alive func() bool, stall time.Duration) error {
	deadline := time.Now().Add(stall)
	cadence := pollInterval(stall)
	portSeen := false
	var lastProgress *float64
	lastCode := -1 // no answer yet
	dead := 0      // consecutive dead liveness verdicts
	var firstDead time.Time
	// lastFetchErr is the most recent transport-level failure. It is kept
	// because "health stalled" on its own cannot tell an engine that never
	// bound its port from one whose handler is wedged, and that is exactly
	// what the operator needs to know to go looking.
	var lastFetchErr error
	client := &http.Client{Timeout: 5 * time.Second}

	for {
		if !alive() {
			if dead == 0 {
				firstDead = time.Now()
			}
			dead++
			if dead >= deadVerdicts && time.Since(firstDead) >= DeadWindow {
				return fmt.Errorf("%w: engine process or container exited while loading%s", ErrBroken, fetchErrSuffix(lastFetchErr))
			}
		} else {
			dead = 0
			firstDead = time.Time{}
		}
		if time.Now().After(deadline) {
			if lastProgress != nil {
				return fmt.Errorf("%w after %s (last progress %v)%s", ErrStalled, stall, *lastProgress, fetchErrSuffix(lastFetchErr))
			}
			return fmt.Errorf("%w after %s%s", ErrStalled, stall, fetchErrSuffix(lastFetchErr))
		}

		if !portSeen {
			if hostPort := hostPortOf(healthURL); hostPort != "" {
				conn, err := net.DialTimeout("tcp", hostPort, cadence/2)
				if err == nil {
					conn.Close()
					portSeen = true
					deadline = time.Now().Add(stall) // first life sign resets once
				}
			}
		}

		code, body, err := fetch(ctx, client, healthURL)
		if err != nil {
			lastFetchErr = err
		} else {
			if code/100 == 2 {
				return nil
			}
			// The endpoint answered: any *change* in the answer (a status it
			// had not served before) is one progress event. An identical
			// status served forever with no numeric progress is a stall —
			// a growing ETA must never extend the deadline.
			if code != lastCode {
				deadline = time.Now().Add(stall)
				lastCode = code
			}
			if p, ok := numericProgress(body); ok {
				if lastProgress == nil || *p != *lastProgress {
					lastProgress = p
					deadline = time.Now().Add(stall)
				}
			}
		}

		// The optional PROGRESS probe carries the numeric load progress for
		// engines whose HEALTH never exposes it.
		if progress != nil {
			ctxP, cancel := context.WithTimeout(ctx, 3*time.Second)
			if pv, perr := progress.ProgressValue(ctxP); perr == nil && pv != nil {
				if lastProgress == nil || *pv != *lastProgress {
					lastProgress = pv
					deadline = time.Now().Add(stall)
				}
			}
			cancel()
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v%s", ErrBroken, ctx.Err(), fetchErrSuffix(lastFetchErr))
		case <-time.After(cadence):
		}
	}
}

// fetchErrSuffix carries the reason the endpoint could not be reached into
// the failure the operator reads. An engine whose health never answered is
// reported differently depending on whether the port refused connections, the
// dial timed out, or the body never arrived.
func fetchErrSuffix(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf(" (last health fetch failed: %v)", err)
}

// fetch GETs the health URL; any HTTP answer (even non-200) is not an error.
func fetch(ctx context.Context, client *http.Client, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// numericProgress extracts a numeric progress field from a JSON body. A
// body without a usable number (llama-server's 503 error object, string
// progress) is not a progress report. The top-level "progress" key is a
// documented compatibility surface with llama-swap's loading reports.
func numericProgress(body []byte) (*float64, bool) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false
	}
	v, ok := doc["progress"]
	if !ok {
		return nil, false
	}
	f, ok := v.(float64)
	if !ok {
		return nil, false
	}
	return &f, true
}

// hostPortOf extracts host:port from an http URL for the port-open check.
func hostPortOf(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil || u.Port() == "" {
		return ""
	}
	return net.JoinHostPort(u.Hostname(), u.Port())
}
