# Parallel query execution (across segments)

> Status: **implemented** in `parallel_scan.go`. A full-scan `Query` can fan out
> across the collection's segments using a bounded worker pool, gated by a work-size
> threshold and a machine-wide worker budget. `Options.QueryParallelism = 0` (the
> default) means **auto** — the library picks the policy (currently: up to 6 workers
> per query, clamped to GOMAXPROCS); `1` forces serial; `N ≥ 2` caps workers per
> query. Serial and parallel produce identical result sets. Indexed queries and
> `QueryRaw` remain serial for now (see Limitations).

## Why it is safe (and cheap)

A query is a scan of immutable segments, and three existing properties remove the
usual hard parts of parallelizing it:

1. **Segments are immutable once written; reads are lock-free.** A scan reads frozen
   `segWindow`s captured by `snapshot()` (which also pins mmap segments). Workers
   share no mutable data, so there is no lock contention on the hot path.
2. **Exactly-once needs no cross-segment coordination.** The version of a key visible
   at the snapshot sequence `S0` lives in *exactly one* segment (the MVCC rule
   `seq ≤ S0 < supersededBySeq`: older versions are superseded, newer ones invisible).
   So workers process disjoint segments and their outputs simply concatenate — no
   dedup, no shared "seen" set. This is the key enabler.
3. **Results are order-insensitive.** No merge or sort; workers emit in any order.

## Design

**Unit of work = one segment window**, tagged with its shard's `S0` (the snapshot
sequence is per shard). `gatherTasks` snapshots every shard once, flattens all
windows into a `[]scanTask`, and returns a `release` that unpins them when the query
finishes. Pins are held for the whole query — they only defer segment reclamation,
never block writers.

- **Task distribution: a shared atomic index, not a channel.** Each worker claims the
  next segment with `atomic.AddInt64(&next,1)`. At segment granularity a task is
  milliseconds of scan, so the atomic is free, and cancellation is trivial (stop when
  the index passes the end or the stop flag is set).
