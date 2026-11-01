package db

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The redacted aggregate ladder over a table carrying the columnar accelerator. The column rungs read
// only columns, which never hold a sealed value, but a record whose constraint the columns cannot
// decide -- here, one reading the sealed Note -- is evaluated per record, and that fallback has to be
// keyless. A table without the accelerator never reaches those rungs, so this is where they are pinned:
// the redacted answer over the sealing table equals the answer over the same rows without Salary and
// Note, and the entitled answer equals the one over the rows in the clear.
func TestRedactedAggregateColumnarRungs(t *testing.T) {
	const n = 900
	open := func(sealed []string, hide bool) *DB {
		d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 14, EncryptedAttrs: sealed,
			PoolKeys: []KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		tx := d.Begin()
		for i := 0; i < n; i++ {
			text := fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nCpus = %d\nRequestMemory = %d\n", i, i%5, 1+i%4, 512*(1+i%6))
			if !hide {
				text += fmt.Sprintf("Salary = %d\nNote = \"n%d\"\n", 1000*(i%7), i%3)
			}
			ad, err := classad.ParseOld(text)
			if err != nil {
				t.Fatal(err)
			}
			tx.NewClassAd(fmt.Sprintf("%d.0", i), ad)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if !d.EnableSchemaScan(2000, 8) {
			t.Fatal("no columnar schema derived")
		}
		return d
	}
	enc, plain, hidden := open([]string{"Salary", "Note"}, false), open(nil, false), open(nil, true)

	type req struct {
		name       string
		constraint string
		groups     []GroupCol
		aggs       []AggSpec
	}
	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	reqs := []req{
		{"count", `Note == "n1" || Cpus == 2`, nil, count},
		{"count-presence", `Note =?= undefined && Cpus > 1`, nil, count},
		{"grouped", `Note == "n1" || Cpus == 2`, []GroupCol{{Attr: "Owner"}}, append(slices.Clone(count), AggSpec{Func: AggMax, Arg: "RequestMemory"})},
		{"grouped-sealed-numeric", `Salary >= 3000 || RequestMemory == 512`, []GroupCol{{Attr: "Cpus"}}, count},
		{"projected", "true", []GroupCol{{Attr: "Salary"}}, count},
	}
	render := func(rows []AggRow, err error) string {
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = strings.Join(r.Group, ",") + "=" + strings.Join(r.Values, ",")
		}
		slices.Sort(out)
		return strings.Join(out, " ")
	}
	for _, r := range reqs {
		want := render(plain.AggregateColsStats(r.constraint, r.groups, r.aggs, nil, nil))
		wantHidden := render(hidden.AggregateColsStatsRedacted(r.constraint, r.groups, r.aggs, nil, nil))
		if want == wantHidden {
			t.Fatalf("%s: does not discriminate: %s", r.name, want)
		}
		if got := render(enc.AggregateColsStatsRedacted(r.constraint, r.groups, r.aggs, nil, nil)); got != wantHidden {
			t.Errorf("%s: redacted = %s\n want %s (the rows without the sealed attributes)\n plaintext %s", r.name, got, wantHidden, want)
		}
		if got := render(enc.AggregateColsStats(r.constraint, r.groups, r.aggs, nil, nil)); got != want {
			t.Errorf("%s: entitled = %s, want %s", r.name, got, want)
		}
	}
}
