package collections

import (
	"fmt"
	"iter"
	"strings"
	"sync"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/tidwall/btree"
)

// SortKey is one component of an ordered index's sort order: an attribute name or
// expression evaluated against each member ad, ascending by default.
type SortKey struct {
	Expr string // attribute name or expression, e.g. "JobPrio" or "QDate"
	Desc bool   // sort this component descending
}

// OrderSpec configures a maintained, filtered, ordered index over a collection --
// the schedd's priority-queue pattern (§3 of docs/MATCH.md). Members are grouped
// into independent runs by Partition (e.g. per Owner); within a partition they are
// ordered by Keys, with insertion order as the final tiebreaker ("the order the job
// entered the queue"). Where restricts membership to the ads that matter (e.g. idle
// jobs), so the index tracks only the churn-prone subset.
type OrderSpec struct {
	Partition string    // attribute partitioning the index; empty = one global partition
	Where     string    // membership predicate; empty = every ad is a member
	Keys      []SortKey // sort order within a partition
}

// orderVal is one comparable, evaluated value (a partition value or a sort-key
// value). Cross-type ordering is by kind, so a mixed or missing attribute still
// yields a total order rather than a panic.
type orderVal struct {
	kind uint8 // ordered: undef < bool < number < string
	num  float64
	str  string
}

const (
	kUndef uint8 = iota
	kBool
	kNum
	kStr
	kMax uint8 = 255 // sentinel greater than every real value; used to build lower-bound pivots
)

func numVal(f float64) orderVal { return orderVal{kind: kNum, num: f} }
func strVal(s string) orderVal  { return orderVal{kind: kStr, str: s} }

// valueToOrderVal projects a classad.Value onto the comparable order domain. Lists,
// nested ads, undefined, and errors all collapse to kUndef (unorderable → grouped).
func valueToOrderVal(v classad.Value) orderVal {
	switch {
	case v.IsBool():
		b, _ := v.BoolValue()
		if b {
			return orderVal{kind: kBool, num: 1}
		}
		return orderVal{kind: kBool, num: 0}
	case v.IsNumber():
		n, _ := v.NumberValue()
		return numVal(n)
	case v.IsString():
		s, _ := v.StringValue()
		return strVal(s)
	default:
		return orderVal{kind: kUndef}
	}
}

// compareVal returns -1/0/+1. Values of different kinds order by kind so the result
// is a total order regardless of attribute types.
func compareVal(a, b orderVal) int {
	if a.kind != b.kind {
		if a.kind < b.kind {
			return -1
		}
		return 1
	}
	switch a.kind {
	case kNum, kBool:
		switch {
		case a.num < b.num:
			return -1
		case a.num > b.num:
			return 1
		default:
			return 0
		}
	case kStr:
		return strings.Compare(a.str, b.str)
	default:
		return 0 // both undefined
	}
}

// orderEntry is one member's position in the index. seq is the ad's insertion
// sequence, stable across attribute updates, providing the final tiebreaker; key is
// the collection key (the payload iteration yields).
type orderEntry struct {
	part orderVal
	keys []orderVal
	seq  uint64
	key  string
}

// entryLess builds the strict weak order for spec: partition (ascending), then each
// sort key honoring Desc, then insertion sequence. seq is unique per member, so the
// order is total (no two entries compare equal) -- required for B-tree identity.
func entryLess(spec OrderSpec) func(a, b orderEntry) bool {
	keys := spec.Keys
	return func(a, b orderEntry) bool {
		if c := compareVal(a.part, b.part); c != 0 {
			return c < 0
		}
		for i := range keys {
			c := compareVal(a.keys[i], b.keys[i])
			if keys[i].Desc {
				c = -c
			}
			if c != 0 {
				return c < 0
			}
		}
		return a.seq < b.seq
	}
}

