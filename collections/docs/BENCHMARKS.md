# Benchmark matrix

> Status: **harness in `bench_matrix_test.go`** (`BenchmarkMatrix`). Driven by
> `go test -bench`; a package-level collector renders the results as a markdown
> table to stdout, and writes markdown/CSV/JSON files when `CLASSAD_BENCH_OUT` is
> set. The table below is a generated snapshot — regenerate it on your own hardware,
> since absolute numbers are machine-specific.

The store comes in three formats built on one engine, so a single parameterized
benchmark sweeps all of them across the same workload/query/concurrency axes.

## Axes

| axis | values |
|------|--------|
| **format** | `mem` (in-memory), `disk` (mmap-persistent `Collection`), `archive` (append-only, rotated) |
| **workload** | `read-only` (0% writes), `read-heavy` (10%), `mixed` (50%), `write-heavy` (90%), `write-only` (100%) |
| **query** | `low-sel` (unindexed scan, matches most), `high-sel` (unindexed scan, ~0 matches), `indexed` (uses the value+categorical index), `single-get` (key lookup) |
| **concurrency** | `1`, `10`, `100` goroutines running the workload mix |

A **read op** runs one query (or one `Get`); a **write op** is one `Put` (mem/disk,
overwriting an existing key) or one `Append` (archive). `ops/s` is aggregate
throughput across the goroutines; `matches` is a one-shot probe of how many ads the
read query returns (a sanity/selectivity check, not timed).

Not every cell is meaningful and those are simply absent:
- **`single-get` on `archive`** — the archive is append-only with no key index (N/A).
- **`write-only`** collapses the query axis (no reads happen), so it appears once.

The store is indexed on `Arch` (categorical) + `Memory` (value); `low-sel`/`high-sel`
query the unindexed `Cpus` to force a scan, while `indexed` queries `Memory`+`Arch`.

## Running it

```sh
# Quick sweep (noisy, ~seconds): small working set, few iterations.
CLASSAD_MATRIX_N=1000 go test -run '^$' -bench BenchmarkMatrix -benchtime 50ms .

# Meaningful numbers: larger working set + longer benchtime.
CLASSAD_MATRIX_N=20000 go test -run '^$' -bench BenchmarkMatrix -benchtime 1s .

# Also write bench_matrix.{md,csv,json} to a directory.
CLASSAD_BENCH_OUT=./out go test -run '^$' -bench BenchmarkMatrix .

# A single slice (standard -bench regex over the sub-benchmark path).
go test -run '^$' -bench 'BenchmarkMatrix/fmt=disk/work=mixed' .
```

`CLASSAD_MATRIX_N` sets the working-set size (default 3000). `CLASSAD_BENCH_ADS`
points at a larger `condor_status -l` dump than the committed sample corpus.

## Snapshot

<!-- BENCH_TABLE_START -->

_Generated: `CLASSAD_MATRIX_N=5000`, `-benchtime 300ms`, 12-core linux dev container, committed 205-ad corpus tiled to 5000 keys. Absolute numbers are machine-specific; the **shape** (relative costs, concurrency scaling) is the point._

