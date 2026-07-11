package collections

import "github.com/PelicanPlatform/classad/classad"

// ProjectionChecksum returns a 64-bit FNV-1a checksum over the given attributes'
// literal expression text in ad, taken in the order given and delimited so two
// distinct projections cannot alias. A missing attribute contributes a fixed
// marker, so "absent" is distinguished from every present value.
//
// This is the standalone, caller-supplied-attrs form of an ordered index's
// cluster Signature (OrderSpec.Cluster). Unlike that index-time signature -- whose
// attribute set is fixed when the index is opened -- ProjectionChecksum takes the
// projection at call time, for callers whose significant attributes vary per query
// (e.g. a schedd whose negotiator sends the significant-attribute set in each
// negotiation header).
//
// It hashes each attribute's *expression text* rather than its evaluated value,
// matching HTCondor autocluster semantics: two job ads whose significant
// attributes are textually identical (same Requirements expression, same
// RequestMemory literal, ...) hash equal, so a run-length fold over a
// priority-ordered stream folds them into one resource request with a
// stored-value compare. A 64-bit collision could merge two adjacent runs; over
// the projected value bytes that is negligible.
func ProjectionChecksum(ad *classad.ClassAd, attrs []string) uint64 {
	const off = 1469598103934665603
	h := uint64(off)
	for _, name := range attrs {
		if e, ok := ad.Lookup(name); ok {
			h = fnv1a(h, e.String())
		} else {
			h = fnv1aByte(h, 0xff) // absent marker (distinct from any present text)
		}
		h = fnv1aByte(h, 0) // field delimiter so ["a","bc"] and ["ab","c"] differ
	}
	return h
}
