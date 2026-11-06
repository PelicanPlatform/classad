package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A subscript over an attribute holding a list is the shape that is native now, so it reaches
// the columnar paths for the first time. The vector executor declines a subscript, so the
// columnar answer has to come from the fallback and still equal the row path's.
func listFixture(tb testing.TB, n int) *Collection {
	tb.Helper()
	cd, err := NewZSTDCodec(nil)
	if err != nil {
		tb.Fatal(err)
	}
	c := New(Options{Shards: 1, SegmentSize: 1 << 16, Codec: cd})
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
			"L = { %d, %d, %d }\nOwner = \"user%d\"",
			i, i%10, 1+i%5, 1024+(i%32)*512, i%5, 7, i%3, i%512)
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"ProcId >= 0", "JobStatus >= 0", "RequestMemory >= 0"} {
		q, perr := vm.Parse(e)
		if perr != nil {
			tb.Fatal(perr)
		}
		for i := 0; i < 20; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}

func TestSubscriptQueryAgreesWithRows(t *testing.T) {
	c := listFixture(t, 4000)
	defer c.Close()

	exprs := []string{
		`L[0] == 3`,
		`L[1] == 7`,
		`L[2] < 2`,
		`L[9] =?= error`,
		`L[ProcId % 3] == 7`,
		`L[0] == 3 && JobStatus == 1`,
		`L[0] + L[2] > 4`,
		`Missing[0] =?= undefined`,
	}
	anyNative := false
	for _, expr := range exprs {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		native := q.Native()
		anyNative = anyNative || native
		got, ok := c.VectorEvalCount(q)
		if !ok {
			t.Errorf("%-28s no columnar state", expr)
			continue
		}
		want := rowCount(t, c, expr)
		if got != want {
			t.Errorf("%-28s native=%v columnar %d != row %d", expr, native, got, want)
		} else {
			t.Logf("%-28s native=%v count=%d agrees", expr, native, got)
		}
	}
	if !anyNative {
		t.Fatal("no expression was native; this test proved nothing about the widened path")
	}
}
