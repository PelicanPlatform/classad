package collections

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Making query demand outlive the process, and stop being a lifetime total.
//
// The demand counters drive index suggestions, auto-tuning, and hot-set ranking. As
// process-local monotonic counters they get both ends of the time axis wrong:
//
//   - Nothing survives a restart. Every attribute reads zero, so a daemon that restarts
//     regularly never accumulates enough evidence to justify anything, and anything that
//     reads a zero as "unwanted" is reading amnesia. Guarding against that (the drop-evidence
//     window) only suppresses the damage; it does not recover the signal.
//   - Nothing ever decays. In a long-lived daemon "demand" means "demand since boot",
//     so a batch job that hammered an attribute once in January keeps its index pinned in
//     July. The longer the process runs, the more the counters describe history rather than
//     the current workload.
//
// Both are fixed by the same thing: an exponentially decayed counter, checkpointed to disk.
// Decay is applied by elapsed time -- at each save, and again on load for the interval the
// process was down -- so the value always means "recent demand, as of now" whether or not
// there was a restart in between. A restart then costs only the demand accrued since the
// last checkpoint, instead of all of it.
//
// Downtime decays the counters but does not count as observation: observedNanos accumulates
// only time the process was actually running, so a daemon that has been up for a minute
// cannot conclude anything from a week-old checkpoint just because the checkpoint is old.

const (
	demandFile = "demand.json"
	// defaultDemandHalfLife is how fast demand fades. Index decisions should track the
	// shape of a workload over days -- long enough that a quiet weekend does not discard
	// what a week of queries established, short enough that a workload which genuinely
	// moves on is followed within a couple of weeks.
	defaultDemandHalfLife = 7 * 24 * time.Hour
	// demandFloor is the decayed value below which an attribute is forgotten rather than
	// checkpointed, so the file does not accumulate every attribute ever queried.
	demandFloor = 0.5
	// demandDecayThreshold is the smallest scaling worth applying. Counters are integers,
	// so a decay too small to change one is not free -- it rounds away a fraction of a
	// count and, charged again on the next pass, erodes counters at a rate set by how often
	// maintenance runs rather than by the half-life. Below this the elapsed time is left on
	// the clock to be charged in a later, larger step. At the default half-life this makes
	// decay land roughly every two hours.
	demandDecayThreshold = 0.99
	demandVer            = 1
)

// persistedDemand is the on-disk checkpoint. Counts are float64 because decay is
// multiplicative; the in-memory counters round them back to integers.
type persistedDemand struct {
	Version   int                        `json:"version"`
	SavedUnix int64                      `json:"saved_unix"` // nanos, for the elapsed-downtime decay
	Observed  int64                      `json:"observed_nanos"`
	Total     float64                    `json:"total"`
	Attrs     map[string]persistedCounts `json:"attrs"` // folded attribute name -> counts
}

type persistedCounts struct {
	Eq    float64 `json:"eq"`
	Rng   float64 `json:"rng"`
	Reads float64 `json:"reads"`
}

// decayFactor is the multiplier for demand that is age old, given a half-life.
func decayFactor(age, halfLife time.Duration) float64 {
	if age <= 0 || halfLife <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(halfLife))
}

// decay scales every counter by f, dropping attributes that fall below the floor. Counters
// are read-modify-written without atomicity against concurrent record() calls: this runs on
// the maintenance path against a statistical signal, so losing a probe that lands mid-scale
// is immaterial, and paying for exact accounting on the query hot path would not be.
func (d *demandTracker) decay(f float64) {
	if f >= 1 {
		return
	}
	d.total.Store(scaleCount(d.total.Load(), f))
	var dead []string
	d.m.Range(func(k, v any) bool {
		c := v.(*demandCounts)
		eq := float64(c.eq.Load()) * f
		rng := float64(c.rng.Load()) * f
		reads := float64(c.reads.Load()) * f
		if eq < demandFloor && rng < demandFloor && reads < demandFloor {
			dead = append(dead, k.(string))
			return true
		}
		c.eq.Store(int64(math.Round(eq)))
		c.rng.Store(int64(math.Round(rng)))
		c.reads.Store(int64(math.Round(reads)))
		return true
	})
	for _, k := range dead {
		d.m.Delete(k)
	}
}

// scaleCount applies a decay factor to an integer counter, rounding rather than truncating.
// Truncation biases every scaling downward by up to a full count, which for a counter in the
// tens is a larger effect than the decay being applied.
func scaleCount(n int64, f float64) int64 { return int64(math.Round(float64(n) * f)) }

