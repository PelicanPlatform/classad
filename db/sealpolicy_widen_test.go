package db

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A table whose sealing policy is widened while it is open (SetEncryptedAttrs) holds the newly named
// attributes in the clear in every record written before -- in its segments, and in the columns and
// computed values of segments columnarized before. These hold every unprivileged read the table serves
// -- the aggregate ladder, top-K, queries -- to the answers a table holding none of those attributes
// gives for the same writes, and check that the next maintenance pass seals what was left in the clear.

const widenN = 600

// widenToken is a per-record secret that shares no run of bytes with any other record's, so a search
// for it on disk finds it only where that record's value is stored in the clear.
func widenToken(i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("salary/%d", i)))
	return "SALARY-" + hex.EncodeToString(sum[:12])
}

// widenAd is record i. SecretNum and Salary are the attributes the widen seals; MemDouble is an
// expression over SecretNum, which columnarizing keeps a computed value for.
func widenAd(t *testing.T, i int, hidden bool) *classad.ClassAd {
	t.Helper()
	src := fmt.Sprintf(`[ ClusterId=%d; JobStatus=%d; RequestMemory=%d; MemDouble = SecretNum * 2; Pad="%0120d"`,
		i, i%5, 1024+i, i)
	if !hidden {
		src += fmt.Sprintf(`; SecretNum=%d; Salary=%q`, 7000+i, widenToken(i))
	}
	ad, err := classad.Parse(src + " ]")
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

func widenPut(t *testing.T, d *DB, hidden bool, from, step int) {
	t.Helper()
	for i := from; i < widenN; i += step {
		if err := d.Put(fmt.Sprintf("%d.0", i), widenAd(t, i, hidden)); err != nil {
			t.Fatal(err)
		}
	}
}

// widenTables returns the widened table and the reference: the same writes with SecretNum and Salary
// left out, in a table that seals nothing.
func widenTables(t *testing.T) (sealed, hidden *DB, dir string) {
	t.Helper()
	dir = t.TempDir()
	pool := KEK{ID: "POOL", Material: []byte("pool-signing-key-material-for-the-widen-test")}
	sealed, err := OpenConfig(Config{Dir: dir, PoolKeys: []KEK{pool}, SegmentSize: 1 << 15})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sealed.Close() })
	hidden, err = OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 15})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hidden.Close() })
	for _, tbl := range []struct {
		d      *DB
		hidden bool
	}{{sealed, false}, {hidden, true}} {
		widenPut(t, tbl.d, tbl.hidden, 0, 1)
		if !tbl.d.EnableSchemaScan(2000, 8) {
			t.Fatal("no columnar schema derived")
		}
		tbl.d.c.ColumnarizeSealed()
	}
	// Premise: the columns serve SecretNum before the widen.
	if st, ok := sealed.NumStats("", "SecretNum"); !ok || st.N == 0 {
		t.Fatalf("fixture: the columns do not serve SecretNum (N=%d ok=%v)", st.N, ok)
	}
	if err := sealed.SetEncryptedAttrs([]string{"SecretNum", "Salary"}); err != nil {
		t.Fatal(err)
	}
	// Premise: right after the widen, before any write seals a cell of it, the entitled column read
	// still answers from the clear SecretNum column -- the one a redacted read must not answer from.
	if st, ok := sealed.NumStats("", "SecretNum"); !ok || st.N != widenN {
		t.Fatalf("fixture: after the widen the columns serve %d SecretNum values (ok=%v), want %d", st.N, ok, widenN)
	}
	return sealed, hidden, dir
}

func renderAggRows(rows []AggRow) string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = strings.Join(r.Group, ",") + " => " + strings.Join(r.Values, ",")
	}
	slices.Sort(out)
	return strings.Join(out, "\n")
}

var widenAggs = []struct {
	where  string
	groups []GroupCol
	aggs   []AggSpec
}{
	{"true", nil, []AggSpec{{Func: AggMax, Arg: "SecretNum"}}},
	{"true", nil, []AggSpec{{Func: AggSum, Arg: "MemDouble"}}},
	{"true", nil, []AggSpec{{Func: AggCount, Arg: "SecretNum"}}},
	{"SecretNum > 7300", nil, []AggSpec{{Func: AggCount}}},
	{"MemDouble > 14600", nil, []AggSpec{{Func: AggCount}}},
	{"RequestMemory > 1200", nil, []AggSpec{{Func: AggMin, Arg: "SecretNum"}}},
	{"true", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggMax, Arg: "SecretNum"}}},
	{"true", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggAvg, Arg: "MemDouble"}}},
	{"true", []GroupCol{{Attr: "SecretNum", BucketWidth: 100}}, []AggSpec{{Func: AggCount}}},
	{"true", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggCount, Filter: "SecretNum > 7300"}}},
	{"RequestMemory > 1100", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggSum, Arg: "RequestMemory", Filter: "MemDouble > 14600"}}},
	{"SecretNum > 7300 || JobStatus == 2", []GroupCol{{Attr: "JobStatus"}}, []AggSpec{{Func: AggCount}}},
}

