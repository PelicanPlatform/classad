package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func mkAd(t *testing.T, text string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	return ad
}

func TestProjectionChecksumEqualForSameProjection(t *testing.T) {
	attrs := []string{"RequestCpus", "RequestMemory", "Requirements"}
	// Two ads that differ only outside the projection must hash equal.
	a := mkAd(t, `[ ClusterId = 5; ProcId = 0; RequestCpus = 1; RequestMemory = 128; Requirements = (OpSys == "LINUX"); ]`)
	b := mkAd(t, `[ ClusterId = 9; ProcId = 3; RequestCpus = 1; RequestMemory = 128; Requirements = (OpSys == "LINUX"); Owner = "bob"; ]`)
	if ProjectionChecksum(a, attrs) != ProjectionChecksum(b, attrs) {
		t.Fatalf("ads with identical projection hashed differently")
	}
}

func TestProjectionChecksumDiffersOnValue(t *testing.T) {
	attrs := []string{"RequestMemory"}
	a := mkAd(t, `[ RequestMemory = 128; ]`)
	b := mkAd(t, `[ RequestMemory = 256; ]`)
	if ProjectionChecksum(a, attrs) == ProjectionChecksum(b, attrs) {
		t.Fatalf("different RequestMemory values hashed equal")
	}
}

func TestProjectionChecksumDistinguishesAbsent(t *testing.T) {
	attrs := []string{"RequestMemory"}
	present := mkAd(t, `[ RequestMemory = 0; ]`)
	absent := mkAd(t, `[ Owner = "bob"; ]`)
	if ProjectionChecksum(present, attrs) == ProjectionChecksum(absent, attrs) {
		t.Fatalf("absent attribute aliased a present value")
	}
}

func TestProjectionChecksumHashesExpressionText(t *testing.T) {
	// Requirements is an expression referencing machine attrs; the checksum must
	// use the expression text, not attempt to evaluate it (which would collapse
	// distinct requirements to the same undefined value).
	attrs := []string{"Requirements"}
	a := mkAd(t, `[ Requirements = (OpSys == "LINUX"); ]`)
	b := mkAd(t, `[ Requirements = (OpSys == "WINDOWS"); ]`)
	if ProjectionChecksum(a, attrs) == ProjectionChecksum(b, attrs) {
		t.Fatalf("different Requirements expressions hashed equal")
	}
}

func TestProjectionChecksumOrderAndDelimiter(t *testing.T) {
	// The per-field delimiter must keep ["a","bc"] distinct from ["ab","c"].
	x := mkAd(t, `[ A = "a"; B = "bc"; ]`)
	y := mkAd(t, `[ A = "ab"; B = "c"; ]`)
	if ProjectionChecksum(x, []string{"A", "B"}) == ProjectionChecksum(y, []string{"A", "B"}) {
		t.Fatalf("delimiter failed to separate adjacent fields")
	}
}

func TestProjectionChecksumStable(t *testing.T) {
	attrs := []string{"RequestCpus", "RequestMemory", "Requirements", "Rank"}
	ad := mkAd(t, `[ RequestCpus = 2; RequestMemory = 512; Requirements = (Arch == "X86_64"); Rank = 0.0; ]`)
	first := ProjectionChecksum(ad, attrs)
	for i := 0; i < 100; i++ {
		if ProjectionChecksum(ad, attrs) != first {
			t.Fatalf("checksum not stable across calls")
		}
	}
}
