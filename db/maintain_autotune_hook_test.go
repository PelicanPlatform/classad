package db

import (
	"fmt"
	"slices"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestMaintainReportsWhatItTuned: a pass that creates an index tells the caller, which until
// OnAutoTune existed it could not. Maintain read res.Changed to decide whether to persist and
// dropped the Changes, so nothing above it could say that an index had appeared, on which
// attribute, or why.
func TestMaintainReportsWhatItTuned(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i := range 500 {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		ad.Set("Owner", fmt.Sprintf("u%d", i%7))
		if err := d.Put(fmt.Sprintf("k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		seq, err := d.Query(`Owner == "u3"`)
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	}

	var got []AutoTuneResult
	d.Maintain(MaintainOptions{
		MinIndexDemand: 5,
		OnAutoTune:     func(res AutoTuneResult) { got = append(got, res) },
	})
	if cat, _ := d.IndexedAttrs(); !slices.Contains(cat, "Owner") {
		t.Fatalf("the tuner indexed %v, not Owner: there is nothing for the hook to report", cat)
	}
	if len(got) != 1 {
		t.Fatalf("OnAutoTune fired %d time(s) for one pass that changed something, want 1", len(got))
	}
	var names []string
	for _, ch := range got[0].Changes {
		names = append(names, ch.Attr)
	}
	if !slices.Contains(names, "Owner") {
		t.Errorf("the reported changes %v do not name Owner", names)
	}
}

// TestMaintainDoesNotReportAnUnchangedPass: the hook fires on change, not on schedule. Almost
// every pass changes nothing, and a caller logging each one would write a line per table per
// interval forever.
func TestMaintainDoesNotReportAnUnchangedPass(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i := range 200 {
		ad := classad.New()
		ad.Set("ClusterId", int64(i))
		if err := d.Put(fmt.Sprintf("k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}
	fired := 0
	d.Maintain(MaintainOptions{
		MinIndexDemand: 1000, // far above anything this table has been asked
		OnAutoTune:     func(AutoTuneResult) { fired++ },
	})
	if fired != 0 {
		t.Errorf("OnAutoTune fired %d time(s) for a pass that tuned nothing", fired)
	}
}
