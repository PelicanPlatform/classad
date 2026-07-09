# Match, rank, order: negotiator/schedd operations at the collection layer

> Status: **in progress.** `Match` / `MatchSorted` are implemented in `match.go`,
> **parallel** across segments with a **wire-native job-side reject** (skip the full
> decode of slots the job definitely rejects). On a 40k-slot pool with half rejected,
> 12-core: 183ms serial → 57.6ms with the reject (3.2×) → 19.1ms at 8 workers (9.6× vs
> the original serial baseline); sound (any undefined/non-literal/non-native case falls
> through to the full match). Still open: the **index candidate pre-filter (A2, below)**
> and the **ordered index (#3)** and **cluster-signature projection (#4)**. This doc
> maps the four HTCondor operations onto the collection engine:
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
- **Snapshot reads under churn.** Negotiation iterates a consistent view while writes
  continue. A fully-persistent copy-on-write B-tree path-copies O(log n) nodes per
  write — fine at low write rates, but the idle-set churn above makes that allocation
  add up, so the structure choice matters: prefer a **mutable ordered structure (B-tree
  or skip list) with reader snapshots** — e.g. writers mutate under the order lock and
  a reader either takes a short read-lock to fix its iteration frontier, or the
  structure keeps a small ring of versioned roots — over per-write full path-copying.
  This is the main design question the churn raises; a benchmark of the real
  insert/delete/read mix should pick it.
- **Resumable by key, not position.** A cursor encodes the last order key delivered;
  next cycle resumes "from priority X downward" even though jobs entered/left idle in
  between — no re-seek from the top, no lost place.
- **Recovery / persistence.** Like the value indexes, the ordered index is *derived*
  state: rebuilt by scanning the ads (and re-evaluating `Where`) on `Open`. Not
  persisted itself.
- **Chained keys.** The order key / membership may live on the proc ad or be inherited
  from the cluster ad; the collection evaluates them over the chained view, so it
  composes with `ParentKeyFor`.

This is the largest new piece (a concurrent, snapshot-able, *filtered* ordered
structure with write-path maintenance), but it removes the "re-sort every time I talk
to the negotiator" cost and matches the priority-queue mental model.

## 4 — RRL windowing / clustering: keep it in the app

Building resource-request lists is "run-length-encode the *ordered* stream by the
clustering signature": start a new RRL when the signature changes from the previous
job (your A,A,B,B → 2 RRLs vs A,B,A,B → 2000 example is exactly RLE over sort order).
It is inherently **serial and order-dependent**, so the collection's parallelism has
nothing to add. It belongs in the schedd, consuming the `Ordered` iteration from #3.

The **one** contribution the collection can make: compute each ad's **cluster
signature** (a hash of the clustering attributes) during the scan — wire-native,
parallel, cheap — and surface it alongside the ad, so the schedd's RLE fold is a
pointer compare rather than re-hashing attributes. The grouping-into-runs stays
app-side.

## Recommended build order

1. **`Match` / `MatchSorted(job, limit)` (A1)** — parallel bilateral scan + top-K by
   rank; reuses `MatchClassAd` and the parallel-scan/merge code. **Done.**
2. **Wire-native job-side reject (A3)** — compile `job.Requirements` once (`vm.Compile`)
   and evaluate it against each slot's wire, skipping the full decode of definite
   rejects; falls through to the full match on any uncertainty. **Done.**
3. **Index candidate pre-filter (A2)** — *not yet built; see below.*
4. **Filtered ordered index (`OrderedBy` / `Ordered`)** — the maintained priority index
   for the schedd (§3 above). The biggest build; settle the snapshot-under-churn
   structure with a benchmark first.
5. **Cluster-signature projection** — a small scan-time helper feeding the app's RRL
   fold. Leave the windowing itself in the schedd.

### A2 — index candidate pre-filter (potential + caveats)

Instead of visiting every slot and wire-rejecting the misses one by one (A3), use the
value/categorical index to visit only *candidate* slots. The mechanism reuses the
existing indexed-query path: rewrite `job.Requirements` into a query over the slot —
`TARGET.attr → attr` (the slot's own), `MY.attr → the job's evaluated literal` — then
`planIndex` + `scanShardIndexed` give candidate offsets; bilaterally match only those.

Worth it **only** when the pool is large, jobs are selective, *and* the queried slot
attributes are indexed. Two things bound it:

- **Soundness.** The pre-filter must be a *superset* — never reject a real match. That
  is safe only for a **conjunction** of indexable `TARGET.attr OP job-constant`
  comparisons: extract those AND-conjuncts as probes and drop the rest (dropping
  conjuncts widens the candidate set, which is sound). A top-level `OR`/`NOT` cannot be
  handled by dropping a side, and a `TARGET`-dependent constant cannot be baked in, so
  those conjuncts (or the whole predicate) fall back to the A1+A3 full scan.
- **Marginal over A3.** A3 already eliminates the expensive decode on rejects; A2
  additionally saves the *cheap* wire-native eval + wire-lookup on non-candidates. Real
  gain only for a huge pool with rare matches over indexed attributes; otherwise it
  adds machinery (constraint extraction, the rewrite) for little. Benchmark before
  committing.

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
