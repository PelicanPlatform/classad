package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// DOES THE SERVER PICK THE SAME TIED ROWS A CLIENT WOULD?
//
// The differential test next door holds the two SERVER rungs together. It cannot see this,
// because both of them keep rows the same way. The question here is different and is the one
// that decides whether a ranking window may be pushed down at all:
//
//	a client ranks by stable-sorting each partition in SCAN ORDER and taking the first K
//	the server ranks by offering every row to a keep that evicts its worst
//
// Where the order attribute is distinct, both name the same K rows. Where it TIES across the
// K-th place, "the top 5" is a set of choices, not a set -- and if the two sides choose
// differently, pushing the window down silently changes which rows a user is shown. Nothing
// about the two implementations makes them agree; the keep never evicts on an equal value, so
// it holds the earliest offered, but the scan is parallel across segments and "earliest
// offered" need not be "earliest in scan order".
//
// MEASURED, AND THEY DO NOT AGREE: 11 of 12 partitions in the fixture below choose different
// tied rows, while all 12 agree position-for-position on the ORDER VALUES. So the ranking is
// right and only the choice among equals differs.
//
// That is conformant -- ROW_NUMBER over a non-unique ORDER BY is non-deterministic in SQL, and
// every engine says so -- but it is not free: a pushdown would change which jobs "the five most
// recent per owner" names, and QDate ties are the common case rather than the exotic one, since
// a cluster submit stamps thousands of jobs with the same second.
//
// So this test asserts the part that is NOT a matter of choice (the order values) and reports
// the part that is. Deciding what to do about the choice -- document it as arbitrary, or give
// both sides a shared tiebreaker such as the record key and make the query deterministic
// whichever path runs -- is a design question this test exists to inform, not to settle.
func TestTopKTieBreakingAgainstAClientRanking(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// HEAVY TIES ON PURPOSE: 8 distinct QDate values across 400 rows per owner, so every
	// partition's K-th place is tied many ways and an arbitrary choice is almost certain to
	// differ if the two rules differ at all. Enough rows and segments to engage the parallel
	// scan, which is the thing that could make scan order and offer order disagree.
	const owners, perOwner, k = 12, 400, 5
	tx := d.Begin()
	n := 0
	for o := 0; o < owners; o++ {
		for r := 0; r < perOwner; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", o))
			ad.InsertAttr("QDate", int64(1700000000+r%8))
			ad.InsertAttrString("Payload", fmt.Sprintf("o%d-r%d", o, r))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d.EnableSchemaScan(1000, 8)

	// The client's answer: every matching row in scan order, stable-sorted per partition,
	// first K. This is what htcondordb's execSelectWindow does today.
	type row struct {
		owner, payload string
		qdate          int64
	}
	seq, err := d.QueryProject("true", []string{"Owner", "QDate", "Payload"})
	if err != nil {
		t.Fatal(err)
	}
	parts := map[string][]row{}
	for vals := range seq {
		owner, oerr := vals[0].StringValue()
		q, qerr := vals[1].IntValue()
		p, perr := vals[2].StringValue()
		if oerr != nil || qerr != nil || perr != nil {
			t.Fatalf("projected row did not decode: %v %v %v", oerr, qerr, perr)
		}
		parts[owner] = append(parts[owner], row{owner: owner, payload: p, qdate: q})
	}
	if len(parts) != owners {
		t.Fatalf("client saw %d partitions, want %d", len(parts), owners)
	}
	clientTop := map[string][]string{}
	for owner, rows := range parts {
		idx := make([]int, len(rows))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return rows[idx[a]].qdate > rows[idx[b]].qdate })
		var got []string
		for i := 0; i < k && i < len(idx); i++ {
			got = append(got, rows[idx[i]].payload)
		}
		clientTop[owner] = got
	}

	server, err := d.TopKRowsPerGroup("true", TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}}, OrderAttr: "QDate", Desc: true, K: k,
		Attrs: []string{"Payload", "QDate"},
	})
	if err != nil {
		t.Fatal(err)
	}

	sameRows, sameOrderValues, differing := 0, 0, 0
	for _, g := range server {
		owner, verr := g.Values[0].StringValue()
		if verr != nil {
			t.Fatal(verr)
		}
		var got []string
		var qdates []int64
		for _, r := range g.Rows {
			p, perr := r[0].StringValue()
			if perr != nil {
				t.Fatal(perr)
			}
			q, qerr := r[1].IntValue()
			if qerr != nil {
				t.Fatal(qerr)
			}
			got = append(got, p)
			qdates = append(qdates, q)
		}
		want := clientTop[owner]
		if strings.Join(got, ",") == strings.Join(want, ",") {
			sameRows++
		} else {
			differing++
		}
		// Whatever rows were chosen, the ORDER VALUES must match position for position --
		// that part is not a matter of choice, and a difference there is a ranking bug
		// rather than a tie-breaking one.
		var wantQ []int64
		for _, p := range want {
			for _, r := range parts[owner] {
				if r.payload == p {
					wantQ = append(wantQ, r.qdate)
					break
				}
			}
		}
		if fmt.Sprint(qdates) != fmt.Sprint(wantQ) {
			t.Errorf("owner %s: server order values %v, client %v -- not a tie-breaking "+
				"difference but a RANKING difference", owner, qdates, wantQ)
		} else {
			sameOrderValues++
		}
	}

	if sameOrderValues != len(server) {
		t.Fatalf("only %d of %d partitions agreed on the order values", sameOrderValues, len(server))
	}
	t.Logf("tie-breaking: %d of %d partitions chose the same rows as a client ranking, %d chose "+
		"different ones; all %d agreed on the order values",
		sameRows, len(server), differing, sameOrderValues)
}
