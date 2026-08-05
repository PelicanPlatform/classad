package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// TestDiagnosticsSchemaScan verifies the columnar accelerator's state travels in Diagnostics: after
// a Maintain pass enables schema-scan, a client's .stats surface reports it enabled, with its hot
// columns and full segment coverage.
func TestDiagnosticsSchemaScan(t *testing.T) {
	d, err := db.OpenConfig(db.Config{Dir: t.TempDir(), SegmentSize: 1 << 9}) // tiny ⇒ segments seal
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 1200; i++ {
		ad, _ := classad.ParseOld(fmt.Sprintf("Memory = %d\nCpus = %d\nName = \"n%05d\"", 1024+(i%64)*256, 1+i%8, i))
		tx.NewClassAd(fmt.Sprintf("%d.0", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ { // Memory read demand -> hot
		seq, err := d.QueryProject("true", []string{"Memory"})
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		}
	}
	d.Maintain(db.MaintainOptions{SchemaScanHotTopN: 4})

	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	diag, err := c.Diagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ss := diag.SchemaScan
	if !ss.Enabled {
		t.Fatalf("SchemaScan.Enabled false over the wire: %+v", ss)
	}
	if ss.SchemaFields == 0 || ss.SealedSegments == 0 || ss.CoveredSegments != ss.SealedSegments {
		t.Fatalf("SchemaScan coverage wrong: %+v", ss)
	}
	if !contains(ss.HotFields, "Memory") {
		t.Fatalf("Memory not in SchemaScan.HotFields %v", ss.HotFields)
	}
}
