package collections

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The sealing policy of a mutable table can be widened while it is open (SetEncryptedAttrs). Records
// written from then on seal the newly named attributes; records written before keep them in the clear
// until a migration rewrites them. A reader not entitled to sealed values must not be able to tell the
// two apart: every record reads, matches, counts, groups, ranks and streams as if each attribute the
// policy now seals were sealed, whatever form its value is stored in. These tests build every record
// layout of the sealed parity tests with the explicit set widened part way through and hold every read
// to the same oracle.

// sealedWidenAt makes sealedPut widen the policy of the collection it is keyed by just before writing
// the record numbered by its value (see openWidenedLayout).
var sealedWidenAt sync.Map // *Collection -> int

// sealedWiden widens c's explicit set to the parity tests' sealed attributes.
func sealedWiden(t testing.TB, c *Collection) {
	t.Helper()
	sealedWidenAt.Delete(c)
	if err := c.SetEncryptedAttrs(sealedParityAttrs); err != nil {
		t.Fatal(err)
	}
}

// widenModes are when the policy is widened relative to building a layout. late builds the whole
// layout under the narrow policy, so its segments, columns and delta records hold the newly sealed
// values in the clear, widens, then rewrites every fourth record as it reads now. early widens half way
// through writing the records, before the layout's maintenance runs over both halves. Interning
// re-encodes every record it moves under the current policy, so in a layout that interns every record
// written before the widen nothing is left in the clear and early adds nothing to late; early runs only
// on the layouts that keep pre-widen records as written (earlyLayouts).
var widenModes = []struct {
	name  string
	early bool
}{{"early", true}, {"late", false}}

var earlyLayouts = map[string]bool{"inline": true, "mixed": true}

// openWidenedLayout opens a store sealing only the private attributes and lays the parity records out
// as lay says with the explicit set widened to sealedParityAttrs at the point early names. Records
// holding a newly sealed value in the clear must still be visible -- otherwise the comparison tests
// nothing -- and some records must be sealed. A reopened layout reopens under the widened
// policy, as a restart that restores the persisted set does.
func openWidenedLayout(t *testing.T, lay sealedLayout, early bool) *Collection {
	t.Helper()
	if lay.name == "reopened" {
		lay = widenedReopened
	}
	dir := t.TempDir()
	opts := sealedParityOpts(dir)
	opts.EncryptedAttrs = nil
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if early {
		sealedWidenAt.Store(c, sealedParityN/2)
	}
	built := lay.build(t, c, dir)
	if _, pending := sealedWidenAt.Load(c); pending {
		sealedWidenAt.Delete(c)
		t.Fatalf("layout %s never wrote record %d, where the policy was to be widened", lay.name, sealedParityN/2)
	}
	c = built
	if in, d, col := sealedLayoutCounts(c); !lay.layoutOK(c, in, d, col) {
		t.Fatalf("layout %s: %d inline, %d interned records, %d columnarized segments", lay.name, in, d, col)
	}
	if !early {
		sealedWiden(t, c)
		tx := c.Begin()
		for i := 1; i < sealedParityN; i += 4 {
			tx.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, sealedParityAd(i, lay.patched)))
		}
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("put conflicted")
		}
	}
	if n := visibleUnsealed(t, c); n == 0 {
		t.Fatalf("layout %s: no visible record holds a newly sealed value in the clear; the test is vacuous", lay.name)
	} else if n == sealedParityN {
		t.Fatalf("layout %s: every visible record holds a newly sealed value in the clear; none was written after the widen", lay.name)
	}
	return c
}

// widenedReopened is the reopened layout with its segments interned rather than recompressed under a
// trained dictionary. Most of a record here is the same from record to record once its values are in
// the clear, and the dictionary builder finds no literals in samples its history already covers, so
// RetrainDict refuses; interning is the other rewrite that leaves a reopened store with interned
// segments.
var widenedReopened = sealedLayout{name: "reopened", build: func(t *testing.T, c *Collection, dir string) *Collection {
	sealedPut(t, c, 0, sealedParityN)
	internAll(c)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(sealedParityOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c2.Close() })
	return c2
}, layoutOK: func(_ *Collection, in, d, _ int) bool { return d > 0 && in+d == sealedParityN }}

