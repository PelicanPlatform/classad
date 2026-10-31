package collections

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The GROUP BY counts read a segment record by record wherever its index or zone maps cannot
// account for it. An interned segment's records key their attributes by ids from the segment's
// own dictionary, and those readers named the ids through the global intern table instead: here
// that table's id for Owner is the dictionary's id for Ac, and CompletionDate's is Ad's, so a
// record was grouped under its Ac value and bucketed by its Ad number. That never reached an
// answer only because the readers also took the segment's dictionary record for an ad, found no
// group in it and declined -- so the bucketed and constrained counts fell back to a scan
// whenever an interned segment straddled a bucket or the constraint.
func TestGroupCountReadersInternedSegment(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 4096, InternAtSeal: true,
		ZoneAttrs: []string{"CompletionDate"}, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// Ac and Ad sort ahead of the grouped attributes in a segment dictionary but are interned
	// after them, which is what shifts the two id spaces apart.
	for i := range 120 {
		ad, err := classad.Parse(fmt.Sprintf(`[Aa = 1; Ab = 2; Ac = "x%d"; Ad = %d; CompletionDate = %d; Owner = "u%d"]`,
			i%2, 1000+i, i, i%3))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	a.Reindex()

	const width = 10
	c := a.c
	zoneID, ok := c.intern.LookupID("CompletionDate")
	if !ok {
		t.Fatal("CompletionDate is not interned")
	}
	interned, straddling := 0, 0
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			d := w.dict()
			if d == nil {
				continue
			}
			interned++
			for _, name := range []string{"Owner", "CompletionDate"} {
				g, _ := c.intern.LookupID(name)
				l, has := d.lookup(name)
				if !has || l == g {
					t.Fatalf("%s: dictionary id %d (found %v), intern id %d; the fixture needs them to differ", name, l, has, g)
				}
			}
			if z, has := w.zones[zoneID]; has {
				if _, one := zoneBucket(z, width); !one {
					straddling++
				}
			}
			// The ungrouped reader over the whole segment, dictionary record included: the index
			// covers every sealed segment here, so the public call never reaches it.
			var dbuf []byte
			counts := map[string]int64{}
			n, read := countAttrRange(c, w, "Owner", 0, w.used, &dbuf, counts)
			if !read || n == 0 {
				t.Errorf("countAttrRange over an interned segment declined (n=%d)", n)
			}
			for v := range counts {
				if !strings.HasPrefix(v, "u") {
					t.Errorf("countAttrRange grouped a record under %q; every Owner is u*", v)
				}
			}
		}
		releaseWindows(wins)
	}
	if interned < 2 || straddling == 0 {
		t.Fatalf("%d interned sealed segments, %d straddling a bucket; the fixture needs several, some straddling", interned, straddling)
	}

	want := map[int64]map[string]int64{}
	for ad := range c.Scan() {
		f, err := ad.EvaluateAttr("CompletionDate").NumberValue()
		if err != nil {
			t.Fatal(err)
		}
		o, _ := ad.EvaluateAttrString("Owner")
		b, _ := bucketOf(f, width)
		if want[b] == nil {
			want[b] = map[string]int64{}
		}
		want[b][o]++
	}
	got, ok := a.CategoricalGroupCountsBucketed("Owner", "CompletionDate", width)
	if !ok {
		t.Errorf("bucketed declined")
	} else if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("bucketed: %v, want %v", got, want)
	}

	// Each bound falls inside an interned segment, so that segment is read record by record.
	for _, cons := range []string{"CompletionDate > 15", "CompletionDate >= 5 && CompletionDate < 75"} {
		want := groupTruth(t, a, cons)
		got, ok := a.CategoricalGroupCountsWhere("Owner", cons)
		if !ok {
			t.Errorf("%s: declined", cons)
		} else if !sameCounts(got, want) {
			t.Errorf("%s: %v, want %v", cons, got, want)
		}
	}
}
