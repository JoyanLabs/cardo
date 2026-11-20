# ADR-0004: Persistent state and rollback to the previous engine

- Status: accepted (2026-11-20)
- Relates to: `up` rollback semantics

## Context

The manual swap case study (examples/): after a failed or finished run, the
previous engine must come back. Rollback needs memory of what was resident,
and that memory has to survive a host reboot — an operator cannot be expected
to remember which engine was up before a crash.

DGX-Model-Manager evidence: ownership is recorded state (a compose label),
never a port scan; cardo must know what it started to be allowed to stop it.
spark-switchboard: adoption of a running process requires proof of ownership
(pidfile) or it is flagged `external` and never stopped.

## Decision

One small state file, derived from the environment (XDG), never hardcoded:

```
$XDG_STATE_DIR/cardo/state.json   (default ~/.local/state/cardo/state.json)
```

Written atomically (temp file + `fsync` + rename) after every state
transition. Content:

```json
{
  "slot": 0,
  "active": { "name": "red-snow-tabby", "started_at": "...", "pid": 4242 },
  "previous": { "name": "tensorfold-qwen", "stopped_at": "..." }
}
```

- `active`: what cardo believes is resident. This is the ownership record:
  `down` and `up` stop engines only by this record (plus, for docker, a name
  match against the spec). If the recorded active engine cannot be found,
  cardo stops with a clear error instead of guessing.
- `previous`: what `up` must restore if the new engine fails health. Set at
  stop time in `up`, cleared only by an explicit successful `up` or when the
  user runs `up` onto the same engine.
- A missing state file means "nothing resident, nothing to roll back to":
  `down` is a no-op with a message; `up` proceeds without a rollback target.
  This must be a typed condition (sentinel error), not a string match.
- Divergence handling: if the recorded active container/process is gone but
  an **untracked** engine answers on the slot, cardo warns and refuses to
  act destructively (ADR-0001 ownership rule); `cardo down --forget` is the
  explicit escape hatch to clear the record.

Rollback sequence on health failure (ADR-0005):

1. stop the failed engine (grace, then SIGKILL group)
2. `previous` engine: `up` it again with a plain health wait (no hooks of
   the failed engine run again; POST_DOWN of the failed engine runs)
3. restore the state record; report the outcome and the health failure
   evidence (last probe error, elapsed, progress seen)

Rollback is best-effort and its own failure is reported loudly but never
recursed (no rollback-of-rollback).

## Consequences

- Reboot-safe: after a crash, `cardo status` reconstructs the picture from
  state plus a live check; stale records degrade to warnings, not kill
  orders.
- The state file is a compatibility surface: schema versioned from day one
  (`"version": 1`).
- Concurrent invocations: a lockfile guards `up`/`down` (single-writer); two
  swappers on one slot is a user error cardo refuses.
