package collections

import (
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// ErrTimeTravelDisabled is returned by the AS OF query paths when the collection has
// no time-travel configuration.
var ErrTimeTravelDisabled = errors.New("collections: time travel is not enabled on this collection")

// Time travel: mapping wall-clock time to commit sequences for point-in-time
// ("AS OF") queries.
//
// The store is already multi-version -- every record carries the commit seq at which
// it became current and the seq at which it was superseded, and forEachVisible yields
// the version visible at any snapshot seq S0 (seq <= S0 < sup). Two things are missing
// for time travel, both provided here plus small hooks elsewhere:
//
//  1. a time->seq map, so a wall-clock instant resolves to the seq that was current
//     then. Commit seqs are strictly per-shard, so the map is per-shard too: each
//     shard records sparse (seq, unixMillis) checkpoints -- at most one per
//     CheckpointInterval, on the first commit that crosses the boundary. A checkpoint
//     is written into the segment arena as a marker entry (see segment.go
//     appendMarker), so it rides the segment's existing msync durability -- no
//     separate file or fsync -- and is rebuilt into the in-memory index on recovery.
//
//  2. retention: compaction must not reclaim a superseded version still within the
//     travel window. retainFloor(now) is the seq that was current at now-MaxDistance;
//     compaction keeps any version whose supersededBySeq is beyond it (see compact.go).
//
// When a collection has no TimeTravel option the whole mechanism is inert: no
// checkpoints are written and retainFloor collapses to the current commit seq, so
// compaction behaves exactly as before with zero write-path cost.

const defaultCheckpointInterval = time.Minute

// ttConfig is a collection's active time-travel configuration; nil (see
// Collection.ttCfg) means disabled. Held behind an atomic pointer so the feature can
// be toggled/retuned at runtime.
type ttConfig struct {
	maxDistance time.Duration
	interval    time.Duration
}

func newTTConfig(o *TimeTravelOptions) *ttConfig {
	if o == nil || o.MaxDistance <= 0 {
		return nil
	}
	iv := o.CheckpointInterval
	if iv <= 0 {
		iv = defaultCheckpointInterval
	}
	return &ttConfig{maxDistance: o.MaxDistance, interval: iv}
}

// nowMillisFn supplies the current wall-clock time in unix milliseconds. It is a
// package var so tests can install a deterministic clock; production uses the wall
// clock. Set it only from a single-goroutine test setup.
var nowMillisFn = func() uint64 { return uint64(time.Now().UnixMilli()) }

// nowMillis is the current wall-clock time in unix milliseconds.
func nowMillis() uint64 { return nowMillisFn() }

// SetTimeTravel enables, retunes, or (with a nil/zero option) disables point-in-time
// queries at runtime. Enabling starts recording checkpoints and retaining superseded
// versions from now on (it is not retroactive); disabling lets the next compaction
// reclaim the retained history. Safe to call concurrently with reads and writes.
func (c *Collection) SetTimeTravel(o *TimeTravelOptions) {
	c.ttCfg.Store(newTTConfig(o))
}

// timeTravel returns the collection's active time-travel config, or nil when disabled.
func (c *Collection) timeTravel() *ttConfig { return c.ttCfg.Load() }

// TimeTravelConfig reports the collection's current time-travel settings and whether
// it is enabled (for persisting the runtime toggle; see db.saveIndexConfig).
func (c *Collection) TimeTravelConfig() (opts TimeTravelOptions, enabled bool) {
	if cfg := c.ttCfg.Load(); cfg != nil {
		return TimeTravelOptions{MaxDistance: cfg.maxDistance, CheckpointInterval: cfg.interval}, true
	}
	return TimeTravelOptions{}, false
}

// resolveAsOf maps wall-clock t to a per-shard commit-sequence vector for a
// point-in-time scan: for each shard, the sequence that was current at t (the last
// checkpoint at or before t). It errors if time travel is disabled or t is older than
// the retained window. A shard with no checkpoint at or before t resolves to seq 0 (it
// held nothing that early). t in the future is clamped to now.
func (c *Collection) resolveAsOf(t time.Time) ([]uint64, error) {
	cfg := c.ttCfg.Load()
	if cfg == nil {
		return nil, ErrTimeTravelDisabled
	}
	now := nowMillis()
	ms := now
	if tm := t.UnixMilli(); tm >= 0 && uint64(tm) < now {
		ms = uint64(tm)
	}
	d := uint64(cfg.maxDistance / time.Millisecond)
	var horizon uint64
	if now > d {
		horizon = now - d
	}
	if ms < horizon {
		return nil, fmt.Errorf("collections: AS OF %s is older than the %s time-travel window",
			t.UTC().Format(time.RFC3339), cfg.maxDistance)
	}
	seqs := make([]uint64, len(c.shards))
	for i, sh := range c.shards {
		if s, ok := sh.tseq.seqAt(ms); ok {
			seqs[i] = s
		}
	}
	return seqs, nil
}

// QueryAsOf runs q against the point-in-time snapshot at time t: it resolves t to a
// per-shard commit sequence and scans each shard at that sequence, so the result is
// exactly the ads that were current at t. It errors if time travel is disabled or t
// predates the retained window. v1 uses a serial full scan per shard (the index,
// parallel, and chained fast paths are current-time only); the query constraint is
// still applied to every candidate.
func (c *Collection) QueryAsOf(q *vm.Query, t time.Time) (iter.Seq[*classad.ClassAd], error) {
	seqs, err := c.resolveAsOf(t)
	if err != nil {
		return nil, err
	}
	return func(yield func(*classad.ClassAd) bool) {
		plan := q.ReadPlan()
		ws := &wireScope{ctx: c}
		qp := queryPlan{
			q:        q,
			plan:     plan,
			m:        q.Matcher(),
			wireOK:   q.Native() && plan.PartialSafe,
			ws:       ws,
			resolver: ws.resolve,
		}
		emit := c.yieldAd(yield)
		for i, sh := range c.shards {
			if !c.scanShardAt(sh, seqs[i], qp, emit) {
				return
			}
		}
	}, nil
}

// retainFloorLocked is the commit sequence below which superseded versions may be
// reclaimed by compaction: the current commitSeq when time travel is off (reclaim
// every superseded version, exactly as before), else the sequence that was current
// MaxDistance ago (retain anything newer for AS OF queries). A version is kept iff its
// supersededBySeq is strictly greater than this. Caller holds at least the read lock.
func (sh *shard) retainFloorLocked() uint64 {
	if sh.tt != nil {
		if cfg := sh.tt.Load(); cfg != nil {
			if f := sh.tseq.retainFloor(nowMillis(), cfg.maxDistance); f < sh.commitSeq {
				return f
			}
		}
	}
	return sh.commitSeq
}

// timeSeqEntry is one checkpoint: seq was the shard's commit sequence at wall-clock
// millis. Both fields are non-decreasing along the slice.
type timeSeqEntry struct {
	seq    uint64
	millis uint64
}

// shardTimeIndex is a shard's in-memory time->seq checkpoint index: a slice of
// checkpoints sorted ascending (by both seq and millis, which advance together). It is
// populated from segment markers on recovery and appended to on commit. Guarded by its
// own lock so query-time resolution does not contend on the shard write lock.
type shardTimeIndex struct {
	mu         sync.RWMutex
	entries    []timeSeqEntry
	lastMillis uint64 // millis of the most recent checkpoint (0 = none yet)
}

// due reports whether a checkpoint should be recorded at nowMs given the interval:
// true if none has been recorded yet or the interval has elapsed since the last one.
// Callers hold the shard write lock (checkpoints are recorded during commit).
func (x *shardTimeIndex) due(nowMs uint64, interval time.Duration) bool {
	x.mu.RLock()
	last := x.lastMillis
	x.mu.RUnlock()
	if last == 0 {
		return true
	}
	return nowMs >= last+uint64(interval/time.Millisecond)
}

// record appends a checkpoint (seq was current at millis). millis is clamped to be
// non-decreasing so a backward clock cannot corrupt the ordering.
func (x *shardTimeIndex) record(seq, millis uint64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.entries) > 0 {
		if last := x.entries[len(x.entries)-1]; millis < last.millis {
			millis = last.millis
		}
	}
	x.entries = append(x.entries, timeSeqEntry{seq: seq, millis: millis})
	x.lastMillis = millis
}

