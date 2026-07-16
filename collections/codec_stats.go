package collections

import "time"

// CodecStats reports the storage codec's state and effectiveness, for the diagnostic
// question "is compression on, when was the dictionary last (re)trained, and how well is
// it compressing?". A collection defaults to the identity codec (no compression) until
// Options.Codec or RetrainDict switches it, so a Ratio near 1.0 with Codec "identity"
// means compression was never enabled.
type CodecStats struct {
	// Codec is the current codec name: "identity" (no compression), "zstd", or "zstd+dict".
	Codec string `json:"codec"`
	// DictBytes is the trained ZSTD dictionary size (0 if none). Set by RetrainDict.
	DictBytes int64 `json:"dictBytes"`
	// LastRetrain is when RetrainDict last succeeded in this process (zero if never; note
	// a persistent collection recovers its codec but not this in-process timestamp).
	LastRetrain time.Time `json:"lastRetrain,omitempty"`
	// SampleRecords is how many live records were sampled for the ratio below.
	SampleRecords int `json:"sampleRecords"`
	// CompressedBytes / UncompressedBytes are the sampled records' stored (compressed) and
	// decompressed sizes; Ratio is UncompressedBytes/CompressedBytes (1.0 = no gain).
	CompressedBytes   int64   `json:"compressedBytes"`
	UncompressedBytes int64   `json:"uncompressedBytes"`
	Ratio             float64 `json:"ratio"`
}

// CodecStats measures the storage codec's effectiveness over a sample of up to sampleMax
// live records: it decompresses each and compares the stored (compressed) size to the
// decompressed size. It takes each shard's read lock briefly, so it is safe alongside
// readers and writers.
func (c *Collection) CodecStats(sampleMax int) CodecStats {
	if sampleMax <= 0 {
		sampleMax = maxDistinctSample
	}
	cs := CodecStats{Codec: c.currentCodec().Name(), DictBytes: c.lastDictBytes.Load()}
	if n := c.lastRetrainUnix.Load(); n > 0 {
		cs.LastRetrain = time.Unix(0, n)
	}
	var buf []byte // reused decompression scratch
	n := 0
	for _, sh := range c.shards {
		if n >= sampleMax {
			break
		}
		s0, wins := sh.snapshot()
		forEachVisible(s0, wins, func(ad []byte, codec Codec) bool {
			w, err := codec.Decompress(buf[:0], ad)
			if err != nil {
				return true
			}
			buf = w
			cs.CompressedBytes += int64(len(ad))
			cs.UncompressedBytes += int64(len(w))
			n++
			return n < sampleMax
		})
		releaseWindows(wins)
	}
	cs.SampleRecords = n
	if cs.CompressedBytes > 0 {
		cs.Ratio = float64(cs.UncompressedBytes) / float64(cs.CompressedBytes)
	}
	return cs
}