| format | workload | query | conc | ns/op | ops/s | matches |
|--------|----------|-------|-----:|------:|------:|--------:|
| mem | read-only | low-sel | 1 | 606798500 | 2 | 3464 |
| mem | read-only | low-sel | 10 | 580880042 | 2 | 3464 |
| mem | read-only | low-sel | 100 | 618611209 | 2 | 3464 |
| mem | read-only | high-sel | 1 | 8826733 | 113 | 0 |
| mem | read-only | high-sel | 10 | 1413229 | 708 | 0 |
| mem | read-only | high-sel | 100 | 1213165 | 824 | 0 |
| mem | read-only | indexed | 1 | 470266625 | 2 | 2804 |
| mem | read-only | indexed | 10 | 469571500 | 2 | 2804 |
| mem | read-only | indexed | 100 | 542406042 | 2 | 2804 |
| mem | read-only | single-get | 1 | 141513 | 7067 | 0 |
| mem | read-only | single-get | 10 | 75865 | 13181 | 0 |
| mem | read-only | single-get | 100 | 79610 | 12561 | 0 |
| mem | read-heavy | low-sel | 1 | 539973937 | 2 | 3464 |
| mem | read-heavy | low-sel | 10 | 279613561 | 4 | 3464 |
| mem | read-heavy | low-sel | 100 | 309311573 | 3 | 3464 |
| mem | read-heavy | high-sel | 1 | 8419811 | 119 | 0 |
| mem | read-heavy | high-sel | 10 | 1252029 | 799 | 0 |
| mem | read-heavy | high-sel | 100 | 1513881 | 661 | 0 |
| mem | read-heavy | indexed | 1 | 433042761 | 2 | 2804 |
| mem | read-heavy | indexed | 10 | 220307021 | 5 | 2804 |
| mem | read-heavy | indexed | 100 | 244319881 | 4 | 2804 |
| mem | read-heavy | single-get | 1 | 135064 | 7404 | 0 |
| mem | read-heavy | single-get | 10 | 75712 | 13208 | 0 |
| mem | read-heavy | single-get | 100 | 79513 | 12577 | 0 |
| mem | mixed | low-sel | 1 | 331656552 | 3 | 3464 |
| mem | mixed | low-sel | 10 | 124879965 | 8 | 3464 |
| mem | mixed | low-sel | 100 | 169326506 | 6 | 3464 |
| mem | mixed | high-sel | 1 | 4971413 | 201 | 0 |
| mem | mixed | high-sel | 10 | 889954 | 1124 | 0 |
| mem | mixed | high-sel | 100 | 610520 | 1638 | 0 |
| mem | mixed | indexed | 1 | 259505231 | 4 | 2804 |
| mem | mixed | indexed | 10 | 98830623 | 10 | 2804 |
| mem | mixed | indexed | 100 | 133926187 | 7 | 2804 |
| mem | mixed | single-get | 1 | 99290 | 10072 | 0 |
| mem | mixed | single-get | 10 | 76697 | 13038 | 0 |
| mem | mixed | single-get | 100 | 83176 | 12023 | 0 |
| mem | write-heavy | low-sel | 1 | 58771312 | 17 | 3464 |
| mem | write-heavy | low-sel | 10 | 63117540 | 16 | 3464 |
| mem | write-heavy | low-sel | 100 | 32900967 | 30 | 3464 |
| mem | write-heavy | high-sel | 1 | 822956 | 1215 | 0 |
| mem | write-heavy | high-sel | 10 | 220873 | 4527 | 0 |
| mem | write-heavy | high-sel | 100 | 218162 | 4584 | 0 |
| mem | write-heavy | indexed | 1 | 50594020 | 20 | 2804 |
| mem | write-heavy | indexed | 10 | 47679235 | 21 | 2804 |
| mem | write-heavy | indexed | 100 | 28070092 | 36 | 2804 |
| mem | write-heavy | single-get | 1 | 58039 | 17230 | 0 |
| mem | write-heavy | single-get | 10 | 80127 | 12480 | 0 |
| mem | write-heavy | single-get | 100 | 77878 | 12841 | 0 |
| mem | write-only | none | 1 | 43771 | 22846 | 0 |
| mem | write-only | none | 10 | 77682 | 12873 | 0 |
| mem | write-only | none | 100 | 76304 | 13105 | 0 |
| disk | read-only | low-sel | 1 | 712951292 | 1 | 3464 |
| disk | read-only | low-sel | 10 | 747615042 | 1 | 3464 |
| disk | read-only | low-sel | 100 | 735915292 | 1 | 3464 |
| disk | read-only | high-sel | 1 | 12375392 | 81 | 0 |
| disk | read-only | high-sel | 10 | 1894377 | 528 | 0 |
| disk | read-only | high-sel | 100 | 1640125 | 610 | 0 |
| disk | read-only | indexed | 1 | 576676084 | 2 | 2804 |
| disk | read-only | indexed | 10 | 559578459 | 2 | 2804 |
| disk | read-only | indexed | 100 | 652389625 | 2 | 2804 |
| disk | read-only | single-get | 1 | 205265 | 4872 | 0 |
| disk | read-only | single-get | 10 | 60832 | 16439 | 0 |
| disk | read-only | single-get | 100 | 55913 | 17885 | 0 |
| disk | read-heavy | low-sel | 1 | 667765789 | 1 | 3464 |
| disk | read-heavy | low-sel | 10 | 198912700 | 5 | 3464 |
| disk | read-heavy | low-sel | 100 | 173671765 | 6 | 3464 |
| disk | read-heavy | high-sel | 1 | 11251878 | 89 | 0 |
| disk | read-heavy | high-sel | 10 | 1999255 | 500 | 0 |
| disk | read-heavy | high-sel | 100 | 2163692 | 462 | 0 |
| disk | read-heavy | indexed | 1 | 539172658 | 2 | 2804 |
| disk | read-heavy | indexed | 10 | 168102939 | 6 | 2804 |
| disk | read-heavy | indexed | 100 | 146500582 | 7 | 2804 |
| disk | read-heavy | single-get | 1 | 245739 | 4069 | 0 |
| disk | read-heavy | single-get | 10 | 71211 | 14043 | 0 |
| disk | read-heavy | single-get | 100 | 75016 | 13331 | 0 |
| disk | mixed | low-sel | 1 | 365947149 | 3 | 3464 |
| disk | mixed | low-sel | 10 | 130053997 | 8 | 3464 |
| disk | mixed | low-sel | 100 | 103545258 | 10 | 3464 |
| disk | mixed | high-sel | 1 | 6367273 | 157 | 0 |
| disk | mixed | high-sel | 10 | 2481898 | 403 | 0 |
| disk | mixed | high-sel | 100 | 855695 | 1169 | 0 |
| disk | mixed | indexed | 1 | 300041319 | 3 | 2804 |
| disk | mixed | indexed | 10 | 92895822 | 11 | 2804 |
| disk | mixed | indexed | 100 | 83238938 | 12 | 2804 |
| disk | mixed | single-get | 1 | 525928 | 1901 | 0 |
| disk | mixed | single-get | 10 | 203454 | 4915 | 0 |
| disk | mixed | single-get | 100 | 134772 | 7420 | 0 |
| disk | write-heavy | low-sel | 1 | 73418137 | 14 | 3464 |
| disk | write-heavy | low-sel | 10 | 74660612 | 13 | 3464 |
| disk | write-heavy | low-sel | 100 | 21618978 | 46 | 3464 |
| disk | write-heavy | high-sel | 1 | 1908728 | 524 | 0 |
| disk | write-heavy | high-sel | 10 | 555747 | 1799 | 0 |
| disk | write-heavy | high-sel | 100 | 282843 | 3536 | 0 |
| disk | write-heavy | indexed | 1 | 58308933 | 17 | 2804 |
| disk | write-heavy | indexed | 10 | 69955645 | 14 | 2804 |
| disk | write-heavy | indexed | 100 | 18630369 | 54 | 2804 |
| disk | write-heavy | single-get | 1 | 785921 | 1272 | 0 |
| disk | write-heavy | single-get | 10 | 298722 | 3348 | 0 |
| disk | write-heavy | single-get | 100 | 165198 | 6053 | 0 |
| disk | write-only | none | 1 | 912705 | 1096 | 0 |
| disk | write-only | none | 10 | 402720 | 2483 | 0 |
| disk | write-only | none | 100 | 112019 | 8927 | 0 |
| archive | read-only | low-sel | 1 | 675316583 | 1 | 3464 |
| archive | read-only | low-sel | 10 | 662581458 | 2 | 3464 |
| archive | read-only | low-sel | 100 | 699412625 | 1 | 3464 |
| archive | read-only | high-sel | 1 | 9496275 | 105 | 0 |
| archive | read-only | high-sel | 10 | 1535550 | 651 | 0 |
| archive | read-only | high-sel | 100 | 1345911 | 743 | 0 |
| archive | read-only | indexed | 1 | 589460042 | 2 | 2804 |
| archive | read-only | indexed | 10 | 555104458 | 2 | 2804 |
| archive | read-only | indexed | 100 | 546081083 | 2 | 2804 |
| archive | read-heavy | low-sel | 1 | 614228493 | 2 | 3464 |
| archive | read-heavy | low-sel | 10 | 289513651 | 3 | 3464 |
| archive | read-heavy | low-sel | 100 | 318559283 | 3 | 3464 |
| archive | read-heavy | high-sel | 1 | 8649573 | 116 | 0 |
| archive | read-heavy | high-sel | 10 | 1371410 | 729 | 0 |
| archive | read-heavy | high-sel | 100 | 1113108 | 898 | 0 |
| archive | read-heavy | indexed | 1 | 489316741 | 2 | 2804 |
| archive | read-heavy | indexed | 10 | 235809805 | 4 | 2804 |
| archive | read-heavy | indexed | 100 | 272233720 | 4 | 2804 |
| archive | mixed | low-sel | 1 | 342835252 | 3 | 3464 |
| archive | mixed | low-sel | 10 | 141465367 | 7 | 3464 |
| archive | mixed | low-sel | 100 | 183582642 | 5 | 3464 |
| archive | mixed | high-sel | 1 | 5064979 | 197 | 0 |
| archive | mixed | high-sel | 10 | 1005830 | 994 | 0 |
| archive | mixed | high-sel | 100 | 716122 | 1396 | 0 |
| archive | mixed | indexed | 1 | 269327964 | 4 | 2804 |
| archive | mixed | indexed | 10 | 114863480 | 9 | 2804 |
| archive | mixed | indexed | 100 | 142478057 | 7 | 2804 |
| archive | write-heavy | low-sel | 1 | 69094813 | 14 | 3464 |
| archive | write-heavy | low-sel | 10 | 67889361 | 15 | 3464 |
| archive | write-heavy | low-sel | 100 | 34480363 | 29 | 3464 |
| archive | write-heavy | high-sel | 1 | 921577 | 1085 | 0 |
| archive | write-heavy | high-sel | 10 | 322661 | 3099 | 0 |
| archive | write-heavy | high-sel | 100 | 242838 | 4118 | 0 |
| archive | write-heavy | indexed | 1 | 56807857 | 18 | 2804 |
| archive | write-heavy | indexed | 10 | 53370011 | 19 | 2804 |
| archive | write-heavy | indexed | 100 | 25913546 | 39 | 2804 |
| archive | write-only | none | 1 | 92898 | 10765 | 0 |
| archive | write-only | none | 10 | 84862 | 11784 | 0 |
| archive | write-only | none | 100 | 119758 | 8350 | 0 |

