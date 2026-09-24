# Changelog: hedged erasure reads (read_fanout_delay)

## Summary

Adds an opt-in "hedged read" strategy for erasure-coded object reads
(`GET`/range reads). A single slow drive no longer stalls a read; the
read races `K+1` shards (K = data blocks) and, if that is not enough,
fans out to all remaining shards after a configurable delay.

## Configuration

New `drive` subsystem setting, applied dynamically (no restart):

```
mc admin config set <alias> drive.read_fanout_delay=250ms
```

- `0` (default): current behavior, hedging disabled.
- `250ms` (example): enable hedged reads; value is per-block fan-out delay.
- Environment override: `MINIO_DRIVE_READ_FANOUT_DELAY=250ms`.
  The environment variable takes precedence over the config value.

## Behavior when enabled

Per erasure block (`Erasure.Decode`, normal object reads only):

1. Launches reads on the first `K+1` online shards (local disks preferred
   via the existing `prefer` ordering). One slow or missing shard is
   covered instantly at the cost of a single extra shard read.
2. A shard that fails immediately (missing, corrupt, disk error) triggers
   an immediate substitute read, as before.
3. If `K` verified shards have not arrived within `read_fanout_delay`,
   reads are launched on every remaining shard in parallel.
4. The block completes as soon as `K` bitrot-verified shards (data or
   parity) have arrived; the decode step reconstructs missing data
   shards from whatever mix arrived.
5. Readers that lose the race ("stragglers") are not waited on. A
   straggler that completes late is discarded for its own block and
   rejoins for the next block; while slow it is simply skipped.

Healing (`Erasure.Heal`) and all write paths are unchanged. Read quorum
failure still surfaces as `SlowDownRead` (503).

## Overheads (when enabled)

- Steady state: one extra shard read per block (`K+1` instead of `K`).
- Full fan-out reads only on blocks that actually stall for the
  configured delay; shards read after the block completed are wasted
  reads, bounded by the fan-out.
- RAM: shard buffers are allocated per launched reader instead of
  seeded from the shared byte pool (straggler goroutines may outlive
  the reader); worst case is `N x shardSize` per in-flight part read.

## Operational notes

- Sensible starting point: `250ms`. Must stay well below
  `drive.max_timeout` (default 30s).
- Bitrot verification applies to every shard that wins the race, so
  the integrity guarantees are unchanged: corrupt shards are replaced
  by parity on the fly and the object is queued for async heal.
- Tune with the existing disk metrics (`total-errs-timeout`) and
  observe straggler frequency before lowering `drive.max_timeout`; a
  slow drive now loses block races instead of blocking them, so the
  two settings compound.

## Files changed

- `internal/config/drive/drive.go`, `internal/config/drive/help.go`:
  new `read_fanout_delay` key, env override, dynamic update, help text.
- `cmd/erasure-decode.go`: hedged read implementation in
  `parallelReader` (`readHedged`), in-flight reader tracking, straggler
  re-join/abandon lifecycle; `Erasure.Decode` wires the config,
  `Erasure.Heal` stays on the plain path.
- `cmd/erasure-decode_test.go`: hedged unit tests (hedge coverage,
  fan-out timer, error substitution, quorum error, straggler rejoin),
  full-stack slow-disk decode test, and byte-pool self-initialization
  for the decode tests (fixes pre-existing panic when run standalone).