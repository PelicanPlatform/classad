# Parent/child (chained) ads

A collection can give an ad a **parent**: a child ad resolves attribute
references it lacks by falling through to its parent. This is the primitive
"join" the HTCondor job queue needs — a proc ad (`cluster.proc`) chains to its
cluster ad (`cluster.-1`), so a query like `DAGManJobId == 42` sees the cluster's
attribute even though it is stored only once, on the cluster ad.

Enable it with two `Options`:

- `ParentKeyFor func(key []byte) []byte` — maps a child key to its parent key
  (`nil` for a top-level ad). Bindings are **immutable**: a key's parent must not
  change across updates (HTCondor forbids re-parenting a proc).
- `IsStructural func(key []byte) bool` — marks a parent-only ad (a cluster ad).
  Structural ads are stored and used as parents but **hidden** from
  Query/Scan/Watch results by default, like `condor_q` omitting cluster ads.

Example (jobs):

```go
c := collections.New(collections.Options{
    ParentKeyFor: func(k []byte) []byte { /* "N.M" -> "N.-1", "N.-1" -> nil */ },
    IsStructural: func(k []byte) bool  { /* proc == -1 */ },
})
```

## Co-location

A whole **family** (a root and all its descendants) is routed to **one shard**:
the shard is chosen by the family root (`hash(rootKey(key))`), while the key's own
hash still keys the bucket chain within the shard. Because a parent and its
children share a shard — and thus a single MVCC snapshot — chained evaluation is
**consistent and lock-free**, with no cross-shard fetch (the engine's hardest
problem, dissolved). The plain single-key routing is unchanged when chaining is
off.

## Evaluation vs. output

Two different needs, handled two different ways:

- **Filtering** (does this ad match the query?) chains at the *wire* level: the
  wire-native resolver, on a miss, falls through to the co-located parent's wire
  bytes — so the fast path stays fast and never builds a `ClassAd` for a
  non-match. The partial- and full-decode fallbacks `SetParent` the decoded ad,
  which the ClassAd evaluator walks for unscoped references.
- **Output** (the ad handed to the consumer) is **flattened**: the parent's
  attributes are materialized into the child (child overrides), so direct reads
  (`EvaluateAttr*`, serialization) see the inherited attributes. `SetParent` alone
  only chains *expression evaluation*, not direct lookups — a query result or a
  watch event must carry the inherited attributes inline. This matches `condor_q`
  showing a job's full ad and `condor_history` flattening it.

The scan is two-pass over one snapshot: pass 1 collects the shard's structural
(parent) ads; pass 2 evaluates each child with its parent chained, hides
structural ads, and flattens matches.

## Semantics (mirroring HTCondor)

- **No re-parenting** — a key's parent is fixed.
- **Auto-delete of an empty parent** — a structural parent is removed when its
  last child leaves (HTCondor `ClusterCleanup`); direct parent delete is not a
  normal operation. *(ref-counted lifecycle — Phase 2)*
- **Archive flattens** — the append-only archive stores materialized standalone
  ads (no parent link), like `condor_history`. *(Phase 4)*
- **Watch fan-out on parent change** — a change to an *inherited* parent attribute
  re-emits the affected children; a **parent-private** attribute set (e.g. job
  factory bookkeeping like `JobMaterialize*`, which children don't inherit) is
  excluded, and a diff suppresses no-op fan-out. *(Phase 3)*

## Current limitations

- Chained collections use a serial two-pass scan; secondary indexes and query
  fan-out are bypassed (they don't yet understand chaining).
- A parent must be `IsStructural` (the two-pass scan collects structural ads as
  the parent pool). For the job model this is exactly the cluster ad.
