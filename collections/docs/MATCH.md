# Match, rank, order: negotiator/schedd operations at the collection layer

> Status: **all four operations implemented.** `Match` / `MatchSorted` (`match.go`,
> `match_index.go`) are **parallel** across segments with a **wire-native job-side
> reject** (skip the full decode of slots the job definitely rejects) and an **index
> candidate pre-filter** (visit only candidate slots when the job is selective and slot
> attributes are indexed). On a 40k-slot pool with half rejected, 12-core: 183ms serial
> → 57.6ms with the reject (3.2×) → 19.1ms at 8 workers (9.6× vs the original serial
> baseline); with the index pre-filter, a ~2.5%-selective job goes 5.78ms → 1.09ms
> (5.3× over the wire-reject full scan). All sound (candidates are re-verified by the
> full match). The maintained **ordered index (#3)** and **cluster-signature projection
> (#4)** are implemented too. This doc maps the four HTCondor operations onto the
> collection engine:
> three are one coherent capability — a **ranked/ordered scan**, with symmetric
> **match** as its richest filter — plus a **maintained filtered ordered index** for
> the schedd's priority-queue pattern. The fourth (RRL windowing) stays in the app.

## The unifying shape

All four cases are variations of: **scan → (filter) → compute a per-ad key →
order / aggregate**. The collection already owns the hard parts — parallel
cross-segment scan (`parallel_scan.go`), wire-native evaluation, value/categorical
indexes, MVCC snapshots, a stable insertion order, and parent/child chaining for the
job queue. The one missing engine primitive is a `TARGET` ad in wire evaluation.

| # | Operation | Where | Rationale |
|---|-----------|-------|-----------|
| 1 | Symmetric **match** | **Collection** | Parallel bilateral-requirements scan + indexed candidate pre-filter + snapshot; reuses `classad.MatchClassAd` |
| 2 | **Rank-sort** the matches | **Collection** (fused into #1) | Per-worker top-K heaps + merge, in the same parallel pass |
| 3 | Schedd queue **ordering** | **Collection** (maintained filtered index) + app policy | Sort key is stable, but membership (idle jobs) churns; maintain incrementally instead of re-sorting each cycle |
| 4 | **RRL windowing / clustering** | **App** | A serial run-length fold over the ordered stream; order-dependent, not parallelizable |

## 1 — Match (symmetric)

`Match(job)` scans the (slot) collection and returns the symmetric matches — where
`job.Requirements` holds with the slot as `TARGET` **and** `slot.Requirements` holds
with the job as `TARGET`. This is where the collection beats app-side matching: it
avoids materializing non-matches and parallelizes.

- **`job.Requirements` / `job.Rank` compile once** and evaluate per slot with the
  scanned slot bound as `TARGET`, resolvable **straight from the slot's wire bytes**
  (the wire-native fast path, parallel across segments). This is the primary win.
- **`slot.Requirements`** (the slot's own expression) needs the slot's Requirements +
  its transitive refs decoded, evaluated with the fixed `job` as `TARGET` — a partial
  decode, only for candidates that already pass the job side.
- **Index pre-filter**: when `job.Requirements` references indexed slot attributes
  (e.g. `TARGET.Memory >= 8192`), visit only candidate slots instead of the whole
  pool — exactly what the negotiator wants.

`classad.MatchClassAd` already provides the two-ad primitive (`Symmetry`, `Match`,
`EvaluateRankLeft`). The collection supplies the scan/index/parallel/snapshot around
it. A useful `classad`-layer addition is a *compiled matcher* for a fixed job
(compile `Requirements`/`Rank` once, evaluate against many targets) so the per-slot
cost is just resolution, not recompilation.

### The one new engine piece: a `TARGET` ad in wire eval

Today `wireScope.resolve` returns **undefined** for `TARGET` ("a collection ad has no
match target"). Match needs the scope to carry a fixed target ad so `TARGET.*`
resolves to the job (and, on the slot side, to the slot). This is the enabling change;
everything else composes existing machinery.

## 2 — Rank-sorted matches

The negotiator wants the matches *ranked*, usually a best/prefix, not an unordered
set. Fold the rank into the match scan: each parallel worker keeps a **bounded top-K
heap** keyed by `job.Rank`, merged across workers at the end (the parallel-scan +
merge path from `Query`). So `MatchSorted(job, limit)` does match + rank + top-K in
one parallel pass — no materialize-then-sort. `limit == 0` gives a full sorted merge.
(HTCondor's fuller policy — `NEGOTIATOR_PRE_JOB_RANK`, the slot's own `Rank` for
preemption — layers as additional sort keys on the same mechanism.)

## 3 — Schedd ordering: a maintained *filtered* ordered index

> **Implemented** (`ordered.go`, `Options.Ordered` / `Collection.Ordered`). Configure
> one or more `OrderSpec{Partition, Where, Keys}`; the index is maintained on
> `Put`/`Update`/`Delete` and read via `Ordered(i, partition, resume)`, which yields
> each member ad plus a resume cursor over an O(1) snapshot. Backed by a copy-on-write
> B-tree (`github.com/tidwall/btree`). Rebuilt from the recovered ads on persistent
> `Open`, and evaluated over the inherited (chained) view — see the resolved notes below.

The schedd walks its queue in a fixed order (JobPrio, pre/post prio, qdate, order
entered the queue) to find the next job to run, per user. Re-sorting the queue every
negotiation cycle throws away work, because the ordering key is **stable across a
job's lifetime**. The right model is a **maintained ordered index**: a persistent,
sorted secondary index the collection keeps up to date on the write path, so the
schedd iterates it directly in priority order — a priority queue that is always ready.

**Why B-tree-like, not a heap.** A heap gives you the top element cheaply but not
*resumable ordered iteration*, and popping is destructive — the schedd wants to walk
*down* the priority list (top RRL, then the next, …) non-destructively, cycle after
cycle, and resume where the negotiator ran out of slots. That is ordered iteration +
seek, which a **B-tree / skip-list / ordered index** provides and a heap does not.

```go
collections.New(collections.Options{
    OrderedBy: &collections.OrderSpec{
        Partition: "Owner",                         // one ordered run per user
        Where:     "JobStatus == 1",                // membership predicate: idle jobs only
        Keys: []collections.SortKey{                // the comparator, high priority first
            {Expr: "JobPrio", Desc: true},
            {Expr: "QDate", Desc: false},
            // ... implicit final tiebreaker: insertion order (see below)
        },
    },
})

// Iterate a partition in priority order, resumable across negotiation cycles.
func (c *Collection) Ordered(partition string, resume []byte) (iter.Seq[*classad.ClassAd], error)
```

- **It is a *filtered* (partial) index.** The negotiator only cares about **idle**
  jobs, so the index carries a membership predicate (`Where`) and holds only ads that
  satisfy it. That makes *state transitions* the real churn: a job entering idle is an
  insert, a job leaving idle (matched → running, evicted → back to idle, held → …) is a
  delete. The sort key is stable, but membership is not — so there are more
  insertions/deletions than "add on submit, remove on completion" would suggest.
- **Maintenance cost, honestly.** On every `Put` the collection re-evaluates
  membership and the order key against the prior version and does the minimum:
  - attribute churn that changes neither membership nor key (the bulk of SetAttribute
    traffic) → **no-op**;
  - a state transition into/out of idle → **one O(log n) insert or delete**;
  - a key change (rare JobPrio edit while idle) → **one O(log n) re-position**.

  So it is *not* "nearly free" — but each event is O(log n) on a single entry, versus
  **O(n log n) over the whole idle set every negotiation cycle** for a re-sort. For a
  large idle queue that trades a per-cycle full sort for per-transition log-time
  updates, which still wins comfortably; the crossover only favors re-sorting if state
  transitions vastly outpace read cycles (they don't — transitions are bounded by the
  slots started/stopped per cycle, while `n` idle jobs can be huge).
- **"Order entered the queue" is the collection's** — the stable insertion sequence
  (a record's first-insert seq). Expose it as the implicit final tiebreaker so the app
  never has to store or reconstruct it.
- **Snapshot reads under churn** *(resolved).* Negotiation iterates a consistent view
  while writes continue. The open question was mutable-with-snapshots vs. copy-on-write;
  it is settled pragmatically by `tidwall/btree`, whose `Copy()` is an O(1) COW clone
  that shares nodes and path-copies only on the *next* write while a snapshot is live.
  Writers mutate one master under a mutex; each read takes a `Copy()` and iterates it
  lock-free. Path-copy cost is incurred only for writes during an active snapshot
  iteration (short, relative to churn), so the allocation concern is bounded. If a
  benchmark ever shows it dominating, the master could instead publish a versioned root
  per read — but the COW path is correct, hardened, and simple.
- **Resumable by key, not position** *(implemented).* The cursor yielded alongside each
  ad encodes that entry's order position (partition, keys, stable seq); passing it back
  resumes strictly after it — "from priority X downward" — even though jobs entered/left
  idle in between. No re-seek from the top, no lost place.
- **Recovery / persistence** *(resolved).* Like the value indexes, the ordered index
  is *derived* state, rebuilt by scanning the recovered ads (re-evaluating `Where`) on
  persistent `Open` — `rebuildOrdered`, run right after `Reindex`. Not persisted itself.
- **Chained keys** *(resolved).* `maintainOrdered` evaluates over the same inherited
  view Scan/Query use: a structural (parent-only) ad is never a member, and a child's
  `Where`/`Partition`/`Keys` resolve parent attributes via the parent chain (attached
  with `SetParent`, O(1), and detached before return, so the caller's ad is untouched).
  Two edges remain by design: the parent must be present when a child is written or
  recovered (submit order / recovery both satisfy this), and a *parent* attribute change
  does not re-evaluate already-indexed children — fine for static parent attributes like
  `Owner`, which is the intended partition key. Unlike `mergeParent`, the parent walk
  does not honor `ParentPrivateAttrs`; ordering expressions are not expected to
  reference parent-private attributes.

This is the largest new piece (a concurrent, snapshot-able, *filtered* ordered
structure with write-path maintenance), but it removes the "re-sort every time I talk
to the negotiator" cost and matches the priority-queue mental model.

### Consistency and MVCC (why not a B-tree per SSTable)

The ordered index is deliberately **not** an MVCC/per-segment structure. It is a single
in-memory secondary index — one copy-on-write B-tree per `OrderSpec`, holding **one
entry per key** (current state), maintained imperatively — in contrast to the
value/categorical index, which is **per-segment** (`seg.idx`) with visibility resolved
at read time by the `seq`/`supersededBySeq` rule.

- **Why not per-SSTable.** Per-run indexes are *forced* in an LSM because on-disk
  SSTables are immutable; you index each run and merge at read. This index is in-memory,
  derived, and rebuilt on `Open` (never persisted), so it is free of that constraint —
  it is a mutable RDBMS-style secondary index, not an LSM per-run index. Per-segment
  would also be *worse* here: the negotiator wants a **global** ordered walk, so
  per-segment trees would force a k-way merge + cross-segment version resolution on
  every cycle (O(n log M) per walk) — re-introducing exactly the per-cycle merge cost
  this feature removes. The single tree gives an O(1) snapshot + O(k) walk, and is
  **decoupled from segment lifecycle** (compaction never rebuilds it, unlike the value
  index).
- **Read consistency.** `Ordered` snapshots the tree (COW `Copy`) and then fetches each
  ad live by key. The *order* is from the snapshot instant; each *ad* is its own
  point-in-time read. This is not a single global snapshot — but the store never offers
  one either (`Scan` concatenates independent per-shard snapshots), so it matches the
  store's consistency level. A member deleted since the snapshot is skipped; a repriced
  one shows new content at its old slot until the next cycle.
- **Write consistency.** Maintenance runs *after* commit, not `seq`-ordered. Under
  **serialized per-key writes** — the schedd's model (its job queue is a single
  serialized log) — the index is exactly consistent. Under genuinely concurrent writes
  to the *same* key from different goroutines, the tree can converge to a stale version
  until that key's next write (the store keeps the authoritative version by max-`seq`;
  the index self-heals). Full correctness there would need either per-segment indexes
  (the expensive-read path above) or version tombstones in the index (unbounded state
  for a filtered index whose non-members dominate) — neither warranted for the
  single-writer-per-key target.
- **Recovery** is the one place it rides MVCC: `rebuildOrdered` re-derives the tree from
  each shard's committed snapshot (`forEachVisibleKeyed(s0, …)`).

**Concurrency control.** Two locks that never contend for long: `oi.mu` guards master
mutation (`upsert`/`remove`) and the `Copy()` in `snapshot()`; and each B-tree carries
its own `sync.RWMutex`. `Copy()` is O(1) (mint new `isoid`s, share the root), so a
writer serializes with a snapshot for microseconds, not the walk's duration. The walk
holds only the *snapshot's* `RLock` (a private tree, uncontended); writers take the
*master's* `Lock` — different mutexes, no cross-blocking. Copy-on-write makes shared
nodes read-only after the copy (both trees `isoid`-mismatch and copy-before-write), so
a reader traversing a node while the master path-copies it is race-free (verified under
`-race`, `TestOrderedIndexConcurrent`).

**A slow one-at-a-time walk (negotiator feeding RRLs over seconds).** Nothing blocks:
`Ordered` snapshots once and releases `oi.mu`, so writers keep committing throughout.
The snapshot freezes *order and membership* at the call, while each ad is fetched live
per yield — so a job that leaves idle mid-walk is still visited (its live ad reflects
the new state; the negotiator re-verifies), a deleted one is skipped (`Get` miss), a
repriced one appears at its old slot with current content, and a newly-idle one is not
seen until a later snapshot. A single long-held walk also pins the T0 node-set, so
heavy churn accrues extra memory until it ends. For long feeds, **chunk with the resume
cursor**: each call re-snapshots in O(1) and resumes after the cursor, bounding pinned
memory and picking up members added below the cursor between chunks (a job repriced
across chunks may be re-yielded or skipped — harmless for a re-verifying negotiator).
See `TestOrderedStreamingWalk`.

## 4 — RRL windowing / clustering: keep it in the app

Building resource-request lists is "run-length-encode the *ordered* stream by the
clustering signature": start a new RRL when the signature changes from the previous
job (your A,A,B,B → 2 RRLs vs A,B,A,B → 2000 example is exactly RLE over sort order).
It is inherently **serial and order-dependent**, so the collection's parallelism has
nothing to add. It belongs in the schedd, consuming the `Ordered` iteration from #3.

The **one** contribution the collection makes *(implemented)*: compute each ad's
**cluster signature** (a 64-bit FNV-1a hash of the `OrderSpec.Cluster` attributes'
values) and surface it as `OrderedAd.Signature`, so the schedd's RLE fold is a
stored-`uint64` compare rather than re-hashing attributes. It is computed once on the
write path (alongside membership and the sort key) and stored on the index entry — a
clustering-attribute change that does not move the sort position refreshes the
signature in place. The grouping-into-runs stays app-side: fold the `Ordered` stream,
starting a new RRL whenever `Signature` changes (see `rleRuns` in the tests). A 64-bit
collision could merge two adjacent runs; over the value bytes that is negligible.

## Recommended build order

1. **`Match` / `MatchSorted(job, limit)` (A1)** — parallel bilateral scan + top-K by
   rank; reuses `MatchClassAd` and the parallel-scan/merge code. **Done.**
2. **Wire-native job-side reject (A3)** — compile `job.Requirements` once (`vm.Compile`)
   and evaluate it against each slot's wire, skipping the full decode of definite
   rejects; falls through to the full match on any uncertainty. **Done.**
3. **Index candidate pre-filter (A2)** — **Done** (`match_index.go`); details below.
4. **Filtered ordered index (`Options.Ordered` / `Collection.Ordered`)** — the
   maintained priority index for the schedd (§3 above). **Done** (COW B-tree, write-path
   maintenance, snapshot+resume reads, recovery rebuild, and chained-view evaluation).
5. **Cluster-signature projection** — **Done** (`OrderSpec.Cluster` →
   `OrderedAd.Signature`). A small write-time helper feeding the app's RRL
   fold. Leave the windowing itself in the schedd.

### A2 — index candidate pre-filter (implemented)

Instead of visiting every slot and wire-rejecting the misses one by one (A3), the
value/categorical index visits only *candidate* slots. The mechanism reuses the
existing indexed path: `rewriteForSlot` turns `job.Requirements` into a predicate over
the slot — `TARGET.attr → attr` (the slot's own), and every job reference → its
evaluated constant (or the undefined literal, poisoning its conjunct). `vm.Compile(…)
.Probes()` extracts the index-satisfiable conjuncts, `planIndex` maps them to the
configured indexes, and `scanShardCandidates` (factored out of `scanShardIndexed`)
visits only candidate records; each is bilaterally matched with the full
`MatchClassAd` (plus the A3 reject). `Match` takes this path when the job yields any
usable probe; otherwise it falls back to the A1+A3 full scan. It fans out across
shards on the same worker budget.

**Soundness — superset by construction.** The rewrite can only ever emit probes on the
slot's *own* attributes against job *constants*: a `TARGET`-dependent or non-scalar job
reference evaluates to undefined and its conjunct is dropped (not baked as a wrong
value); every opaque node (`ifThenElse`, list, ternary, select, …) collapses to the
undefined literal, so no `MY` reference can leak through as a bare self-scoped
reference the extractor would misread as a slot attribute. Top-level `OR`/`NOT` yield
no probe and fall back. Dropping or broadening a probe only widens the candidate set,
and `MatchClassAd` re-verifies every candidate — so a wrong probe costs selectivity,
never correctness. Tested: indexed `Match` == full-scan `Match` (and top-K order) over
a large pool, plus the partial-probe and `TARGET`-dependent-constant paths.

**Payoff.** Bounded by A3 (which already skips the expensive decode on rejects) — the
extra win is skipping the wire eval + lookup on non-candidates, real only when the pool
is large, jobs are selective, *and* the slot attributes are indexed. Measured on a
40k-slot pool with a ~2.5%-selective job: full scan (A3) 5.78ms → indexed (A2) 1.09ms,
**5.3×**. When indexes are unconfigured or the job has no usable probe, there is no
change from A1+A3. (The value index is reindex-on-demand: A2 helps once `Reindex` has
run; an unbuilt index just full-scans the same snapshot.)

## Caveats / open questions

- **Bilateral wire-native eval** only fully applies to the *job* side (fixed,
  compiled, `TARGET` resolved from the scanned slot's wire). The *slot* side
  (arbitrary per-slot `Requirements`) needs a partial decode; short-circuit by
  evaluating the more selective / cheaper side first, and pre-filter with indexes.
- **Ordered-index structure** under the idle-set churn: mutable B-tree/skip-list with
  reader snapshots vs. persistent copy-on-write — decide by benchmarking the real
  insert/delete/read mix (see #3).
- **Multiple orderings.** The schedd has essentially one ordering; each additional
  maintained index carries its own write-path cost — keep the set small and explicit.
