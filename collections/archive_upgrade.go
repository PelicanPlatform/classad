package collections

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Re-encoding old archive segments under a newer dictionary.
//
// RetrainDict installs a new dictionary for new writes and stops there: old segments keep
// the dictionary they were written under, and recovery reconstructs every registered one, so
// they stay readable forever. Bringing them onto the new dictionary is an optimization, and
// at archive scale an expensive one -- rewriting the whole archive is hours of I/O and a
// flushed page cache.
//
// Worse, the gain can be negative. A dictionary trained on a sample of recent ads is fit to
// the CURRENT distribution; older segments written when the distribution differed may
// compress worse under it. So this pass does not assume improvement -- it measures, per
// segment, and re-encodes only where the measurement says it pays.
//
// A measurement that says "no" has to be remembered, or every pass re-measures the same
// segments forever. The verdict is recorded against the dictionary generation it was made
// under and persisted beside the shard, so a restart does not restart the sampling either. A
// later retrain produces a new generation, which invalidates the old verdicts exactly as it
// should -- the answer may genuinely differ under a different dictionary.

// UpgradeOptions tunes a codec-upgrade pass. The zero value is usable.
type UpgradeOptions struct {
	// MinGainFrac is the fraction of a segment's bytes a re-encode must be projected to
	// save before it is worth doing. Default defaultMinGainFrac.
	MinGainFrac float64
	// SampleRecords is how many records are decompressed and re-compressed to project the
	// gain. The estimate only has to separate "clearly worth it" from "not", so this stays
	// small; the cost of being wrong is one segment, not the archive.
	SampleRecords int
	// MaxBytesPerPass bounds the source bytes one pass re-encodes, as for merging. Zero
	// uses the default.
	MaxBytesPerPass int64
	// MaxSegments bounds how many segments one pass upgrades.
	MaxSegments int
}

const (
	defaultMinGainFrac    = 0.10
	defaultSampleRecords  = 128
	defaultUpgradeBytes   = 1 << 30
	defaultUpgradeSegs    = 32
	upgradeVerdictsFile   = "upgrade.json"
	upgradeVerdictVersion = 1
)

func (o UpgradeOptions) withDefaults() UpgradeOptions {
	if o.MinGainFrac <= 0 {
		o.MinGainFrac = defaultMinGainFrac
	}
	if o.SampleRecords <= 0 {
		o.SampleRecords = defaultSampleRecords
	}
	if o.MaxBytesPerPass <= 0 {
		o.MaxBytesPerPass = defaultUpgradeBytes
	}
	if o.MaxSegments <= 0 {
		o.MaxSegments = defaultUpgradeSegs
	}
	return o
}

// upgradeVerdicts remembers, per segment file, the dictionary generation whose re-encode was
// measured and rejected. Keyed by file name because that is what survives a restart.
type upgradeVerdicts struct {
	Version  int               `json:"version"`
	Rejected map[string]uint32 `json:"rejected"` // segment file -> dict id judged not worth it
}

// UpgradeCodecPass re-encodes sealed segments that are still on an older dictionary, where
// measurement says the newer one compresses them meaningfully better. Returns the number of
// segments upgraded.
//
// Segments measured as no better (or worse) are recorded and not measured again until the
// dictionary changes, so a pass over a fully-evaluated archive costs a directory read.
func (a *Archive) UpgradeCodecPass(opts UpgradeOptions) int {
	return a.c.upgradeCodecPass(opts.withDefaults())
}

