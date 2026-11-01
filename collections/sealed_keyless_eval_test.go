package collections

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Evaluation for a reader not entitled to sealed values. Redacting what a read returns is not enough:
// which records match, how many do, which group each lands in and where it sorts are all answers about
// the values the evaluation read. A redacted read therefore evaluates with no key, and must select,
// count, group and rank exactly as the plain evaluation of the same records with every sealed attribute
// REMOVED does. TestSealedValueDictSegmentParity runs these checks in every record layout (inline,
// interned, mixed, columnarized, delta records over interned bases, reopened), serially and in parallel.

// sealedCheckKeyless runs every keyless evaluating read of q over c against wantHidden, the records the
// plain evaluation of the sealed-attribute-free sources selects.
func sealedCheckKeyless(t *testing.T, c *Collection, lbl func(string) string, q *vm.Query, wantHidden map[int]bool, patched bool) {
	t.Helper()
	var ids []int
	for ra := range c.QueryRawRedacted(q) {
		ids = append(ids, checkSealedRow(t, lbl("QueryRawRedacted"), rawAdMap(t, ra), true, patched))
	}
	sameIDs(t, lbl("QueryRawRedacted"), ids, wantHidden)

	ids = ids[:0]
	seq, _ := c.QueryRawFromSeqRedacted(q, sealedProj, SeqCursor{}, 0)
	for ra := range seq {
		ids = append(ids, checkSealedRow(t, lbl("QueryRawFromSeqRedacted"), rawAdMap(t, ra), true, patched))
	}
	sameIDs(t, lbl("QueryRawFromSeqRedacted"), ids, wantHidden)

	// The column tiers. Each may decline (ok=false); one that answers must answer for the keyless reader.
	for _, count := range []struct {
		name string
		fn   func(*vm.Query) (int, bool)
	}{
		{"CountQueryRedacted", c.CountQueryRedacted},
		{"columnarEvalCount(redact)", func(q *vm.Query) (int, bool) { return c.columnarEvalCountAs(q, true) }},
		{"vectorEvalCount(redact)", func(q *vm.Query) (int, bool) { return c.vectorEvalCountAs(q, true) }},
	} {
		if n, ok := count.fn(q); ok && n != len(wantHidden) {
			t.Errorf("%s = %d, want %d", lbl(count.name), n, len(wantHidden))
		}
	}

	// QueryProject is what aggregates and top-K scan: the projected sealed values are undefined, and the
	// expression beside them still evaluates.
	ids = ids[:0]
	for vals := range c.QueryProjectRedacted(q, sealedProjectAttrs) {
		m := map[string]string{}
		for i, v := range vals {
			switch {
			case v.IsUndefined():
			case v.IsInteger():
				n, _ := v.IntValue()
				m[sealedProjectAttrs[i]] = strconv.FormatInt(n, 10)
			default:
				m[sealedProjectAttrs[i]] = v.String()
			}
		}
		id := checkSealedRow(t, lbl("QueryProjectRedacted"), m, true, patched)
		if want := strconv.Itoa(2 * 512 * (1 + id%4)); m["MemDouble"] != want {
			t.Fatalf("%s: record %d MemDouble = %q, want %s", lbl("QueryProjectRedacted"), id, m["MemDouble"], want)
		}
		ids = append(ids, id)
	}
	sameIDs(t, lbl("QueryProjectRedacted"), ids, wantHidden)

	// Grouped columns: a record the columns cannot decide is matched keyless. Group values are the
	// plaintext JobStatus column, so the answer is the keyless records' histogram.
	byStatus := map[string]int{}
	var memSum int64
	for i := range wantHidden {
		byStatus[strconv.Itoa(sealedParityJobStatus(i, patched))]++
		memSum += int64(512 * (1 + i%4))
	}
	if gs, ok := c.groupStatsQuery(q, []GroupSpec{{Attr: "JobStatus"}}, nil, nil, nil, true); ok {
		got := map[string]int{}
		for _, s := range gs {
			got[s.Values[0].String()] = s.Count
		}
		if !sealedMapsEqual(got, byStatus) {
			t.Errorf("%s by JobStatus = %v, want %v", lbl("groupStatsQuery(redact)"), got, byStatus)
		}
	}

	// The column statistics take no entitlement: they read only columns and decline any cell they
	// cannot read as a literal, a sealed one included. So whenever they answer, the answer must be the
	// keyless reader's as well as the entitled one's (which TestSealedValueDictSegmentParity checks).
	if ns, ok := c.NumStatsQuery(q, "RequestMemory"); ok && (ns.N != len(wantHidden) || ns.IntSum != memSum) {
		t.Errorf("%s(RequestMemory) = n %d sum %d, want n %d sum %d (keyless)", lbl("NumStatsQuery"),
			ns.N, ns.IntSum, len(wantHidden), memSum)
	}
	if ns, ok := c.NumStatsQuery(q, "SealedNum"); ok && ns.N != 0 {
		t.Errorf("%s(SealedNum) answered from %d sealed values", lbl("NumStatsQuery"), ns.N)
	}
	if _, seen, ok := c.TopKOrderThreshold(q, "RequestMemory", true, 5, nil); ok && seen != len(wantHidden) {
		t.Errorf("%s(RequestMemory): seen %d, want %d (keyless)", lbl("TopKOrderThreshold"), seen, len(wantHidden))
	}
	if _, seen, ok := c.TopKOrderThreshold(q, "SealedNum", true, 5, nil); ok && seen != 0 {
		t.Errorf("%s(SealedNum): ordered %d sealed values", lbl("TopKOrderThreshold"), seen)
	}

	// The key scans behind KeysWhere/DeleteWhere, and a redacted transaction's query.
	ids = ids[:0]
	c.ForEachAdRedacted(func(_ string, ad *classad.ClassAd) bool {
		if q.Matches(ad) {
			ids = append(ids, checkSealedRow(t, lbl("ForEachAdRedacted"), adRow(ad), true, patched))
		}
		return true
	})
	sameIDs(t, lbl("ForEachAdRedacted"), ids, wantHidden)

	tx := c.BeginRedacted()
	ids = ids[:0]
	for ad := range tx.Query(q) {
		ids = append(ids, checkSealedRow(t, lbl("Txn(redacted).Query"), adRow(ad), true, patched))
	}
	sameIDs(t, lbl("Txn(redacted).Query"), ids, wantHidden)
	ids = ids[:0]
	for key := range tx.KeysWhere(q) {
		var cl, proc int
		if _, err := fmt.Sscanf(key, "%d.%d", &cl, &proc); err != nil {
			t.Fatalf("%s: key %q", lbl("Txn(redacted).KeysWhere"), key)
		}
		ids = append(ids, cl)
	}
	sameIDs(t, lbl("Txn(redacted).KeysWhere"), ids, wantHidden)
}

