package collections

import (
	"iter"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Query returns an iterator over the archived ads matching q, newest first. Each
// segment is pruned by its zone maps, then its sidecar index narrows the candidate
// records, and every candidate is re-verified against the full predicate — so the
// result is identical to a full scan. The active (unsealed) segment is full-scanned.
func (a *Archive) Query(q *vm.Query) iter.Seq[*classad.ClassAd] {
	return a.QueryLimit(q, 0)
}

// QueryLimit is Query capped at limit matches (limit <= 0 means unlimited). Because
// iteration is newest-first, QueryLimit(q, K) yields the K most recent matches and
// stops — the common condor_history "last K" pattern, which lets the scan terminate
// after touching only the newest satisfying segments.
func (a *Archive) QueryLimit(q *vm.Query, limit int) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		probes := q.Probes()
		usable := a.planIndex(probes)
		wins := a.snapshotWindows()
		defer func() {
			for _, w := range wins {
				w.seg.unpin()
			}
		}()

		// Wire-native match plan: each candidate is re-verified against the encoded
		// bytes without building a ClassAd (falling back to a partial/full decode only
		// when a queried attribute is a non-literal expression), and only true matches
		// are decoded to be yielded. This is the same path Collection scans use.
		plan := q.ReadPlan()
		ws := &wireScope{ctx: a}
		qp := queryPlan{
			q:        q,
			plan:     plan,
			m:        q.Matcher(),
			wireOK:   q.Native() && plan.PartialSafe,
			ws:       ws,
			resolver: ws.resolve,
		}

		n := 0
		var buf []byte
		for _, w := range wins {
			if zonePrune(w.zones, probes, a.intern) {
				continue // no record in this segment can satisfy a required conjunct
			}
			if a.scanned != nil {
				a.scanned(w.seg.id)
			}
			idx := a.ensureIndex(w.as)
			offs := candidateOffsets(idx, w.data, w.used, usable)
			// Newest-first within the segment: records are laid out oldest-first, so
			// walk the candidate offsets in reverse.
			for i := len(offs) - 1; i >= 0; i-- {
				dec, err := a.codec.Decompress(buf[:0], recAd(w.data, offs[i]))
				if err != nil {
					continue
				}
				buf = dec
				if !matchWire(dec, qp) {
					continue
				}
				ad, err := wire.Decode(dec, a.intern)
				if err != nil {
					continue
				}
				if !yield(classad.FromAST(ad)) {
					return
				}
				n++
				if limit > 0 && n >= limit {
					return
				}
			}
		}
	}
}

// archiveWindow is a query's pinned view of one segment: immutable bytes up to a
// snapshotted watermark, its owning archiveSeg (for lazy index load), and zone maps.
type archiveWindow struct {
	seg   *segment
	as    *archiveSeg
	data  []byte
	used  int
	zones map[uint32]zoneRange
}

// snapshotWindows returns the segments to scan, newest first (active segment, then
// sealed segments in reverse id order), each pinned against reclamation. The caller
// must unpin every returned window.
func (a *Archive) snapshotWindows() []archiveWindow {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var wins []archiveWindow
	add := func(as *archiveSeg, used int) {
		as.seg.pin()
		wins = append(wins, archiveWindow{
			seg: as.seg, as: as, data: as.seg.data, used: used, zones: as.zones,
		})
	}
	if a.active != nil && a.active.seg.used > 0 {
		add(a.active, a.active.seg.used)
	}
	for i := len(a.segs) - 1; i >= 0; i-- {
		add(a.segs[i], a.segs[i].seg.used)
	}
	return wins
}

// candidateOffsets returns the ascending record offsets in a window to re-verify: the
// index-narrowed set when the segment has a covering index and usable probes,
// otherwise every record (full scan).
func candidateOffsets(idx *segIndex, data []byte, used int, usable []usableProbe) []uint32 {
	if len(usable) > 0 && idx != nil && idx.covers(usable) {
		if cand := idx.candidateOffsets(usable); cand != nil {
			return cand.ToArray()
		}
		return nil
	}
	return scanOffsets(data, used)
}

// scanOffsets walks a segment's records in [0, used) and returns their offsets.
func scanOffsets(data []byte, used int) []uint32 {
	var offs []uint32
	for off := 0; off < used; {
		o := uint32(off)
		total := recTotalLen(data, o)
		if total == 0 {
			break
		}
		offs = append(offs, o)
		off += int(total)
	}
	return offs
}

// planIndex matches the query's probes against the archive's configured indexes.
func (a *Archive) planIndex(probes []vm.Probe) []usableProbe {
	if !a.spec.any() {
		return nil
	}
	var out []usableProbe
	for _, p := range probes {
		id, ok := a.intern.LookupID(p.Attr)
		if !ok {
			continue
		}
		if _, isCat := a.spec.cat[id]; isCat {
			if up, ok := catUsable(id, p); ok {
				out = append(out, up)
			}
			continue
		}
		if _, isVal := a.spec.val[id]; isVal {
			if up, ok := valUsable(id, p); ok {
				out = append(out, up)
			}
		}
	}
	return out
}

// zonePrune reports whether a segment can be skipped entirely: some required
// conjunct (a top-level AND probe) on a zone-mapped attribute cannot be satisfied by
// any record whose value lies in the segment's [min,max] for that attribute.
func zonePrune(zones map[uint32]zoneRange, probes []vm.Probe, intern *wire.InternTable) bool {
	if len(zones) == 0 {
		return false
	}
	for _, p := range probes {
		id, ok := intern.LookupID(p.Attr)
		if !ok {
			continue
		}
		z, ok := zones[id]
		if !ok {
			continue
		}
		vals := probeFloats(p)
		if len(vals) == 0 {
			continue // non-numeric constraint: zone map can't rule it out
		}
		if !zoneMayMatch(z, p.Op, vals) {
			return true
		}
	}
	return false
}

// zoneMayMatch reports whether a segment with the given [min,max] for an attribute
// could contain a record satisfying (op, vals). A false result means the segment is
// prunable for this required conjunct.
func zoneMayMatch(z zoneRange, op string, vals []float64) bool {
	switch op {
	case "==", "in":
		for _, v := range vals {
			if v >= z.Min && v <= z.Max {
				return true
			}
		}
		return false
	case "<":
		return z.Min < vals[0]
	case "<=":
		return z.Min <= vals[0]
	case ">":
		return z.Max > vals[0]
	case ">=":
		return z.Max >= vals[0]
	default:
		return true // != and anything else: not prunable
	}
}

// probeFloats returns the numeric values of a probe, or nil if any value is
// non-numeric (in which case the zone map cannot reason about it).
func probeFloats(p vm.Probe) []float64 {
	out := make([]float64, 0, len(p.Vals))
	for _, v := range p.Vals {
		f, ok := numericFloat(v)
		if !ok {
			return nil
		}
		out = append(out, f)
	}
	return out
}