// orderedIndex is a maintained, snapshot-readable ordered index. Writes (upsert,
// remove) mutate a master B-tree under mu; a read takes an O(1) copy-on-write
// snapshot and iterates it lock-free while writers continue. byKey maps a collection
// key to its current entry so an update can relocate (delete old + insert new) and a
// delete can find its entry, both O(log n).
type orderedIndex struct {
	spec OrderSpec

	// Compiled membership/partition/sort expressions (populated by compile). where
	// is nil for an unfiltered index; part is nil for a single global partition.
	where    *classad.Expr
	part     *classad.Expr
	keyExprs []*classad.Expr

	mu    sync.Mutex
	tree  *btree.BTreeG[orderEntry]
	byKey map[string]orderEntry
	seq   uint64
}

func newOrderedIndex(spec OrderSpec) *orderedIndex {
	return &orderedIndex{
		spec:  spec,
		tree:  btree.NewBTreeG[orderEntry](entryLess(spec)),
		byKey: make(map[string]orderEntry),
	}
}

// compile parses the spec's Where, Partition, and Key expressions once. A malformed
// expression is a static configuration error, reported to New.
func (oi *orderedIndex) compile() error {
	if oi.spec.Where != "" {
		e, err := classad.ParseExpr(oi.spec.Where)
		if err != nil {
			return fmt.Errorf("ordered index Where %q: %w", oi.spec.Where, err)
		}
		oi.where = e
	}
	if oi.spec.Partition != "" {
		e, err := classad.ParseExpr(oi.spec.Partition)
		if err != nil {
			return fmt.Errorf("ordered index Partition %q: %w", oi.spec.Partition, err)
		}
		oi.part = e
	}
	for _, sk := range oi.spec.Keys {
		e, err := classad.ParseExpr(sk.Expr)
		if err != nil {
			return fmt.Errorf("ordered index Key %q: %w", sk.Expr, err)
		}
		oi.keyExprs = append(oi.keyExprs, e)
	}
	return nil
}

// evalAd evaluates the ad against the index: whether it is a member (Where is true,
// or absent), and if so its partition value and sort-key tuple. Expressions resolve
// against the ad as given (a chained child's inherited parent attributes are not
// applied here).
func (oi *orderedIndex) evalAd(ad *classad.ClassAd) (member bool, part orderVal, keys []orderVal) {
	if oi.where != nil && !isTrueValue(oi.where.Eval(ad)) {
		return false, orderVal{}, nil
	}
	if oi.part != nil {
		part = valueToOrderVal(oi.part.Eval(ad))
	}
	keys = make([]orderVal, len(oi.keyExprs))
	for i, e := range oi.keyExprs {
		keys[i] = valueToOrderVal(e.Eval(ad))
	}
	return true, part, keys
}

// upsert inserts or repositions key's entry for the given partition and sort-key
// values. An existing member keeps its original insertion sequence (a stable
// tiebreaker) even when its sort keys change.
func (oi *orderedIndex) upsert(key string, part orderVal, keys []orderVal) {
	oi.mu.Lock()
	defer oi.mu.Unlock()
	var seq uint64
	if old, ok := oi.byKey[key]; ok {
		if entriesEqual(old, part, keys) {
			return // no change to partition or order: nothing to do
		}
		seq = old.seq
		oi.tree.Delete(old)
	} else {
		oi.seq++
		seq = oi.seq
	}
	e := orderEntry{part: part, keys: keys, seq: seq, key: key}
	oi.tree.Set(e)
	oi.byKey[key] = e
}

// remove drops key from the index if it is a member.
func (oi *orderedIndex) remove(key string) {
	oi.mu.Lock()
	defer oi.mu.Unlock()
	if old, ok := oi.byKey[key]; ok {
		oi.tree.Delete(old)
		delete(oi.byKey, key)
	}
}

// snapshot returns a consistent, independent view for iteration. It is a copy-on-
// write clone: later writes to the master path-copy shared nodes and never disturb
// the returned tree.
func (oi *orderedIndex) snapshot() *btree.BTreeG[orderEntry] {
	oi.mu.Lock()
	defer oi.mu.Unlock()
	return oi.tree.Copy()
}

