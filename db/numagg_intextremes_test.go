package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// An all-integer MIN/MAX renders from NumStats.IntMin/IntMax, which hold the int64 extremes. The
// float64 Min/Max cannot tell integers past 2^53 apart, and rendering from them gave a value no record
// held. Two storage keys that render as one group are merged before rendering, so the merge has to
// carry the integer extremes too.
func TestNumAggValueIntExtremesMerged(t *testing.T) {
	const p53 = int64(1) << 53
	stats := func(lo, hi int64) collections.NumStats {
		return collections.NumStats{N: 2, Min: float64(lo), Max: float64(hi), IntMin: lo, IntMax: hi,
			IntSum: lo + hi, Sum: float64(lo) + float64(hi)}
	}
	a := collections.GroupStats{Values: []classad.Value{classad.NewIntValue(1)}, Count: 2, Stats: []collections.NumStats{stats(p53, p53+1)}}
	b := collections.GroupStats{Values: []classad.Value{classad.NewIntValue(1)}, Count: 2, Stats: []collections.NumStats{stats(p53-1, p53+3)}}
	mergeGroupStats(&a, &b)
	if got, want := numAggValue(AggMin, a.Stats[0]), "9007199254740991"; got != want {
		t.Errorf("MIN = %s, want %s", got, want)
	}
	if got, want := numAggValue(AggMax, a.Stats[0]), "9007199254740995"; got != want {
		t.Errorf("MAX = %s, want %s", got, want)
	}

	// Once a real contributes, MIN/MAX are reals rendered from the float64 extremes.
	r := collections.GroupStats{Values: []classad.Value{classad.NewIntValue(1)}, Count: 1,
		Stats: []collections.NumStats{{N: 1, Min: 0.5, Max: 0.5, Sum: 0.5, AnyReal: true}}}
	mergeGroupStats(&a, &r)
	if got := numAggValue(AggMin, a.Stats[0]); got != ValueText(classad.NewRealValue(0.5)) {
		t.Errorf("MIN with a real = %s, want 0.5 as a real", got)
	}
}
