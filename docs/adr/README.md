# Architecture decision records

Short, numbered decisions with their context and consequences. Superseded
ADRs stay here, marked so.

| # | Decision | Status |
|---|---|---|
| 0001 | Engine abstraction over docker/compose/command types; ownership by recorded state, never ports | accepted |
| 0002 | BUSY contract: `url#selector` numeric count probe; unknown probe answers are busy, never free | accepted |
| 0003 | Engine hooks `PRE_UP`/`POST_DOWN` with fixed timeout; failure rolls the swap back | accepted |
| 0004 | Persistent state file (XDG state dir) as the ownership/rollback record; atomic writes | accepted |
| 0005 | `WAIT` is a stall timeout; deadline resets on loading progress (optional `PROGRESS` key) | accepted |
| 0006 | Flat `.conf` stays; strictly parsed, unknown keys fail load | accepted |
