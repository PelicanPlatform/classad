package wire

import (
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

// TestStandaloneAdLookup: Ad.Lookup reads an ad that carries its own intern table. Such an ad holds
// the table between the header and the body, so finding an attribute means skipping it first -- and a
// reader that does not lands in the middle of the names and reads them as the body.
func TestStandaloneAdLookup(t *testing.T) {
	ad, err := parser.ParseClassAd(`[ OnlyAttr = 1234 ]`)
	if err != nil {
		t.Fatal(err)
	}
	enc := EncodeStandalone(ad)
	if len(enc) < 3 || enc[2]&flagStandalone == 0 {
		t.Fatal("fixture: EncodeStandalone did not embed a table, so nothing has to be skipped")
	}
	// One attribute, interned into a fresh table while the body was written: it holds the first id.
	node, ok := Ad(enc).Lookup(0)
	if !ok {
		t.Fatal("Lookup found nothing in a standalone ad")
	}
	lit, ok := LiteralValue(node)
	if !ok || lit.Kind != LitInt || lit.Int != 1234 {
		t.Fatalf("Lookup returned %+v, want the integer 1234", lit)
	}
}
