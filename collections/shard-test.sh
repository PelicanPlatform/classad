#!/usr/bin/env bash
# Run one shard of the collections module's tests, under the race detector.
#
# WHY SHARD AT ALL: every test in this module is `package collections` -- 360 test files, all of them
# reaching into package internals -- so there is exactly ONE test binary, and `go test -p` (which
# parallelizes across PACKAGES) has nothing to work with. The suite is over an hour of CI-runner time
# under -race, which no single job can fit under any sane timeout. Sharding by test name across N jobs
# buys the cores a single runner does not have, without skipping the race detector or dropping a
# single test.
#
# 388 top-level tests now call t.Parallel(), covering 71% of the time this table records, so a shard
# also overlaps work within itself. What is left serial is mostly tests that touch the package's
# process-global state -- the test hooks, the counters tests assert deltas on, the process-global
# block-cache budgets -- which cannot be made parallel without moving that state.
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
# no recorded duration is charged the MEAN of the recorded costs.
#
# WHY THE MEAN, AND NOT A SMALL CONSTANT: the table went stale (603 of 1455 tests unrecorded) while
# every unrecorded test was charged a flat 1s. Those 603 tests were really 3883s of the suite's 10458s,
# so 37% of the cost was invisible to the packer. The packer then reported all six shards at an
# identical 643s estimate while they actually ran 760s to 1522s, and it had piled 553 of the 1455 tests
# onto one shard because that shard looked cheap. A mean-cost default cannot produce that: it tracks
# what an unknown test typically costs (7.2s against the 3883s/603 = 6.4s those tests really cost), so
# a stale table degrades toward balancing by count -- the right thing to do when you know nothing about
# a test -- instead of concentrating every new test in one place.
#
# REFRESHING THE TABLE: each shard prints every test's duration on a "shard-duration:" line (in a
# collapsed log group). From a green run of the workflow, with all shards on the same commit:
#   gh run view <run-id> --repo PelicanPlatform/classad --log \
#     | sed -n 's/.*shard-duration: //p' | LC_ALL=C sort -k2,2 > shard-durations.txt
# Refresh it when a shard warns that too many tests are unrecorded, or that it ran over budget. A shard
# that FAILED contributes only the tests that finished, so refresh from a run where every shard is green.
#
# THE THREE ALARMS, and why they are separate. Each watches one thing and is expressed in the units of
# the thing it watches, because the version of this script that mixed them up is how the suite drifted
# into failing: its only alarm compared the packer's estimate, in inflated wall-seconds, against a
# budget in real seconds, so it fired on every shard of every run and became something to scroll past.
#   SHARD_STALE_PCT     -- share of tests the duration table does not know. Watches the TABLE.
#   SHARD_WEIGHT_BUDGET -- recorded seconds each shard carries. Watches the SUITE's size against the
#                          matrix, and is the one number here that does not move with runner speed.
#   SHARD_BUDGET        -- real elapsed seconds. Watches a single RUN for a near miss on the timeout.
# Only the last can be set off by an unlucky host, which is why it is the one set closest to failure.
#
# Usage: SHARD=<0-based index> SHARDS=<count> ./shard-test.sh [extra go test flags...]
#        SHARD_LIST_ONLY=1 ... prints the partition and exits without running anything.
#        SHARD_TIMEOUT, SHARD_WEIGHT_BUDGET, SHARD_BUDGET, SHARD_STALE_PCT override the four settings
#        below; the defaults are what CI uses.
#
# A WARNING ABOUT READING THE ESTIMATE: `go test -v` reports a parallel test's WALL time, which
# includes everything that overlapped it. In a suite that is 71% parallel the table therefore records
# considerably more time than those tests cost -- measured across a full run, a shard's elapsed time
# comes out at about 0.59x the sum of its recorded durations, plus ~90s of race-detector build. So an
# estimate is a RELATIVE weight for packing, never a prediction of seconds. Only the elapsed time this
# script prints at the end of a real run is in real seconds. Balancing the estimate is still the right
# thing to do -- it is what keeps one shard from being handed four times another's work -- it just does
# not promise that two equally weighted shards finish together, because what they finish in is mostly
# down to the host.
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
# Deadlock detection for the test binary: what turns a wedged test into a goroutine dump instead of a
# job that runs until the runner is killed. It must clear the SLOWEST runner the pool can hand us, not
# the typical one -- see the note where it is passed to go test.
SHARD_TIMEOUT="${SHARD_TIMEOUT:-40m}"

# GROWTH ALARM, in recorded seconds per shard. This is the one number here that does not move with
# runner speed: it is just how much of the suite each shard has been given, so it answers "has the suite
# outgrown this matrix" without being drowned out by which host a job landed on. 900s per shard is the
# size at which a shard runs about ten minutes on a typical runner and about sixteen on the slowest
# batch of hosts seen so far, which is the shape worth holding.
SHARD_WEIGHT_BUDGET="${SHARD_WEIGHT_BUDGET:-900}"

# NEAR-MISS ALARM, in real elapsed seconds: a run that got this close to SHARD_TIMEOUT still passed, but
# only just, and the next one may not. 70% of the timeout.
SHARD_BUDGET="${SHARD_BUDGET:-1680}"

