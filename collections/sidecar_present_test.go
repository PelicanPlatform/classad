package collections

import (
	"fmt"
	"testing"
)

// HasSealedSidecar must answer exactly what SidecarSizes().Segments > 0 answers.
//
// It exists only because the two cost different amounts -- SidecarSizes parses every
// sidecar to break its bytes out by structure, which a query path may not do -- so the one
// thing worth pinning is that the cheap answer is the same answer. Both the empty case and
// the sealed one are checked: an implementation that always said false would be cheaper
// still, and would quietly turn off every index probe that asks this question.
func TestHasSealedSidecarMatchesSidecarSizes(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	opts := Options{Shards: 2, Dir: t.TempDir(), SegmentSize: 1 << 13,
		CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}}
	c, err := Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	// Nothing written: no segment has sealed, so neither reports one.
	if c.HasSealedSidecar() {
		t.Error("an empty collection reports a sealed sidecar")
	}
	if got, want := c.HasSealedSidecar(), c.SidecarSizes().Segments > 0; got != want {
		t.Errorf("empty: HasSealedSidecar=%v, SidecarSizes().Segments>0=%v", got, want)
	}

	for i := 0; i < 5000; i++ {
		if perr := c.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Owner="u%d"; Memory=%d ]`, i, i%40, (i%16+1)*512))); perr != nil {
			t.Fatal(perr)
		}
	}
	c.Reindex()

	if !c.HasSealedSidecar() {
		t.Fatal("segments sealed but no sidecar is reported; every index probe would be " +
			"declined on a table that does have an index")
	}
	if got, want := c.HasSealedSidecar(), c.SidecarSizes().Segments > 0; got != want {
		t.Errorf("sealed: HasSealedSidecar=%v, SidecarSizes().Segments>0=%v", got, want)
	}
}
