package collections

import (
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
// calling fn with each collection key. It stops at the partition boundary. resume is
// the entry last yielded (its exclusive successor is where iteration continues).
func (oi *orderedIndex) ascendPartition(snap *btree.BTreeG[orderEntry], part orderVal, resume *orderEntry, fn func(key string) bool) {
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
		return fn(e.key)
	})
}