# Warn when this percentage of the listed tests has no recorded duration. Unrecorded tests are charged
# the table's mean, which is a reasonable guess for a handful of them and a worse one for hundreds: past
# this fraction the packing is guesswork and the table should be refreshed.
SHARD_STALE_PCT="${SHARD_STALE_PCT:-5}"

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
	warn "$DURATIONS is missing: every test costs the same, so the shards are balanced by count only"
	DURATIONS=/dev/null
fi

# Mean of the recorded costs: what an unrecorded test is charged. Falls back to 1 when the table is
# missing or empty, which makes every test cost the same and so balances by count.
DEFAULT_COST=$(awk '
	!/^#/ && NF >= 2 { s += $1; n++ }
	END { printf "%.2f", (n > 0 ? s / n : 1) }
' "$DURATIONS")

# Floor on any test's cost, half the table's one-decimal resolution. 629 of the 1455 tests are recorded
# at 0.0s, and a test that adds literally nothing to a shard's load leaves that shard the lightest
# forever: the packer handed all 629 of them to one shard, which then ran 69 tests' worth of real work
# plus 629 "free" ones. They are not free -- each still pays go test's per-test bookkeeping and a
# goroutine under -race. With a floor they accumulate, so tests the table cannot tell apart end up
# spread by count, which is the right answer when their costs are indistinguishable.
MIN_COST=0.05

# "cost name" for every listed test, slowest first, ties broken by name so every shard sorts the same.
costed=$(printf '%s\n' "$listed" | awk -v table="$DURATIONS" -v def="$DEFAULT_COST" -v floor="$MIN_COST" '
	BEGIN { while ((getline line < table) > 0) if (line !~ /^#/ && split(line, f, " ") >= 2) cost[f[2]] = f[1] }
	{
		c = ($1 in cost) ? cost[$1] : def
		print (c < floor ? floor : c), $1
	}
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
if [ "$heaviest" -gt "$SHARD_WEIGHT_BUDGET" ]; then
	want=$(awk -v h="$heaviest" -v n="$SHARDS" -v b="$SHARD_WEIGHT_BUDGET" 'BEGIN { printf "%d", int(h * n / b) + 1 }')
	warn "each shard carries ${heaviest}s of recorded test time, over the ${SHARD_WEIGHT_BUDGET}s per-shard budget: the suite needs about $want shards, there are $SHARDS. Add indices to the collections matrix in .github/workflows/test.yml"
fi
# The estimate is in inflated wall-seconds and is NOT compared against the budget; see the header. The
# share of tests the table does not know is what tells you the packing has stopped being trustworthy.
if [ "$unrecorded" -gt 0 ]; then
	stale_pct=$(awk -v u="$unrecorded" -v t="$listed_total" 'BEGIN { printf "%.0f", 100 * u / t }')
	echo "$unrecorded of $listed_total tests ($stale_pct%) have no recorded duration and count as ${DEFAULT_COST}s each"
	if [ "$stale_pct" -ge "$SHARD_STALE_PCT" ]; then
		warn "$unrecorded of $listed_total tests ($stale_pct%, threshold $SHARD_STALE_PCT%) have no recorded duration: refresh collections/shard-durations.txt from a run where every shard is green (see shard-test.sh)"
	fi
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
#
# -timeout is deadlock detection, not a budget. It has to clear the slowest runner the pool can hand us,
# and that spread is LARGE: in run 41924017120 the six shards carried identical recorded weight (1748s
# each) and finished in 752s, 796s, 1048s, 1071s, 1235s and one that hit the 25m timeout -- a 2x spread
# with nothing to do with what each shard was asked to run. The fixed-size "Build and vet" step in those
# same six jobs took 13s, 13s, 14s, 18s, 18s and 19s, in the same order, so it is the hosts, not the
# partition. A timeout at twice the typical shard is therefore not slack at all; it is a coin flip.
#
# This is the second time it has been raised, and the reason it had to be is worth naming: it was the
# only thing watching the suite's size, so it was set just above what the suite took and was overtaken
# the moment the suite grew. That job now belongs to SHARD_WEIGHT_BUDGET, which measures how much of the
# suite each shard carries and does not move with runner speed, so it says "add shards" on a green run
# instead of waiting to fail one. With a real growth alarm in place the timeout is free to be sized for
# what it is for, and 40m is about 3x the typical shard.
#
# It is not obviously safe to put it back to 25m once the matrix grows, and the measurement says so.
# Run 41932733548 drew a uniformly slow batch of hosts -- "Build and vet" took 18-20s in every job,
# against 13-14s in the fastest jobs of the run before -- and the same shards that had taken 1048s and
# 752s took 1891s and 1131s. Scale that slow run down to twelve shards and its worst shard is still
# about 16 minutes, which is 1.5x under a 25m timeout and 2.4x under this one. Twelve shards AND this
# timeout, then; dropping back to 25m wants sixteen.
set +e
go test -race -count=1 -timeout "$SHARD_TIMEOUT" -p 4 -v -run "$re" "$@" ./... >"$log" 2>&1
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
	warn "took ${elapsed}s against a ${SHARD_TIMEOUT} timeout, past the ${SHARD_BUDGET}s near-miss mark: this run passed with little to spare. Add indices to the collections matrix in .github/workflows/test.yml (see shard-test.sh)"
fi
exit "$rc"
