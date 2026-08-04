package dbrpc

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestAggregateCountConstraintColumnar checks the dbrpc COUNT(*)-WHERE fast path routes through
// the columnar schema scan when enabled and returns counts identical to the projected scan.
func TestAggregateCountConstraintColumnar(t *testing.T) {
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 800; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf("Memory = %d\nCpus = %d\nName = \"n%03d\"", 1024+(i%64)*256, 1+i%8, i))
		if perr != nil {
			t.Fatal(perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Read demand on Memory so it lands in the hot tier, then enable the columnar scan.
	for i := 0; i < 15; i++ {
		seq, err := d.QueryProject("true", []string{"Memory"})
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	}
	d.EnableSchemaScan(2000, 4)
	if _, ok := d.CountConstraint("Memory > 4096"); !ok {
		t.Fatal("CountConstraint declined an eligible predicate after EnableSchemaScan")
	}

	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConn(sconn) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()
	ctx := context.Background()

	for _, expr := range []string{"Memory > 4096", "Memory <= 4096", "Memory >= 2000 && Memory < 9000"} {
		rows, err := c.Aggregate(ctx, expr, nil, []AggSpec{{Func: AggCount, Arg: "*"}})
		if err != nil {
			t.Fatalf("%s: aggregate: %v", expr, err)
		}
		want := 0
		seq, err := d.QueryProject(expr, []string{"Memory"})
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
			want++
		}
		if len(rows) != 1 || len(rows[0].Values) != 1 || rows[0].Values[0] != strconv.Itoa(want) {
			t.Errorf("%s: aggregate COUNT(*) = %+v, want %d", expr, rows, want)
		}
	}
}
