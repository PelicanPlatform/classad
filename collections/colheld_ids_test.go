package collections

import (
	"fmt"
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestHeldIndexIsAskedByGlobalID: the held index lists, per schema field, the records holding that
// field in the record (an expression the columns could not take), keyed by the process's global
// intern ids. Asked with any other id -- the segment dictionary's, say -- it names a different field.
// The projection then skips a record's escaped string as if the record held it, and drops it; the
// count, aggregate and group readers call a held expression undefined instead of evaluating it.
//
// Every record carries one field as a string where the schema says integer (so it escapes into the
// cold tail) and another as an expression (so the record holds it), and across the records every
// ordered pair of fields occurs. In the first pass the expression names an attribute the record lacks,
// so no value is computed for it when the segment is columnarized and the readers meet it held; Owner,
// a string column, is held the same way in a few records. The store is interned and reopened, so the
// dictionary's ids, the global ids before the restart and those after it are three numberings. The
// test requires that some field's dictionary id is another field's global id, so a wrong key names a
// field some record really holds.
func TestHeldIndexIsAskedByGlobalID(t *testing.T) {
	t.Parallel()
	const nf, reps = 16, 4
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	n := 0
	for rep := range reps {
		for x := range nf {
			for y := range nf {
				if x == y {
					continue
				}
				text := fmt.Sprintf("[ ClusterId=%d; ProcId=%d", n, rep)
				if rep == 0 && x == 0 {
					text += `; Owner=ifThenElse(isUndefined(NoSuchAttr), strcat("user", ClusterId % 7), "")`
				} else {
					text += fmt.Sprintf(`; Owner="user%d"`, n%7)
				}
				for f := range nf {
					switch f {
					case x:
						text += fmt.Sprintf(`; F%02d="text%d"`, f, n)
					case y:
						if rep == 0 {
							text += fmt.Sprintf("; F%02d=ifThenElse(isUndefined(NoSuchAttr), ClusterId + %d, -1)", f, f)
						} else {
							text += fmt.Sprintf("; F%02d=ClusterId + %d", f, f)
						}
					default:
						text += fmt.Sprintf("; F%02d=%d", f, n*nf+f)
					}
				}
				ad, err := classad.Parse(text + " ]")
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Put([]byte(fmt.Sprintf("%d.0", n)), ad); err != nil {
					t.Fatal(err)
				}
				n++
			}
		}
	}
	// Each field is a string in reps*(nf-1) records and a number (a literal or the held expression's
	// value) in the rest; no record lacks it.
	numeric := n - reps*(nf-1)
	if _, err := c.RetrainDict(0); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema")
	}

	// The test discriminates only if some field's dictionary id is another field's global id (so a
	// wrong key names a field some record really holds), and Owner's is not its own.
	requireCollision := func(stage string, c *Collection) {
		t.Helper()
		global := map[uint32]bool{}
		for f := range nf {
			if g, ok := c.intern.LookupID(fmt.Sprintf("F%02d", f)); ok {
				global[g] = true
			}
		}
		owner, _ := c.intern.LookupID("Owner")
		columnar, collide, ownerMoved := 0, false, false
		for _, seg := range c.shards[0].segs {
			if seg == nil {
				continue
			}
			cn := seg.colNative.Load()
			if cn == nil || cn.dict == nil {
				continue
			}
			columnar++
			for g := range global {
				if l, ok := cn.localOf[g]; ok && l != g && global[l] {
					collide = true
				}
			}
			if l, ok := cn.localOf[owner]; ok && l != owner {
				ownerMoved = true
			}
		}
		switch {
		case columnar == 0:
			t.Fatalf("%s: no dictionary-keyed columnarized segment", stage)
		case !collide:
			t.Fatalf("%s: no field's dictionary id is another field's global id; the test cannot tell the two apart", stage)
		case !ownerMoved:
			t.Fatalf("%s: Owner's dictionary id is its global id in every segment", stage)
		}
	}
	parse := func(e string) *vm.Query {
		t.Helper()
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	served, asked := 0, 0
	count := func(stage, e string, want int) {
		t.Helper()
		asked++
		if got, ok := c.CountQuery(parse(e)); ok {
			served++
			if got != want {
				t.Fatalf("%s, counting %s: %d, want %d", stage, e, got, want)
			}
		}
	}
	groupBy := func(stage, attr string) {
		t.Helper()
		asked++
		gs, ok := c.GroupCountQuery(parse("ClusterId >= 0"), attr)
		if !ok {
			return
		}
		served++
		total := 0
		for _, g := range gs {
			if g.Value.IsUndefined() {
				t.Fatalf("%s, GROUP BY %s: %d records in the undefined group, want none", stage, attr, g.Count)
			}
			total += g.Count
		}
		if total != n {
			t.Fatalf("%s, GROUP BY %s: %d records, want %d", stage, attr, total, n)
		}
	}
	check := func(stage string) {
		t.Helper()
		requireCollision(stage, c)
		served, asked = 0, 0
		for f := range nf {
			attr := fmt.Sprintf("F%02d", f)
			proj := []string{attr}
			got, _ := seqPage(c, nil, proj, SeqCursor{}, 0)
			want, _ := refQueryRawFromSeq(c, nil, proj, SeqCursor{}, 0)
			if !slices.Equal(got, want) {
				for i := range got {
					if i < len(want) && got[i] != want[i] {
						t.Fatalf("%s, projecting %v: row %d reads %q, the whole record %q", stage, proj, i, got[i], want[i])
					}
				}
				t.Fatalf("%s, projecting %v: %d rows, the whole records give %d", stage, proj, len(got), len(want))
			}
			count(stage, attr+" >= 0", numeric)    // the column scan
			count(stage, attr+" + 1 > 0", numeric) // the vectorized evaluator
			count(stage, attr+" is undefined", 0)  // the presence count
			asked++
			if st, ok := c.NumStatsQuery(parse(attr+" >= 0"), attr); ok {
				served++
				if st.N != numeric {
					t.Fatalf("%s, COUNT(%s) WHERE %s >= 0: %d, want %d", stage, attr, attr, st.N, numeric)
				}
			}
			groupBy(stage, attr)
		}
		groupBy(stage, "Owner") // a string column
		t.Logf("%s: %d of %d column queries served", stage, served, asked)
	}
	check("before the restart")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// The reopened process interns the names in another order, so the global ids move against the
	// dictionary's.
	c = open()
	defer c.Close()
	check("after the restart")
}
