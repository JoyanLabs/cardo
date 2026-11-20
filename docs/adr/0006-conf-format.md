# ADR-0006: Config format — the flat `.conf` stays, strictly parsed

- Status: accepted (2026-11-20)
- Relates to: ADR-0002 (BUSY), ADR-0003 (hooks), ADR-0005 (PROGRESS)

## Context

The two merged examples (`tensorfold.conf`, `red-snow-tabby.conf`) define a
de facto stable format: one file per engine, flat `KEY="value"` lines,
`#` comments. DESIGN.md fixed the vocabulary (engine, slot) and the keys.
The question: keep it or migrate to YAML/TOML.

Evidence: llama-swap validates strictly (unknown keys and dangling
references fail load); spark-switchboard keeps per-engine behavior as
declarable data. cardo's format must be easy to write by hand on a server
with an editor, and diffable.

## Decision

Keep the flat `.conf` as the only config format.

Format rules, now normative:

- One file per engine under the configured engines directory (default
  `$XDG_CONFIG_DIR/cardo/engines.d/`, `--conf-dir` override; no lab paths in
  code). Files ignored when the name starts with `.` or ends in `~`.
- `KEY="value"` lines; blank lines and `#` comments ignored; double quotes
  optional but stripped; backslash escapes nothing (values are raw).
- Duplicate keys in one file → load error. Duplicate NAME across files →
  load error.
- **Unknown keys fail the load** with the file, line and key named. This
  catches `PREUP`/`PREUP=` typos that would otherwise silently skip a hook
  (ADR-0003) and pins the schema.
- Keys and their types:

| Key | Types on | Required | Meaning |
|---|---|---|---|
| NAME | all | yes | unique engine name |
| DESCRIPTION | all | no | human line for `list` |
| TYPE | all | yes | `docker` \| `compose` \| `command` |
| CONTAINER | docker | yes* | container name |
| START | command | yes* | foreground command; cardo owns the process |
| COMPOSE_PROJECT | compose | yes* | project directory or -p name |
| HEALTH | all | yes | HTTP URL polled 2xx |
| BUSY | all | no | `url#selector` count probe (ADR-0002) |
| PROGRESS | all | no | `url#selector` loading progress (ADR-0005) |
| DRAIN | all | no | stall-free drain timeout seconds (default 60) |
| WAIT | all | no | health stall timeout seconds (default 120) |
| SLOT | all | no | slot id, integer, default 0 |
| PRE_UP | all | no | hook before start (ADR-0003) |
| POST_DOWN | all | no | hook after stop (ADR-0003) |

\* required for that TYPE; any other TYPE value is a load error (explicit
enum, no catch-all).

- Validation is separated from parsing: parse (syntax) → validate (types,
  enums, required-by-type, cross-file NAME uniqueness). `cardo doctor` runs
  the same validate plus live connectivity checks.
- No include/import in v1. Adding one is a small ADR later if a real need
  appears.

## Alternatives rejected

- YAML/TOML: better tooling, worse hand-editing story for one-file-per-engine
  on a server, and a bigger dependency for a format that is already working.
  Revisit only if configs grow structured per-engine sections (e.g. compose
  service overrides).
- `KEY=value` without quotes support: the examples already use quotes; keep
  both accepted, output canonical.

## Consequences

- The examples stay valid bit-for-bit; hook/progress keys are additive.
- The parser is a small package with table tests; strictness makes `doctor`
  and CI config checks meaningful.
