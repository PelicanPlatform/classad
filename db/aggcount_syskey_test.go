package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// COUNT(*) must not count the reserved system keyspace.
//
// The unconstrained COUNT(*) rung answers from Collection.Len(), which is the live key count
// across shards. Every client-facing enumeration hides system-keyed records -- Query, Scan and
// DeleteWhere all skip them -- so a count that includes them reports more rows than any way of
// listing the table can produce, and the difference grows with every marker written.
//
// Each case asserts the count against the number of ads a match-all Query returns, because that
// is the question the caller asked: a COUNT(*) that disagrees with SELECT * is wrong whichever
// rung answered it.
func TestCountStarExcludesSystemRecords(t *testing.T) {
	for _, tc := range []struct {
		name    string
		markers int
	}{
		{"no markers", 0},
		{"one marker", 1},
		{"many markers", 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Open("")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = d.Close() }()

			const ads = 40
			for i := range ads {
				putAd(t, d, string(rune('a'+i%26))+string(rune('a'+i/26)), `State = "Idle"`)
			}
			for i := range tc.markers {
				key := SystemKey("idem:batch-" + string(rune('A'+i%26)) + string(rune('A'+i/26)))
				if putErr := d.Put(key, mustAd(t, "IdemAt = 1700000000")); putErr != nil {
					t.Fatal(putErr)
				}
			}

			// What a client can actually list.
			seq, err := d.Query("true")
			if err != nil {
				t.Fatal(err)
			}
			visible := 0
			for range seq {
				visible++
			}
			if visible != ads {
				t.Fatalf("match-all Query returned %d ads, want %d", visible, ads)
			}

			rows, err := d.AggregateColsStats("true", nil,
				[]AggSpec{{Func: AggCount, Arg: "*"}}, &collections.ScanStats{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || len(rows[0].Values) != 1 {
				t.Fatalf("COUNT(*) returned %+v, want one row of one value", rows)
			}
			if got := rows[0].Values[0]; got != "40" {
				t.Errorf("COUNT(*) = %s with %d system records present, want %d (what Query returns)",
					got, tc.markers, visible)
			}
		})
	}
}
