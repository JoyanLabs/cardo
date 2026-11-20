# Example engines

Two real engine configs sharing one GPU slot on a DGX Spark (GB10, 128 GiB
unified memory): TensorFold serving a Qwen NVFP4 checkpoint as the default
resident engine, and TabbyAPI serving a ~97 GiB EXL3 pack swapped in on
demand. They are data, not code: they show the shape of a working setup,
including the parts cardo does not solve yet.

## The swap run this documents

A full round trip, as done manually on 2026-11-19/20:

1. Announce and drain: the resident engine was idle (0 live connections) but
   a real drain step belongs here.
2. Stop the resident engine (`docker stop`, its memory returns to the pool).
3. Drop the page cache (`sync; echo 3 > /proc/sys/vm/drop_caches`): on GB10
   the page cache counts into `cudaMemGetInfo`, so a ~97 GiB load fails with
   "Insufficient VRAM in split" even when ~115 GiB look free. **This step has
   no config key yet** (see the hook proposal in the tracker).
4. Start the new engine and wait for health: first load takes ~2 minutes for
   the big pack; a fixed short timeout aborts a healthy load mid-way, so
   `WAIT` is generous and health should extend the timeout on progress.
5. Validate (functional checks against the OpenAI endpoint), use the engine,
   then stop it and restore the previous engine (`up tensorfold-qwen`).

This is exactly `cardo up red-snow-tabby` followed by
`cardo up tensorfold-qwen`, minus steps 3 and the announcement, which are the
two open design questions.

## Lessons for the CLI

- **Health must tolerate long loads.** A 97 GiB pack needs minutes before its
  HTTP health endpoint answers; the timeout contract has to distinguish
  "still loading" from "broken".
- **Rollback needs memory of the previous engine.** The restore step above is
  what the rollback design must record (and survive a host reboot).
- **BUSY differs per engine.** TensorFold exposes `$.live.connections` in its
  health JSON; TabbyAPI's live counters are behind an opt-in `/metrics`;
  ComfyUI-style engines have a job queue instead of in-flight requests. One
  contract, documented per key, is needed (see the tracker proposal).
- **Engines outside the registry exist.** A manual `docker run` during an
  experiment must not be killed by a swap; detect and warn.