// visibleUnsealed counts the visible records whose stored form holds a value the current policy seals,
// other than sealed.
func visibleUnsealed(t *testing.T, c *Collection) int {
	t.Helper()
	n := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		forEachVisibleRef(s0, wins, func(r recRef) bool {
			if isSystemKeyBytes(r.key()) {
				return true
			}
			w, err := c.wireAt(r, s0, nil, sealedUnopened)
			if err != nil {
				t.Fatalf("%s: %v", r.key(), err)
			}
			dict := r.dict
			if recIsDelta(r.w.data, r.off) {
				dict = nil // a merged chain is inline-names, whatever segments its versions sit in
			}
			if c.recordHasUnsealedValue(dict, w) {
				n++
			}
			return true
		})
		releaseWindows(wins)
	}
	return n
}

// TestWidenedSealPolicyParity: in every layout, with the policy widened before or after the layout is
// built, every read of a filter on a newly sealed attribute returns for a reader not entitled to sealed
// values exactly what the plain evaluation of the source ads with every sealed attribute removed
// returns, and for an entitled reader what the plain evaluation of the source ads returns.
//
// Serial: the reads switch the package's match configuration (narrowBenchArm.set) as they go.
func TestWidenedSealPolicyParity(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, lay := range sealedLayouts {
		for _, mode := range widenModes {
			if mode.early && !earlyLayouts[lay.name] {
				continue
			}
			t.Run(lay.name+"/"+mode.name, func(t *testing.T) {
				sealedCheckLayout(t, openWidenedLayout(t, lay, mode.early), lay)
			})
		}
	}
}

// TestWidenedSealPolicyEmittingReads: the reads that emit what they read rather than evaluate it --
// the unfiltered scans, raw renders, transactions -- emit no value the policy now seals, in
// any widened layout, and none of the redacted reads opens a sealed value to get there.
func TestWidenedSealPolicyEmittingReads(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Parallel()
	for _, lay := range sealedLayouts {
		for _, mode := range widenModes {
			if mode.early && !earlyLayouts[lay.name] {
				continue
			}
			t.Run(lay.name+"/"+mode.name, func(t *testing.T) {
				t.Parallel()
				c := openWidenedLayout(t, lay, mode.early)
				ctr := &countingSealer{inner: c.sealer}
				c.sealer = ctr
				noPlain := func(read string, b []byte) {
					t.Helper()
					for _, pt := range sealedPlaintexts {
						if bytes.Contains(b, []byte(pt)) {
							t.Fatalf("%s emitted plaintext %q: %.300q", read, pt, b)
						}
					}
				}
				noPlainAd := func(read string, ad *classad.ClassAd) {
					t.Helper()
					noPlain(read, []byte(ad.StringWithPrivate()))
				}
				for ra := range c.ScanRawRedacted() {
					noPlain("ScanRawRedacted", rawAdBytes(ra))
				}
				q := mustParseQ(t, `ClusterId >= 0`)
				for ra := range c.QueryRawRedacted(q) {
					noPlain("QueryRawRedacted", rawAdBytes(ra))
				}
				for _, proj := range [][]string{nil, sealedProj} {
					for _, chase := range []bool{false, true} {
						for ra := range c.ScanRawProjected(proj, chase, true) {
							noPlain("ScanRawProjected", rawAdBytes(ra))
						}
						for ra := range c.QueryRawProjected(q, proj, chase, true) {
							noPlain("QueryRawProjected", rawAdBytes(ra))
						}
					}
					for _, parallel := range []bool{false, true} {
						if parallel {
							c.parallelMinBytes = 0
						} else {
							c.parallelMinBytes = 1 << 62
						}
						for row := range c.ScanRawWire(proj, true) {
							noPlain("ScanRawWire", []byte(strings.Join(rowExprs(t, row, true), "\n")))
						}
						for row := range c.ScanRawWireRefs(proj, true) {
							noPlain("ScanRawWireRefs", []byte(strings.Join(rowExprs(t, row, true), "\n")))
						}
						for row := range c.QueryRawWire(q, proj, true) {
							noPlain("QueryRawWire", []byte(strings.Join(rowExprs(t, row, true), "\n")))
						}
					}
				}
				n := 0
				for ad := range c.scanAs(true) {
					noPlainAd("scan(redact)", ad)
					n++
				}
				if n != sealedParityN {
					t.Fatalf("scan(redact) returned %d ads, want %d", n, sealedParityN)
				}
				c.ForEachAdRedacted(func(_ string, ad *classad.ClassAd) bool {
					noPlainAd("ForEachAdRedacted", ad)
					return true
				})
				tx := c.BeginRedacted()
				for i := 0; i < sealedParityN; i++ {
					ad, ok := tx.Get([]byte(fmt.Sprintf("%d.0", i)))
					if !ok {
						t.Fatalf("Txn(redacted).Get(%d.0): missing", i)
					}
					noPlainAd("Txn(redacted).Get", ad)
				}

				if o, s := ctr.opens.Load(), ctr.seals.Load(); o != 0 || s != 0 {
					t.Errorf("redacted reads used the key: %d opens, %d seals", o, s)
				}
			})
		}
	}
}