func (c *Collection) upgradeCodecPass(opts UpgradeOptions) int {
	if !c.appendOnly() || c.dir == "" {
		return 0
	}
	c.maintMu.Lock()
	defer c.maintMu.Unlock()

	sh := c.shards[0]
	if sh.segDir == "" {
		return 0
	}
	target := c.currentCodec()
	targetID, ok := c.dicts.idFor(target)
	if !ok {
		return 0
	}
	verdicts := loadUpgradeVerdicts(sh.segDir)

	// Candidates: sealed, non-empty, not already on the target codec, and not already
	// judged against it.
	sh.mu.RLock()
	var cand []*segment
	for _, s := range sh.segs {
		if s == nil || s == sh.act || s.used == 0 || s.codec == target {
			continue
		}
		if verdicts.Rejected[filepath.Base(s.path)] == targetID {
			continue
		}
		cand = append(cand, s)
	}
	sh.mu.RUnlock()

	upgraded, changed := 0, false
	var moved int64
	for _, src := range cand {
		if upgraded >= opts.MaxSegments || moved >= opts.MaxBytesPerPass {
			break
		}
		gain, ok := c.projectRecodeGain(src, target, opts.SampleRecords)
		if !ok || gain < opts.MinGainFrac {
			// Measured and not worth it. Record it against this dictionary so the next
			// pass does not pay the sampling again; a later retrain clears the verdict by
			// changing the id.
			verdicts.Rejected[filepath.Base(src.path)] = targetID
			changed = true
			continue
		}
		sz := int64(src.used)
		if !c.recodeSegment(sh, src, target) {
			continue
		}
		upgraded++
		moved += sz
	}
	if changed {
		saveUpgradeVerdicts(sh.segDir, verdicts)
	}
	if upgraded > 0 {
		c.Reindex() // the re-encoded segments need their sidecars rebuilt
		c.pruneDicts()
	}
	return upgraded
}

// projectRecodeGain estimates the fraction of a segment's bytes re-encoding under target
// would save, by recompressing a sample of its records. ok is false if the segment cannot be
// sampled at all.
//
// The sample is taken from the front of the segment; records within one segment are adjacent
// in time and therefore similar in shape, which is the property that makes a small sample
// informative here and would not hold across an archive.
func (c *Collection) projectRecodeGain(src *segment, target Codec, sample int) (float64, bool) {
	if src.columnarized() || src.colDamaged.Load() {
		return 0, false
	}
	var oldBytes, newBytes int64
	n := 0
	var dbuf []byte
	for off := 0; off < src.used && n < sample; {
		o := uint32(off)
		rl := recTotalLen(src.data, o)
		if rl == 0 {
			break
		}
		off += int(rl)
		if recIsMarker(src.data, o) {
			continue
		}
		// Stored bytes on purpose: this estimates what RECODING the segment's records would gain,
		// so it must weigh them as they are stored. A columnarized segment is not a recode
		// candidate -- its records are remnants, and re-encoding them would measure the wrong thing.
		stored := recAd(src.data, o)
		raw, err := src.codec.Decompress(dbuf[:0], stored)
		if err != nil {
			return 0, false
		}
		dbuf = raw
		oldBytes += int64(len(stored))
		newBytes += int64(len(target.Compress(nil, raw)))
		n++
	}
	if n == 0 || oldBytes == 0 {
		return 0, false
	}
	return float64(oldBytes-newBytes) / float64(oldBytes), true
}

// recodeSegment re-encodes one segment under target and swaps it in, reusing the append-only
// reseal path (which preserves seq, key, and time markers, so watch cursors stay valid).
func (c *Collection) recodeSegment(sh *shard, src *segment, target Codec) bool {
	newseg := c.resealOneSegment(sh, src, target)
	if newseg == nil {
		return false
	}
	sh.mu.Lock()
	if int(src.id) >= len(sh.segs) || sh.segs[src.id] != src {
		sh.mu.Unlock()
		newseg.retire()
		newseg.reapAndHook()
		return false
	}
	sh.segs[src.id] = newseg
	retire := src.retire()
	sh.mu.Unlock()
	if retire {
		src.reapAndHook()
	}
	return true
}

func loadUpgradeVerdicts(dir string) *upgradeVerdicts {
	v := &upgradeVerdicts{Version: upgradeVerdictVersion, Rejected: map[string]uint32{}}
	data, err := os.ReadFile(filepath.Join(dir, upgradeVerdictsFile))
	if err != nil {
		return v
	}
	var got upgradeVerdicts
	if json.Unmarshal(data, &got) != nil || got.Version != upgradeVerdictVersion || got.Rejected == nil {
		return v // unreadable or from another version: re-measure rather than guess
	}
	return &got
}

func saveUpgradeVerdicts(dir string, v *upgradeVerdicts) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = writeFileSync(filepath.Join(dir, upgradeVerdictsFile), data) // best-effort
}
