#!/usr/bin/env bash
# Run one shard of the collections module's tests, under the race detector.
#
# WHY SHARD AT ALL: every test in this module is `package collections` -- 270 test files, all of them
# reaching into package internals -- so there is exactly ONE test binary, and `go test -p` (which
# parallelizes across PACKAGES) has nothing to work with. Only a fifth of the test files use
# t.Parallel(), so the suite is mostly serial: ~55 minutes of CI-runner time under -race, which no
# single job can fit under the 25m timeout. Sharding by test name across N jobs buys the cores a single
# runner does not have, without skipping the race detector or dropping a single test.
#
# WHY NOT SUBMODULES: moving the heavy tests into a sub-package would let -p parallelize them, but every
# one of the heavy test files uses 3-24 unexported identifiers from the package (segWindow, colSegment,
# lastVecSplit, scopeFixtureCodec, ...). They can only move behind a large export surface existing solely
# for tests, which is a worse trade than sharding the job.
#
# HOW TESTS ARE PLACED: greedy longest-first packing over the per-test durations recorded in
# shard-durations.txt. Each test, slowest first, goes to the shard with the least recorded time so far.
# Round-robin by name (the previous scheme) balanced the COUNT of tests, not their cost: with a handful
# of tests at 1-4 minutes and hundreds under a second, one shard routinely ran twice as long as another
# and finally hit the timeout.
#
# The table is a scheduling hint, never a source of truth about what runs: every test `go test -list`
# finds is placed, recorded or not, and a recorded name that no longer exists is ignored. A test with
# no recorded duration counts as DEFAULT_COST seconds until the table is refreshed.
#
# REFRESHING THE TABLE: each shard prints every test's duration on a "shard-duration:" line (in a
# collapsed log group). From a green run of the workflow, with all shards on the same commit:
#   gh run view <run-id> --repo PelicanPlatform/classad --log \
#     | sed -n 's/.*shard-duration: //p' | LC_ALL=C sort -k2,2 > shard-durations.txt
# Refresh it when a shard's warning says it ran over budget or many tests are unrecorded.
#
# Usage: SHARD=<0-based index> SHARDS=<count> ./shard-test.sh [extra go test flags...]
#        SHARD_LIST_ONLY=1 ... prints the partition and exits without running anything.
#        SHARD_BUDGET=<seconds> overrides the per-shard budget the warnings compare against.
#
# Bash 3.2 compatible (macOS ships 3.2): no mapfile, no associative arrays.
set -euo pipefail

SHARD="${SHARD:?SHARD (0-based shard index) must be set}"
SHARDS="${SHARDS:?SHARDS (total shard count) must be set}"

if [ "$SHARD" -lt 0 ] || [ "$SHARD" -ge "$SHARDS" ]; then
	echo "SHARD=$SHARD is out of range for SHARDS=$SHARDS" >&2
	exit 2
fi

here=$(cd "$(dirname "$0")" && pwd)
DURATIONS="$here/shard-durations.txt"
# Seconds charged to a test the table does not know. Most tests take well under a second; a new test
# that is really slow shows up in the next refresh.
DEFAULT_COST=1
# Per-shard budget in seconds of test time (setup and compilation come on top). The go test timeout
# below is 25m; a shard past this budget still passes but is the first sign the table needs refreshing
# or the matrix needs another shard.
SHARD_BUDGET="${SHARD_BUDGET:-600}"

# warn prints a GitHub Actions warning annotation (shown on the run summary) or, locally, a plain line.
warn() {
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::warning title=collections shard $SHARD/$SHARDS::$*"
	else
		echo "WARNING: $*" >&2
	fi
}

# Every test/fuzz/example in the module, deduplicated across packages and in a stable order, so all shards
# compute the same partition. A name in two packages lands in one shard and runs in both -- -run applies
# the same regex to every package -- which keeps the partition total.
listed=$(go test -list '.*' ./... 2>/dev/null | grep -E '^(Test|Fuzz|Example)' | LC_ALL=C sort -u)
if [ -z "$listed" ]; then
	echo "no tests listed: the module did not build, or -list found nothing" >&2
	exit 1
fi
listed_total=$(printf '%s\n' "$listed" | wc -l | tr -d ' ')

if [ ! -f "$DURATIONS" ]; then
	warn "$DURATIONS is missing: every test costs ${DEFAULT_COST}s, so the shards are balanced by count only"
	DURATIONS=/dev/null
fi

