package collections

import (
	"iter"
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
// yielded row aliases a buffer reused across the iteration. Non-inline (RAM)
// collections yield nothing: their ads are not self-contained, and an
// in-process consumer reads them through the collection directly.
func (c *Collection) ScanRawWire(projection []string, redact bool) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		if !c.inline {
			return
		}
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
	return func(yield func([]byte) bool) {
		if !c.inline {
			return
		}
		sel := c.newWireSubsetSelector(projection, redact)
		emit := c.yieldWireRow(yield, sel)
		plan := q.ReadPlan()
		ws := &wireScope{ctx: c}
		qp := queryPlan{
			q:        q,
			plan:     plan,
			m:        q.Matcher(),
			wireOK:   q.Native() && plan.PartialSafe,
			ws:       ws,
			resolver: ws.resolve,
		}
		probes := q.Probes()
		c.demand.record(probes)
		usable := c.planIndex(probes)
		for _, sh := range c.shards {
			var cont bool
			if len(usable) > 0 {
				cont = c.scanShardIndexed(sh, usable, qp, emit)
			} else {
				cont = c.scanShard(sh, qp, emit)
			}
			if !cont {
				return
			}
		}
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

func (c *Collection) yieldWireRow(yield func([]byte) bool, sel *wireSubsetSelector) func(w []byte) bool {
	var scratch []byte
	var sc wire.SubsetScratch
	needed := sel.neededCount()
	return func(w []byte) bool {
		out, ok := wire.AppendAdSubsetInlineHotFirst(scratch[:0], wire.Ad(w), sel.keep, needed, c.sealer, &sc)
		scratch = out
		if !ok {
			return true // not an inline ad / malformed: skip, keep scanning
		}
		return yield(out)
	}
}

// wireSubsetSelector decides, per entry, whether a wire-form row keeps it: the
// same name-hash-first matching the projected text scans use, with private
// names pre-pruned from a redacted projection so redaction costs nothing per
// entry on the projected path.
type wireSubsetSelector struct {
	wantAll bool
	redact  bool
	names   []string
	hashes  []uint32
}

func (c *Collection) newWireSubsetSelector(projection []string, redact bool) *wireSubsetSelector {
	sel := &wireSubsetSelector{wantAll: len(projection) == 0, redact: redact}
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

func (sel *wireSubsetSelector) keep(name, _ []byte) bool {
	// The type fields always ship so the row stays typed (never private).
	if wire.FoldEqualBytes(name, "MyType") || wire.FoldEqualBytes(name, "TargetType") {
		return true
	}
	if sel.wantAll {
		return !(sel.redact && isPrivateNameBytes(name))
	}
	h := wire.NameHash32Bytes(name)
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
		buf, aerr = wire.AppendNodeTextInline(buf, node)
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
