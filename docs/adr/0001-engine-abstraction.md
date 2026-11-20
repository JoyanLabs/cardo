# ADR-0001: Engine abstraction over docker, compose and command types

- Status: accepted (2026-11-20)
- Relates to: issue #2 (hooks), ai-lab tracker (ComfyUI job queue)

## Context

cardo swaps which engine holds one GPU slot. Engines arrive in three shapes:
a docker container by name, a docker compose project, and a bare command that
must stay in the foreground while cardo owns its lifetime. A fourth shape is
coming: job-queue engines such as ComfyUI, which are "busy" because of queued
jobs, not in-flight HTTP requests (tracked separately).

Upstream evidence gathered in phase 0:

- llama-swap models every engine as a command (`cmd` + `cmdStop`); docker is
  just `cmd: docker run ...`, `cmdStop: docker stop ...`. Small process
  surface: `internal/process/process.go` (~8 methods).
- spark-switchboard never hardcodes engine logic in its scheduler; per-engine
  behavior is declarable data (its `busy_probe`).
- DGX-Model-Manager refuses to touch services it did not start, keyed on a
  compose label, never on a port scan.

## Decision

Keep the declared `TYPE` enum from the stable `.conf` format — `docker`,
`compose`, `command` — with one small built-in implementation per type over a
single internal interface:

```go
type Engine interface {
    Spec() config.EngineSpec
    Start(ctx context.Context) (running Running, err error)
    Stop(ctx context.Context, running Running, grace time.Duration) error
}
```

- `docker`: shells out to the docker CLI (`docker start/stop`), no Docker SDK
  dependency. Stop honors the CLI's own grace period.
- `compose`: shells out to `docker compose up -d` / `docker compose down`.
- `command`: runs the START command in the foreground as a child process; stop
  is SIGTERM → grace timeout → SIGKILL the **process group**, driven directly
  (never via context cancellation, which silently caps graceful shutdown —
  the llama-swap "v219 bug" lesson).

Job-queue engines (ComfyUI) are NOT a fourth TYPE in v1. They are covered by
the drain contract (ADR-0002): a queue length is just another busy probe. A
future TYPE is a new `Engine` implementation plus a documented drain probe —
adding one must be data plus one file, not scheduler surgery.

## Ownership and untracked safety

cardo only stops an engine it started or recorded as active (its persistent
state, ADR-0004). If the slot is occupied by something cardo did not start
(a manual `docker run` during an experiment), cardo reports it, warns, and
refuses to stop it. No port-based or name-guess fallback (DGX-Model-Manager
rule). `watch`/`status` may observe untracked engines; `up`/`down` never
kill them.

## Consequences

- Positive: the three current `.conf` examples stay valid unchanged; adding
  an engine type is additive; no SDK dependency.
- Negative: shelling out to the docker CLI couples us to CLI output shape for
  inspect commands; mitigated by keeping docker interactions to a handful of
  verbs with their own tests and a fake docker shim in CI.