func entriesEqual(old orderEntry, part orderVal, keys []orderVal) bool {
	if compareVal(old.part, part) != 0 || len(old.keys) != len(keys) {
		return false
	}
	for i := range keys {
		if compareVal(old.keys[i], keys[i]) != 0 {
			return false
		}
	}
	return true
}

// startPivot builds the lower-bound entry for a partition: for each sort key it uses
// the smallest value under that key's direction (kUndef for ascending, the kMax
// sentinel for descending, which Desc inverts to the front), and seq 0. No real
// member can compare below it, so Ascend(startPivot) lands on the partition's first
// member.
func (oi *orderedIndex) startPivot(part orderVal) orderEntry {
	keys := make([]orderVal, len(oi.spec.Keys))
	for i, sk := range oi.spec.Keys {
		if sk.Desc {
			keys[i] = orderVal{kind: kMax}
		} else {
			keys[i] = orderVal{kind: kUndef}
		}
	}
	return orderEntry{part: part, keys: keys, seq: 0}
}

// ascendPartition iterates snap over one partition in order, starting at the first
// member strictly after resume (or the partition's first member when resume is nil),
// calling fn with each entry. It stops at the partition boundary. resume is the entry
// last yielded (its exclusive successor is where iteration continues).
func (oi *orderedIndex) ascendPartition(snap *btree.BTreeG[orderEntry], part orderVal, resume *orderEntry, fn func(e orderEntry) bool) {
	pivot := oi.startPivot(part)
	if resume != nil {
		pivot = *resume
	}
	snap.Ascend(pivot, func(e orderEntry) bool {
		if compareVal(e.part, part) != 0 {
			return false // moved past the requested partition
		}
		if resume != nil && e.seq == resume.seq && e.key == resume.key {
			return true // Ascend is inclusive; skip the resume entry itself
		}
		return fn(e)
	})
}

// maintainOrdered updates every ordered index for a just-committed ad: a member is
// inserted or repositioned, a non-member is removed (it may have just transitioned
// out, e.g. an idle job that started running). Called after the store commit.
func (c *Collection) maintainOrdered(key []byte, ad *classad.ClassAd) {
	if len(c.ordered) == 0 {
		return
	}
	k := string(key)
	for _, oi := range c.ordered {
		if member, part, keys := oi.evalAd(ad); member {
			oi.upsert(k, part, keys)
		} else {
			oi.remove(k)
		}
	}
}

// removeOrdered drops a deleted key from every ordered index.
func (c *Collection) removeOrdered(key []byte) {
	if len(c.ordered) == 0 {
		return
	}
	k := string(key)
	for _, oi := range c.ordered {
		oi.remove(k)
	}
}

// OrderCursor marks a position in an ordered scan. Its zero value starts at the
// beginning of a partition; the value yielded alongside an ad resumes strictly after
// that ad, so a negotiator can iterate a partition across several calls.
type OrderCursor struct {
	entry *orderEntry
}

// Ordered iterates one partition of the index-th configured ordered index in sort
// order, yielding each member ad together with a cursor that resumes right after it.
// Iteration is over an O(1) snapshot taken at the call, so it is stable even as the
// index churns; each ad is fetched live by key, so a member deleted since the
// snapshot is skipped. For an index configured without a Partition, the partition
// argument is ignored (there is a single global run). resume's zero value starts at
// the beginning.
func (c *Collection) Ordered(index int, partition classad.Value, resume OrderCursor) iter.Seq2[*classad.ClassAd, OrderCursor] {
	return func(yield func(*classad.ClassAd, OrderCursor) bool) {
		if index < 0 || index >= len(c.ordered) {
			return
		}
		oi := c.ordered[index]
		var part orderVal
		if oi.part != nil {
			part = valueToOrderVal(partition)
		}
		snap := oi.snapshot()
		oi.ascendPartition(snap, part, resume.entry, func(e orderEntry) bool {
			ad, ok := c.Get([]byte(e.key))
			if !ok {
				return true // concurrently deleted since the snapshot: skip
			}
			ec := e
			return yield(ad, OrderCursor{entry: &ec})
		})
	}
}
