package collections

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// The narrowed match and the conjunct rejection decide a record without rebuilding it. Both are
// only worth anything if they give exactly the answer the plain scan gives, so these tests hold them
// to it over the shapes that make a narrowed record differ from a whole one: per-segment schemas,
// segments that are not columnarized, the active tail, deltas, sealed values, expression-valued
// attributes (chains, eval(), references to absent attributes), CurrentTime, and ERROR operands.

// narrowTestAd is ad i of the fixture. phase 1 carries VacateReasonCode in nearly every record, the
// attribute the fixture's second schema adds.
func narrowTestAd(t *testing.T, i, phase int) *classad.ClassAd {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nT = %d\n", i/10, i%10, i%5, 1000+i)
	if i%20 != 0 {
		typ := "EPOCH"
		if i%9 == 0 {
			typ = "SPAWN"
		}
		fmt.Fprintf(&b, "EpochAdType = %q\n", typ)
	}
	if (phase == 0 && i%10 == 3) || (phase == 1 && i%20 != 7) {
		fmt.Fprintf(&b, "VacateReasonCode = %d\n", 20+i%7)
	}
	some := func(k, pct int) bool { return (i*7919+k*104729)%100 < pct }
	if some(1, 80) {
		fmt.Fprintf(&b, "RSS = %d\n", 100000+i*37)
	}
	if some(2, 85) {
		b.WriteString("MemoryUsage = ((RSS + 1023) / 1024)\n")
	}
	// A reference chain A -> B -> C, each link present in most records.
	if some(3, 70) {
		fmt.Fprintf(&b, "C = %d\n", i%11)
	}
	if some(4, 80) {
		b.WriteString("B = C * 2\n")
	}
	if some(5, 80) {
		b.WriteString("A = B + 1\n")
	}
	// eval() names its attribute only at run time.
	if some(6, 80) {
		fmt.Fprintf(&b, "Foo = %d\n", i%3)
	}
	if some(7, 50) {
		b.WriteString("E = eval(\"Foo\")\n")
	}
	if i%13 == 0 {
		b.WriteString("Bad = \"str\"\n") // a string where queries compare a number: ERROR
	} else {
		fmt.Fprintf(&b, "Bad = %d\n", i%4)
	}
	if i%17 == 0 {
		b.WriteString("ErrLit = error\n")
	} else if some(8, 60) {
		b.WriteString("ErrLit = true\n")
	}
	if i%29 == 0 {
		b.WriteString("CurrentTime = 5\n") // a record defining CurrentTime evaluates its own
	}
	fmt.Fprintf(&b, "Secret = \"tok%d\"\n", i)
	if i%31 == 0 {
		// A second sealed attribute most records lack. Sealed values are strings by contract: one
		// that refers to another attribute is not followed by the narrowed match or the partial
		// decode, so its answer would legitimately differ from a whole-ad evaluation's (pinned
		// separately, in TestSealedValueReferencesAreNotFollowed).
		fmt.Fprintf(&b, "Token = \"tk%d\"\n", i)
	}
	for k := 0; k < 12; k++ {
		if some(20+k, 65) {
			fmt.Fprintf(&b, "Filler%02d = %d\n", k, (i*k)%1000)
		}
	}
	ad, err := classad.ParseOld(b.String())
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// nmFixture is one storage configuration the scan must agree with itself on.
type nmFixture struct {
	name    string
	opts    Options
	updates bool // rewrite some sealed keys after columnarizing, leaving deltas in the active tail
}

func narrowFixtures(t *testing.T) []nmFixture {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	base := Options{Shards: 1, SegmentSize: 1 << 15, ColumnarSegmentBudget: 2, GroupSchemaCount: -1}
	enc := base
	enc.DataKey, enc.EncryptedAttrs = key, []string{"Secret", "Token"}
	delta := base
	delta.DeltaMax = 8
	interned := base
	interned.AppendOnly, interned.InternAtSeal = true, true
	return []nmFixture{
		{name: "inline", opts: base},
		{name: "interned", opts: interned},
		{name: "encrypted", opts: enc},
		{name: "deltas", opts: delta, updates: true},
	}
}

// buildNarrowFixture writes two phases of ads, columnarizing a few sealed segments under each phase's
// schema (the rest get sidecar blocks), then leaves an active tail.
func buildNarrowFixture(t *testing.T, f nmFixture) *Collection {
	t.Helper()
	opts := f.opts
	opts.Dir = t.TempDir()
	codec, err := NewZSTDCodec(nil)
	if err != nil {
		t.Fatal(err)
	}
	opts.Codec = codec
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	put := func(i, phase int) {
		if err := c.Put([]byte(fmt.Sprintf("%05d.0", i)), narrowTestAd(t, i, phase)); err != nil {
			t.Fatal(err)
		}
	}
	const n0, n1 = 600, 600
	for i := 0; i < n0; i++ {
		put(i, 0)
	}
	c.InternSealed() // a no-op unless the fixture interns: only an append-only store does
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	for i := n0; i < n0+n1; i++ {
		put(i, 1)
	}
	c.InternSealed()
	// A second schema carrying VacateReasonCode, which the first does not: sealed segments
	// columnarized from here on hold it in a column, the earlier ones in their cold tails.
	// Installed directly rather than through ReschemaScan, which rewrites the already-columnarized
	// segments onto the new schema and would leave the fixture with one schema, not two.
	c.AddSchemaRequireAttrs("VacateReasonCode")
	s2, hot2, ok := c.deriveSchema(4096, 8)
	if !ok || !c.installSchemaScan(s2, hot2) {
		t.Fatal("second schema declined")
	}
	c.ColumnarizeSealed()
	if f.updates {
		// Patches to keys whose current versions are in columnarized segments: each is a delta,
		// merged over the record it patches whenever it is read.
		for round := 0; round < 3; round++ {
			for i := 0; i < n0; i += 37 {
				patch := classad.New()
				patch.InsertAttr("ProcId", int64((i+round)%10))
				patch.InsertAttr("VacateReasonCode", int64(30+round))
				tx := c.Begin()
				tx.PatchAttrs([]byte(fmt.Sprintf("%05d.0", i)), patch, nil)
				if r := tx.Commit(); r.Conflicted() {
					t.Fatalf("patch %d conflicted", i)
				}
			}
		}
	}
	for i := n0 + n1; i < n0+n1+60; i++ {
		put(i, 1)
	}

	schemas := map[string]bool{} // by field set: blocks of one schema need not share a pointer
	sidecar, columnar, active, dicts, deltas := 0, 0, 0, 0, 0
	sh := c.shards[0]
	for _, seg := range sh.segs {
		if seg != nil {
			for off := 0; off < seg.used; {
				total := recTotalLen(seg.data, uint32(off))
				if total == 0 {
					break
				}
				if recIsDelta(seg.data, uint32(off)) {
					deltas++
				}
				off += int(total)
			}
		}
		switch {
		case seg == nil:
		case seg == sh.act:
			active++
		case seg.columnarized():
			columnar++
			cn := seg.colNative.Load()
			if cn.dict != nil {
				dicts++
			}
			for _, blk := range cn.seg.blocks {
				ids := make([]string, 0, len(blk.schema.fields))
				for _, f := range blk.schema.fields {
					name, _ := c.intern.Name(f.id)
					ids = append(ids, strings.ToLower(name))
				}
				sort.Strings(ids)
				schemas[strings.Join(ids, ",")] = true
			}
		case seg.colblk.Load() != nil:
			sidecar++
		}
	}
	if columnar == 0 || sidecar == 0 || active == 0 || len(schemas) < 2 {
		t.Fatalf("fixture %s: %d columnarized (%d schemas), %d sidecar-only, %d active; want each shape",
			f.name, columnar, len(schemas), sidecar, active)
	}
	if (dicts > 0) != f.opts.InternAtSeal {
		t.Fatalf("fixture %s: %d of %d columnarized segments are interned", f.name, dicts, columnar)
	}
	if (deltas > 0) != f.updates {
		t.Fatalf("fixture %s: %d delta records", f.name, deltas)
	}
	return c
}

// narrowTestQueries are the constraints the differential runs. Each names what it is there to break.
var narrowTestQueries = []string{
	// The production shape: two schema conjuncts and one the schema may not carry.
	`(EpochAdType =?= undefined || EpochAdType == "EPOCH") && T >= 1100 && VacateReasonCode isnt undefined`,
	// Conjuncts whose attribute is an expression, reaching outside the query's own attributes.
	`ProcId < 5 && MemoryUsage > 200`,
	`Owner == "u1" && A > 10`,
	`A =?= undefined && ProcId == 2`,
	// eval() in a stored expression.
	`ProcId >= 0 && E == 1`,
	`E =?= undefined && VacateReasonCode isnt undefined`,
	// CurrentTime, which some records define for themselves.
	`T < CurrentTime && VacateReasonCode isnt undefined`,
	`CurrentTime > 100 && ProcId < 3`,
	`CurrentTime - T > 100 && VacateReasonCode > 22`,
	// Attributes no record has.
	`NeverThere is undefined && ProcId == 1`,
	`NeverThere == 1 && ProcId == 1`,
	`ProcId == 1 && NeverThere`,
	// ERROR operands, in each position.
	`Bad > 1 && ProcId < 5`,
	`ProcId < 5 && Bad > 1`,
	`ProcId < 5 && VacateReasonCode isnt undefined && Bad > 1`,
	`ErrLit && ProcId < 5`,
	`ProcId < 5 && ErrLit`,
	`ProcId < 5 && "str"`,
	`Owner && ProcId < 5`,
	// Numbers are logically true inside && but do not match on their own. Foo is never a schema
	// field, so these reach the conjunct rejection rather than the whole-query prefilter.
	`ProcId && VacateReasonCode isnt undefined`,
	`ProcId && Foo isnt undefined`,
	`ProcId - 1 && Foo == 1`,
	`VacateReasonCode && Foo >= 0`,
	`VacateReasonCode && ProcId`,
	`ProcId`,
	`ProcId - 1 && T > 0`,
	`ProcId * 1.0 && Owner == "u2"`,
	// || at the top is not a conjunction, and && nested inside one does not make it one.
	`ProcId == 1 || VacateReasonCode isnt undefined`,
	`(ProcId == 1 && VacateReasonCode isnt undefined) || Owner == "u2"`,
	`ProcId == 1 || (Owner == "u2" && C > 5)`,
	// Nested && is still a conjunction.
	`(ProcId < 3) && (Owner == "u1" && VacateReasonCode > 20)`,
	`!(ProcId >= 3 || Owner != "u1") && A > 3`,
	// Sealed attributes: one every record has, and one most lack.
	`Secret == "tok42" && ProcId == 2`,
	`Token isnt undefined && ProcId < 5`,
	// Decided wholly from columns (in the schemas that carry the fields).
	`ProcId < 3 && ClusterId > 10`,
	`T >= 1100 && T < 2000`,
	// eval() in the query itself: no partial decode, no narrowed match.
	`eval("ProcId") == 1 && VacateReasonCode isnt undefined`,
	`true`,
}

var narrowTestProjection = []string{"ProcId", "Owner", "VacateReasonCode", "MemoryUsage", "A", "E", "Secret",
	"Token", "NeverThere", "T"}

// classAdSummary renders an ad order-insensitively, expressions by their text.
func classAdSummary(ad *classad.ClassAd) string {
	var parts []string
	for _, name := range ad.GetAttributes() {
		e, _ := ad.Lookup(name)
		parts = append(parts, strings.ToLower(name)+"="+e.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ";")
}

func rawAdText(ra RawAd) string {
	var b strings.Builder
	for _, e := range ra.Exprs {
		b.Write(e)
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "MyType=%s TargetType=%s", ra.MyType, ra.TargetType)
	return b.String()
}

func sameBag(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = slices.Clone(a), slices.Clone(b)
	sort.Strings(a)
	sort.Strings(b)
	return slices.Equal(a, b)
}

// TestNarrowMatchAgreesWithPlainScan is the differential: every query, on every storage shape,
// under every combination of the two savings, against two references --
//
//   - the plain no-plan scan: every visible ad decoded whole and the query evaluated on it, which is
//     what the answer means; and
//   - the same store with both savings off, which must produce the same RawAd bytes in the same
//     order, since the savings only decide records and never change what is emitted.
func TestNarrowMatchAgreesWithPlainScan(t *testing.T) {
	for _, f := range narrowFixtures(t) {
		t.Run(f.name, func(t *testing.T) {
			c := buildNarrowFixture(t, f)
			var all []*classad.ClassAd
			for ad := range c.Scan() {
				all = append(all, ad)
			}
			decided := map[string]int{}
			for _, expr := range narrowTestQueries {
				q, err := vm.Parse(expr)
				if err != nil {
					t.Fatalf("%s: %v", expr, err)
				}
				var want []string
				for _, ad := range all {
					if q.Matches(ad) {
						want = append(want, classAdSummary(ad))
					}
				}
				var baseRaw [2][]string
				var baseWire [2][]string
				for _, arm := range narrowBenchArms {
					restore := arm.set()
					var got []string
					for ad := range c.Query(q) {
						got = append(got, classAdSummary(ad))
					}
					if !sameBag(got, want) {
						t.Errorf("%s [%s]: Query returned %d ads, plain scan %d (or they differ)", expr, arm.name, len(got), len(want))
					}
					for ci, chase := range []bool{false, true} {
						var raw []string
						var st ScanStats
						for ra := range c.QueryRawProjectedStats(q, narrowTestProjection, chase, false, &st) {
							raw = append(raw, rawAdText(ra))
						}
						decided[arm.name] += st.RecordsConjunctRejected + st.RecordsNarrowMatched
						if len(raw) != len(want) {
							t.Errorf("%s [%s chase=%v]: QueryRawProjected %d rows, plain scan %d", expr, arm.name, chase, len(raw), len(want))
						}
						if arm.name == "base" {
							baseRaw[ci] = raw
						} else if !slices.Equal(raw, baseRaw[ci]) {
							t.Errorf("%s [%s chase=%v]: QueryRawProjected rows differ from the base arm", expr, arm.name, chase)
						}
						// The wire-row projection takes a projection plan, so a record the savings
						// decide is built from its columns where the base arm reassembled it.
						read := c.QueryRawWire
						if chase {
							read = c.QueryRawWireRefs
						}
						var rows []string
						for w := range read(q, narrowTestProjection, false) {
							rows = append(rows, string(w))
						}
						if len(rows) != len(want) {
							t.Errorf("%s [%s chase=%v]: QueryRawWire %d rows, plain scan %d", expr, arm.name, chase, len(rows), len(want))
						}
						if arm.name == "base" {
							baseWire[ci] = rows
						} else if !slices.Equal(rows, baseWire[ci]) {
							t.Errorf("%s [%s chase=%v]: QueryRawWire rows differ from the base arm", expr, arm.name, chase)
						}
					}
					restore()
				}
			}
			// The differential is only worth something if the savings actually ran.
			if decided["narrow"] == 0 || decided["conjunct"] == 0 {
				t.Errorf("savings never engaged: %v", decided)
			}
			t.Logf("records decided without reassembly, per arm: %v", decided)
		})
	}
}

// TestProjectFromColumnsIsReassemblyNarrowed holds the column builder to its definition: for every
// columnarized record, the ad it builds is byte-for-byte the reassembled record with every attribute
// outside the plan removed -- same entries, same order. A consumer that renders in ad order (the text
// projection) depends on the order, and a narrowed match depends on the entries.
func TestProjectFromColumnsIsReassemblyNarrowed(t *testing.T) {
	for _, f := range narrowFixtures(t)[:2] {
		t.Run(f.name, func(t *testing.T) {
			c := buildNarrowFixture(t, f)
			p := c.newProjPlan([]string{"ProcId", "Owner", "VacateReasonCode", "MemoryUsage", "A", "B", "RSS",
				"Filler01", "Filler07", "EpochAdType", "T", "NeverThere"})
			st := c.schemaScan.Load()
			cs := &colScope{bc: st.cache, c: c}
			var sc projectScratch
			var nsc narrowScratch
			sh := c.shards[0]
			s0, wins := sh.snapshot()
			defer releaseWindows(wins)
			checked := 0
			forEachVisibleRef(s0, wins, func(r recRef) bool {
				cn := r.w.seg.colNative.Load()
				if cn == nil {
					return true
				}
				k, ok := cn.indexOf(r.off)
				if !ok {
					return true
				}
				got, ok := c.projectFromColumns(cn, r, k, p, cs, &sc)
				if !ok {
					t.Errorf("record %s: no columnar build", r.key())
					return true
				}
				full, err := c.wireAt(r, s0, nil, sealedOpened)
				if err != nil {
					t.Fatal(err)
				}
				var want map[uint32]struct{}
				if cn.dict != nil {
					want = p.localWanted(cn.dict)
				}
				exp, ok := narrowAd(&nsc, full, func(id uint32, name string) bool {
					if name != "" {
						return p.wantsName(name)
					}
					if want == nil {
						_, in := p.ids[id]
						return in
					}
					_, in := want[id]
					return in
				})
				if !ok {
					t.Fatal("reassembled record does not parse")
				}
				if !bytes.Equal(got, exp) {
					t.Errorf("record %s: column build differs from the narrowed reassembly\n got %s\nwant %s",
						r.key(), adSummaryOrdered(c, cn.dict, got), adSummaryOrdered(c, cn.dict, exp))
				}
				checked++
				return true
			})
			if checked == 0 {
				t.Fatal("no columnarized record was checked")
			}
		})
	}
}

// adSummaryOrdered names an ad's attributes in stored order, for a failure message.
func adSummaryOrdered(c *Collection, dict *segDictHandle, w []byte) string {
	var names []string
	wire.Ad(w).ForEachRaw(func(id uint32, name string, _ []byte) bool {
		if name == "" {
			if dict != nil {
				name = string(dict.name(id))
			} else if n, ok := c.intern.Name(id); ok {
				name = n
			}
		}
		names = append(names, name)
		return true
	})
	return strings.Join(names, ",")
}

// TestStoredEvalMatchesWholeAd: a stored expression calling eval() reads an attribute its static
// references do not name, so a partial decode built from those references lacks it. The scan used
// to evaluate such a record on the partial ad and miss the match the whole ad gives.
func TestStoredEvalMatchesWholeAd(t *testing.T) {
	for _, dir := range []string{"", t.TempDir()} {
		c, err := Open(Options{Dir: dir, Shards: 1})
		if err != nil {
			t.Fatal(err)
		}
		ad, err := classad.Parse(`[ Foo = 1; E = eval("Foo"); G = E + 0 ]`)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte("k"), ad); err != nil {
			t.Fatal(err)
		}
		for _, expr := range []string{"E == 1", "G == 1"} {
			q, err := vm.Parse(expr)
			if err != nil {
				t.Fatal(err)
			}
			if !q.Matches(ad) {
				t.Fatalf("%s does not match the ad itself", expr)
			}
			n := 0
			for range c.Query(q) {
				n++
			}
			if n != 1 {
				t.Errorf("dir=%q %s: scan matched %d ads, want 1", dir, expr, n)
			}
		}
		c.Close()
	}
}

// TestNarrowMatchRefusesBadChecksum: a record whose checksum fails is skipped by the plain scan and
// counted as a read skip, whether or not the query would have matched it. Deciding such a record on
// its narrowed form reads its remnant, so it must be refused first -- otherwise a rejected record is
// dropped silently instead of counted, and an answer that came back short looks complete.
//
// A rejection from the columns alone is different: it reads none of the record's own bytes, which is
// what the checksum covers, so -- as for the whole-query prefilter -- the record is skipped without
// being read and without being counted. The arms with the conjunct rejection may therefore count
// fewer skips, never more, and must return the same rows.
func TestNarrowMatchRefusesBadChecksum(t *testing.T) {
	dir := t.TempDir()
	opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 16}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1600; i++ {
		if i == 1500 && !a.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("no columnar schema derived")
		}
		if err = a.Append(narrowTestAd(t, i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	// Flip a bit of the stored checksum of the first few ads of every segment: the ad bytes are
	// untouched, so a reader that does not verify decodes them perfectly well.
	flipped := 0
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".dat") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		done := 0
		for _, off := range recordOffsets(t, path) {
			if done == 4 {
				break
			}
			if recIsMarker(b, uint32(off)) {
				continue
			}
			kl := int(binary.LittleEndian.Uint32(b[off+recKeyLenOff:]) & keyLenMask)
			adLenOff := off + recKeyOff + kl
			flipBit(t, path, adLenOff+4+int(binary.LittleEndian.Uint32(b[adLenOff:])))
			done++
		}
		flipped += done
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err = OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := a.c
	if !c.VerifyingReads() || columnarSegmentsForTest(c) == 0 {
		t.Fatal("need read verification and a columnarized segment")
	}
	skips := func() int64 { return RecordReadSkips()["record-crc"] }
	for _, expr := range []string{
		`ProcId == 100 && VacateReasonCode isnt undefined`, // rejects every record
		`ProcId < 5 && VacateReasonCode isnt undefined`,
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		var baseRows int
		var baseSkips int64
		for _, arm := range narrowBenchArms {
			restore := arm.set()
			before := skips()
			var st ScanStats
			rows := 0
			for range c.QueryRawProjectedStats(q, narrowTestProjection, false, false, &st) {
				rows++
			}
			n := skips() - before
			restore()
			if arm.name == "base" {
				baseRows, baseSkips = rows, n
				if n == 0 {
					t.Fatalf("%s: the plain scan refused no record; the corruption missed (flipped %d)", expr, flipped)
				}
				continue
			}
			if rows != baseRows || n > baseSkips || (!arm.conjunct && n != baseSkips) {
				t.Errorf("%s [%s]: %d rows and %d checksum skips, plain scan %d and %d",
					expr, arm.name, rows, n, baseRows, baseSkips)
			}
			if arm.narrow && st.RecordsNarrowMatched == 0 {
				t.Errorf("%s [%s]: the narrowed match never ran", expr, arm.name)
			}
		}
	}
}
