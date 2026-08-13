#!/usr/bin/env bash
# Run one shard of the collections module's tests, under the race detector.
#
# WHY SHARD AT ALL: every test in this module is `package collections` -- 180 test files, all of them
# reaching into package internals -- so there is exactly ONE test binary, and `go test -p` (which
# parallelizes across PACKAGES) has nothing to work with. The suite does use t.Parallel(), so it scales
# with cores: ~7 min on a 12-core box, ~20 min on a 4-vCPU runner, which is within two minutes of the 25m
# timeout. It went red on one merge commit and green on the next with the same code. Sharding by test name
# across N jobs buys the cores a single runner does not have, without skipping the race detector or
# dropping a single test.
#
# WHY NOT SUBMODULES: moving the heavy tests into a sub-package would let -p parallelize them, but every
# one of the heavy test files uses 3-24 unexported identifiers from the package (segWindow, colSegment,
# lastVecSplit, scopeFixtureCodec, ...). They can only move behind a large export surface existing solely
# for tests, which is a worse trade than sharding the job.
#
# Usage: SHARD=<0-based index> SHARDS=<count> ./shard-test.sh [extra go test flags...]
#        SHARD_LIST_ONLY=1 ... prints the partition and exits without running anything.
#
# Bash 3.2 compatible (macOS ships 3.2): no mapfile, no associative arrays.
set -euo pipefail

SHARD="${SHARD:?SHARD (0-based shard index) must be set}"
SHARDS="${SHARDS:?SHARDS (total shard count) must be set}"

if [ "$SHARD" -lt 0 ] || [ "$SHARD" -ge "$SHARDS" ]; then
	echo "SHARD=$SHARD is out of range for SHARDS=$SHARDS" >&2
	exit 2
fi

# HEAVY lists the slowest tests, measured under -race, longest first. They are placed before everything
# else so consecutive shards get one each instead of one shard collecting several: the top test alone is
# ~10% of the suite, so leaving placement to chance leaves one shard twice as long as its siblings.
#
# A name that no longer exists is simply absent from the test list and drops out -- this is a scheduling
# hint, never a source of truth about what runs. Refresh it with:
#   go test -race -count=1 -timeout 40m -v ./ | grep '^--- PASS' | sed 's/.*: //' | sort -t'(' -k2 -rn | head -20
HEAVY="
TestQueryFastPathReal
TestOSPoolHotHeaderVariants
TestCodecDensityReport
TestPersistentDensityReport
TestDictStrategyByGroupSize
TestOSPoolClosureStorageCost
TestRetrainPrunesOldDicts
TestDictStrategiesRealOSPool
TestZonePruneAnalyzedOncePerQuery
TestConcurrentRetrainUnderUpdates
TestStrDictCost
TestDictStrategyNetBytes
TestInterningSizeTradeoff
TestAutoRetrain
TestRecentSamplerDictQuality
TestRetrainDictImprovesDensityAndPreservesData
"

# Every test/fuzz/example in the module, deduplicated across packages and in a stable order, so all shards
# compute the same partition. A name in two packages lands in one shard and runs in both -- -run applies
# the same regex to every package -- which keeps the partition total.
listed=$(go test -list '.*' ./... 2>/dev/null | grep -E '^(Test|Fuzz|Example)' | sort -u)
if [ -z "$listed" ]; then
	echo "no tests listed: the module did not build, or -list found nothing" >&2
	exit 1
fi

# ordered = heavy tests that actually exist, then everything else. Both filters run against the same
# listing, so a heavy test cannot appear twice and a listed test cannot be dropped.
present_heavy=""
for t in $HEAVY; do
	if printf '%s\n' "$listed" | grep -qx "$t"; then
		present_heavy="$present_heavy$t
"
	fi
done
rest=$(printf '%s\n' "$listed" | grep -vxF "$(printf '%s' "$present_heavy")" || true)
ordered=$(printf '%s%s\n' "$present_heavy" "$rest" | grep -v '^$')

total=$(printf '%s\n' "$ordered" | wc -l | tr -d ' ')
listed_total=$(printf '%s\n' "$listed" | wc -l | tr -d ' ')
if [ "$total" -ne "$listed_total" ]; then
	echo "reordering changed the test count ($listed_total listed, $total ordered): refusing to run a" \
		"partition that may drop or duplicate tests" >&2
	exit 1
fi

# Partition by position, computed identically in every shard, so the shards are disjoint and cover
# everything by construction. Verified below rather than assumed: a test landing in no shard would
# silently stop running while CI stayed green.
mine=$(printf '%s\n' "$ordered" | awk -v s="$SHARD" -v n="$SHARDS" '(NR-1) % n == s')
sizes=$(printf '%s\n' "$ordered" | awk -v n="$SHARDS" '{c[(NR-1)%n]++} END {for (i=0;i<n;i++) printf "%d ", c[i]}')
covered=0
for c in $sizes; do covered=$((covered + c)); done
if [ "$covered" -ne "$total" ]; then
	echo "shard partition covers $covered of $total tests: some test would never run" >&2
	exit 1
fi
count=$(printf '%s\n' "$mine" | grep -c . || true)
if [ "$count" -eq 0 ]; then
	echo "shard $SHARD/$SHARDS is empty: more shards than tests?" >&2
	exit 1
fi

echo "shard $SHARD/$SHARDS: $count of $total tests (shard sizes: $sizes)"
if [ -n "${SHARD_LIST_ONLY:-}" ]; then
	printf '%s\n' "$mine"
	exit 0
fi

# ^(A|B|C)$ so a shard's names cannot prefix-match a test assigned to another shard.
re="^($(printf '%s\n' "$mine" | paste -sd '|' -))\$"

start=$SECONDS
# -p 4 overlaps this module's four packages (collections, crypt, vm, wire); the small three finish while
# the big one is still going.
go test -race -count=1 -timeout 25m -p 4 -run "$re" "$@" ./...
echo "shard $SHARD/$SHARDS finished in $((SECONDS - start))s"