// TestSealedParentValueKeyless is the chained form of the keyless check: a job's Owner and SealedNum live
// on its cluster ad (the parent), sealed there, so a redacted filter on them reads the parent with no key.
func TestSealedParentValueKeyless(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const clusters, procs = 40, 4
	parentAd := func(cl int, hide bool) string {
		if hide {
			return fmt.Sprintf(`[ ClusterId=%d; Cmd="/bin/sleep" ]`, cl)
		}
		return fmt.Sprintf(`[ ClusterId=%d; Owner="user%d"; SealedNum=%d; Cmd="/bin/sleep" ]`, cl, cl%7, cl%11)
	}
	childAd := func(cl, p int) string {
		return fmt.Sprintf(`[ ProcId=%d; JobStatus=%d ]`, p, 1+(cl+p)%5)
	}
	opts := sealedParityOpts(t.TempDir())
	opts.ParentKeyFor, opts.IsStructural = jobParentKey, jobStructural
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	tx := c.Begin()
	for cl := 1; cl <= clusters; cl++ {
		tx.Put([]byte(fmt.Sprintf("%d.-1", cl)), mustAd(t, parentAd(cl, false)))
		for p := 0; p < procs; p++ {
			tx.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), mustAd(t, childAd(cl, p)))
		}
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("put conflicted")
	}
	internAll(c)
	for _, f := range []string{`Owner == "user3"`, `Owner =?= undefined && ProcId < 2`, `SealedNum > 5 || JobStatus == 2`} {
		q := mustParseQ(t, f)
		oracle := func(hide bool) map[string]bool {
			out := map[string]bool{}
			for cl := 1; cl <= clusters; cl++ {
				parent := mustAd(t, parentAd(cl, hide))
				for p := 0; p < procs; p++ {
					child := mustAd(t, childAd(cl, p))
					child.SetParent(parent)
					if q.Matches(child) {
						out[fmt.Sprintf("%d.%d", cl, p)] = true
					}
				}
			}
			return out
		}
		want, wantHidden := oracle(false), oracle(true)
		if sealedMapsEqualBool(want, wantHidden) {
			t.Fatalf("%s: the filter does not discriminate", f)
		}
		read := func(seq func(func(*classad.ClassAd) bool)) map[string]bool {
			got := map[string]bool{}
			for ad := range seq {
				cl, _ := ad.EvaluateAttrInt("ClusterId")
				p, _ := ad.EvaluateAttrInt("ProcId")
				got[fmt.Sprintf("%d.%d", cl, p)] = true
			}
			return got
		}
		if got := read(c.QueryRedacted(q)); !sealedMapsEqualBool(got, wantHidden) {
			t.Errorf("QueryRedacted: %s: %d records, want %d", f, len(got), len(wantHidden))
		}
		if got := read(c.Query(q)); !sealedMapsEqualBool(got, want) {
			t.Errorf("Query: %s: %d records, want %d", f, len(got), len(want))
		}
	}
}

