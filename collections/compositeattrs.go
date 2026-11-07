package collections

// CompositeAttrs reports the attribute tuples this collection indexes compositely, each in
// the order its composite was configured.
//
// It is the composite counterpart of IndexedAttrs, and exists for the same reason: a caller
// deciding whether to build a constraint an index can actually answer cannot otherwise see
// the index set. The distinction matters more here than it does for a single attribute. A
// composite answers the FULL TUPLE only, so a conjunction pinning every component to one
// value is resolved by one lookup, while the same conjunction without a composite behind it
// becomes one probe per component whose postings must then be intersected. A caller that
// shapes its constraint for a composite it does not have has made the query slower, not
// faster, which is why availability is reported rather than assumed.
//
// Order is part of a composite's identity -- (ClusterId, ProcId) and (ProcId, ClusterId) are
// two indexes -- so both can appear here. Matching a conjunction does not depend on that
// order: a composite applies when the conjunction pins each of its components, whichever
// order they were written in.
//
// The returned slices are copies; the caller may keep or modify them.
func (c *Collection) CompositeAttrs() [][]string {
	spec := c.spec.Load()
	out := make([][]string, 0, len(spec.comps))
	for _, cs := range spec.comps {
		out = append(out, append([]string(nil), cs.names...))
	}
	return out
}

// CompositeAttrs reports the archive's composite indexes. See [Collection.CompositeAttrs].
//
// As with a single-attribute index, configuration is not the same as a built index: an
// archive's postings live in a sealed segment's sidecar, so a composite configured on an
// archive with nothing sealed answers nothing yet. HasSealedSidecar is what reports that,
// and a caller pricing a probe needs both.
func (a *Archive) CompositeAttrs() [][]string { return a.c.CompositeAttrs() }
