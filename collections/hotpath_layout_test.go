package collections

import (
	"math/rand"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"unsafe"
)

// TestBlockZoneEntrySize pins the per-block zone entry at 32 bytes. There is one per numeric field per
// columnar block, so a field added here grows a store's zone heap by the same ratio (embedding
// zoneRange, with its Full flag, made it 40 and the zone heap 40% larger). Change the number only on
// purpose.
func TestBlockZoneEntrySize(t *testing.T) {
	if got := unsafe.Sizeof(blockZoneEntry{}); got != 32 {
		t.Fatalf("unsafe.Sizeof(blockZoneEntry{}) = %d, want 32", got)
	}
}

// TestDamageHoldsMatchesCovers checks holds, the inlinable form segWindow.visible uses, against covers
// on random span sets, at every 8-byte offset in and around them.
func TestDamageHoldsMatchesCovers(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 200; iter++ {
		var spans []damageSpan
		pos := uint32(rng.Intn(4)) * 8
		for n := 1 + rng.Intn(6); n > 0; n-- {
			start := pos + uint32(rng.Intn(4))*8
			end := start + 8 + uint32(rng.Intn(4))*8
			spans = append(spans, damageSpan{start, end})
			pos = end
		}
		d := newDamageSet(spans)
		for o := uint32(0); o <= d.end+16; o += 8 {
			if got, want := d.holds(o), d.covers(o); got != want {
				t.Fatalf("spans %v: holds(%d) = %v, covers = %v", spans, o, got, want)
			}
		}
		enc := encodeDamage(d)
		if dec, ok := decodeDamage(enc); !ok || dec.end != d.end {
			t.Fatalf("spans %v: decoded set end = %v (ok %v), want %d", spans, dec, ok, d.end)
		}
	}
}

// TestSegWindowVisibleInlinable fails if segWindow.visible stops being inlinable. It runs once per row
// of every columnar count and aggregate; out of line, the call and the copy of the window cost those
// scans 10-37%.
func TestSegWindowVisibleInlinable(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the package")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command on PATH")
	}
	out, err := exec.Command(goBin, "build", "-gcflags=-m=2", "-o", os.DevNull, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build -gcflags=-m=2: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`(?m): can inline segWindow\.visible\b`).Match(out) {
		t.Fatalf("segWindow.visible is no longer inlinable: %s",
			regexp.MustCompile(`(?m)^.*inline segWindow\.visible.*$`).Find(out))
	}
}
