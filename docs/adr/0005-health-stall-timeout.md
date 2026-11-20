# ADR-0005: Health wait with stall timeout, not total timeout

- Status: accepted (2026-11-20)

## Context

A ~97 GiB pack needs minutes before its HTTP health endpoint answers; a fixed
total timeout aborts a healthy load mid-way. examples/ already set `WAIT=300`
as a workaround. llama-swap's solved design: `healthCheckTimeout` is a
**stall** timeout — the deadline restarts every time the *numeric loading
progress changes*; a message alone does not count (it can change while the
load is stuck, e.g. a growing ETA). Its tests pin exactly that
(`LoadingProgressExtendsTimeout`, `LoadingProgressStalls`).

## Decision

`WAIT` is redefined as a **stall** timeout: the maximum time health may make
no observable progress, not the total wait.

Progress sources, in order:

1. **PROGRESS key (optional)**: `PROGRESS="url#selector"` — same selector
   grammar as BUSY (ADR-0002), pointing at a numeric loading progress. While
   the value keeps changing, the deadline resets. A stuck value does not.
   This is the TensorFold/TabbyAPI-compatible generic hook and mirrors
   llama-swap's loading progress.
2. **Endpoint answered**: any HTTP response from HEALTH (even non-200) counts
   as one progress event (resets the deadline once per response change).
3. **Port opened**: the TCP port accepting connections counts as the first
   progress event.

Broken-vs-loading classification, checked in this order:

- process dead (child exited / container exited) → **broken**, stop waiting
- deadline expired without any progress event → **broken** (stall), report
  "no progress for WAIT"
- still no port/endpoint but alive → **loading**, keep waiting
- endpoint answering non-200 forever with no progress change → stall applies

Health success: HEALTH returns 2xx within the current deadline. `GET` only.

`WAIT` default when absent: 120 s (stall). The examples' `WAIT=300` becomes
"no stall longer than 300 s", which is correct for a slow load.

Error text distinguishes the two outcomes and includes the last probe error,
elapsed time and the last progress value seen, so a failed `up` report is
actionable.

## Consequences

- The examples' workaround value becomes semantically correct without edits.
- One probe package serves both BUSY and PROGRESS (same selector grammar).
- A health endpoint that never exposes progress still works via the
  port/answer signals; total wait is then bounded only by repeated stall
  resets — documented so operators pin PROGRESS when their engine can.