// TestWidenedSealPolicyAsOf: a version written before the policy was widened is withheld from a reader
// not entitled to sealed values when read AS OF a time it was current, as the current version is.
func TestWidenedSealPolicyAsOf(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	set := fakeClock(t, 10_000)
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = nil
	opts.DeltaMax = 0
	opts.TimeTravel = &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Second}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	set(10_000)
	sealedPut(t, c, 0, sealedParityN)
	set(20_000)
	sealedWiden(t, c)
	// Every record rewritten after the widen: its current version is sealed, the one before is not.
	sealedPut(t, c, 0, sealedParityN)
	set(30_000)
	_ = c.Put([]byte("tick"), mustAd(t, `[ ClusterId = -1 ]`))
	hidden := sealedHiddenSources(t, false)
	for _, at := range []uint64{15_000, 25_000} {
		for _, f := range sealedParityFilters {
			q := mustParseQ(t, f)
			seq, qerr := c.QueryAsOfRedacted(q, time.UnixMilli(int64(at)))
			if qerr != nil {
				t.Fatal(qerr)
			}
			var ids []int
			for ad := range seq {
				ids = append(ids, checkSealedRow(t, fmt.Sprintf("QueryAsOfRedacted@%d %s", at, f), adRow(ad), true, false))
			}
			sameIDs(t, fmt.Sprintf("QueryAsOfRedacted@%d %s", at, f), ids, sealedOracle(q, hidden))
		}
	}
	// Premise: the entitled AS OF read of the older version sees its plaintext.
	seq, err := c.QueryAsOf(mustParseQ(t, `Owner == "user3"`), time.UnixMilli(15_000))
	if err != nil {
		t.Fatal(err)
	}
	if n := countSeq(seq); n != len(sealedOracle(mustParseQ(t, `Owner == "user3"`), sealedSources(t, false))) {
		t.Fatalf("entitled AS OF read matched %d", n)
	}
}