<!-- BENCH_TABLE_END -->

## Reading the results

- **Single-item `Get` is the fast path everywhere** (~5k–18k ops/s) and improves with
  concurrency. On `disk` it rides the page cache, so it tracks `mem`.
- **Query cost is dominated by *materializing matches*, not candidate selection.**
  `low-sel` and `indexed` both return thousands of ads (3464 / 2804 of 5000) and land
  at ~1–3 ops/s — the cost is decompressing + decoding every match. `high-sel`
  (0 matches) is 100–1600 ops/s because the wire-native fast path rejects each ad on
  a partial decode without ever materializing it.
- **Caveat on the `indexed` row:** the query here (`Memory>=1000 && Arch=="X86_64"`)
  matches ~56% of ads, so the index cannot beat a scan — both must still decode every
  match. The index wins only on a *selective* predicate (few candidates); that
  contrast is measured directly by `BenchmarkSelectiveWithIndex` vs
  `BenchmarkSelectiveFullScan`. Treat this column as "indexed candidate-selection with
  a large result set," not as the index's best case.
- **Strict-durability writes amortize with concurrency.** `disk write-only` climbs
  1096 → 2483 → 8927 ops/s as concurrency goes 1 → 10 → 100: group commit coalesces
  many writers into one `msync`. `mem` (~13–23k) and `archive` (~8–12k) writes are not
  fsync-bound and stay flat-to-down as contention rises.
- **Read format parity:** `mem ≈ disk ≈ archive` for queries — all serve reads from
  RAM / mmap page cache, so the durable and archived formats cost little on reads.
- **Concurrency helps selective reads and durable writes**, but not full-scan reads
  (`low-sel` barely moves with more goroutines — it is bound by decoding thousands of
  ads and GC, not by lock contention).
