package db

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// REDACTION IS NOT OPTIONAL ON THIS RUNG, AND ITS ABSENCE WOULD NOT SHOW.
//
// The rows this rung returns are the STORED BYTES. A caller that asks for Payload and gets a
// row back has no reason to look at what else the row carries -- and the scan chases
// references, so a row can carry attributes the caller never named. Serve that to a reader not
// entitled to them and the private ones go with it, invisibly: every assertion such a caller
// writes is about the attribute it asked for, and that one is fine.
//
// THE TEST READS THE RAW BYTES, not a rendering. The first version of it rendered each row with
// ClassAd.String() and found no secrets anywhere -- including in the control, which is what gave
// it away. String() redacts private attributes itself, so a row full of them renders clean and
// a rendering-based test reports success no matter what crossed. What crosses is bytes, so the
// question has to be asked of bytes.
func TestTopKRowsWireRedactsPrivateAttributes(t *testing.T) {
	d, err := OpenConfig(Config{Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 40; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("o%d", i%4))
		ad.InsertAttr("QDate", int64(1700000000+i))
		ad.InsertAttrString("Payload", fmt.Sprintf("p%d", i))
		// One of each private flavour: a V1 name and a V2 prefix.
		ad.InsertAttrString("ClaimId", fmt.Sprintf("secret-claim-%d", i))
		ad.InsertAttrString("_condor_privToken", fmt.Sprintf("secret-token-%d", i))
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	spec := TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy:   OrderOne("QDate", true),
		K:         2,
		Attrs:     []string{"Payload", "ClaimId", "_condor_privToken"},
	}
	carries := func(groups []TopKRowsWireGroup, needle string) bool {
		for _, g := range groups {
			for _, row := range g.Rows {
				if bytes.Contains(row, []byte(needle)) {
					return true
				}
			}
		}
		return false
	}

	// The unredacted rung is the CONTROL. If it does not carry the secrets, nothing about
	// the fixture puts them in a row and the assertion below proves nothing.
	plain, err := d.TopKRowsPerGroupWire("true", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !carries(plain, "secret-claim-") || !carries(plain, "secret-token-") {
		t.Fatal("unredacted rows carry neither secret, so this test cannot detect a leak")
	}

	red, err := d.TopKRowsPerGroupWireRedacted("true", spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(red) != len(plain) {
		t.Fatalf("redacted rung returned %d groups, unredacted %d", len(red), len(plain))
	}
	if carries(red, "secret-claim-") {
		t.Error("redacted rows still carry ClaimId")
	}
	if carries(red, "secret-token-") {
		t.Error("redacted rows still carry _condor_privToken")
	}
	// And it still answers the question that was asked.
	if !carries(red, "p3") {
		t.Error("redaction dropped the attribute the caller asked for")
	}
}
