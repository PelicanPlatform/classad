package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// TestSchemaUncompressedNumericSize characterizes the target design: numeric prefix left
// UNCOMPRESSED (for the no-decode scan), string column and cold tail each compressed as their
// own stream (per block). Reports size vs the current wire baseline; the scan is the
// SchemaUncompressedPrefix number (~3 us / ~9300x).
func TestSchemaUncompressedNumericSize(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	_, wires := encodeOSPool(t, ads)
	var baseCat []byte
	for _, w := range wires {
		baseCat = append(baseCat, w...)
	}
	baseZ := zstdLen(baseCat)

	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	prefixLen := s.escBytes + s.fixedLen
	for _, block := range []int{len(wires), 512} {
		var prefixRaw, strZ, coldZ int
		for start := 0; start < len(wires); start += block {
			end := start + block
			if end > len(wires) {
				end = len(wires)
			}
			var strCat, coldCat []byte
			for _, w := range wires[start:end] {
				r := s.encode(wire.Ad(w))
				_, str, cold := s.splitRecord(r)
				prefixRaw += prefixLen // uncompressed
				strCat = append(strCat, str...)
				coldCat = append(coldCat, cold...)
			}
			strZ += zstdLen(strCat)
			coldZ += zstdLen(coldCat)
		}
		total := prefixRaw + strZ + coldZ
		t.Logf("block=%-5d | numeric UNCOMPRESSED=%dKB + strings zstd=%dKB + cold zstd=%dKB = TOTAL %dKB vs baseline %dKB (%+.1f%%)  [scan ~3us / ~9300x]",
			block, prefixRaw/1024, strZ/1024, coldZ/1024, total/1024, baseZ/1024,
			100*float64(total-baseZ)/float64(baseZ))
	}
}
