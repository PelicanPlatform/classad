package collections

import (
	"math/rand/v2"
)

// Dictionary training samples: RANDOM records drawn from RECENT segments, budgeted in bytes.
//
// CollectSamples takes the first max visible records in arena order, which for an append-only table
// means the OLDEST records in it. A history table with millions of ads trains its dictionary on the
// first two thousand it ever stored -- the least representative sample of what is arriving next, and
// the one most likely to predate whatever changed the ads' shape. It is also budgeted in RECORD
// COUNT, so what it actually costs depends on how fat the ads happen to be: 2000 real slot ads is
// ~15 MB decompressed and copied, of which TrainDictSize uses the first 112 KB as dictionary content
// (the remainder only trains entropy tables).
//
// Three properties fix that, and they compose:
//
//   - BYTE BUDGET. Stop at the bytes the dictionary can actually consume, rather than at a record
//     count that means a different amount of work per table.
//   - RECENCY WINDOW. Draw only from the newest lookBackBytes of segments. Future writes resemble
//     recent history; an archive's oldest segments may predate the current schema or workload.
//   - RANDOM SELECTION. Within that window, pick records at random rather than taking a prefix, so
//     the sample is representative of the window instead of of whichever segment sorts first.
//
// The pass walks record HEADERS to build its candidate list and decompresses only the records it
// keeps, so the bytes it touches are bounded by the budget rather than by the window. That is what
// keeps a random draw from being the expensive option: random selection is cheap, random
// DECOMPRESSION would not be.

// defaultSampleBytes is the byte budget for a training pass. TrainDictSize uses DefaultDictSize of
// dictionary content and trains entropy tables on everything it is given, so a few times the
// dictionary size supplies both without collecting an unbounded multiple of what is used.
const defaultSampleBytes = 4 * DefaultDictSize

// statSampleBytes is the byte ceiling for a STATISTICAL sample (schema derivation, hot set, fit
// report, index profiling) rather than a dictionary-training one.
//
// Those callers need sample COUNT, not sample bytes: buildAdSchema decides field presence against a
// 0.90 threshold and chooseIntWidth picks a width at a fit percentile, and both get noisy on a few
// dozen records. The dictionary budget (defaultSampleBytes) is sized for what TrainDictSize consumes
// -- a few hundred KB, which on fat slot ads is only a few dozen records -- so reusing it here would
// trade a biased sample for an underpowered one. This ceiling is set high enough that the record
// count is what binds in practice, and exists only so a pathological table cannot read unboundedly.
const statSampleBytes = 64 << 20

// defaultLookBackBytes is how far back a training pass reaches, in segment bytes from the newest.
// Large enough that the window holds many segments' worth of diversity rather than only the newest
// one, and bounded so a table with years of history does not read all of it to build a 112 KB
// dictionary.
const defaultLookBackBytes = 64 << 20

// CollectSamplesRecent returns record samples drawn at random from the newest lookBackBytes of
// segment data, honoring MVCC visibility, bounded by BOTH a record count and a byte budget --
// whichever binds first.
//
// Both bounds exist because they answer different questions and callers supply different ones.
// maxRecords is what every existing caller has (RetrainDict's sampleMax), and keeping it a count
// means switching to this sampler cannot change what an existing call collects by reinterpreting its
// units. maxBytes is what the work actually scales with, since a record's size varies by two orders
// of magnitude across tables.
//
// maxRecords <= 0 means no count bound; maxBytes <= 0 uses defaultSampleBytes; lookBackBytes <= 0
// uses defaultLookBackBytes. Samples are flattened to inline wire exactly as CollectSamples does, so
// the two are interchangeable as TrainDict input.
//
// Returns nil when the collection holds no visible records.
func (c *Collection) CollectSamplesRecent(maxRecords, maxBytes, lookBackBytes int) [][]byte {
	return c.collectSamplesRecent(maxRecords, maxBytes, lookBackBytes, rand.Uint64)
}

// CollectSamplesRecentN returns up to maxRecords records drawn at random from the recent window,
// with no meaningful byte bound -- the sampler for decisions that need a representative COUNT of
// records rather than a fixed volume of bytes. See statSampleBytes.
func (c *Collection) CollectSamplesRecentN(maxRecords int) [][]byte {
	return c.CollectSamplesRecent(maxRecords, statSampleBytes, 0)
}

// collectSamplesRecent is CollectSamplesRecent with an injectable random source, so a test can pin
// the draw. rnd returns a uniformly distributed uint64.
func (c *Collection) collectSamplesRecent(maxRecords, maxBytes, lookBackBytes int, rnd func() uint64) [][]byte {
	if maxBytes <= 0 {
		maxBytes = defaultSampleBytes
	}
	if lookBackBytes <= 0 {
		lookBackBytes = defaultLookBackBytes
	}
	if len(c.shards) == 0 {
		return nil
	}
	// Split the budget across shards so one shard cannot spend all of it; a shard that cannot fill
	// its share simply contributes less.
	share := maxBytes / len(c.shards)
	if share <= 0 {
		share = maxBytes
	}
	recShare := maxRecords / len(c.shards)
	if maxRecords > 0 && recShare <= 0 {
		recShare = 1
	}
	var out [][]byte
	for _, sh := range c.shards {
		out = append(out, sh.recentRecordSamples(c, recShare, share, lookBackBytes, rnd)...)
	}
	return out
}

// recordSite is one candidate record: which window it lives in and where.
type recordSite struct {
	win int
	off uint32
}

// recentRecordSamples draws random visible records from this shard's newest segments, up to
// maxBytes of flattened content.
func (sh *shard) recentRecordSamples(c *Collection, maxRecords, maxBytes, lookBackBytes int, rnd func() uint64) [][]byte {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)

	// Candidate sites, newest window first, until the look-back budget is spent. Only headers are
	// read here -- no decompression -- so building the list is cheap even over a large window.
	var sites []recordSite
	seen := 0
	for i := len(wins) - 1; i >= 0; i-- {
		w := wins[i]
		if w.used == 0 {
			continue
		}
		for off := 0; off < w.used; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if !recIsMarker(w.data, o) && recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0 {
				sites = append(sites, recordSite{i, o})
			}
			off += int(total)
		}
		seen += w.used
		if seen >= lookBackBytes {
			break // far enough back: older segments predate what future writes will look like
		}
	}
	if len(sites) == 0 {
		return nil
	}
	// Partial Fisher-Yates: draw distinct sites at random and decompress each as it is drawn,
	// stopping at the byte budget. Only the drawn records are decompressed.
	var out [][]byte
	var buf []byte
	bytes := 0
	for n := len(sites); n > 0 && bytes < maxBytes; n-- {
		if maxRecords > 0 && len(out) >= maxRecords {
			break
		}
		j := int(rnd() % uint64(n))
		sites[j], sites[n-1] = sites[n-1], sites[j]
		s := sites[n-1]
		w := wins[s.win]
		ww, err := w.codec.Decompress(buf[:0], recAd(w.data, s.off))
		if err != nil {
			continue
		}
		buf = ww
		iw := c.wireToInline(w.dict(), ww)
		cp := make([]byte, len(iw))
		copy(cp, iw)
		out = append(out, cp)
		bytes += len(cp)
	}
	return out
}