// TestMatchKeyless: a redacted match evaluates the job's Requirements and Rank, and the slot's
// Requirements, against the slot as its reader is shown it. A slot's sealed attribute is undefined to
// the match exactly as it would be if the slot did not carry it, in every layout the matcher reads
// (inline, interned, parallel), while the entitled match is unchanged.
func TestMatchKeyless(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
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
	// oracle matches job against the source slots the reference way.
	oracle := func(job string, hide bool, limit int) []string {
		j := mustAd(t, job)
		m := classad.NewMatchClassAd(j, nil)
		var rm []RankedMatch
		for i := 0; i < slots; i++ {
			ad := mustAd(t, slotAd(i, hide))
			if ok, r, hr := matchOne(m, ad); ok {
				rm = append(rm, RankedMatch{Ad: ad, Rank: r, HasRank: hr})
			}
		}
		return rankedNames(sortRanked(rm), limit)
	}
	for _, interned := range []bool{false, true} {
		c, err := Open(sealedParityOpts(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		tx := c.Begin()
		for i := 0; i < slots; i++ {
			tx.Put([]byte(fmt.Sprintf("slot%02d", i)), mustAd(t, slotAd(i, false)))
		}
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("put conflicted")
		}
		if interned {
			internAll(c)
		}
		for _, parallel := range []bool{false, true} {
			if parallel {
				c.parallelMinBytes = 0
			} else {
				c.parallelMinBytes = 1 << 62
			}
			for _, job := range jobs {
				for _, limit := range []int{0, 3} { // 3 defers materialization to the returned top-N
					lbl := fmt.Sprintf("interned=%v parallel=%v limit=%d %s", interned, parallel, limit, job)
					want, wantHidden := oracle(job, false, limit), oracle(job, true, limit)
					if slices.Equal(want, wantHidden) {
						t.Fatalf("%s: the job does not discriminate", lbl)
					}
					if got := rankedNames(c.MatchSortedRankedRedacted(mustAd(t, job), limit), limit); !sameRankedSet(got, wantHidden, limit == 0) {
						t.Errorf("%s: MatchSortedRankedRedacted = %v, want %v", lbl, got, wantHidden)
					}
					got, err := c.MatchSortedRankedFilteredRedacted(mustAd(t, job), `SealedNum =!= undefined || Memory > 0`, limit)
					if err != nil {
						t.Fatal(err)
					}
					if names := rankedNames(got, limit); !sameRankedSet(names, wantHidden, limit == 0) {
						t.Errorf("%s: MatchSortedRankedFilteredRedacted = %v, want %v", lbl, names, wantHidden)
					}
					for _, m := range c.MatchSortedRankedRedacted(mustAd(t, job), limit) {
						if v := m.Ad.EvaluateAttr("Owner"); !v.IsUndefined() {
							t.Fatalf("%s: a redacted match returned Owner = %s", lbl, v.String())
						}
					}
					if got := rankedNames(c.MatchSortedRanked(mustAd(t, job), limit), limit); !sameRankedSet(got, want, limit == 0) {
						t.Errorf("%s: MatchSortedRanked = %v, want %v", lbl, got, want)
					}
				}
			}
		}
		c.Close()
	}
}

// sortRanked orders matches as MatchSortedRanked does: ranked first, then by Rank descending.
func sortRanked(rm []RankedMatch) []RankedMatch {
	slices.SortStableFunc(rm, func(a, b RankedMatch) int {
		switch {
		case a.HasRank != b.HasRank:
			if a.HasRank {
				return -1
			}
			return 1
		case a.HasRank && a.Rank != b.Rank:
			if a.Rank > b.Rank {
				return -1
			}
			return 1
		}
		return 0
	})
	return rm
}

// rankedNames renders a ranked result as "rank:name" in rank order, truncated to limit. Ties in Rank
// have no specified order, so sameRankedSet compares within each rank as a set.
func rankedNames(rm []RankedMatch, limit int) []string {
	if limit > 0 && len(rm) > limit {
		rm = rm[:limit]
	}
	out := make([]string, len(rm))
	for i, m := range rm {
		name, _ := m.Ad.EvaluateAttrString("Name")
		r := "-"
		if m.HasRank {
			r = strconv.FormatFloat(m.Rank, 'g', -1, 64)
		}
		out[i] = r + ":" + name
	}
	return out
}

// sameRankedSet compares two rankedNames results: the same sequence of ranks, and -- for an untruncated
// result (full) -- the same entries. Ties in Rank have no specified order, so entries compare as a set;
// a truncation that cuts through a tie may keep different members of it, so a truncated result is
// compared on its rank sequence alone.
func sameRankedSet(a, b []string, full bool) bool {
	if len(a) != len(b) {
		return false
	}
	rank := func(s string) string { return s[:strings.IndexByte(s, ':')] }
	for i := range a {
		if rank(a[i]) != rank(b[i]) {
			return false
		}
	}
	return !full || slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// TestKeylessEvalNeverUsesKey: a redacted evaluation does not open a sealed value at all -- not to read
// it, and not to open it and discard or re-seal it. Equal answers cannot show that (a path that opens a
// value and seals it again answers the same), so this counts every use of the key while the redacted
// reads run, in every record layout.
func TestKeylessEvalNeverUsesKey(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	job := `[ RequestMemory=512; Requirements = TARGET.Owner == "user3" || TARGET.ProcId < 3; Rank = TARGET.SealedNum ]`
	for _, lay := range sealedLayouts {
		if lay.patched {
			// Not this layout: a delta chain whose base is interned and whose patches are inline cannot be
			// spliced as bytes, so reading it -- for any reader -- merges the decoded versions and
			// re-encodes them (materializeAt), opening each sealed value and sealing it again. Nothing
			// opened there reaches the reader: the merged record is sealed as stored, and a redacted
			// read evaluates it with no key, which TestSealedValueDictSegmentParity checks on this layout.
			continue
		}
		t.Run(lay.name, func(t *testing.T) {
			c := openSealedLayout(t, lay)
			ctr := &countingSealer{inner: c.sealer}
			c.sealer = ctr
			for _, f := range sealedParityFilters {
				q := mustParseQ(t, f)
				for range c.QueryRedacted(q) {
				}
				for range c.QueryRawRedacted(q) {
				}
				for range c.QueryRawProjected(q, sealedProj, true, true) {
				}
				for range c.QueryRawWire(q, sealedProj, true) {
				}
				for range c.QueryRawWireRefs(q, sealedProj, true) {
				}
				for range c.QueryProjectRedacted(q, sealedProjectAttrs) {
				}
				c.CountQueryRedacted(q)
				c.groupStatsQuery(q, []GroupSpec{{Attr: "JobStatus"}}, nil, nil, nil, true)
				for range c.BeginRedacted().Query(q) {
				}
			}
			c.ForEachAdRedacted(func(string, *classad.ClassAd) bool { return true })
			for _, limit := range []int{0, 3} {
				c.MatchSortedRankedRedacted(mustAd(t, job), limit)
				if _, err := c.MatchSortedRankedFilteredRedacted(mustAd(t, job), `Token == "tok4" || ProcId < 5`, limit); err != nil {
					t.Fatal(err)
				}
			}
			if o, s := ctr.opens.Load(), ctr.seals.Load(); o != 0 || s != 0 {
				t.Errorf("redacted evaluation used the key: %d opens, %d seals", o, s)
			}
			// Premise: the entitled reads of the same records do use it.
			for range c.Query(mustParseQ(t, `Owner == "user3"`)) {
			}
			c.MatchSortedRanked(mustAd(t, job), 0)
			if ctr.opens.Load() == 0 {
				t.Fatal("the entitled reads opened nothing; the count proves nothing")
			}
		})
	}
}
