package collections

import "time"

// Rebuilding the derived schema, and deciding when to.
//
// The schema is recovered by sampling at first enable, and routine maintenance re-derives it and
// adopts the re-derivation when the difference is material (see reviewBaseSchema). SchemaFit measures
// the drift for an operator; ReschemaScan adopts a fresh derivation unconditionally.

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
	fc := measureSchemaFit(s, samples)
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
			Escaped: fc.rate(fc.n - fc.stored[i]),
			Missing: fc.rate(fc.n - fc.present[i]),
		})
	}
	return out, len(samples)
}

// ReschemaScan derives a NEW schema from a fresh sample and adopts it as the committed one,
// unconditionally -- the deliberate counterpart of the maintenance review, which adopts a
// re-derivation only when the difference is material. Returns false if there was nothing to sample,
// leaving the existing schema in place.
//
// What it does to existing segments:
//
//   - Sidecar blocks (a segment whose records still hold every attribute) are rebuilt against the
//     new schema before this returns, as they always were. Each block is replaced in place, so a
//     concurrent query reads either the old block or the new one, never neither.
//   - Columnarized segments hold their schema'd attributes ONLY in their payload, so adopting a new
//     schema there means rewriting the segment. That is NOT done synchronously for the whole table:
//     at archive scale it is hours of I/O behind one admin command. One budgeted pass runs here
//     (ColumnarSegmentBudget segments) and routine maintenance converges the rest, the same budget per
//     pass. Until a segment is rewritten it is read under the schema it was written with -- correct,
//     only not yet accelerated for the new fields. SchemaScanInfo.StaleSegments reports how many
//     remain.
//
// The new schema is committed to disk before any segment is touched, so a restart mid-convergence
// resumes converging toward it rather than reverting to the old one.
func (c *Collection) ReschemaScan(sampleMax, hotTopN int) bool {
	// Encryption is no longer exclusive with the accelerator; see EnableSchemaScan.
	c.schemaMu.Lock()
	s, hot, ok := c.deriveSchema(sampleMax, hotTopN)
	if ok {
		ok = c.installSchemaScanLocked(s, hot, "schema rebuild")
		c.reviewed = schemaReviewMark{at: time.Now(), require: c.SchemaRequireAttrs()}
	}
	c.schemaMu.Unlock()
	if !ok {
		return false
	}
	c.schemaScanPass()
	c.recolumnarizeStale()
	c.rebuildStaleSidecarsAll()
	return true
}
