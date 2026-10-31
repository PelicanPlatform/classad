package collections

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// COMPUTED VALUES ACROSS ID SPACES.
//
// A computed cell is found by its field's POSITION in the block schema, its expression is resolved
// through the record's own attribute keys when the segment is columnarized, and an uncomputed cell is
// evaluated through the record's key space when it is read. Three id spaces meet there: this process's
// intern ids (handed out in first-seen order at every Open), a segment dictionary's ids (an interned
// segment's records), and the ids a payload was written with. Where two of them coincide a mix-up is
// invisible, and a single-schema store opened in one process makes them coincide.
//
// These tests pull them apart: the store is reopened with the names interned in the REVERSE of the
// order the writer saw them, on an interned archive as well as a mutable table and a plain archive,
// and a second schema is adopted mid-convergence so the same computed field sits at different
// positions in different segments. Every attribute is a distinct range and every check compares a
// value with the one its own record evaluates to, so a value read from a neighbouring attribute or
// another record cannot pass.

// idsAdText is record i. A, B, C are disjoint ranges; each derived field is its source plus a constant,
// so `X - source == constant` holds for every record exactly when X came from the right attribute of
// the right record.
//
//	CompA, CompB   computed from A and B
//	CompS          computed string from C
//	Exc            B + 7 everywhere: a literal in most records, and in one in 13 an expression naming
//	               TARGET that no record can compute -- read by evaluating that record alone
//	Pin            strcat("p", A) in half the records, so it is under the presence floor until
//	               required; a string, so the second schema places it after every int and the order
//	               of the int fields is decided by this process's ids alone
func idsAdText(i int) string {
	exc := strconv.Itoa(200000 + i%1000 + 7)
	if i%13 == 5 {
		exc = "ifThenElse(isUndefined(TARGET.Memory), B + 7, 0)"
	}
	pin := ""
	if i%2 == 0 {
		pin = `; Pin = strcat("p", A)`
	}
	return fmt.Sprintf(`[ ClusterId = %d; JobStatus = %d; A = %d; B = %d; C = %d; CompA = A + 1; CompB = B + 1; `+
		`CompS = strcat("s", C); Exc = %s%s ]`, i, 1+i%4, 100000+i%1000, 200000+i%1000, 300000+i%1000, exc, pin)
}

// idsNames is every attribute of the fixture, in the order the writer first sees them.
var idsNames = []string{"ClusterId", "JobStatus", "A", "B", "C", "CompA", "CompB", "CompS", "Exc", "Pin"}

func reversedNames(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[len(names)-1-i] = n
	}
	return out
}

const idsCount = 3000

type idsStore struct {
	t        *testing.T
	dir      string
	archive  bool
	interned bool
	c        *Collection
	a        *Archive
	close    func() error
}

func (s *idsStore) open(hot []string, budget int) {
	s.t.Helper()
	if s.archive {
		o := ArchiveOptions{Dir: s.dir, SegmentSize: 1 << 16, GroupSchemaCount: -1, InternAtSeal: s.interned, HotAttrs: hot}
		a, err := CreateArchive(o)
		if err != nil {
			a, err = OpenArchive(o)
		}
		if err != nil {
			s.t.Fatal(err)
		}
		s.c, s.a, s.close = a.c, a, a.Close
	} else {
		c, err := Open(Options{Dir: s.dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, HotAttrs: hot})
		if err != nil {
			s.t.Fatal(err)
		}
		s.c, s.close = c, c.Close
	}
	s.c.colBudget = budget
}