// TestWidenedSealPolicyWatch: a watcher not entitled to sealed values receives, from a store whose
// policy was widened after the records it catches up on were written, exactly the stream a store
// holding none of the newly sealed values produces -- in catch-up, live, filtered on the attribute,
// through chained parents, coalesced or not, inline and interned. An entitled watcher receives the
// plaintext store's stream.
func TestWidenedSealPolicyWatch(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Parallel()
	open := func(t *testing.T, store kwStore, coalesce time.Duration) *Collection {
		t.Helper()
		opts := Options{
			Shards: 1, Dir: t.TempDir(), SegmentSize: 1 << 16, WatchHistory: 1024, WatchCoalesce: coalesce,
			ParentKeyFor: jobParentKey, IsStructural: jobStructural,
		}
		if store == kwSealed {
			_, opts.DataKey = deriveDataKey(t) // the explicit set starts empty: Secret is in the clear
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	for _, lay := range kwLayouts {
		for _, coalesce := range []time.Duration{0, 5 * time.Millisecond} {
			for _, f := range kwFilters {
				t.Run(fmt.Sprintf("%s/coalesce=%v/%q", lay.name, coalesce, f), func(t *testing.T) {
					t.Parallel()
					run := func(store kwStore, redact bool) []string {
						c := open(t, store, coalesce)
						return kwRun(t, c, store, redact, f, func(c *Collection) {
							if lay.layout != nil {
								lay.layout(c)
							}
							if store == kwSealed {
								if err := c.SetEncryptedAttrs([]string{"Secret"}); err != nil {
									t.Fatal(err)
								}
							}
						})
					}
					if coalesce > 0 {
						// Coalescing folds writes by timing, so a stream is compared only for what it
						// must never carry.
						for _, line := range run(kwSealed, true) {
							for _, s := range append(kwPre, kwLive...) {
								if s.secret != "" && strings.Contains(line, fmt.Sprintf("%q", s.secret)) {
									t.Fatalf("keyless coalesced watch carried %q: %s", s.secret, line)
								}
							}
						}
						return
					}
					kwSame(t, "keyless watch of the widened store vs the hidden store",
						run(kwSealed, true), run(kwHidden, false))
					kwSame(t, "entitled watch of the widened store vs the plaintext store",
						run(kwSealed, false), run(kwPlaintext, false))
				})
			}
		}
	}
}

// TestWidenedSealPolicyMatch: a redacted match reads a slot's newly sealed attribute as undefined
// whether the slot was written before the policy was widened or after, inline or interned, serially or
// in parallel -- the answer TestMatchKeyless requires of slots sealed from the start.
func TestWidenedSealPolicyMatch(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Parallel()
	const slots = 60
	slotAd := func(i int, hide bool) string {
		s := fmt.Sprintf(`[ Name="slot%02d"; Memory=%d; Requirements = TARGET.RequestMemory <= Memory`, i, 1024*(1+i%4))
		if !hide {
			s += fmt.Sprintf(`; Owner="user%d"; SealedNum=%d`, i%7, i)
		}
		return s + " ]"
	}
	jobs := []string{
		`[ RequestMemory=1024; Requirements = TARGET.Owner == "user3"; Rank = TARGET.Memory ]`,
		`[ RequestMemory=2048; Requirements = TARGET.Memory >= MY.RequestMemory; Rank = TARGET.SealedNum ]`,
		`[ RequestMemory=1024; Requirements = TARGET.SealedNum =?= undefined; Rank = TARGET.Memory ]`,
	}
	oracle := func(job string, hide bool, filter *vm.Query) []string {
		m := classad.NewMatchClassAd(mustAd(t, job), nil)
		var rm []RankedMatch
		for i := 0; i < slots; i++ {
			ad := mustAd(t, slotAd(i, hide))
			if filter != nil && !filter.Matches(ad) {
				continue
			}
			if ok, r, hr := matchOne(m, ad); ok {
				rm = append(rm, RankedMatch{Ad: ad, Rank: r, HasRank: hr})
			}
		}
		return rankedNames(sortRanked(rm), 0)
	}
	put := func(t *testing.T, c *Collection, from, step int) {
		tx := c.Begin()
		for i := from; i < slots; i += step {
			tx.Put([]byte(fmt.Sprintf("slot%02d", i)), mustAd(t, slotAd(i, false)))
		}
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("put conflicted")
		}
	}
	const filter = `SealedNum =!= undefined || Memory > 0`
	for _, interned := range []bool{false, true} {
		t.Run(fmt.Sprintf("interned=%v", interned), func(t *testing.T) {
			t.Parallel()
			opts := sealedParityOpts(t.TempDir())
			opts.EncryptedAttrs = nil
			c, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			put(t, c, 0, 1)
			if interned {
				internAll(c)
			}
			if err := c.SetEncryptedAttrs([]string{"Owner", "SealedNum"}); err != nil {
				t.Fatal(err)
			}
			put(t, c, 1, 3) // a third of the slots rewritten under the widened policy
			for _, parallel := range []bool{false, true} {
				if parallel {
					c.parallelMinBytes = 0
				} else {
					c.parallelMinBytes = 1 << 62
				}
				for _, job := range jobs {
					lbl := fmt.Sprintf("parallel=%v %s", parallel, job)
					want, wantHidden := oracle(job, false, nil), oracle(job, true, nil)
					if slices.Equal(want, wantHidden) {
						t.Fatalf("%s: the job does not discriminate", lbl)
					}
					if got := rankedNames(c.MatchSortedRankedRedacted(mustAd(t, job), 0), 0); !sameRankedSet(got, wantHidden, true) {
						t.Errorf("%s: MatchSortedRankedRedacted = %v, want %v", lbl, got, wantHidden)
					}
					got, err := c.MatchSortedRankedFilteredRedacted(mustAd(t, job), filter, 0)
					if err != nil {
						t.Fatal(err)
					}
					if names := rankedNames(got, 0); !sameRankedSet(names, oracle(job, true, mustParseQ(t, filter)), true) {
						t.Errorf("%s: MatchSortedRankedFilteredRedacted = %v, want %v", lbl, names, wantHidden)
					}
					for _, ad := range c.MatchSortedRedacted(mustAd(t, job), 0) {
						if v := ad.EvaluateAttr("Owner"); !v.IsUndefined() {
							t.Fatalf("%s: a redacted match returned Owner = %s", lbl, v.String())
						}
					}
					if got := rankedNames(c.MatchSortedRanked(mustAd(t, job), 0), 0); !sameRankedSet(got, want, true) {
						t.Errorf("%s: MatchSortedRanked = %v, want %v", lbl, got, want)
					}
				}
			}
		})
	}
}

// widenedColumnar is a store whose every record was written, interned and columnarized before its
// policy was widened to sealedParityAttrs, so its columns hold SealedNum -- a numeric column the
// column tiers answer from -- in the clear.
func widenedColumnar(t *testing.T) *Collection {
	t.Helper()
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = nil
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	sealedPut(t, c, 0, sealedParityN)
	internAll(c)
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	if columnarSegmentsForTest(c) == 0 {
		t.Fatal("fixture: no columnarized segment")
	}
	sealedWiden(t, c)
	// Premise: the entitled column reads still answer from the clear SecretNum column.
	if st, ok := c.NumStatsQuery(nil, "SealedNum"); !ok || st.N == 0 {
		t.Fatalf("fixture: the columns do not serve SealedNum (N=%d ok=%v); the checks test nothing", st.N, ok)
	}
	return c
}

// TestWidenedSealPolicyColumnTiers: the column tiers never answer a reader not entitled to sealed values
// from a column holding a newly sealed attribute's clear values. Each either declines -- the keyless
// scan then reads the attribute as undefined -- or answers as if every value were undefined.
func TestWidenedSealPolicyColumnTiers(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Parallel()
	c := widenedColumnar(t)
	if st, ok := c.NumStatsQueryRedacted(nil, "SealedNum"); ok && st.N != 0 {
		t.Errorf("redacted MIN/MAX/SUM over SealedNum read %d values from the columns", st.N)
	}
	if st, ok := c.NumStatsQueryRedacted(mustParseQ(t, `SealedNum > 5`), "RequestMemory"); ok && st.N != 0 {
		t.Errorf("redacted aggregate under a sealed constraint read %d values", st.N)
	}
	if _, seen, ok := c.TopKOrderThresholdRedacted(nil, "SealedNum", true, 5, nil); ok {
		t.Errorf("redacted top-K cutoff ordered %d records by a sealed attribute", seen)
	}
	if _, seen, ok := c.TopKOrderThresholdRedacted(mustParseQ(t, `SealedNum > 5`), "RequestMemory", true, 5, nil); ok && seen != 0 {
		t.Errorf("redacted top-K cutoff under a sealed constraint saw %d records", seen)
	}
	for _, f := range []string{`SealedNum > 5`, `Owner == "user3"`, `SealedNum > 5 && ProcId < 4`} {
		q := mustParseQ(t, f)
		for _, cnt := range []struct {
			name string
			fn   func() (int, bool)
		}{
			{"CountConstraintRedacted", func() (int, bool) { return c.CountConstraintRedacted(f) }},
			{"CountQueryRedacted", func() (int, bool) { return c.CountQueryRedacted(q) }},
			{"columnarEvalCount(redact)", func() (int, bool) { return c.columnarEvalCountAs(q, true) }},
			{"vectorEvalCount(redact)", func() (int, bool) { return c.vectorEvalCountAs(q, true) }},
		} {
			if n, ok := cnt.fn(); ok && n != 0 {
				t.Errorf("%s(%s) = %d, want 0", cnt.name, f, n)
			}
		}
	}
	groupBys := []struct {
		name    string
		groups  []GroupSpec
		aggs    []GroupAggCol
		filters []string
	}{
		{"group by sealed", []GroupSpec{{Attr: "SealedNum"}}, nil, nil},
		{"aggregate sealed", []GroupSpec{{Attr: "JobStatus"}}, []GroupAggCol{{Attr: "SealedNum"}}, nil},
		{"filter on sealed", []GroupSpec{{Attr: "JobStatus"}}, []GroupAggCol{{Attr: "RequestMemory", Filter: `SealedNum > 5`}}, nil},
		{"count filter on sealed", []GroupSpec{{Attr: "JobStatus"}}, nil, []string{`SealedNum > 5`}},
	}
	for _, g := range groupBys {
		if gs, ok := c.GroupStatsAllAggsRedacted(g.groups, g.aggs, g.filters); ok {
			t.Errorf("%s: redacted match-all GROUP BY answered from the columns (%d groups)", g.name, len(gs))
		}
		if gs, ok := c.GroupStatsConstraintAggsRedacted(`RequestMemory > 0`, g.groups, g.aggs, g.filters); ok {
			t.Errorf("%s: redacted GROUP BY answered from the columns (%d groups)", g.name, len(gs))
		}
	}
	// Premise: an entitled GROUP BY over the sealed column is served from the columns.
	if _, ok := c.GroupStatsAllAggs([]GroupSpec{{Attr: "SealedNum"}}, nil, nil); !ok {
		t.Error("fixture: GROUP BY SealedNum is not served from the columns even for an entitled reader")
	}
	if gs, ok := c.GroupStatsConstraintAggsRedacted(`SealedNum > 5`, []GroupSpec{{Attr: "JobStatus"}}, nil, nil); ok {
		for _, g := range gs {
			if g.Count != 0 {
				t.Errorf("redacted grouped count under a sealed constraint counted %d", g.Count)
			}
		}
	}
	for vals := range c.QueryProjectRedacted(mustParseQ(t, `true`), []string{"SealedNum", "Owner"}) {
		if !vals[0].IsUndefined() || !vals[1].IsUndefined() {
			t.Fatalf("redacted projection read SealedNum = %s, Owner = %s", vals[0].String(), vals[1].String())
		}
	}
}

// TestWidenedSealPolicyComputedValues: a computed value kept beside a column is a function of the
// attributes its expression reads. One computed before the policy sealed such an attribute is a function
// of that attribute's plaintext, and a reader not entitled to sealed values must not be answered from
// it: MemDouble = RequestMemory * 2 reads as undefined to such a reader once RequestMemory is sealed.
func TestWidenedSealPolicyComputedValues(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Parallel()
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = nil
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sealedPut(t, c, 0, sealedParityN)
	internAll(c)
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	computed := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cs := seg.colblk.Load(); cs != nil && cs.hasComputed() {
				computed++
			}
		}
		sh.mu.RUnlock()
	}
	if computed == 0 {
		t.Fatal("fixture: no segment keeps a computed value; the test is vacuous")
	}
	if err := c.SetEncryptedAttrs([]string{"RequestMemory"}); err != nil {
		t.Fatal(err)
	}
	// Premise: the entitled count is served, from the computed values, with the real answer.
	want := 0
	for _, ad := range sealedSources(t, false) {
		if mustParseQ(t, `MemDouble > 2000`).Matches(ad) {
			want++
		}
	}
	if n, ok := c.CountQuery(mustParseQ(t, `MemDouble > 2000`)); !ok || n != want {
		t.Fatalf("fixture: entitled count = %d ok=%v, want %d", n, ok, want)
	}
	for _, f := range []string{`MemDouble > 2000`, `MemDouble > 2000 || ProcId == 100`, `MemDouble =!= undefined`} {
		q := mustParseQ(t, f)
		for _, cnt := range []struct {
			name string
			fn   func() (int, bool)
		}{
			{"CountQueryRedacted", func() (int, bool) { return c.CountQueryRedacted(q) }},
			{"columnarEvalCount(redact)", func() (int, bool) { return c.columnarEvalCountAs(q, true) }},
			{"vectorEvalCount(redact)", func() (int, bool) { return c.vectorEvalCountAs(q, true) }},
		} {
			if n, ok := cnt.fn(); ok && n != 0 {
				t.Errorf("%s(%s) = %d, want 0", cnt.name, f, n)
			}
		}
		if n := countSeq(c.QueryRedacted(q)); n != 0 {
			t.Errorf("QueryRedacted(%s) matched %d", f, n)
		}
	}
	if st, ok := c.NumStatsQueryRedacted(nil, "MemDouble"); ok && st.N != 0 {
		t.Errorf("redacted aggregate over MemDouble read %d computed values", st.N)
	}
	if _, seen, ok := c.TopKOrderThresholdRedacted(nil, "MemDouble", true, 5, nil); ok && seen != 0 {
		t.Errorf("redacted top-K cutoff ordered %d records by computed values", seen)
	}
	for _, g := range [][]GroupSpec{{{Attr: "MemDouble"}}, {{Attr: "JobStatus"}}} {
		gs, ok := c.GroupStatsAllAggsRedacted(g, []GroupAggCol{{Attr: "MemDouble"}}, nil)
		if !ok {
			continue
		}
		for _, s := range gs {
			if len(s.Stats) > 0 && s.Stats[0].N != 0 {
				t.Errorf("redacted GROUP BY %s aggregated %d computed values", g[0].Attr, s.Stats[0].N)
			}
			if g[0].Attr == "MemDouble" && !s.Values[0].IsUndefined() {
				t.Errorf("redacted GROUP BY MemDouble produced group %s", s.Values[0].String())
			}
		}
	}
	// A migration rewrites every segment that holds RequestMemory in the clear, computed values and
	// all, and certifies the store; the redacted count is then served and still undefined throughout.
	if c.MigrateSealedAttrs(0) == 0 {
		t.Fatal("the migration rewrote nothing")
	}
	if !c.computedTrusted() {
		t.Fatal("a clean migration did not certify the store")
	}
	if n, ok := c.CountQueryRedacted(mustParseQ(t, `MemDouble > 2000`)); ok && n != 0 {
		t.Errorf("after the migration CountQueryRedacted(MemDouble > 2000) = %d, want 0", n)
	}
	if n, ok := c.CountQuery(mustParseQ(t, `MemDouble > 2000`)); ok && n != want {
		t.Errorf("after the migration the entitled count = %d, want %d", n, want)
	}
}

