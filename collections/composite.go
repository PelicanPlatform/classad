package collections

import (
	"encoding/binary"
	"math"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// Composite (multi-column) indexes: one keyed structure over an attribute TUPLE.
//
// Without one, `ClusterId == 7 && ProcId == 3` is satisfied by probing each attribute's
// index separately and intersecting the two posting lists. Both postings are wider than their
// intersection -- `ProcId == 3` posts one record per cluster in the segment -- so the
// intersection pays to merge thousands of offsets to keep one, or (when the planner's
// worthProbing rule drops the wide probe) hands the scan every record of the cluster to
// re-verify. A composite posts the record once under the pair, so the same query is one
// lookup returning the answer. BenchmarkComposite* measures both.
//
// FULL TUPLE ONLY. A composite answers an equality on EVERY one of its components and
// nothing else: no prefix probe on the leading attribute, no range on any of them. Keys are
// therefore hashed (in RAM) or byte-sorted (in the sidecar) rather than order-preserving --
// there is no ordering to preserve.
//
// For a component that is also indexed in its own right that costs nothing: the leading
// attribute alone is already served by its own index, as is any range. For a COMPONENT-ONLY
// component (indexSpec.compOnly) it costs exactly what it says -- a conjunction pinning
// fewer than all the components gets no index from the composite, so `ProcId == 5` alone
// scans, as does `ProcId >= 2` and `ProcId != 1` in any combination. That is the trade an
// installation makes when it declares a component-only attribute, and it is worth making
// only when nothing queries the attribute by itself.
//
// SOUNDNESS. The composite is a candidate SUPERSET like every other index path, and the
// store re-verifies each candidate against the real predicate:
//
//   - A record that posted the exact tuple is a candidate.
//   - A record EXCEPTIONAL in any component (a value the component's own index could not
//     key on: an expression, a string under a value index, a NaN) is a candidate, because
//     the index cannot classify it.
//   - A record MISSING a component is not a candidate, and must not be: `A == x` is
//     UNDEFINED when A is absent, so the conjunction is not TRUE. This is the same rule the
//     single-attribute equality path follows -- post ∪ exc, which absent records are in
//     neither of.
//
// The one place the composite is not simply narrower than the intersection is the exception
// set: it unions EVERY component's exceptions, where the intersection would have cut one
// component's exceptions against another component's postings. compositeWins prices that,
// so a composite over a messy column is declined rather than used because it exists.

// maxCompositeAttrs bounds a composite's width. The sidecar stores each component's kind in
// a bitmask word, and a tuple this wide is already past the point where the demand to
// justify it could be observed.
const maxCompositeAttrs = 8

// compositeSpec is one configured composite index: an ordered attribute tuple, with each
// component's kind (categorical or value) fixed at configuration time so the record side
// and the probe side encode a component the same way.
type compositeSpec struct {
	ids  []uint32
	cats []bool // component i is categorical (a folded string key) rather than value (a float key)
	// key identifies the tuple: the ids in order, little-endian. It is the map key of the
	// per-segment postings and of the sidecar directory, so the ORDER of the components is
	// part of the identity -- (ClusterId, ProcId) and (ProcId, ClusterId) are two composites.
	key string
	// names is the tuple's attribute names as configured, for reporting (IndexSizes,
	// explain). Display only -- identity is key.
	names []string
	auto  bool // created by the auto-tuner rather than configured by a human
}

// compositeKey builds a compositeSpec's identity from its component ids.
func compositeKey(ids []uint32) string {
	b := make([]byte, 0, 4*len(ids))
	for _, id := range ids {
		b = binary.LittleEndian.AppendUint32(b, id)
	}
	return string(b)
}

// label renders the tuple for a human: "ClusterId+ProcId".
func (cs compositeSpec) label() string { return strings.Join(cs.names, "+") }

// --- key encoding ----------------------------------------------------------------
//
// A composite key is the concatenation of its components' encodings, each tagged with its
// kind so a string component can never be confused with a value one. Components are
// self-delimiting (a string carries its length), so distinct tuples always encode to
// distinct bytes. Each component is folded EXACTLY as its own index folds it: a categorical
// component by catFold (ClassAd string equality is case-insensitive), a value component to
// the float64 the value index keys on.

func appendCompStr(b []byte, s string) []byte {
	b = append(b, 's')
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

func appendCompFloat(b []byte, f float64) []byte {
	// -0 and +0 are the same value and the same posting: the value index keys them together
	// (a Go map treats them as one float64 key), so the composite must too, which the raw
	// bit pattern would not.
	if f == 0 { // true for -0 as well, and the literal assigns +0
		f = 0
	}
	b = append(b, 'f')
	return binary.BigEndian.AppendUint64(b, math.Float64bits(f))
}

// --- probe matching --------------------------------------------------------------

// compositeEqProbe reports whether a planned probe pins its attribute to exactly one value,
// which is what a composite component can answer. `in` with one surviving value counts
// (foldEqualities narrows a membership to its admissible values); `!=`, ranges and the
// presence probes do not.
func compositeEqProbe(up usableProbe) bool {
	if !isEqOp(up.op) || up.neverMatches() {
		return false
	}
	if up.cat {
		return len(up.svals) == 1
	}
	return len(up.fvals) == 1
}

// compositeTupleKey encodes the composite key the conjunction pins, and the indices of the
// probes it consumes, or ok=false when usable does not pin every component exactly once.
func compositeTupleKey(cs compositeSpec, usable []usableProbe) (key string, used []int, ok bool) {
	at := make([]int, len(cs.ids))
	for i := range at {
		at[i] = -1
	}
	for i, up := range usable {
		if !compositeEqProbe(up) {
			continue
		}
		for j, id := range cs.ids {
			if up.attrID != id || up.cat != cs.cats[j] || at[j] >= 0 {
				continue
			}
			at[j] = i
			break
		}
	}
	b := make([]byte, 0, 16*len(cs.ids))
	used = make([]int, 0, len(cs.ids))
	for j, i := range at {
		if i < 0 {
			return "", nil, false // a component this conjunction does not pin
		}
		if cs.cats[j] {
			b = appendCompStr(b, usable[i].svals[0])
		} else {
			b = appendCompFloat(b, usable[i].fvals[0])
		}
		used = append(used, i)
	}
	return string(b), used, true
}

// compositeMatch is one segment's answer for the composite covering a conjunction: the
// records posted under the tuple, plus the composite's exception set. Both bitmaps belong
// to the index and must not be mutated; candidates() returns a fresh union.
type compositeMatch struct {
	spec compositeSpec
	used []int           // indices into the usable slice the composite covers
	post *roaring.Bitmap // records posted under the exact tuple (nil when the key is absent)
	exc  *roaring.Bitmap // records exceptional in some component (nil when there are none)
	excN uint64          // |exc|, kept separately so the sidecar need not page the bitmap to count
	// certainWin marks the case that needs no estimate at all: the composite covers EVERY
	// probe of the conjunction and the segment has no exceptional record for it, so its
	// candidates are the tuple's posting, which is a subset of every component's posting
	// and so of every probe's candidate set. The planner can then skip estimating
	// selectivity entirely -- which on a high-cardinality value attribute means not
	// scanning the heavy-hitter list once per probe to price probes it will not run.
	certainWin bool
}

// count is the exact number of candidates the composite admits in the indexed prefix. Exact,
// not estimated -- which is the second thing a composite knows that the single-attribute
// path does not: conjunctSelectivity has to multiply independent per-attribute estimates,
// and ClusterId and ProcId are about as correlated as two columns get.
func (cm compositeMatch) count() uint64 {
	var n uint64
	if cm.post != nil {
		n = cm.post.GetCardinality()
	}
	return n + cm.excN
}

// empty reports that the indexed prefix provably holds no record satisfying the covered
// conjuncts: nothing posted the tuple and no record was exceptional in any component, so a
// matching record would have had to be in the posting.
func (cm compositeMatch) empty() bool {
	return cm.excN == 0 && (cm.post == nil || cm.post.IsEmpty())
}

// candidates returns a fresh, mutable bitmap of the composite's candidates.
func (cm compositeMatch) candidates() *roaring.Bitmap {
	switch {
	case cm.post == nil && cm.exc == nil:
		return roaring.New()
	case cm.post == nil:
		return cm.exc.Clone()
	case cm.exc == nil:
		return cm.post.Clone()
	}
	return roaring.Or(cm.post, cm.exc)
}

// compositeWins prices the composite against the intersection it would replace: it is taken
// only when it admits no more candidates than the most selective single probe it covers
// would. With a clean column (no exceptions) that always holds -- the tuple posting is a
// subset of every component's posting -- and the composite is a strict win: one lookup
// instead of several posting merges. With a messy one it need not, because the composite
// unions every component's exceptions where the intersection could have cut them against
// another component's postings, and then the composite is declined.
func compositeWins(cm compositeMatch, est []float64) bool {
	if cm.certainWin {
		return true
	}
	best := math.Inf(1)
	for _, i := range cm.used {
		best = math.Min(best, est[i])
	}
	return float64(cm.count()) <= best
}

// --- per-segment postings (the in-RAM tier) ---------------------------------------

// compPostings is one segment's postings for one composite: the encoded tuple key to the
// offsets of the records holding it, plus the records the index could not classify.
type compPostings struct {
	spec compositeSpec
	post map[string]*roaring.Bitmap
	exc  *roaring.Bitmap
	excN uint64 // |exc|, computed by finishStats, so a probe never pages the bitmap to count
}

// finishStats completes the postings after the last record is indexed. Nothing may be
// added afterwards.
func (cp *compPostings) finishStats() { cp.excN = cp.exc.GetCardinality() }

// match answers compositeMatch for this segment, or ok=false when usable does not pin the
// whole tuple.
func (cp *compPostings) match(usable []usableProbe) (compositeMatch, bool) {
	key, used, ok := compositeTupleKey(cp.spec, usable)
	if !ok {
		return compositeMatch{}, false
	}
	cm := compositeMatch{spec: cp.spec, used: used, post: cp.post[key], excN: cp.excN}
	if cp.excN > 0 {
		cm.exc = cp.exc
	}
	cm.certainWin = cp.excN == 0 && len(used) == len(usable)
	return cm, true
}

// compositeMatchIn picks the composite to use for a conjunction from those a segment holds:
// the one covering the most probes, ties broken by tuple identity so the choice is
// deterministic across segments and processes.
func compositeMatchIn(comps map[string]*compPostings, usable []usableProbe) (compositeMatch, bool) {
	if len(comps) == 0 || len(usable) < 2 {
		return compositeMatch{}, false
	}
	var best compositeMatch
	var found bool
	keys := make([]string, 0, len(comps))
	for k := range comps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cm, ok := comps[k].match(usable)
		if !ok {
			continue
		}
		if !found || len(cm.used) > len(best.used) {
			best, found = cm, true
		}
	}
	return best, found
}

// --- spec construction -------------------------------------------------------------

// newCompositeSpecs resolves configured attribute tuples against the index configuration
// already in s, and returns the composites that are admissible.
//
// A component must have a KIND in s -- from its own index, or from a component-only
// declaration (indexSpec.compOnly). A tuple naming an attribute s knows nothing about is
// REFUSED, silently: the record side and the probe side have to fold that component the same
// way, and with no kind there is no way to fold it at all.
//
// What is NOT required any more is that every component be indexed in its own right. The
// reason it once was -- that everything the composite does not answer falls back to the
// components' own postings -- holds, and the fallback is still what happens; the change is
// that an installation may decide a particular component is not worth the fallback. A
// history archive asking only `ClusterId == c && ProcId == p` gets nothing from a standalone
// ProcId index and pays for it per record, and on a real pool one cluster carries nearly
// three million distinct ProcIds. Declaring ProcId component-only buys the tuple lookup and
// gives up `ProcId == 5` on its own, which becomes a scan.
//
// Duplicate components, a tuple of fewer than two attributes, and a tuple wider than
// maxCompositeAttrs are refused as before: none of them describes an index.
//
// Every refusal is also RETURNED, as a CompositeRefusal naming the tuple and the reason, so
// a drop is something the caller can read rather than something it has to infer from a
// missing index. Refusing is still all that happens -- the tuples that survive are exactly
// the tuples that survived before.
func (s *indexSpec) newCompositeSpecs(intern *wire.InternTable, tuples [][]string, auto bool) ([]compositeSpec, []CompositeRefusal) {
	var out []compositeSpec
	var refused []CompositeRefusal
	refuse := func(t []string, reason, attr string) {
		refused = append(refused, CompositeRefusal{
			Attrs:  append([]string(nil), t...),
			Reason: reason,
			Attr:   attr,
		})
	}
	seen := map[string]bool{}
	for _, t := range tuples {
		if len(t) < 2 {
			refuse(t, CompositeRefusedTooFewComponents, "")
			continue
		}
		if len(t) > maxCompositeAttrs {
			refuse(t, CompositeRefusedTooManyComponents, "")
			continue
		}
		cs := compositeSpec{auto: auto}
		dup := map[uint32]bool{}
		bad := false
		for _, name := range t {
			id, cat, ok := s.resolveComponent(intern, name)
			if !ok {
				refuse(t, CompositeRefusedUnresolvableComponent, name)
				bad = true
				break
			}
			if dup[id] {
				refuse(t, CompositeRefusedDuplicateComponent, name)
				bad = true
				break
			}
			dup[id] = true
			cs.ids = append(cs.ids, id)
			cs.cats = append(cs.cats, cat)
			cs.names = append(cs.names, name)
		}
		if bad {
			continue
		}
		cs.key = compositeKey(cs.ids)
		if seen[cs.key] {
			refuse(t, CompositeRefusedDuplicateTuple, "")
			continue
		}
		seen[cs.key] = true
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out, refused
}

// CompositeRefusal is one configured composite tuple that was not installed, and why.
//
// It exists because the failure it describes has no other symptom. A refused tuple does not
// break the store and does not fail the open: the table comes up, the queries that were
// meant to use the composite still answer correctly, and they answer by intersecting
// postings or by scanning. The only thing an operator sees is that something got slower,
// with nothing anywhere to point at -- which is how a typo in a component name, or dropping
// an index some composite depended on, survives for months.
//
// WHAT A CALLER IS EXPECTED TO DO WITH IT. Log it at open and treat a non-empty slice as a
// configuration bug to fix in the configuration, not as a condition to handle at runtime.
// There is nothing to retry and nothing to repair: the store is already as configured, minus
// the tuple. Fixing the spelling (or restoring the component's index, or declaring it
// component-only) and reopening installs the composite. An empty slice is the normal state
// and the only one worth alerting on the absence of.
//
// It is deliberately NOT an error. A collection's configuration is applied in New, which
// cannot fail, and again on every reopen of a persistent store -- so a refusal raised as an
// error would either have to be unreportable in the in-memory tier or stop an existing
// deployment from opening its own data because a tuple in its saved configuration stopped
// resolving. Turning a missing index into a refusal to start is the worse failure of the two.
type CompositeRefusal struct {
	// Attrs is the tuple exactly as it was configured, including spelling and order.
	Attrs []string `json:"attrs"`
	// Reason is one of the CompositeRefused* constants below.
	Reason string `json:"reason"`
	// Attr is the component at fault for the reasons that name one, and empty for the
	// reasons that are about the tuple as a whole. Only the FIRST offending component is
	// named: resolution stops at it, so a second one has not been looked at.
	Attr string `json:"attr,omitempty"`
}

// The reasons a configured composite tuple is refused.
//
// A component that is not in the spec's id space at all (a name nothing was ever configured
// under) and one that is in it but carries no kind (indexed neither in its own right nor as
// a declared component) are ONE reason, not two. Which of the two a given name hits depends
// on the id space: an inline spec knows only the names its own configuration mentions, an
// interned one knows every name the store has ever interned. Splitting them would make the
// same configuration report a different reason in memory than on disk, which is a worse
// diagnostic than the slightly coarse one -- and the fix is the same either way.
const (
	// CompositeRefusedUnresolvableComponent: Attr is neither indexed in its own right nor
	// declared a composite component, so there is no kind to fold it by.
	CompositeRefusedUnresolvableComponent = "unresolvable-component"
	// CompositeRefusedDuplicateComponent: Attr appears twice in the tuple. The second
	// occurrence pins nothing the first did not.
	CompositeRefusedDuplicateComponent = "duplicate-component"
	// CompositeRefusedTooFewComponents: fewer than two components, which is an
	// attribute index written as a tuple, not a composite.
	CompositeRefusedTooFewComponents = "too-few-components"
	// CompositeRefusedTooManyComponents: wider than maxCompositeAttrs.
	CompositeRefusedTooManyComponents = "too-many-components"
	// CompositeRefusedDuplicateTuple: these components in this order are already
	// configured. The same components in a DIFFERENT order are a different composite and
	// are not refused here.
	CompositeRefusedDuplicateTuple = "duplicate-tuple"
)

// String renders a refusal as one log line: "ClusterId+ProcId: unresolvable-component ProcId".
func (r CompositeRefusal) String() string {
	s := strings.Join(r.Attrs, "+") + ": " + r.Reason
	if r.Attr != "" {
		s += " " + r.Attr
	}
	return s
}

// resolveIndexed returns the id of an attribute that is already indexed in this spec
// (categorically or by value), in either id mode. A component-only attribute is NOT indexed
// and does not resolve here; that is what keeps the auto-tuner's components-first rule
// (SuggestComposites) meaning what it always meant.
func (s *indexSpec) resolveIndexed(intern *wire.InternTable, name string) (uint32, bool) {
	id, ok := s.resolveID(intern, name)
	if !ok {
		return 0, false
	}
	return id, s.catHas(id) || s.valHas(id)
}

// resolveComponent returns the id and kind of an attribute this spec can use as a composite
// component: one indexed in its own right, or one declared component-only.
func (s *indexSpec) resolveComponent(intern *wire.InternTable, name string) (id uint32, cat, ok bool) {
	if id, ok = s.resolveID(intern, name); !ok {
		return 0, false, false
	}
	cat, ok = s.componentKind(id)
	return id, cat, ok
}

// resolveID looks a configured attribute name up in whichever id space this spec uses. It
// does not say whether the attribute is indexed, only what it would be keyed as.
func (s *indexSpec) resolveID(intern *wire.InternTable, name string) (uint32, bool) {
	fold := strings.ToLower(name)
	if s.inline {
		id, ok := s.nameToID[fold]
		return id, ok
	}
	if intern == nil {
		return 0, false
	}
	return intern.LookupID(fold)
}

// setComposites installs the configured tuples on a freshly built spec and recomputes its
// generation. Tuples naming an attribute the spec gives no kind to -- neither indexed nor
// declared component-only -- are dropped (see newCompositeSpecs), and the dropped ones are
// returned so the caller can say so. The returned slice holds at most one entry per tuple
// passed in, which is what keeps it bounded by the configuration rather than by the store's
// history: a caller that re-applies a configuration REPLACES what it holds with this, it
// never appends.
func (s *indexSpec) setComposites(intern *wire.InternTable, tuples [][]string, auto bool) []CompositeRefusal {
	if len(tuples) == 0 {
		return nil
	}
	var refused []CompositeRefusal
	s.comps, refused = s.newCompositeSpecs(intern, tuples, auto)
	s.gen = s.signature()
	return refused
}

// pruneComposites drops every composite a change to the attribute configuration has
// invalidated: a component that no longer has a kind at all (what a DropIndex on one of its
// attributes leaves behind), or one whose KIND has changed, which an AddIndex naming a
// value-indexed attribute as categorical does. A composite whose component kind is stale
// would fold every record's value the wrong way and post the whole segment as exceptional.
//
// A component-only component is judged by exactly the same rule, through componentKind: it
// is live while its declaration stands and dies with it. DropIndex removes the declaration
// along with a real index, so dropping ProcId takes (ClusterId, ProcId) with it whether
// ProcId was indexed or only a component.
func (s *indexSpec) pruneComposites() {
	if len(s.comps) == 0 {
		return
	}
	kept := s.comps[:0:0]
	for _, cs := range s.comps {
		live := true
		for i, id := range cs.ids {
			cat, ok := s.componentKind(id)
			if !ok || cat != cs.cats[i] {
				live = false
				break
			}
		}
		if live {
			kept = append(kept, cs)
		}
	}
	s.comps = kept
}

// compositePinned marks, in into, the component ids of every configured composite whose
// every component usable pins to a single value.
//
// This is the planner's gate on a component-only probe. Such a probe can only ever be
// answered by a composite -- there are no postings for it to fall back to -- so one that no
// configured composite can consume is dropped from the plan entirely rather than carried
// into the single-attribute machinery, where an absent posting map reads as "no record
// matches" and silently drops rows.
func (s *indexSpec) compositePinned(usable []usableProbe, into map[uint32]struct{}) {
	for _, cs := range s.comps {
		if _, _, ok := compositeTupleKey(cs, usable); !ok {
			continue
		}
		for _, id := range cs.ids {
			into[id] = struct{}{}
		}
	}
}
