# Watch: resumable full-ad subscriptions

> Status: **implemented** — `Collection.Watch` (`watch.go`, enabled by
> `Options.WatchHistory > 0`) and `Archive.Watch` (`archive.go`, a thin wrapper
> over the append-only Collection's Watch, always available). The Collection reuses the MVCC sequence machinery; its one real
> limitation is that precise *deletes* are only replayable within the retention window
> (`WatchHistory`), older resumes falling back to a full replay the client rebuilds
> from. The Archive is append-only, so its Watch is a durable log tail with none of
> that. The **Event types & client model** and **API** sections below are the
> canonical protocol for building a listener.

## Goal

A client subscribes and receives the **full ad** (not a delta) for every add or
update, and a **tombstone** for every delete. It can disconnect and later resume
from where it left off using an opaque **cursor** it persisted, and the server
replays everything that *potentially* changed since then. Semantics are
**at-least-once**: over-delivery (an ad the client already has) is fine; a missed
change is not. In the worst case the server may replay *all* current ads.

## The cursor (a.k.a. txn id)

There is no single global transaction counter — each shard has its own monotonic
`commitSeq`, by design (a global counter would reintroduce write contention). So the
cursor is **opaque to the client** and internally is:

```
cursor = { epoch, perShardSeq[nShards] }     // one commitSeq per shard
```

The Archive uses the same cursor: it is an append-only Collection, whose record `seq`s
are assigned in append order and never change.

- **`perShardSeq`** — for each shard, the `commitSeq` up to which the client has
  processed. On resume, shard *i* replays records with `seq > perShardSeq[i]`. This
  is exact per shard, needs no global ordering, and costs nothing on the write path.
- **`epoch`** — the identity of the sequence space the numbers belong to. If it does
  not match, the client's sequence numbers are from a different store generation, so
  the server ignores them and does a **full replay** (`WatchReset`). It is a random
  value chosen per process, except for a persistent append-only collection (an
  Archive), which carries it across a clean restart — see **Epoch across a restart**.
- A cursor whose `seq` for some shard is **ahead of** that shard's current head was
  not issued by the log as it now stands (e.g. an older copy of the store reopened),
  so it also gets a full replay — never a resume that would skip the records later
  assigned those numbers.

The client treats the whole thing as opaque bytes, stores the latest one it fully
processed, and hands it back on reconnect.

### Epoch across a restart

**Mutable collections** (tables) pick a fresh random epoch every time they are opened,
so **every watcher Resets after a server restart**. That is deliberate: the delete
journal lives in memory, so a resume could not replay deletes precisely anyway.

**Persistent append-only collections** (`Archive`, and so `db.ArchiveTable`) keep their
epoch across a **clean** restart, so a cursor issued before `Close` resumes after
`Open` with no Reset, as long as the records it points past are still retained (at or
above the append floor; otherwise the rotation-gap Reset below applies).

Persisting the epoch alone would be unsafe. A commit's `seq` becomes visible
(`WatchCursor`, catch-up) before its msync completes, so a watcher can hold a cursor
past a record that a crash then loses; recovery rebuilds `commitSeq` from what is on
disk, and the lost numbers would be reassigned to *different* records that a resumed
cursor silently skips. The protocol (`watchepoch.go`) therefore only vouches for an
epoch the previous process proved safe:

1. **Open consumes `<dir>/watch.epoch`** before mapping any segment: it reads the
   epoch and the per-shard high-water `commitSeq`, removes the file, and fsyncs the
   directory. From then until a successful `Close`, there is no marker on disk.
2. **Close**, after every segment flushed without error and with no write/sync error
   pending, fsyncs each shard directory and only then durably writes the marker
   (tmp + fsync + rename + dir fsync) with the epoch and each shard's `commitSeq`. A
   marker on disk thus implies every record issued under its epoch is durable.
3. On reopen with a valid marker, the epoch is adopted and each shard's `commitSeq`
   becomes `max(recovered, high-water)`, so a number issued before the restart is never
   reissued even if its record is gone (`Truncate`, or a shard `Rotate` emptied).
4. A missing, corrupt, or mismatched (shard count) marker — any crash, kill, or failed
   `Close` — means a **new random epoch**: every cursor Resets. Conservative (a crash
   whose data all survived still Resets) but never a silent skip.

A store directory copied while cleanly closed carries its marker; reopening an *older*
copy is only caught while its head is still behind the cursor (the ahead-of-head
check). Delete `watch.epoch` when restoring a store from a copy.

## Collection Watch: catch-up then live

Two phases, arranged so there is no gap between them.

1. **Register for live first.** Attach the watcher to the commit path (see below) and
   snapshot the current per-shard `commitSeq` as `S_reg`. Because the watcher is
   attached *before* the snapshot, every commit at `seq > S_reg` is captured live.
2. **Catch-up scan** for `(cursor, S_reg]`. This is the existing exactly-once scan
   with one added filter: emit a current record only when `seq > cursor[shard]`.
   Reading `seq` is cheap and needs no decode, so old records (the vast majority for a
   recent cursor) are skipped without cost. Each survivor is decoded and emitted as a
   full ad.
3. **Drain the live buffer**, then stream live. Events buffered during catch-up (all
   at `seq > S_reg`) are flushed; any overlap with the catch-up tail is a harmless
   duplicate (at-least-once). From here the watcher streams live commits.

The union of (catch-up `(cursor, S_reg]`) and (live `> S_reg`) covers everything
`> cursor` with no gap — the correctness crux.

### Live notification

Commits finalize under the shard lock in `applyOne`/`applyBatch` (`commit.go`), right
where `commitSeq` is bumped. Add a hook there that, for each committed write, hands
the watcher `{key, wireAd, codec, seq}` (deletes hand `{key, tombstone, seq}`). The
watcher decodes to a full ad off the write path. This reuses the group-commit
structure — one notification per coalesced batch.

## The hard part: deletes across compaction

A delete writes **no new record**; it only stamps `supersededBySeq` on the old record.
Compaction then **discards superseded records** — so the evidence that "key K was
deleted at seq S" is eventually garbage-collected. A client resuming from before S
would never learn K is gone. This is the only part that isn't free.

**Within a retention window** it is exact. Keep a bounded horizon `H` (by seq lag or
wall-clock age) and teach compaction to *retain* superseded records whose
`supersededBySeq > H` (they are not yet reclaimable). Then catch-up can emit
tombstones precisely: during the scan, collect superseded records with
`sup > cursor[shard]`; any such key with no current version emitted is a delete →
tombstone. `H` is advertised to clients as the oldest resumable point.

**Beyond the window** (cursor older than `H`, or a mismatched epoch) the server cannot
reconstruct which keys were deleted. Fall back to **full replay** of all current ads
and let the client **reconcile deletes itself**: it diffs the replayed key set against
the keys it currently holds and drops the ones absent from the replay. This needs the
client to hold its key set, which a full-state subscriber does anyway. This is the
"replay all ads for reconnecting clients" case the design explicitly allows.

(Alternative if we don't want compaction to retain tombstones: a separate append-only
**delete journal** — `(key, seq)` per delete — retained for `H` and rotated. Cleaner
separation, one more structure to persist. The retention-in-compaction option reuses
existing machinery and is the recommended first cut.)

## Making catch-up cheap

A recent cursor should not require reading every record's `seq` across the whole
store. Records are appended in commit order, so a *freshly written* segment's `seq`s
are monotonic and it carries a `[minSeq, maxSeq]` range — segments with
`maxSeq ≤ cursor` are skipped whole. Compaction, which gathers current records by key,
breaks per-segment monotonicity, so a compacted segment just records its actual
`[minSeq, maxSeq]` (a superset scan within it is fine — over-delivery is allowed). The
archive already keeps per-segment zone maps; the collection would add a cheap min/max
`seq` per segment. Fallback when ranges are unavailable: a full `seq`-filtered scan,
which is correct, just O(records).

## Archive Watch (`Archive.Watch`, implemented)

The archive is append-only — no updates, no deletes — so Watch is a log tail:

```go
func (a *Archive) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error)
```

- **Cursor** = the Collection's `{epoch, perShardSeq[]}`. Record `seq`s are assigned
  in append order and preserved by merge, re-encode, and recovery, so a `seq` is a
  durable **log position**. The epoch survives a clean restart (see **Epoch across a
  restart**), so **a cursor resumes incrementally across a clean reopen**; after a
  crash it Resets. No delete journal.
- **Events**: only `WatchUpsert` (an appended ad; `Key` is nil, `Ad` is the payload),
  plus `WatchSynced`/`WatchResync`. `WatchReset` here means a **history gap** — the
  cursor is older than what rotation still retains (or from a different archive), so
  the replay restarts from the current floor; a log consumer notes it may have missed
  entries (there is no current state to rebuild).
- **Catch-up** replays every retained record after the cursor, oldest-first; **live**
  streams new appends. `Append` publishes under its lock so live events are strictly
  in log order. Always available (no option); zero cost when no one is watching.

## Back-pressure

Each watcher has a bounded buffer. A slow client that overflows it is **demoted**, not
allowed to block writers: the server drops its live buffer and the client simply
reconnects with its last acked cursor, re-entering catch-up. At-least-once holds
because the cursor is only advanced by the client after it durably processes an event.

## API (implemented)

```go
// Enable Watch by giving the collection a delete-retention journal:
c := collections.New(collections.Options{
    Shards:       16,
    WatchHistory: 4096, // per-shard deletes retained; 0 ⇒ Watch disabled
    WatchBuffer:  1024, // per-watcher live buffer (default 1024)
})

type WatchKind uint8
const (
    WatchUpsert WatchKind = iota // Ad set: key added or updated (full ad)
    WatchDelete                  // Ad nil: key removed
    WatchReset                   // discard state; a full snapshot of Upserts follows
    WatchSynced                  // end of catch-up; now live. Cursor is a resume point
    WatchResync                  // stream fell behind; reconnect with your last cursor
)

type WatchEvent struct {
    Kind   WatchKind
    Key    []byte
    Ad     *classad.ClassAd // set for WatchUpsert
    Cursor []byte           // opaque; persist after processing (set on live events + Synced)
}

// Watch replays everything that may have changed since cursor (nil ⇒ full replay from
// empty), then streams live changes until ctx is cancelled or the consumer stops.
// Returns an error only if Watch is disabled (WatchHistory == 0).
func (c *Collection) Watch(ctx context.Context, cursor []byte) (iter.Seq[WatchEvent], error)
```

### Event types

| Kind | Meaning | Carries |
|------|---------|---------|
| `WatchUpsert` | key added or updated | `Key`, `Ad` (full) |
| `WatchDelete` | key removed | `Key` |
| `WatchReset` | discard local state; an authoritative full snapshot of `Upsert`s follows | — |
| `WatchSynced` | catch-up complete, now live | `Cursor` (durable resume point; swap the shadow here) |
| `WatchResync` | live stream lagged — reconnect with the last cursor | — |

`Upsert`/`Delete` are applied identically whether catching up or live — there is no
separate "mode". The only control the client must honor is `Reset`, which exists
**solely to convey deletes that fell out of the retention window**: rather than
diffing, the client rebuilds from the following snapshot, so deleted keys simply never
reappear. (An add/update-only store would need none of `Reset`/`Synced`.)

### Client model

```go
var live map[string]*classad.ClassAd    // committed state
var shadow map[string]*classad.ClassAd  // built during a Reset snapshot; nil otherwise

seq, err := c.Watch(ctx, lastCursor)    // lastCursor persisted from a prior run (nil first time)
for ev := range seq {
    switch ev.Kind {
    case WatchReset:
        shadow = map[string]*classad.ClassAd{}          // start a fresh snapshot
    case WatchUpsert:
        target(shadow, live)[string(ev.Key)] = ev.Ad
    case WatchDelete:
        delete(target(shadow, live), string(ev.Key))
    case WatchSynced:
        if shadow != nil { live, shadow = shadow, nil }  // atomically adopt the snapshot
        persist(ev.Cursor)                               // durable resume point
    case WatchResync:
        break // reconnect: call Watch(lastCursor) again
    }
    if ev.Cursor != nil { persist(ev.Cursor) }           // live events advance the cursor
}
// target(shadow, live) returns shadow while a snapshot is in progress, else live.
```

Only advance the persisted cursor *after* the event is durably applied — that is what
makes resume at-least-once. On restart, call `Watch(lastCursor)`.

## Limitations / non-goals

- **At-least-once, not exactly-once** — duplicates are possible by design.
- **No global order across shards** — the stream is per-shard-ordered and interleaved.
  Fine for independent full-ad updates; not a global change log.
- **Deletes beyond the retention horizon** cannot be replayed precisely → full replay
  + client-side reconcile. This is the one inherent limit.
- **Very slow clients** are demoted to a fresh catch-up rather than throttling writers.

## Status of the plan

Done (Collection):

1. **Cursor + catch-up** — opaque `{epoch, perShardSeq[]}` cursor, `seq`-filtered
   catch-up emitting `Upsert`s (deletes emitted first so re-adds end present).
2. **Live streaming** — commit-path hook (`applyOne`/`applyWrites`/`applyBatch` +
   `Delete`), per-watcher bounded buffer, register-then-snapshot handoff, non-blocking
   publish with `Resync`-on-overflow.
3. **Deletes** — implemented as a bounded per-shard **delete journal**
   (`Options.WatchHistory`), not compaction-retained tombstones (simpler, self-
   contained). Catch-up emits precise `Delete`s within the window; a cursor past the
   journal horizon (or a mismatched epoch) gets a `Reset` + full replay.

Also done:

4. **Archive Watch** — the append-only Collection's Watch: oldest-first catch-up, live
   tail, Reset-on-rotation-gap. The epoch and per-shard high-water seq are persisted
   at `<dir>/watch.epoch` on a clean `Close` (consumed by `Open`), so archive resumes
   survive a clean reopen incrementally and Reset after a crash. (Also fixed a `Rotate` bug it surfaced: an index-less
   archive failed to drop segments because it tried to unlink a sidecar that was never
   written.)

Deferred:

5. **Collection catch-up efficiency** — currently a full `seq`-filtered scan of every
   segment (correct, O(records)). Per-segment `[minSeq,maxSeq]` to skip old segments
   is a follow-up.
6. **Mutable Collection epoch persistence** — a mutable collection's epoch is
   per-process, so **a reopened persistent table gets a new epoch and forces watchers
   to full-replay** (safe, but not incremental across a server restart). Persisting
   the delete journal under `Dir` as well, and reusing the archive's clean-shutdown
   marker, is a follow-up. Append-only collections already resume across a clean
   reopen.
