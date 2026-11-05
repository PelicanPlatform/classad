package collections

import (
	"strconv"
	"strings"
	"sync"
)

// Giving every segment that describes the SAME schema the same schema OBJECT.
//
// A sidecar block is built in this process and points at the live committed schema, so every
// sealed segment of a table shares one *adSchema. A columnarized segment does not: its schema
// travels inside its own payload and comes back from readAdSchema as a fresh object per segment.
// Both describe the same fields; only the pointers differ.
//
// That distinction is invisible until something keys on the pointer, and the scan does, in the
// places where re-deriving per segment is the cost being avoided:
//
//   - prunePlan.resolve caches the zone-prunable probe analysis under `s == p.schema`. The
//     analysis depends only on the query and the schema, and a scan visits one window per
//     SEGMENT, so on a table whose segments each carry their own schema object the cache misses
//     every time and the analysis runs once per segment -- measured at 226 analyses over 226
//     segments, which is exactly the repetition the cache exists to remove (11% of a scan's time
//     and 44% of its allocations when it was first measured).
//
//   - a refresh pass asks whether each sealed segment's block was built against the live schema.
//     Under pointer identity a columnarized segment always answers no, so it reads as orphaned
//     onto the fallback while being perfectly current.
//
// Both were green in test because every fixture that measures them is an IN-MEMORY collection,
// and until recently an in-memory collection never columnarized -- so the one backing that could
// observe the problem was the one backing that could not produce it.
//
// So loaded schemas are canonicalized here: a schema whose fields match one already seen becomes
// that object. Equality is sameSchemaLayout's -- the ordered (id, kind, width, unsigned) tuples --
// which is deliberately the SAME notion of "the same schema" that staleness already uses, so
// canonicalizing can never make a segment look current to one check and stale to another.
//
// It is an optimization and never a correctness dependency: a missed canonicalization costs a
// recomputation, exactly as today, and a schema is only ever replaced by one that compares equal.

// schemaCanonMax bounds the table. One entry per DISTINCT schema the collection has seen, so a
// steady table holds one and a drifting one gains an entry per adopted change. The cap is there so
// a pathologically churning schema cannot accumulate forever; past it the table is dropped and
// refills from the live schema, which costs a round of recomputation and nothing else.
const schemaCanonMax = 64

// schemaCanon maps a schema's layout key to the one object the collection uses for it.
type schemaCanon struct {
	mu sync.Mutex
	m  map[string]*adSchema
}

// schemaCanonKey renders a schema's layout as a comparison key. It carries exactly what
// sameSchemaLayout compares, in order, so two schemas share a key if and only if that function
// calls them the same.
func schemaCanonKey(s *adSchema) string {
	var b strings.Builder
	b.Grow(len(s.fields) * 12)
	for i := range s.fields {
		f := &s.fields[i]
		b.WriteString(strconv.FormatUint(uint64(f.id), 36))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(int(f.kind)))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(f.width))
		if f.unsigned {
			b.WriteByte('u')
		}
		b.WriteByte(';')
	}
	return b.String()
}

// adoptCanonSchema makes s the canonical object for its layout, displacing any earlier object with
// the same layout.
//
// Called when a schema is COMMITTED, so that a segment loaded afterwards canonicalizes onto the
// live object rather than onto some earlier equal one. Displacing rather than keeping the first is
// what makes the live schema the one every later load converges on; segments canonicalized to the
// displaced object keep it, and remain correct, because the two compare equal everywhere.
func (c *Collection) adoptCanonSchema(s *adSchema) {
	if s == nil || len(s.fields) == 0 {
		return
	}
	c.canon.mu.Lock()
	defer c.canon.mu.Unlock()
	if len(c.canon.m) >= schemaCanonMax {
		c.canon.m = nil // see schemaCanonMax
	}
	if c.canon.m == nil {
		c.canon.m = make(map[string]*adSchema, 4)
	}
	c.canon.m[schemaCanonKey(s)] = s
}

// canonSchema returns the collection's object for s's layout, registering s as that object if this
// is the first schema with this layout. nil and empty schemas pass through untouched.
func (c *Collection) canonSchema(s *adSchema) *adSchema {
	if s == nil || len(s.fields) == 0 {
		return s
	}
	key := schemaCanonKey(s)
	c.canon.mu.Lock()
	defer c.canon.mu.Unlock()
	if got, ok := c.canon.m[key]; ok {
		return got
	}
	if len(c.canon.m) >= schemaCanonMax {
		c.canon.m = nil
	}
	if c.canon.m == nil {
		c.canon.m = make(map[string]*adSchema, 4)
	}
	c.canon.m[key] = s
	return s
}

// canonColSchemas rewrites a freshly loaded payload's schema objects to the collection's canonical
// ones -- the base schema every block of the segment shares, and each group schema.
//
// The block's LAYOUT is not touched and does not need to be: it was resolved from a schema that
// compares equal to the canonical one field for field, so it describes the same bytes. Only the
// identity of the object changes.
func (c *Collection) canonColSchemas(cs *colSegment) {
	if cs == nil || c == nil {
		return
	}
	if s := cs.schema(); s != nil {
		if canon := c.canonSchema(s); canon != s {
			for _, b := range cs.blocks {
				if b != nil && b.schema == s {
					b.schema = canon
				}
			}
		}
	}
	for _, g := range cs.groups {
		if g == nil || g.schema == nil {
			continue
		}
		gs := g.schema
		canon := c.canonSchema(gs)
		if canon == gs {
			continue
		}
		g.schema = canon
		for _, gb := range g.blocks {
			if gb.blk != nil && gb.blk.schema == gs {
				gb.blk.schema = canon
			}
		}
	}
}