// observedFor reports how long this tracker has actually been watching: the time
// accumulated over previous runs plus this session so far. Downtime is excluded, which is
// the difference between "we have a week-old checkpoint" and "we have watched for a week".
func (d *demandTracker) observedFor() time.Duration {
	var session time.Duration
	if st := d.startedUnix.Load(); st != 0 {
		session = time.Since(time.Unix(0, st))
	}
	return time.Duration(d.observedNanos.Load()) + session
}

// snapshot renders the tracker for checkpointing, after folding this session's elapsed time
// into the accumulated observation total.
func (d *demandTracker) snapshot(now time.Time) persistedDemand {
	if st := d.startedUnix.Load(); st != 0 {
		d.observedNanos.Add(int64(now.Sub(time.Unix(0, st))))
		d.startedUnix.Store(now.UnixNano()) // this session's time is now accounted for
	}
	p := persistedDemand{
		Version:   demandVer,
		SavedUnix: now.UnixNano(),
		Observed:  d.observedNanos.Load(),
		Total:     float64(d.total.Load()),
		Attrs:     map[string]persistedCounts{},
	}
	d.m.Range(func(k, v any) bool {
		c := v.(*demandCounts)
		eq, rng, reads := float64(c.eq.Load()), float64(c.rng.Load()), float64(c.reads.Load())
		if eq >= demandFloor || rng >= demandFloor || reads >= demandFloor {
			p.Attrs[k.(string)] = persistedCounts{Eq: eq, Rng: rng, Reads: reads}
		}
		return true
	})
	return p
}

// restore loads a checkpoint, decaying it by the time since it was written. Counts are
// ADDED to whatever the tracker already holds so a restore is safe on a live tracker.
func (d *demandTracker) restore(p persistedDemand, halfLife time.Duration, now time.Time) {
	f := decayFactor(now.Sub(time.Unix(0, p.SavedUnix)), halfLife)
	if f <= 0 {
		return
	}
	d.observedNanos.Add(p.Observed)
	d.total.Add(int64(math.Round(p.Total * f)))
	for name, pc := range p.Attrs {
		eq, rng, reads := pc.Eq*f, pc.Rng*f, pc.Reads*f
		if eq < demandFloor && rng < demandFloor && reads < demandFloor {
			continue
		}
		v, _ := d.m.LoadOrStore(name, &demandCounts{})
		c := v.(*demandCounts)
		c.eq.Add(int64(math.Round(eq)))
		c.rng.Add(int64(math.Round(rng)))
		c.reads.Add(int64(math.Round(reads)))
	}
}

// demandHalfLife is the configured half-life, or the default.
func (c *Collection) demandHalfLife() time.Duration {
	if c.demandHalf > 0 {
		return c.demandHalf
	}
	if c.demandHalf < 0 {
		return 0 // explicitly disabled: counters do not decay
	}
	return defaultDemandHalfLife
}

// SaveDemand checkpoints query demand to the collection's directory, decaying it first by
// the time since the last checkpoint. Call it from maintenance; it is a no-op on an
// in-memory collection.
//
// Best-effort by design, as for the other sidecar metadata: a failed write costs the
// interval's demand, not correctness. Nothing reads this file except a later restore, and a
// restore that finds it missing or unparseable simply starts from what it has.
func (c *Collection) SaveDemand() {
	if c.dir == "" {
		return
	}
	now := time.Now()
	if hl := c.demandHalfLife(); hl > 0 {
		if last := c.demand.lastDecayUnix.Load(); last != 0 {
			// Charge decay only once it is large enough to move an integer counter
			// honestly; otherwise leave the clock alone so the elapsed time accumulates
			// into a later step rather than being rounded away now.
			if f := decayFactor(now.Sub(time.Unix(0, last)), hl); f <= demandDecayThreshold {
				c.demand.decay(f)
				c.demand.lastDecayUnix.Store(now.UnixNano())
			}
		} else {
			c.demand.lastDecayUnix.Store(now.UnixNano())
		}
	}
	data, err := json.Marshal(c.demand.snapshot(now))
	if err != nil {
		return
	}
	_ = writeFileSync(filepath.Join(c.dir, demandFile), data)
}

// loadDemand restores the checkpoint written by SaveDemand. Any problem reading it leaves
// the tracker empty, which is exactly the pre-checkpoint behaviour.
func (c *Collection) loadDemand() {
	if c.dir == "" {
		return
	}
	c.demand.lastDecayUnix.Store(time.Now().UnixNano())
	data, err := os.ReadFile(filepath.Join(c.dir, demandFile))
	if err != nil {
		return
	}
	var p persistedDemand
	if json.Unmarshal(data, &p) != nil || p.Version != demandVer || p.SavedUnix == 0 {
		return
	}
	c.demand.restore(p, c.demandHalfLife(), time.Now())
}
