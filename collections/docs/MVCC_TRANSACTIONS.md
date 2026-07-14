# Multi-writer MVCC transactions (optimistic concurrency control)

## Goal

Let the collection back multiple concurrent writers (the future multi-writer job
queue / general DB) instead of only the schedd's single-writer model, without
locks held across a transaction. Writers run optimistically against a snapshot and
detect conflicts at commit.

This is the concurrency-control layer only. It is *not* the RPC/C-library DB
surface (`condor_qmgr.h` replacement) — that is a separate module. This layer gives
that module the transactional primitive it needs.

## Isolation: snapshot isolation, write-write conflicts only

We adopt **snapshot isolation (SI)**:

- A transaction reads a consistent snapshot as of its begin sequence `S0`.
- On commit, a write to key `K` succeeds unless `K` was modified by another
  committer after `S0` (a **write-write conflict**). Reads are *not* tracked — we
  do not detect read-write conflicts, so table scans / constraint queries impose no
  bookkeeping. This is the explicitly-accepted relaxation ("I'm OK with snapshot
  isolation").

SI is sound for the observed HTCondor workload: transactions treat each ad
independently — a read of ad A never feeds a write to ad B (no `B.x = A.y`); the
common shapes are read-modify-write on one ad (increment a counter, set JobStatus)
and blind sets. Cross-ad invariants that would need serializability do not occur.

## Per-ad independent commit (not all-or-nothing)

Because ads are independent, a large transaction (a constraint query selecting many
ads, then editing each) is treated as a **batch of independent single-ad
transactions**. Commit applies each write on its own:

- writes whose key is unchanged since `S0` commit;
- writes whose key was modified since `S0` are reported as conflicts;
- there is **no rollback of the successful writes** — the caller gets the set of
  conflicted keys and retries just those (re-read, re-apply, re-commit).

This is opt-in per transaction (`CommitIndependent`), matching how the schedd
actually uses large transactions. A strict all-or-nothing mode
(`Commit`, any conflict aborts the whole batch) is also offered for callers that
want it.

## How it maps onto the existing store

The store is already MVCC:

- Each record carries `seq` (the commit sequence at which it became current) and
  `supersededBySeq` (the sequence at which it was overwritten/deleted; `seqMax`
  while current). `commitSeq` is **per shard**; a key lives in exactly one shard.
- `findCurrent(head, key)` returns the live record; `recSeq` is its version.
- Writes apply under the shard write lock (`applyWrites` → `sh.put`).

So the OCC check is local and cheap:

> **`conflictSince(key, S0)`**: walk the key's bucket chain (superseded versions are
> retained until compaction) and report a conflict if any record for the key has
> `recSeq > S0` (a version written after the snapshot: update or insert) **or** was
> superseded at a sequence in `(S0, ∞)` (the snapshot-era version was updated or
> deleted after `S0`). Delete leaves no new record, so the supersede clause is what
> catches a concurrent delete.

The commit does `conflictSince` then `put`/`del` for each write, all under the one
`sh.mu.Lock()` — so the check and the apply are atomic with respect to other
committers, and first-committer-wins falls out naturally.

### Single-writer fast path

All of a shard's buffered writes share one snapshot `S0[shard]`. At commit, under
the shard lock, if `sh.commitSeq == S0[shard]` — nobody committed to this shard
since the snapshot — then **no key can have changed**, so every write is applied
with **no per-key conflict check at all**. This is the schedd's current
single-writer model: transactions pay zero conflict-detection cost when they are in
fact the only writer, and degrade to the per-key `conflictSince` check only once a
concurrent committer has advanced the shard's sequence. (`ConflictChecks()` exposes
the cumulative per-key checks, so the fast path is observable/testable — it stays
flat under a single writer.)

## API sketch

```go
tx := c.Begin()                 // captures S0 lazily, per shard first touched
ad, ok := tx.Get(key)           // reads at S0 (and the txn's own buffered writes)
tx.Put(key, ad)                 // buffer an insert/update
tx.Delete(key)                  // buffer a delete
res := tx.Commit()              // apply; res.Conflicts lists the keys that lost
// res.Committed, res.Conflicts [][]byte
```

- `Get`/`Query`/`Scan` inside a txn use the txn's `S0` for MVCC visibility (the
  existing `forEachVisible(s0, …)` path), plus read-your-own-writes from the buffer.
- Writes are buffered (nothing mutates the store until `Commit`), grouped by shard,
  and applied with `conflictSince(key, S0[shard])` as the base test.
- `Put` (the existing non-transactional API) is unchanged: unconditional
  last-write-wins. Transactions are the new opt-in OCC path.

## Snapshot capture

`S0` is captured **lazily per shard** on first touch (read `commitSeq` under the
shard read lock), not eagerly across all shards — a txn that touches three keys
snapshots three shards, not sixteen. Per-shard snapshots are independent and sound
because ads are independent; there is no cross-shard consistency requirement under
this workload.

## Known limitations / follow-ups

- **Long transactions vs compaction.** `conflictSince` relies on superseded
  versions still being in the chain. Compaction reclaims versions below the minimum
  active snapshot; a transaction must therefore register `S0` as an active snapshot
  (as scans do) so its conflict evidence is not GC'd. v1 keeps transactions short
  and documents this; registering txn snapshots with the compactor is the immediate
  follow-up for long-running transactions.
- **Cross-shard atomicity.** Independent per-ad commit needs none. A future
  serializable mode spanning shards would need a global sequence or 2-phase commit;
  out of scope.
- **Group-commit coalescing.** v1 commits a transaction under its own `sh.mu`
  apply (serialized with `Put`), bypassing the `Put` group-commit coalescer. Since
  the durability sync is currently a no-op, this is a wash; coalescing txn commits
  is a later optimization.

## Phasing

1. `conflictSince` + shard OCC apply (`commitTxn`) — the core primitive.
2. `Txn` type: `Begin`/`Get`/`Put`/`Delete`/`Commit` with buffered writes,
   lazy per-shard snapshot, read-your-writes.
3. `Scan`/`Query` at the txn snapshot.
4. Tests: concurrent writers, write-write conflict detection, per-ad partial
   commit, blind-write and read-modify-write conflict, SI read stability.