// newIDsStore writes the fixture and columnarizes it in one process, then closes it.
func newIDsStore(t *testing.T, archive, interned bool) *idsStore {
	t.Helper()
	s := &idsStore{t: t, dir: t.TempDir(), archive: archive, interned: interned}
	s.open(nil, 1<<20)
	for i := range idsCount {
		ad := mustAd(t, idsAdText(i))
		var err error
		if archive {
			err = s.a.Append(ad)
		} else {
			err = s.c.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !s.c.BuildAndEnableSchemaScan(4096, 4) {
		t.Fatal("no schema derived")
	}
	for s.c.ColumnarizeSealed() > 0 {
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	return s
}

// checkIDs compares every reader that touches a computed or evaluated cell with plain evaluation.
func checkIDs(t *testing.T, c *Collection, when string) {
	t.Helper()
	// Reassembly, record by record: every attribute is the one its own record evaluates to.
	n := 0
	for ad := range c.Scan() {
		n++
		cid, ok := ad.EvaluateAttrInt("ClusterId")
		if !ok {
			t.Fatalf("%s: a record without ClusterId", when)
		}
		i := int(cid)
		want := map[string]int64{"A": int64(100000 + i%1000), "B": int64(200000 + i%1000), "C": int64(300000 + i%1000)}
		want["CompA"], want["CompB"], want["Exc"] = want["A"]+1, want["B"]+1, want["B"]+7
		for k, w := range want {
			if got, ok := ad.EvaluateAttrInt(k); !ok || got != w {
				t.Fatalf("%s: record %d %s = %d (ok=%v), want %d", when, i, k, got, ok, w)
			}
		}
		if s, _ := ad.EvaluateAttrString("CompS"); s != fmt.Sprintf("s%d", 300000+i%1000) {
			t.Fatalf("%s: record %d CompS = %q", when, i, s)
		}
		p, hasPin := ad.EvaluateAttrString("Pin")
		if (i%2 == 0) != hasPin || (hasPin && p != fmt.Sprintf("p%d", 100000+i%1000)) {
			t.Fatalf("%s: record %d Pin = %q (present=%v)", when, i, p, hasPin)
		}
	}
	if n != idsCount {
		t.Fatalf("%s: scan returned %d of %d records", when, n, idsCount)
	}

	// Per-record identities, answered by the column readers. Each holds for every record, so a count
	// short of idsCount names a record that read another attribute's or another record's value.
	parse := func(e string) *vm.Query {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	pinned := 0
	for i := range idsCount {
		if i%2 == 0 {
			pinned++
		}
	}
	for _, tc := range []struct {
		expr string
		want int
	}{
		{"CompA - A == 1", idsCount},
		{"CompB - B == 1", idsCount},
		{"Exc - B == 7", idsCount},
		{`CompS == strcat("s", C)`, idsCount},
		{`Pin == strcat("p", A)`, pinned},
		{`Pin >= "p"`, pinned},
		{"Pin is undefined", idsCount - pinned},
		{"CompA > 150000", 0},
		{"CompB < 150000", 0},
		{"Exc < 150000", 0},
		{"Exc isnt undefined", idsCount},
	} {
		q := parse(tc.expr)
		// CountQuery serves every native query; a delegated one (a function call) is the scan's.
		if got, ok := c.CountQuery(q); (!ok && q.Native()) || (ok && got != tc.want) {
			t.Errorf("%s: CountQuery(%s) = %d (ok=%v), want %d", when, tc.expr, got, ok, tc.want)
		}
		if got, ok := c.ColumnarEvalCount(q); ok && got != tc.want {
			t.Errorf("%s: ColumnarEvalCount(%s) = %d, want %d", when, tc.expr, got, tc.want)
		}
		if got, ok := c.VectorEvalCount(q); ok && got != tc.want {
			t.Errorf("%s: VectorEvalCount(%s) = %d, want %d", when, tc.expr, got, tc.want)
		}
		if got, ok := c.PresenceCountQuery(q); ok && got != tc.want {
			t.Errorf("%s: PresenceCountQuery(%s) = %d, want %d", when, tc.expr, got, tc.want)
		}
	}

	// Aggregates: each field's range and exact integer sum, which a neighbouring attribute's values
	// (a disjoint range) or a shifted record cannot reproduce.
	all := parse("ClusterId >= 0")
	for attr, off := range map[string]int64{"CompA": 100001, "CompB": 200001, "Exc": 200007} {
		var sum, lo, hi int64
		cnt := 0
		lo, hi = 1<<62, -1<<62
		for i := range idsCount {
			v := off + int64(i%1000)
			sum += v
			if v < lo {
				lo = v
			}
			if v > hi {
				hi = v
			}
			cnt++
		}
		st, ok := c.NumStatsQuery(all, attr)
		if !ok {
			continue
		}
		if st.N != cnt || st.IntSum != sum || st.IntMin != lo || st.IntMax != hi {
			t.Errorf("%s: NumStatsQuery(%s) = N %d sum %d [%d,%d], want N %d sum %d [%d,%d]",
				when, attr, st.N, st.IntSum, st.IntMin, st.IntMax, cnt, sum, lo, hi)
		}
		if gs, ok := c.GroupStatsQuery(all, "JobStatus", []string{attr}); ok {
			var gsum int64
			for _, g := range gs {
				gsum += g.Stats[0].IntSum
			}
			if gsum != sum {
				t.Errorf("%s: GroupStatsQuery SUM(%s) = %d, want %d", when, attr, gsum, sum)
			}
		}
	}

	// Projection of the computed fields with their sources, each read from its own record.
	nproj := 0
	for w := range c.QueryRawWire(parse("ClusterId >= 0"), []string{"ClusterId", "A", "CompA", "B", "Exc"}, false) {
		ad, err := c.decodeWireAny(w)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := ad.EvaluateAttrInt("A")
		ca, _ := ad.EvaluateAttrInt("CompA")
		b, _ := ad.EvaluateAttrInt("B")
		e, _ := ad.EvaluateAttrInt("Exc")
		if ca-a != 1 || e-b != 7 {
			t.Fatalf("%s: projected record has A=%d CompA=%d B=%d Exc=%d", when, a, ca, b, e)
		}
		nproj++
	}
	if nproj != idsCount {
		t.Errorf("%s: projection returned %d of %d records", when, nproj, idsCount)
	}
}

// decodeWireAny decodes a reassembled ad in whichever key encoding it carries.
func (c *Collection) decodeWireAny(w []byte) (*classad.ClassAd, error) {
	if _, _, _, inline, ok := wire.Ad(w).SplitBody(); ok && inline {
		a, err := wire.DecodeInline(w)
		if err != nil {
			return nil, err
		}
		return classad.FromAST(a), nil
	}
	a, err := wire.Decode(w, c.intern)
	if err != nil {
		return nil, err
	}
	return classad.FromAST(a), nil
}

// idsReversed reports whether the reopened process numbered the fixture's names differently from the
// writer: the fixture only tests anything if it did.
func idsReversed(c *Collection) bool {
	a, okA := c.intern.LookupID("A")
	p, okP := c.intern.LookupID("Pin")
	return okA && okP && p < a
}

func runIDSpaces(t *testing.T, archive, interned bool) {
	s := newIDsStore(t, archive, interned)
	computedBefore := computedCellsBuilt.Load()

	// Process B: the names interned in the reverse of the writer's order.
	s.open(reversedNames(idsNames), 1)
	if !idsReversed(s.c) {
		t.Fatal("fixture: the reopened process numbered the names as the writer did")
	}
	if interned {
		dicts := 0
		for _, sh := range s.c.shards {
			for _, seg := range sh.segs {
				if seg.dict.Load() != nil && seg.columnarized() {
					dicts++
				}
			}
		}
		if dicts == 0 {
			t.Fatal("fixture: no columnarized segment is interned")
		}
	}
	if !schemaHas(s.c, "CompA") || !schemaHas(s.c, "CompS") || schemaHas(s.c, "Pin") {
		t.Fatal("fixture: the first schema should carry the computed fields and not Pin")
	}
	if bad := idsComputedEverywhere(s.c); bad != "" {
		t.Fatalf("reopened: %s", bad)
	}
	exc := heldExceptionEvals.Load()
	checkIDs(t, s.c, "reopened, reversed ids")
	if heldExceptionEvals.Load() == exc {
		t.Error("no uncomputed cell was evaluated on its own")
	}

	// A second schema, adopted mid-convergence: Pin joins, the computed fields move, and one segment is
	// rewritten under it while the rest keep the first.
	if !s.c.SetSchemaRequireAttrs([]string{"Pin"}) {
		t.Fatal("require set did not change")
	}
	s.c.BuildAndEnableSchemaScan(4096, 4)
	if !schemaHas(s.c, "Pin") {
		t.Fatal("fixture: the second schema did not adopt Pin")
	}
	if stale := s.c.staleSegmentCount(s.c.schemaScan.Load()); stale == 0 {
		t.Fatal("fixture: no segment is left under the first schema")
	}
	if computedCellsBuilt.Load() == computedBefore {
		t.Fatal("fixture: the rewrite computed nothing")
	}
	if !idsMoved(s.c) {
		t.Fatal("fixture: the computed fields sit at the same positions in both schemas")
	}
	if bad := idsComputedEverywhere(s.c); bad != "" {
		t.Fatalf("two schemas: %s", bad)
	}
	checkIDs(t, s.c, "two schemas, reversed ids")
	if err := s.close(); err != nil {
		t.Fatal(err)
	}

	// Process C: the writer's order again, over both schemas.
	s.open(idsNames, 1)
	defer s.close()
	checkIDs(t, s.c, "two schemas, reopened")
}

// idsComputedEverywhere reports which columnarized segment, if any, lacks computed values for a field
// every record computes: each block must carry a section at that field's position in its own schema.
func idsComputedEverywhere(c *Collection) string {
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			for _, b := range cn.seg.blocks {
				if b.n == 0 {
					continue
				}
				for _, n := range []string{"CompA", "CompB", "CompS"} {
					id, _ := c.intern.LookupID(n)
					idx, ok := b.schema.byID[id]
					if !ok {
						return seg.path + ": no field " + n
					}
					if _, ok := b.computedEntryOf(idx); !ok {
						return seg.path + ": " + n + " not computed"
					}
				}
			}
		}
	}
	return ""
}

// idsMoved reports whether two computed int fields sit in opposite orders in two of the store's segment
// schemas: a position read under the wrong schema then lands on the OTHER computed field.
func idsMoved(c *Collection) bool {
	orders := map[bool]bool{}
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			pos := map[string]int{}
			for idx, f := range cn.seg.schema().fields {
				n, _ := c.intern.Name(f.id)
				pos[n] = idx
			}
			orders[pos["CompA"] < pos["CompB"]] = true
		}
	}
	return len(orders) == 2
}

func TestComputedIDSpacesMutable(t *testing.T)         { runIDSpaces(t, false, false) }
func TestComputedIDSpacesArchive(t *testing.T)         { runIDSpaces(t, true, false) }
func TestComputedIDSpacesInternedArchive(t *testing.T) { runIDSpaces(t, true, true) }
