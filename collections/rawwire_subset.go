package collections

import (
	"iter"
	"slices"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// ScanRawWire yields each ad as a WIRE-FORM ROW: a self-contained inline-names
// ad holding only the selected entries, assembled by slice copies from the
// stored bytes (see wire.AppendAdSubsetInline) -- nothing is decoded or
// rendered here. It is the relay scan for shipping a persistent table's ads
// across a process boundary (dbrpc) with the old-ClassAd render deferred to the
// far edge (RenderRawAdInline); the consumer needs no intern table and no data
// key (at-rest-encrypted values are opened during assembly).
//
// projection restricts the entries to the named attributes (case-insensitive;
// MyType/TargetType always ship so the ad stays typed); empty means every
// entry. redact drops private attributes -- pre-pruned from a projection, or
// tested per entry with a byte-gated predicate for the whole-ad case. The
// yielded row aliases a buffer reused across the iteration.
//
// An IN-MEMORY collection serves this scan too. Its records are not self-contained as stored
// -- their keys are ids against the shared intern table -- but the difference from the
// persistent form is only how names are spelled, so the row is assembled by rewriting those
// ids back to names as it is built (see appendWireRow). Refusing RAM tables here was a
// property of the first implementation, not of what they store.
func (c *Collection) ScanRawWire(projection []string, redact bool) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		sel := c.newWireSubsetSelector(projection, redact)
		// Decompression dominates a compressed scan and parallelizes across
		// segments; the serial path below is the fallback (small scans, no budget,
		// encrypted stores).
		if c.runParallelWireScan(sel, sel.neededCount(), yield) {
			return
		}
		emit := c.yieldWireRow(yield, sel)
		q := queryPlan{}
		for _, sh := range c.shards {
			if !c.scanShard(sh, q, emit) {
				return
			}
		}
	}
}

// QueryRawWire is ScanRawWire restricted to ads matching q.
func (c *Collection) QueryRawWire(q *vm.Query, projection []string, redact bool) iter.Seq[[]byte] {
	return c.QueryRawWireStats(q, projection, redact, nil)
}

// QueryRawWireStats is QueryRawWire that also fills stats (may be nil) with the per-scan
// work breakdown. Iterate the returned sequence to completion (or stop early) before
// reading stats; a partial iteration leaves partial counts.
func (c *Collection) QueryRawWireStats(q *vm.Query, projection []string, redact bool, stats *ScanStats) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		sel := c.newWireSubsetSelector(projection, redact)
		// The projection plan names the type fields as well: a columnar segment builds the record
		// from the columns the plan asks for, and sel.keep ships MyType/TargetType whatever the
		// caller projected -- so a plan without them produced typeless rows off columnar segments.
		var proj *projPlan
		if len(projection) > 0 {
			proj = c.newProjPlan(append(slices.Clip(projection), rawTypeFieldNames...))
		}
		qp := c.newQueryPlan(q, proj, stats)
		rp := c.planReadQuery(qp)
		// A full-scan constrained read fans out across segments, on the same terms the full-decode
		// Query does: no index plan to walk (the fan-out visits whole segments), and no reverse
		// order to preserve. The helper declines when the scan is too small or the worker budget
		// is busy, and then the serial run below covers it -- the read is planned once either way.
		if c.queryPar > 1 && !rp.disjunctive() && len(rp.usable) == 0 && !c.reverseScan {
			if c.runParallelWireQuery(qp, sel, sel.neededCount(), yield) {
				return
			}
		}
		c.runQuery(qp, rp, c.yieldWireRow(yield, sel))
	}
}

// neededCount is the hot fast path's early-exit target: the projected names
// plus the two type fields. 0 (whole-ad) disables the shortcut.
func (sel *wireSubsetSelector) neededCount() int {
	if sel.wantAll {
		return 0
	}
	return len(sel.names) + 2
}

func (c *Collection) yieldWireRow(yield func([]byte) bool, sel *wireSubsetSelector) scanEmit {
	var scratch []byte
	needed := sel.neededCount()
	var narrow wireNarrower
	asm := c.newWireRowAssembler(sel)
	return func(w []byte, dict *segDictHandle) bool {
		// Narrow in ID SPACE first, so the conversion below only converts what the caller asked
		// for (see narrowProjected).
		w = c.narrowProjected(&narrow, sel, dict, w)
		// A per-segment INTERNED record (a sealed segment of a persistent store) is flattened
		// here, because its ids resolve against that segment's dict rather than against
		// anything appendWireRow can reach. An in-memory record is left alone -- dict is nil --
		// and its global ids are resolved inside the assembler instead. Both this conversion
		// and the assembler below take the key only when the reader is entitled to it: an
		// inline segment converts nothing, so for that case the assembler's key is the ONLY
		// thing standing between a redacted read and a sealed value.
		w = c.wireToInlineRead(dict, w, sel.redact)
		out, ok := asm.appendRow(scratch[:0], wire.Ad(w), needed, c.renderKey(sel.redact))
		scratch = out
		if !ok {
			return true // malformed (or unresolvable) record: skip, keep scanning
		}
		return yield(out)
	}
}

