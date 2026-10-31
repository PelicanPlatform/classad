package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// refFixtureAd is one probe ad. Requirements and Rank are expressions over siblings the
// projection does NOT name, and MemoryUsage chains two more levels, so a closure that stops
// before the set is stable is visibly wrong. Every third ad holds Requirements as a LITERAL
// instead, which is what makes the closure a per-record property rather than a per-query one.
//
// CgroupFloor exists to make the chase need a SECOND walk and not merely a longer first one.
// Entries are stored in sorted order, so a reference that sorts AFTER the expression naming
// it is picked up later in the same pass; only one that sorts before it -- CgroupFloor,
// reached and skipped before MemoryUsage makes it wanted -- forces the walk to repeat.
func refFixtureAd(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=%d; JobStatus=%d; Owner="user%d"`,
		i, i%10, 1+i%5, i%7)
	fmt.Fprintf(&b, `; RequestMemory=%d; RequestDisk=%d; ResidentSetSize=%d; DiskUsage=%d`,
		(1+i%8)*1024, (1+i%4)*2048, (1+i%6)*512, (1+i%3)*256)
	fmt.Fprintf(&b, `; CgroupFloor=%d`, 64+i%5)
	fmt.Fprintf(&b, `; MemoryUsage = ((ResidentSetSize + 1023) / 1024) + CgroupFloor`)
	if i%3 == 0 {
		fmt.Fprintf(&b, `; Requirements = true; Rank = 0.0`)
	} else {
		fmt.Fprintf(&b, `; Requirements = (TARGET.Memory >= MemoryUsage) && (TARGET.Disk >= RequestDisk) && (Owner != "nobody")`)
		fmt.Fprintf(&b, `; Rank = (DiskUsage / 1024.0)`)
	}
	for f := 0; f < 12; f++ {
		fmt.Fprintf(&b, `; Filler%02d="filler %d-%d"`, f, i, f)
	}
	b.WriteString(" ]")
	return b.String()
}

// refFixture builds a store that holds BOTH stored representations at once: sealed segments
// that maintenance has turned interned (and, with columnar, columnarized) and an active
// segment still carrying inline-name records. The two are narrowed, converted and assembled
// by different code, and the whole point of the closure being per record is that it must come
// out the same from either -- so a fixture with only one of them would prove nothing.
//
// It returns how many records are in interned segments; the caller asserts the mix is real.
func refFixture(t testing.TB, n int, columnar bool) (*Collection, int) {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("%d.%d", i, i%10)), mustAd(t, refFixtureAd(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(0, n*3/4)
	if _, err := c.RetrainDict(4000); err != nil {
		t.Fatal(err)
	}
	if columnar {
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("no columnar schema derived: the columnar arm would not be covered")
		}
		c.ColumnarizeSealed()
	}
	c.Reindex()
	put(n*3/4, n) // lands inline, in the active segments

	interned, inline := 0, 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, s := range sh.segs {
			if s == nil || s.used == 0 {
				continue
			}
			if s.dict.Load() != nil {
				interned++
			} else {
				inline++
			}
		}
		sh.mu.RUnlock()
	}
	// Assert the fixture really has both, or the comparison below is a comparison of one
	// representation with itself.
	if interned == 0 || inline == 0 {
		t.Fatalf("fixture has %d interned and %d inline segments; it must have both", interned, inline)
	}
	if columnar && columnarSegmentsForTest(c) == 0 {
		t.Fatal("fixture columnarized nothing")
	}
	return c, interned
}

// wireRowMap renders a wire-form row to name -> value.
func wireRowMap(t *testing.T, row []byte) map[string]string {
	t.Helper()
	buf, offs, myType, targetType, ok := RenderRawAdInline(row, nil, nil)
	if !ok {
		t.Fatal("RenderRawAdInline refused a wire row")
	}
	m := map[string]string{}
	if myType != "" {
		m["MyType"] = `"` + myType + `"`
	}
	if targetType != "" {
		m["TargetType"] = `"` + targetType + `"`
	}
	for i := 0; i+1 < len(offs); i++ {
		name, val, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
		if !found {
			t.Fatalf("malformed expression %q", buf[offs[i]:offs[i+1]])
		}
		m[name] = val
	}
	return m
}

func rawAdMap(t *testing.T, ra RawAd) map[string]string {
	t.Helper()
	m := map[string]string{}
	if ra.MyType != "" {
		m["MyType"] = `"` + ra.MyType + `"`
	}
	if ra.TargetType != "" {
		m["TargetType"] = `"` + ra.TargetType + `"`
	}
	for _, e := range ra.Exprs {
		name, val, found := strings.Cut(string(e), " = ")
		if !found {
			t.Fatalf("malformed expression %q", e)
		}
		m[name] = val
	}
	return m
}

// TestQueryRawWireRefsMatchesTextPath is the contract: a reference-chasing WIRE read returns
// exactly what the reference-chasing TEXT read returns for the same projection -- same
// attributes, same values -- for every record, over a corpus holding both stored
// representations and both shapes of the projected expression (an expression over siblings,
// and a literal).
//
// The text path is the oracle because it is the one already in production for evaluators;
// comparing the new path against the old whole-ad wire read would only show that both ship
// enough bytes, not that the closure is right.
func TestQueryRawWireRefsMatchesTextPath(t *testing.T) {
	t.Parallel()
	for _, columnar := range []bool{false, true} {
		name := "interned+inline"
		if columnar {
			name = "columnar+inline"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := refFixture(t, 4000, columnar)
			projs := [][]string{
				{"ClusterId", "ProcId", "Requirements"},
				{"ClusterId", "Requirements", "Rank"},
				{"ClusterId", "MemoryUsage"},
				{"ClusterId"}, // nothing to chase: the closure must not add anything
			}
			for _, q := range []string{"true", "ProcId >= 0", "ClusterId < 500"} {
				for _, proj := range projs {
					label := q + "/" + strings.Join(proj, "+")
					want := map[string]map[string]string{}
					for ra := range c.QueryRawProjected(mustParseQ(t, q), proj, true, false) {
						m := rawAdMap(t, ra)
						want[m["ClusterId"]] = m
					}
					if len(want) == 0 {
						t.Fatalf("%s: the oracle matched nothing", label)
					}
					got := map[string]map[string]string{}
					for row := range c.QueryRawWireRefs(mustParseQ(t, q), proj, false) {
						m := wireRowMap(t, row)
						got[m["ClusterId"]] = m
					}
					if len(got) != len(want) {
						t.Fatalf("%s: wire-refs returned %d rows, text-refs %d", label, len(got), len(want))
					}
					for cid, w := range want {
						g := got[cid]
						if g == nil {
							t.Fatalf("%s: wire-refs dropped ClusterId %s", label, cid)
						}
						// The text path carries MyType/TargetType as RawAd fields rather than
						// expressions, so compare on the expression set the two share.
						delete(g, "MyType")
						delete(g, "TargetType")
						delete(w, "MyType")
						delete(w, "TargetType")
						gk, wk := sortedKeys(g), sortedKeys(w)
						if strings.Join(gk, ",") != strings.Join(wk, ",") {
							t.Fatalf("%s: ClusterId %s attributes %v, text-refs %v", label, cid, gk, wk)
						}
						for _, k := range wk {
							if g[k] != w[k] {
								t.Fatalf("%s: ClusterId %s %s = %q, text-refs %q", label, cid, k, g[k], w[k])
							}
						}
					}
				}
			}
		})
	}
}

// TestQueryRawWireRefsClosureIsPerRecord pins what the client-side version of this could not
// do: in one scan, over one projection, some records need two extra attributes and some need
// none, because the SAME attribute is an expression in one ad and a literal in another.
//
// It also pins the transitive step: Requirements refers to MemoryUsage, which refers to
// ResidentSetSize, which Requirements never mentions. A closure that made a single pass would
// return the first and miss the second, and every assertion above would still pass.
func TestQueryRawWireRefsClosureIsPerRecord(t *testing.T) {
	t.Parallel()
	c, _ := refFixture(t, 2000, true)
	proj := []string{"ClusterId", "Requirements"}
	literal, expression := 0, 0
	for row := range c.QueryRawWireRefs(mustParseQ(t, "true"), proj, false) {
		m := wireRowMap(t, row)
		var cid int
		if _, err := fmt.Sscanf(m["ClusterId"], "%d", &cid); err != nil {
			t.Fatal(err)
		}
		names := strings.Join(sortedKeys(m), ",")
		if cid%3 == 0 {
			literal++
			if names != "ClusterId,MyType,Requirements,TargetType" {
				t.Fatalf("ClusterId %d holds a literal Requirements but the row carries %s", cid, names)
			}
			continue
		}
		expression++
		// Requirements -> MemoryUsage, RequestDisk, Owner; MemoryUsage -> ResidentSetSize
		// and CgroupFloor, the second of which only a repeated walk reaches.
		if names != "CgroupFloor,ClusterId,MemoryUsage,MyType,Owner,RequestDisk,Requirements,ResidentSetSize,TargetType" {
			t.Fatalf("ClusterId %d: row carries %s", cid, names)
		}
	}
	if literal == 0 || expression == 0 {
		t.Fatalf("the corpus produced %d literal and %d expression records; it must produce both", literal, expression)
	}
}

// TestQueryRawWireRefsRedactsReferences: a private attribute never leaves a redacted row,
// and being referenced by a projected expression is not an exception.
func TestQueryRawWireRefsRedactsReferences(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 50; i++ {
		ad := fmt.Sprintf(`[ MyType="Machine"; Name="slot%d"; ClaimId="secret-%d"; Cpus=%d;
			Start = (Cpus > 0) && (ClaimId != "") ]`, i, i, 1+i%4)
		if err := c.Put([]byte(fmt.Sprintf("slot%d", i)), mustAd(t, ad)); err != nil {
			t.Fatal(err)
		}
	}
	rows := 0
	for row := range c.QueryRawWireRefs(mustParseQ(t, "true"), []string{"Name", "Start"}, true) {
		m := wireRowMap(t, row)
		if _, leak := m["ClaimId"]; leak {
			t.Fatalf("a redacted reference-chasing row carries ClaimId: %v", sortedKeys(m))
		}
		if _, ok := m["Cpus"]; !ok {
			t.Fatalf("the non-private reference was dropped along with the private one: %v", sortedKeys(m))
		}
		rows++
	}
	if rows != 50 {
		t.Fatalf("got %d rows, want 50", rows)
	}
}

// TestQueryRawWireRefsParallelMatchesSerial: the fan-out holds per-worker closure state, and
// one worker reading another's would produce rows with the wrong attributes rather than a
// crash. Pin the two implementations against each other, with the path chosen rather than
// hoped for.
func TestQueryRawWireRefsParallelMatchesSerial(t *testing.T) {
	c, _ := refFixture(t, 4000, true)
	proj := []string{"ClusterId", "Requirements", "Rank"}
	run := func(parallel bool) map[string]map[string]string {
		if parallel {
			c.parallelMinBytes = 0
		} else {
			c.parallelMinBytes = 1 << 62
		}
		before := wireQueryFanouts.Load()
		out := map[string]map[string]string{}
		for row := range c.QueryRawWireRefs(mustParseQ(t, "ProcId >= 0"), proj, false) {
			m := wireRowMap(t, row)
			out[m["ClusterId"]] = m
		}
		switch fanned := wireQueryFanouts.Load() != before; {
		case parallel && !fanned && c.queryPar < 2:
			t.Skip("this machine resolves to serial query parallelism; nothing to fan out")
		case parallel && !fanned:
			t.Fatal("the parallel arm did not fan out")
		case !parallel && fanned:
			t.Fatal("the serial arm fanned out; the comparison would be the parallel path against itself")
		}
		return out
	}
	par, ser := run(true), run(false)
	if len(par) != len(ser) || len(par) != 4000 {
		t.Fatalf("parallel %d rows, serial %d, want 4000", len(par), len(ser))
	}
	for cid, p := range par {
		s := ser[cid]
		if s == nil {
			t.Fatalf("the serial arm dropped ClusterId %s", cid)
		}
		if strings.Join(sortedKeys(p), ",") != strings.Join(sortedKeys(s), ",") {
			t.Fatalf("ClusterId %s: parallel %v, serial %v", cid, sortedKeys(p), sortedKeys(s))
		}
		for k, v := range s {
			if p[k] != v {
				t.Fatalf("ClusterId %s %s: parallel %q, serial %q", cid, k, p[k], v)
			}
		}
	}
}

// TestAppendAdSubsetInlineRefsNeedsAnEarlierPass: the referenced attribute sits BEFORE the
// expression that names it, so a single forward pass cannot find it. Built at the wire level
// so the repetition is pinned where it lives rather than through a whole store.
func TestAppendAdSubsetInlineRefsNeedsAnEarlierPass(t *testing.T) {
	ad := mustAd(t, `[ Alpha = 7; Beta = 9; Gamma = Alpha + Beta ]`)
	w := wire.EncodeInline(nil, ad.AST())
	keepNames := map[string]bool{"Gamma": true}
	refs := map[string]bool{}
	var scratch [][]byte
	var sc wire.SubsetScratch
	keep := func(name []byte, _ uint32, _ []byte) bool { return keepNames[string(name)] || refs[string(name)] }
	note := func(node []byte) bool {
		scratch = wire.AppendNodeRefNames(node, scratch[:0])
		added := false
		for _, rn := range scratch {
			if !keepNames[string(rn)] && !refs[string(rn)] {
				refs[string(rn)] = true
				added = true
			}
		}
		return added
	}
	// Always outstanding, so the hot shortcut cannot fire and the cold-tail walk this test
	// is about is the thing being exercised.
	pending := func() int { return 1 }
	out, ok := wire.AppendAdSubsetInlineRefs(nil, wire.Ad(w), keep, note, pending, 1, nil, &sc)
	if !ok {
		t.Fatal("the assembler refused a well-formed inline ad")
	}
	got := map[string]bool{}
	wire.Ad(out).ForEachNameNode(func(name, _ []byte) bool {
		got[string(name)] = true
		return true
	})
	for _, want := range []string{"Alpha", "Beta", "Gamma"} {
		if !got[want] {
			t.Fatalf("the row is missing %s; it carries %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("the row carries %v, want exactly Alpha, Beta, Gamma", got)
	}
}