# "cost name" for every listed test, slowest first, ties broken by name so every shard sorts the same.
costed=$(printf '%s\n' "$listed" | awk -v table="$DURATIONS" -v def="$DEFAULT_COST" '
	BEGIN { while ((getline line < table) > 0) if (line !~ /^#/ && split(line, f, " ") >= 2) cost[f[2]] = f[1] }
	{ print (($1 in cost) ? cost[$1] : def), $1 }
' | LC_ALL=C sort -k1,1nr -k2,2)
unrecorded=$(printf '%s\n' "$listed" | awk -v table="$DURATIONS" '
	BEGIN { while ((getline line < table) > 0) if (line !~ /^#/ && split(line, f, " ") >= 2) known[f[2]] = 1 }
	!($1 in known) { n++ }
	END { print n + 0 }
')

# Longest-processing-time packing: each test goes to the currently lightest shard (lowest index on a
# tie). Every shard runs the same computation on the same input, so they agree on the partition.
# Output: "shard cost name", one line per test.
assigned=$(printf '%s\n' "$costed" | awk -v n="$SHARDS" '
	BEGIN { for (i = 0; i < n; i++) load[i] = 0 }
	{
		best = 0
		for (i = 1; i < n; i++) if (load[i] < load[best]) best = i
		load[best] += $1
		print best, $1, $2
	}
')

# Verify rather than assume: every listed test assigned exactly once, to a shard that exists. A test
# landing in no shard would silently stop running while CI stayed green.
assigned_names=$(printf '%s\n' "$assigned" | awk -v n="$SHARDS" '$1 >= 0 && $1 < n { print $3 }' | LC_ALL=C sort)
if [ "$assigned_names" != "$listed" ]; then
	echo "shard partition does not match the test list ($listed_total listed, $(printf '%s\n' "$assigned" | wc -l | tr -d ' ') assigned): refusing to run a partition that may drop or duplicate tests" >&2
	exit 1
fi

mine=$(printf '%s\n' "$assigned" | awk -v s="$SHARD" '$1 == s { print $3 }')
count=$(printf '%s\n' "$mine" | grep -c . || true)
if [ "$count" -eq 0 ]; then
	echo "shard $SHARD/$SHARDS is empty: more shards than tests?" >&2
	exit 1
fi
# Per-shard "count/estimated seconds", e.g. "212/598s 213/601s ...".
summary=$(printf '%s\n' "$assigned" | awk -v n="$SHARDS" '
	{ c[$1]++; l[$1] += $2 }
	END { for (i = 0; i < n; i++) printf "%d/%.0fs ", c[i], l[i] }
')
estimate=$(printf '%s\n' "$assigned" | awk -v s="$SHARD" '$1 == s { l += $2 } END { printf "%.0f", l }')
heaviest=$(printf '%s\n' "$assigned" | awk -v n="$SHARDS" '
	{ l[$1] += $2 }
	END { m = 0; for (i = 0; i < n; i++) if (l[i] > m) m = l[i]; printf "%.0f", m }
')

echo "shard $SHARD/$SHARDS: $count of $listed_total tests, ~${estimate}s recorded (tests/estimate per shard: $summary)"
if [ "$unrecorded" -gt 0 ]; then
	echo "$unrecorded tests have no recorded duration and count as ${DEFAULT_COST}s each"
fi
if [ "$heaviest" -gt "$SHARD_BUDGET" ]; then
	warn "the heaviest shard is estimated at ${heaviest}s, over the ${SHARD_BUDGET}s budget: add a shard to the collections matrix in .github/workflows/test.yml"
fi
if [ -n "${SHARD_LIST_ONLY:-}" ]; then
	printf '%s\n' "$mine"
	exit 0
fi

# ^(A|B|C)$ so a shard's names cannot prefix-match a test assigned to another shard.
re="^($(printf '%s\n' "$mine" | paste -sd '|' -))\$"

log="${TMPDIR:-/tmp}/shard-test.$SHARD.$$.log"
trap 'rm -f "$log"' EXIT

start=$SECONDS
# -v for the per-test durations. The verbose log goes to a file: on success only the package summary
# lines are printed, on failure the whole log is, so a failure reads exactly as it would locally.
# -p 4 overlaps this module's four packages (collections, crypt, vm, wire); the small three finish while
# the big one is still going.
set +e
go test -race -count=1 -timeout 25m -p 4 -v -run "$re" "$@" ./... >"$log" 2>&1
rc=$?
set -e
elapsed=$((SECONDS - start))

if [ "$rc" -eq 0 ]; then
	grep -E '^(ok|FAIL|\?)[[:space:]]' "$log" || true
else
	cat "$log"
fi

# Per-test durations of top-level tests, summed when a name runs in more than one package.
durations=$(sed -n 's/^--- [A-Z]*: \([^ ]*\) (\([0-9.]*\)s)$/\2 \1/p' "$log" | awk '
	{ d[$2] += $1 }
	END { for (t in d) printf "%.1f %s\n", d[t], t }
' | LC_ALL=C sort -k1,1nr -k2,2)

echo "slowest tests in shard $SHARD/$SHARDS (seconds, under -race):"
printf '%s\n' "$durations" | sed -n '1,25s/^/  /p'
echo "::group::shard $SHARD/$SHARDS per-test durations (for shard-durations.txt)"
printf '%s\n' "$durations" | sed 's/^/shard-duration: /'
echo "::endgroup::"

echo "shard $SHARD/$SHARDS finished in ${elapsed}s (estimated ${estimate}s)"
if [ "$rc" -eq 0 ] && [ "$elapsed" -gt "$SHARD_BUDGET" ]; then
	warn "took ${elapsed}s, over the ${SHARD_BUDGET}s budget: refresh shard-durations.txt or add a shard (see shard-test.sh)"
fi
exit "$rc"
