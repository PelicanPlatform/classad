package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// orderedFixture builds a newest-first (ReverseScan) append log of n ads over small segments, so
// there are many of them to fan out across.
func orderedFixture(t testing.TB, n, par int) *Collection {
	t.Helper()
	c, err := Open(Options{
		Dir:              t.TempDir(),
		AppendOnly:       true,
		ReverseScan:      true,
		SegmentSize:      1 << 16,
		QueryParallelism: par,
		ValueAttrs:       []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i := 0; i < n; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nOwner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"",
			i, i%10, 1+i%5, i%512, i%512))
		if perr != nil {
			t.Fatal(perr)
		}
		if aerr := c.Put([]byte(fmt.Sprintf("%d", i)), ad); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return c
}

func orderedClusterIds(t *testing.T, c *Collection, constraint string) []int64 {
	t.Helper()
	q, err := vm.Parse(constraint)
	if err != nil {
		t.Fatal(err)
	}
	var got []int64
	for row := range c.QueryRawWire(q, []string{"ClusterId"}, false) {
		ad, derr := wire.DecodeInline(row)
		if derr != nil {
			t.Fatalf("row did not decode: %v", derr)
		}
		v, verr := classad.FromAST(ad).EvaluateAttr("ClusterId").IntValue()
		if verr != nil {
			t.Fatal(verr)
		}
		got = append(got, v)
	}
	return got
}

// The point of the ordered fan-out: a newest-first collection fans out AND stays newest-first.
// The counter is checked because declining to fan out is silent -- without it this would pass
// having compared the serial path with itself.
func TestOrderedWireQueryFansOutNewestFirst(t *testing.T) {
	const n = 20000
	c := orderedFixture(t, n, 8)
	before := wireOrderedFanouts.Load()
	got := orderedClusterIds(t, c, "JobStatus >= 0")
	if wireOrderedFanouts.Load() == before {
		t.Fatal("the read did not fan out; this would have compared the serial path with itself")
	}
	if len(got) != n {
		t.Fatalf("%d rows, want %d", len(got), n)
	}
	for i, v := range got {
		if want := int64(n - 1 - i); v != want {
			t.Fatalf("row %d: ClusterId %d, want %d (newest-first)", i, v, want)
		}
	}
}

// The ordered fan-out must agree with the serial walk exactly, not just on the first row.
func TestOrderedWireQueryMatchesSerial(t *testing.T) {
	const n = 20000
	par := orderedFixture(t, n, 8)
	ser := orderedFixture(t, n, 1)
	for _, constraint := range []string{"JobStatus >= 0", "JobStatus == 3", "ProcId == 7 && JobStatus < 3", "false"} {
		before := wireOrderedFanouts.Load()
		gp := orderedClusterIds(t, par, constraint)
		fanned := wireOrderedFanouts.Load() != before
		gs := orderedClusterIds(t, ser, constraint)
		if !fanned && constraint != "false" {
			t.Fatalf("%q: did not fan out", constraint)
		}
		if len(gp) != len(gs) {
			t.Fatalf("%q: %d parallel rows, %d serial", constraint, len(gp), len(gs))
		}
		for i := range gp {
			if gp[i] != gs[i] {
				t.Fatalf("%q: row %d: parallel %d, serial %d", constraint, i, gp[i], gs[i])
			}
		}
	}
}

// A consumer that stops early still gets the K NEWEST rows, which is the whole promise an
// archive's LIMIT rests on -- and the one a fan-out is most likely to break, since the workers
// behind the first segment are already running.
func TestOrderedWireQueryEarlyStopIsNewest(t *testing.T) {
	const n = 20000
	c := orderedFixture(t, n, 8)
	q, err := vm.Parse("JobStatus >= 0")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []int{1, 5, 1000} {
		var got []int64
		for row := range c.QueryRawWire(q, []string{"ClusterId"}, false) {
			ad, derr := wire.DecodeInline(row)
			if derr != nil {
				t.Fatal(derr)
			}
			v, verr := classad.FromAST(ad).EvaluateAttr("ClusterId").IntValue()
			if verr != nil {
				t.Fatal(verr)
			}
			got = append(got, v)
			if len(got) == k {
				break
			}
		}
		if len(got) != k {
			t.Fatalf("k=%d: got %d rows", k, len(got))
		}
		for i, v := range got {
			if want := int64(n - 1 - i); v != want {
				t.Fatalf("k=%d row %d: ClusterId %d, want %d", k, i, v, want)
			}
		}
	}
}