// TestWidenedSealPolicyMigration: a seal migration run on the open store after a widen -- what table
// maintenance does -- leaves no newly sealed value in the clear in any layout, every read still answers
// as the oracle says, and the computed values the store keeps afterwards are trusted again. The reads
// run serially under the default match configuration only (see TestWidenedSealPolicyParity).
func TestWidenedSealPolicyMigration(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			c := openWidenedLayout(t, lay, false)
			if c.computedTrusted() {
				t.Fatal("computed values are trusted right after a widen")
			}
			if c.MigrateSealedAttrs(0) == 0 {
				t.Fatal("the migration rewrote nothing after a widen")
			}
			if n := visibleUnsealed(t, c); n != 0 {
				t.Fatalf("%d visible records hold a newly sealed value in the clear after the migration", n)
			}
			if !c.computedTrusted() {
				t.Fatal("a clean migration did not certify the store")
			}
			serial := lay
			serial.every = false
			sealedCheckLayout(t, c, serial)
		})
	}
}

// TestWidenDuringMigrationIsNotCertified: a widen that lands while a migration runs is not covered by
// it. Records the pass had already copied hold the newly named attribute in the clear, so the pass must
// neither write a marker for the widened policy nor certify the store under it, and the next pass must
// seal what is left.
func TestWidenDuringMigrationIsNotCertified(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = nil
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sealedPut(t, c, 0, sealedParityN)
	sealedWiden(t, c) // Owner, Token, SealedNum: the pass below has work to do
	sealMigrateHook = func(stage string) {
		if stage == "resealed" {
			if err := c.SetEncryptedAttrs(append(slices.Clone(sealedParityAttrs), "JobStatus")); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { sealMigrateHook = nil }()
	if c.MigrateSealedAttrs(1) == 0 {
		t.Fatal("the first pass rewrote nothing")
	}
	sealMigrateHook = nil
	if c.sealMigrationDone() {
		t.Error("the marker certifies a policy widened while the pass ran")
	}
	if c.computedTrusted() {
		t.Error("the store is certified under a policy widened while the pass ran")
	}
	if visibleUnsealed(t, c) == 0 {
		t.Fatal("fixture: nothing is left in the clear under the widened policy; the check tests nothing")
	}
	if c.MigrateSealedAttrs(1) == 0 {
		t.Fatal("the next pass did not run")
	}
	if n := visibleUnsealed(t, c); n != 0 {
		t.Fatalf("%d records still hold a value the policy names in the clear", n)
	}
	if !c.sealMigrationDone() || !c.computedTrusted() {
		t.Error("a clean pass under the current policy did not certify it")
	}
}

// TestWidenMigrationSealFailureIsNotCertified: the migration maintenance runs after a widen re-seals
// each record that holds a newly named attribute in the clear. When those seals fail the pass rewrites
// nothing, so the store still holds that plaintext: it must neither write a marker for the widened
// policy nor certify the store (computed values derived from the plaintext stay untrusted), unentitled
// reads must keep withholding the attribute, and no record may be lost. Once sealing works the next
// pass seals what is left and certifies the store.
func TestWidenMigrationSealFailureIsNotCertified(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = nil
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sealedPut(t, c, 0, sealedParityN)
	sealedWiden(t, c)
	before := visibleUnsealed(t, c)
	if before == 0 {
		t.Fatal("fixture: nothing is in the clear under the widened policy; the pass has nothing to do")
	}
	withheld := mustParseQ(t, `Owner =!= undefined`)
	fs := injectSealFailure(c)
	if got := c.MigrateSealedAttrs(1); got != 0 {
		t.Errorf("MigrateSealedAttrs rewrote %d segments while every Seal failed", got)
	}
	if c.sealMigrationDone() {
		t.Error("the marker certifies a policy no record could be sealed under")
	}
	if c.computedTrusted() {
		t.Error("the store is certified clean while it still holds newly sealed values in the clear")
	}
	if n := visibleUnsealed(t, c); n != before {
		t.Errorf("%d records hold a newly sealed value in the clear after the failed pass, want the %d before it", n, before)
	}
	if n := c.Len(); n != sealedParityN {
		t.Errorf("the failed pass left %d records, want %d", n, sealedParityN)
	}
	if n := countSeq(c.QueryRedacted(withheld)); n != 0 {
		t.Errorf("after the failed pass an unentitled read sees Owner in %d records", n)
	}
	fs.failing.Store(false)
	if c.MigrateSealedAttrs(1) == 0 {
		t.Fatal("the pass after sealing recovered rewrote nothing")
	}
	if n := visibleUnsealed(t, c); n != 0 {
		t.Fatalf("%d records still hold a value the policy names in the clear", n)
	}
	if !c.sealMigrationDone() || !c.computedTrusted() {
		t.Error("a clean pass under the current policy did not certify it")
	}
	if n := c.Len(); n != sealedParityN {
		t.Errorf("the migration left %d records, want %d", n, sealedParityN)
	}
}