// wireSubsetSelector decides, per entry, whether a wire-form row keeps it: the
// same name-hash-first matching the projected text scans use, with private
// names pre-pruned from a redacted projection so redaction costs nothing per
// entry on the projected path.
//
// Immutable once built, which is what lets the parallel scan share one across its workers. Any
// per-scan state that has to be written belongs in the per-worker wireNarrower, not here.
type wireSubsetSelector struct {
	wantAll bool
	redact  bool
	// chaseRefs closes the selection under the emitted expressions' own attribute
	// references, per record (see rawwire_refs.go). It changes what every path below
	// assembles and narrows, which is why it lives on the selector all of them already
	// carry rather than being threaded separately through each.
	chaseRefs bool
	names     []string
	hashes    []uint32
}

func (c *Collection) newWireSubsetSelector(projection []string, redact bool) *wireSubsetSelector {
	return c.newWireSubsetSelectorRefs(projection, redact, false)
}

func (c *Collection) newWireSubsetSelectorRefs(projection []string, redact, chaseRefs bool) *wireSubsetSelector {
	sel := &wireSubsetSelector{wantAll: len(projection) == 0, redact: redact, chaseRefs: chaseRefs}
	seen := make(map[string]struct{}, len(projection))
	for _, name := range projection {
		fold := strings.ToLower(name)
		if _, dup := seen[fold]; dup {
			continue
		}
		seen[fold] = struct{}{}
		if redact && classad.IsPrivateAttribute(name) {
			continue
		}
		sel.names = append(sel.names, name)
		sel.hashes = append(sel.hashes, wire.NameHash32(name))
	}
	if len(projection) > 0 {
		// Projection demand steers RefreshHotSet exactly as the text scans do.
		c.demand.recordReads(projection)
		c.demand.recordReads(rawTypeFieldNames)
	}
	return sel
}

func (sel *wireSubsetSelector) keep(name, node []byte) bool {
	return sel.keepHashed(name, wire.NameHash32Bytes(name), node)
}

// keepHashed is keep for a caller that has already hashed the name -- the reference closure,
// which hashes each name once per record and then tests it on every pass.
func (sel *wireSubsetSelector) keepHashed(name []byte, h uint32, _ []byte) bool {
	// The type fields always ship so the row stays typed (never private).
	if wire.FoldEqualBytes(name, "MyType") || wire.FoldEqualBytes(name, "TargetType") {
		return true
	}
	if sel.wantAll {
		return !(sel.redact && isPrivateNameBytes(name))
	}
	return sel.inProjection(name, h)
}

// inProjection reports whether name is one of the projected attributes: the same
// hash-first match keep makes, factored out so the reference closure can ask it directly
// (a reference to an attribute the caller already projected is not a closure addition).
func (sel *wireSubsetSelector) inProjection(name []byte, h uint32) bool {
	for i, wh := range sel.hashes {
		if wh == h && wire.FoldEqualBytes(name, sel.names[i]) {
			return true
		}
	}
	return false
}

// RenderRawAdInline renders a wire-form row (a self-contained inline-names ad,
// e.g. one shipped by ScanRawWire over dbrpc) to old-ClassAd "Name = Value"
// expression text: each expression is buf[offs[i]:offs[i+1]], with
// MyType/TargetType lifted out exactly as the collection scans do -- the shape
// message.PutClassAdRawBytes consumes. This is the LAST-MINUTE conversion at
// the client edge; everything upstream of it stays in wire form. buf/offs are
// reset and reused (pass the previous call's returns to avoid allocation).
func RenderRawAdInline(w []byte, buf []byte, offs []int) (outBuf []byte, outOffs []int, myType, targetType string, ok bool) {
	buf = buf[:0]
	offs = append(offs[:0], 0)
	good := true
	walked := wire.Ad(w).ForEachNameNode(func(name, node []byte) bool {
		if wire.FoldEqualBytes(name, "MyType") {
			if lit, lok := wire.LiteralValue(node); lok && lit.Kind == wire.LitString {
				myType = lit.Str
				return true
			}
		}
		if wire.FoldEqualBytes(name, "TargetType") {
			if lit, lok := wire.LiteralValue(node); lok && lit.Kind == wire.LitString {
				targetType = lit.Str
				return true
			}
		}
		buf = append(buf, name...)
		buf = append(buf, ' ', '=', ' ')
		var aerr error
		buf, aerr = wire.AppendNodeTextInlineOld(buf, node)
		if aerr != nil {
			good = false
			return false
		}
		offs = append(offs, len(buf))
		return true
	})
	if !walked || !good {
		return buf, offs, "", "", false
	}
	return buf, offs, myType, targetType, true
}
