# ADR-0003: Engine hooks — PRE_UP and POST_DOWN

- Status: accepted (2026-11-20)
- Relates to: issue #2

## Context

Issue #2: loading a ~97 GiB model pack on GB10 needs
`sync; echo 3 > /proc/sys/vm/drop_caches` before the load, because the page
cache counts into `cudaMemGetInfo`. That is server-specific and belongs in
the engine config file, not in code. The `.conf` format had no hook key.

llama-swap evidence: its only hook is `on_startup` with a strict schema
(`additionalProperties: false`, unknown references fail config load). cardo
must design swap-bound hooks itself.

## Decision

Two optional hook keys in the `.conf`, per engine:

```
PRE_UP="sync && echo 3 > /proc/sys/vm/drop_caches"
POST_DOWN="true"
```

Execution points in the `up` sequence:

1. drain (BUSY, ADR-0002)
2. stop the resident engine
3. **PRE_UP**
4. start the new engine
5. health (ADR-0005) — on failure: rollback (ADR-0004)

`PRE_UP` runs after the resident engine is stopped (its memory is already
back in the pool; page cache released before the new load maps it) and
before the new engine starts.

`POST_DOWN` runs after the stop step, in both `up` (after stopping the old
engine) and `down` (after stopping the active engine). One hook, one
semantic: "just stopped something on this slot".

Hook execution rules:

- Run through `sh -c`, in the foreground, with a fixed timeout of 60 s
  (hooks must never hang a swap; if 60 s is not enough the hook is wrong).
  Output is captured and shown on failure, logged otherwise.
- Exit code != 0 → the swap fails at that point. Because PRE_UP runs after
  the stop but before the start, the recorded previous engine is restored by
  the normal rollback path (ADR-0004); the new engine was never started.
- Hooks are config data: the parser rejects unknown keys (ADR-0006), so a
  typo like `PREUP` fails load instead of silently not running.
- Hooks are executed by cardo with the caller's privileges; the README says
  so plainly. They are not a sandbox.

## Alternatives rejected

- `PRE_UP` before stopping the resident engine: would release page cache
  while the old engine still holds device memory; the GB10 case wants both
  orders (stop first, then drop caches), and one timing keeps the contract
  simple.
- Per-hook timeout key: rejected for v1; a fixed value plus documentation
  keeps the format small. Revisit only with a real case.
- `PRE_STOP`/`POST_UP` variants: no driver case yet; add with an ADR when a
  real one exists.

## Consequences

- The GB10 case is data: a `.conf` with `PRE_UP` needs no cardo code change.
- Rollback interacts: a failing PRE_UP restores the resident engine, which is
  what the user expects from a failed swap.