- **Workers match *and* decode.** Each worker runs the full per-record path —
  decompress (into its own reused buffer), `matchWire`, and for matches
  `decodeWire` + `FromAST` — producing **owned** `*classad.ClassAd`s. This
  parallelizes the expensive half (decoding matches), leaving only `yield` serial.
  `decodeWire` is concurrency-safe (the intern table's `Name` is lock-free).
- **Per-worker state.** The plan/program (`ReadPlan`, `Native`, probes, segment
  indexes) is immutable and shared; only the execution state is per-worker: a fresh
  `Matcher` (`q.Matcher()`), a `wireScope` + bound `resolver`, and scratch buffers.
- **Results: one bounded channel, sub-batched.** A worker flushes a batch of decoded
  ads (capped, so a low-selectivity segment doesn't buffer unboundedly) to a channel
  sized to the worker count. The consumer goroutine (the `iter.Seq` range) drains it
  and calls `yield`. One send per batch keeps the channel — the only real
  coordination cost — cheap.
- **Early stop** (`yield` returns false, or the consumer breaks): set an atomic stop
  flag; the consumer keeps draining the channel (without yielding) until it closes, so
  a worker blocked on a send always makes progress, all workers exit, and only then is
  `release` (unpin) called — never a use-after-unmap.

## When it engages

Fan-out is **gated**, because for cheap queries on small collections the fixed cost
(snapshotting all shards, spinning up workers, channel setup) exceeds the scan:

- `Options.QueryParallelism` ≠ 1 (0 ⇒ auto, N ≥ 2 ⇒ explicit cap; 1 ⇒ always serial).
- The scan must be large enough: total live segment bytes ≥ a threshold
  (`parallelMinBytes`, benchmark-tuned).
- There must be at least two segments to split.

**Machine-wide worker budget.** Intra-query parallelism helps the *latency* of one
query and helps throughput only when query concurrency is *low* — a store already
fielding many concurrent queries saturates its cores without it, and the coordination
would then be pure loss. So a Collection-wide semaphore (size `GOMAXPROCS`) bounds
total scan workers across all in-flight queries: each query takes whatever tokens are
free with a **non-blocking greedy** try — it never queues or blocks on a worker slot,
and if it cannot get at least two it runs **serial** immediately. Net effect: as
concurrency rises the budget is spoken for and queries fall back to serial rather than
oversubscribing. The auto per-query cap (6) is deliberately below the budget so that
*several* concurrent queries can each fan out — fairness — instead of one query taking
the whole machine and starving the rest to serial.

## Contention: what oversubscription actually does

`BenchmarkQueryContention` runs `GOMAXPROCS` concurrent queries (the saturation
regime) and varies workers-per-query, with the budget on (real) and off (each query
gets its full worker count). On a 12-core box, decode-heavy query, aggregate
throughput:

| workers/query | budget on | budget off |
|---------------|----------:|-----------:|
| 1 (serial)    | **53 q/s** (baseline) | — |
| 2             | 46 q/s | 41 q/s |
| 4             | 49 q/s | 45 q/s |
| 8             | 47 q/s | 42 q/s |

Reading it: when the queries alone already fill every core, fan-out **cannot** raise
throughput — 12 serial queries is the ceiling (53 q/s). Adding workers **degrades
gracefully** to ~45–49 q/s (≈10–15% off) — it never collapses, even with the budget
off and up to `12 × 8 = 96` worker goroutines contending (Go's scheduler is
work-conserving). The budget is worth keeping: `budget on` beats `budget off` at every
level, i.e. it measurably softens the oversubscription penalty.

The practical consequence for `auto`: fan-out is a large **latency** win at low
concurrency (up to 8× above) and a small **throughput** cost at saturation. The budget
keeps that cost bounded and graceful; a future auto policy could drive it to zero by
also declining to fan out when it observes many queries already in flight (the
semantics of `auto` are deliberately free to change — see the Status note).

## Correctness

Exactly-once holds by construction (§2): each key's visible version is in one
segment, workers own disjoint segments, `S0` is fixed per shard at snapshot, and the
per-record MVCC check is identical to the serial path. Concurrent writes after `S0`
are invisible; concurrent compaction is handled by the pins plus the existing
superseded-stamp transfer, exactly as in a serial scan. `TestParallelQueryMatchesSerial`
asserts the parallel result *set* equals the serial one across sizes and
selectivities, and the concurrency stress runs under `-race`.

## Limitations / follow-ups

- **Indexed queries** (`planIndex` hit) and **`QueryRaw`** stay serial. Indexed scans
  already visit far fewer records (less to gain), and `RawAd` aliases reused buffers,
  so parallelizing it needs owned copies. Both are straightforward extensions of the
  same task/worker structure.
- **`Scan`** (unfiltered) could reuse this path (it is `Query` with an always-true
  predicate); left serial until the benchmarks justify it.
- The threshold and cap are constants today; they could become adaptive (e.g. scale
  workers to measured scan size, or track live core occupancy).

## Benchmarks

Parallelism is justified only by numbers, so `parallel_query_bench_test.go` measures
**single-query latency** (distinct from `BenchmarkMatrix`, which measures
concurrent-query *throughput* — the opposite regime) across the factors that decide
whether fan-out wins:

- **collection size** (total segment bytes) — more work, more benefit;
- **segment count** (`SegmentSize`) — granularity; a few huge segments cap fan-out;
- **selectivity** — high (match-only) vs low (decode-heavy; parallel decode helps most);
- **query cost** — cheap literal reject (wire-native) vs expensive expression.

The same benchmark runs at `QueryParallelism` = 1, 2, 4, 8 so the speedup and the
serial/parallel crossover are read straight off the table.

### Results (12-core dev box, real-ad corpus, `ns/query`)

| scan | selectivity | par=1 | par=2 | par=4 | par=8 | speedup@8 |
|------|-------------|------:|------:|------:|------:|----------:|
| 40k ads | low (decode-heavy) | 7038ms | 3103ms | 1465ms | 855ms | **8.2×** |
| 40k ads | high (match-only)  | 178ms  | 88ms   | 48ms   | 29ms  | **6.1×** |
| 2k ads  | low                | 243ms  | 144ms  | 81ms   | 56ms  | 4.3× |
| 2k ads  | high               | 9.4ms  | 4.7ms  | 2.6ms  | 1.6ms | 5.9× |
| 200 ads | high (forced)      | 0.91ms | 0.56ms | 0.52ms | 0.37ms | 2.5× |

Near-linear on large scans; low-selectivity (parallel *decode*) scales best. The 200-ad
high-selectivity row shows returns flattening (par4 ≈ par2) as fan-out overhead starts
to matter — the regime the `parallelMinBytes` gate keeps on the serial path.