// TestWidenedSealPolicyTableReads: every unprivileged read of a widened table answers as the table
// without the newly sealed attributes does, before and after the maintenance pass that migrates it,
// and that pass leaves none of their values in the clear on disk.
func TestWidenedSealPolicyTableReads(t *testing.T) {
	t.Parallel()
	sealed, hidden, dir := widenTables(t)
	check := func(when string) {
		t.Helper()
		for _, a := range widenAggs {
			got, err := sealed.AggregateColsStatsRedacted(a.where, a.groups, a.aggs, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want, err := hidden.AggregateColsStats(a.where, a.groups, a.aggs, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if g, w := renderAggRows(got), renderAggRows(want); g != w {
				t.Errorf("%s: redacted aggregate %q %v %v\n got: %s\nwant: %s", when, a.where, a.groups, a.aggs, g, w)
			}
		}
		for _, k := range []struct {
			where, order string
		}{{"", "SecretNum"}, {"", "MemDouble"}, {"SecretNum > 7300", "RequestMemory"}, {"MemDouble > 14600", "RequestMemory"}, {"", "RequestMemory"},
			// A disjunction on the sealed attribute: this reader's top five are JobStatus == 2's.
			{"SecretNum > 7300 || JobStatus == 2", "RequestMemory"}} {
			got, err := sealed.TopKRedacted(k.where, []string{"ClusterId", "SecretNum", "Salary", "MemDouble"}, k.order, true, 5)
			if err != nil {
				t.Fatal(err)
			}
			want, err := hidden.TopK(k.where, []string{"ClusterId", "SecretNum", "Salary", "MemDouble"}, k.order, true, 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Errorf("%s: redacted top-K %q by %s returned %d rows, want %d", when, k.where, k.order, len(got), len(want))
				continue
			}
			for i, row := range got {
				for j := 1; j < len(row); j++ {
					if !row[j].IsUndefined() {
						t.Errorf("%s: redacted top-K %q by %s row %d carries %s", when, k.where, k.order, i, row[j].String())
					}
				}
				if k.order == "RequestMemory" && row[0].String() != want[i][0].String() {
					t.Errorf("%s: redacted top-K %q by RequestMemory row %d = %s, want %s", when, k.where, i, row[0].String(), want[i][0].String())
				}
			}
		}
		for _, where := range []string{`SecretNum > 7300`, `Salary =!= undefined`, `MemDouble > 14600`, `true`} {
			seq, err := sealed.QueryRedacted(where)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for ad := range seq {
				if bytes.Contains([]byte(ad.StringWithPrivate()), []byte("SALARY-")) {
					t.Fatalf("%s: redacted query %s returned a sealed value", when, where)
				}
				n++
			}
			hseq, err := hidden.Query(where)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			for range hseq {
				want++
			}
			if n != want {
				t.Errorf("%s: redacted query %s matched %d, want %d", when, where, n, want)
			}
		}
	}
	check("after the widen")
	// A quarter of the records rewritten after the widen, as they read now.
	widenPut(t, sealed, false, 1, 4)
	widenPut(t, hidden, true, 1, 4)
	check("after writes under the widened policy")
	// Premise: the entitled reads still see the plaintext.
	rows, err := sealed.AggregateColsStats("true", nil, []AggSpec{{Func: AggMax, Arg: "SecretNum"}}, nil, nil)
	if err != nil || renderAggRows(rows) != " => "+fmt.Sprint(7000+widenN-1) {
		t.Fatalf("entitled MAX(SecretNum) = %s (%v)", renderAggRows(rows), err)
	}

	clearBefore := 0
	disk := readAll(t, dir)
	for i := 0; i < widenN; i++ {
		if bytes.Contains(disk, []byte(widenToken(i))) {
			clearBefore++
		}
	}
	if clearBefore == 0 {
		t.Fatal("fixture: no record's Salary is in the clear on disk before maintenance; the migration check tests nothing")
	}
	sealed.Maintain(MaintainOptions{})
	disk = readAll(t, dir)
	for i := 0; i < widenN; i++ {
		if bytes.Contains(disk, []byte(widenToken(i))) {
			t.Fatalf("record %d's Salary is still in the clear on disk after maintenance (%d were before)", i, clearBefore)
		}
	}
	if n := sealed.c.MigrateSealedAttrs(1); n != 0 {
		t.Fatalf("a second migration rewrote %d segments; maintenance left the first incomplete", n)
	}
	check("after maintenance")
}