// seqAt returns the largest checkpoint seq whose millis is <= the given millis (the
// commit sequence that was current at that wall-clock instant), and ok=false if the
// instant predates the earliest checkpoint (nothing to resolve to).
func (x *shardTimeIndex) seqAt(millis uint64) (uint64, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	e := x.entries
	if len(e) == 0 || millis < e[0].millis {
		return 0, false
	}
	// Largest index with e[i].millis <= millis (entries ascending by millis).
	lo, hi := 0, len(e)
	for lo < hi {
		mid := (lo + hi) / 2
		if e[mid].millis <= millis {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return e[lo-1].seq, true
}

// retainFloor returns the seq below which superseded versions may be reclaimed for a
// window of maxDistance ending now: the seq that was current at now-maxDistance. If the
// entire recorded history is younger than maxDistance (the cutoff predates the earliest
// checkpoint) it returns 0 -- retain everything. Callers pass this to compaction as the
// gate `keep any version with supersededBySeq > retainFloor`.
func (x *shardTimeIndex) retainFloor(nowMs uint64, maxDistance time.Duration) uint64 {
	d := uint64(maxDistance / time.Millisecond)
	if nowMs < d {
		return 0
	}
	seq, ok := x.seqAt(nowMs - d)
	if !ok {
		return 0
	}
	return seq
}

// trim drops checkpoints whose seq is at or below floor (their window has aged out);
// they are no longer needed once compaction has reclaimed everything below floor. It
// keeps the last trimmed entry as a sentinel so seqAt for a time just after the floor
// still resolves. Callers hold no lock.
func (x *shardTimeIndex) trim(floor uint64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	// Find the first entry with seq > floor; keep one entry before it as the sentinel.
	cut := 0
	for cut < len(x.entries) && x.entries[cut].seq <= floor {
		cut++
	}
	if cut > 1 {
		x.entries = append(x.entries[:0], x.entries[cut-1:]...)
	}
}
