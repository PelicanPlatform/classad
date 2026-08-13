package collections

import (
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Rebuilding the derived schema, and deciding when to.
//
// The schema is recovered by sampling once, at first enable, and then deliberately held stable:
// every segment's columnar block is built against a particular schema, so swapping the current
// schema mid-flight would leave earlier blocks unmatched. That stability is right for routine
// maintenance and wrong forever -- a workload drifts, and nothing here noticed. SchemaFit
// measures the drift; ReschemaScan acts on it.

// SchemaFieldFit reports how well one schema field still fits the data, measured against a fresh
// sample rather than the one the schema was built from.
//
// A field escapes when its value is not in the fixed slot -- either the attribute is absent
// (Missing) or it is present but unstorable in the slot: a different kind, or an int too wide for
// the width chosen (Escaped - Missing). Escapes are what the schema exists to avoid: a queried
// column that escapes forces the block's cold-stream decompression and a cold-tail walk, which is
// the slow path the accelerator was meant to skip.
//
// Rates are fractions of Sampled (see SchemaFit), in [0,1].
type SchemaFieldFit struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Width is the fixed slot's size in bytes (0 for a bit-packed bool).
	Width int  `json:"width,omitempty"`
	Hot   bool `json:"hot,omitempty"`
	// Escaped is the fraction of sampled records whose value was not in the fixed slot, for
	// any reason. This is the number that matters for speed.
	Escaped float64 `json:"escaped"`
	// Missing is the fraction absent from the record entirely -- an escape that no width or
	// kind change would fix. Escaped-Missing is the part a re-schema could recover.
	Missing float64 `json:"missing"`
}

// SchemaFit measures the current schema against a fresh sample of up to sampleMax records,
// reporting per-field escape rates and the number of records actually sampled.
//
// It reads samples and re-runs the encoder's storability test on each; it encodes nothing and
// writes nothing. Returns nil when the accelerator is not enabled (there is no schema to judge)
// or there is nothing to sample.
func (c *Collection) SchemaFit(sampleMax int) ([]SchemaFieldFit, int) {
	st := c.schemaScan.Load()
	if st == nil {
		return nil, 0
	}
	samples := c.CollectSamplesRecentN(sampleMax)
	if c.inline {
		interned := make([][]byte, 0, len(samples))
		for _, w := range samples {
			if iw, ok := c.recordToInterned(nil, w); ok {
				interned = append(interned, iw)
			}
		}
		samples = interned
	}
	if len(samples) == 0 {
		return nil, 0
	}
	s := st.schema
	hot := make(map[int]bool, len(st.hot))
	for _, idx := range st.hot {
		hot[idx] = true
	}
	missing := make([]int, len(s.fields))
	escaped := make([]int, len(s.fields))
	present := make([]bool, len(s.fields)) // reused per record
	stored := make([]bool, len(s.fields))  // reused per record
	for _, w := range samples {
		for i := range present {
			present[i] = false
			stored[i] = false
		}
		wire.Ad(w).ForEach(func(id uint32, node []byte) bool {
			idx, ok := s.byID[id]
			if !ok {
				return true // not a schema field: lives in the cold tail either way
			}
			present[idx] = true
			// The same test adSchema.encode applies when deciding the fixed slot.
			f := &s.fields[idx]
			k, lit := nodeKind(node)
			if k == f.kind && (f.kind != akInt || intFits(lit.Int, f.width, f.unsigned)) {
				stored[idx] = true
			}
			return true
		})
		for i := range s.fields {
			if !present[i] {
				missing[i]++
			}
			if !stored[i] {
				escaped[i]++
			}
		}
	}
	n := float64(len(samples))
	out := make([]SchemaFieldFit, 0, len(s.fields))
	for i, f := range s.fields {
		name, ok := c.schemaFieldName(f.id)
		if !ok {
			name = "?"
		}
		out = append(out, SchemaFieldFit{
			Name:    name,
			Kind:    f.kind.String(),
			Width:   f.width,
			Hot:     hot[i],
			Escaped: float64(escaped[i]) / n,
			Missing: float64(missing[i]) / n,
		})
	}
	return out, len(samples)
}

// ReschemaScan derives a NEW schema from a fresh sample and rebuilds every sealed segment's
// columnar block against it, replacing the schema that BuildAndEnableSchemaScan pinned at first
// enable. This is the deliberate, heavy operation that routine maintenance refuses to do: it
// re-encodes and re-persists a block per sealed segment, so its cost scales with the whole
// table, not with what changed.
//
// Use it when SchemaFit shows the schema no longer matching the data -- escapes climbing on a
// queried column, or a field that has become common since. Returns false if the accelerator
// cannot run here (encryption at rest) or there was nothing to sample, leaving the existing
// schema in place.
//
// Existing blocks are dropped first: EnableSchemaScan builds only where a segment has none, so
// without that it would keep every old block and the new schema would match none of them.
// Queries during the rebuild take the row path, which is correct but slower.
func (c *Collection) ReschemaScan(sampleMax, hotTopN int) bool {
	// Encryption is no longer exclusive with the accelerator; see EnableSchemaScan.
	s, hot, ok := c.deriveSchema(sampleMax, hotTopN)
	if !ok {
		return false
	}
	// Stand down before dropping blocks, so a concurrent query sees "no accelerator" (row
	// path) rather than a live schema with no blocks behind it.
	c.schemaScan.Store(nil)
	for _, sh := range c.shards {
		sh.mu.RLock()
		act := sh.act
		segs := make([]*segment, 0, len(sh.segs))
		for _, seg := range sh.segs {
			if seg != nil && seg != act && seg.used > 0 {
				segs = append(segs, seg)
			}
		}
		sh.mu.RUnlock()
		for _, seg := range segs {
			// A scan that already loaded the old block keeps reading it safely; only new
			// lookups see nil and fall back until the rebuild republishes.
			seg.colblk.Store(nil)
		}
	}
	c.EnableSchemaScan(s, hot)
	return true
}
