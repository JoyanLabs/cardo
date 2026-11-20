# ADR-0002: BUSY contract — one probe shape, typed emptiness

- Status: accepted (2026-11-20)
- Relates to: issue #3

## Context

Issue #3: BUSY semantics differ per engine. TensorFold reports live
connections in `/health` JSON (`$.live.connections`), TabbyAPI has opt-in
`/metrics` counters, ComfyUI has a job queue. The drain decision needs one
contract with documented per-key expectations.

Anti-pattern found in our own bash predecessor (`motor-switch.sh`): a broken
probe (empty or unparseable answer) was indistinguishable from an empty
queue, so the drain loop broke silently and could swap onto working jobs.
spark-switchboard fixes this class of bug by declaring the probe as data and
treating "cannot answer" as busy, never as free.

## Decision

`BUSY` is one string with two parts:

```
BUSY="url#selector"
```

- `url`: an HTTP endpoint returning JSON.
- `selector`: a JSON pointer-ish path to a numeric field. The value is a
  count. `0` means free; anything else (or a non-number) means busy.

Semantics:

1. **Busy is a number, not a boolean.** Engines grow (connections, queued
   jobs, in-flight generations); a count supports drain-waiting for the queue
   to reach zero and reporting deltas in `watch`.
2. **No BUSY key** → cardo cannot ask. Drain degrades to a grace wait of
   `DRAIN` seconds with an explicit warning in the output. Documented, not
   silent.
3. **Probe present but broken** (HTTP error, non-JSON, selector missing) →
   the answer is "unknown". Unknown is treated as **busy**, never as empty:
   the drain loop keeps polling until `DRAIN` times out, then the swap aborts
   and the resident engine is untouched. Failing closed; never failing open.
4. **Never interrupt work** (spark-switchboard invariant): `up` pauses before
   stop until the count reaches 0; a forced interrupt is out of scope for v1.

Per-type expectations (documented in `examples/` and the README):

| Engine | BUSY example |
|---|---|
| TensorFold (docker) | `http://localhost:8888/health#$.live.connections` |
| TabbyAPI (command) | `http://127.0.0.1:8908/metrics#generations_active` |
| ComfyUI (job queue, future) | `http://127.0.0.1:8188/queue#$.queue_running.length` style count of running+pending |
| docker container without an HTTP probe | omit BUSY; grace-wait drain applies |

Selector grammar in v1: dot path with `$.`, array indices, and `.length` on
arrays. No full jq. If an engine needs more than that, its BUSY is expressed
against an endpoint that exposes the count directly (opt-in `/metrics`).

## Consequences

- The drain logic is engine-agnostic: it consumes a typed probe result
  (`free | busy | unknown`) produced by one small probe package.
- Job-queue engines need no code change: their queue length is the count.
- The selector grammar is a compatibility surface; it stays minimal and is
  pinned by tests.
