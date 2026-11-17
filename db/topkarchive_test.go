package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// A HISTORY TABLE MUST ANSWER THIS TOO, AND ANSWER IT THE SAME WAY.
//
// "The top N per group" points at history more often than at the queue: the five longest jobs
// each owner ran last month is a question about completed jobs. The archive and the mutable
// table expose the same two scans with the same signatures, which is why one implementation
// serves both -- and this is what holds that claim up. A parallel archive copy that drifted
// would be the fifty-first entry in the list of method pairs this codebase already carries.
//
// The ordering here is TOTAL (QDate then Rowid, unique), so the answer is determined and the
// two tables must produce it identically. With a tie the two would be free to differ, and the
// test would be pinning an accident.
func TestTopKArchiveMatchesTheMutableTable(t *testing.T) {
	const owners, perOwner, k = 6, 300, 5

	mut, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer mut.Close()
	arch, err := openArchiveTable(t.TempDir(), ArchiveConfig{SegmentSize: 1 << 14}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer arch.Close()

	tx := mut.Begin()
	n := 0
	for o := 0; o < owners; o++ {
		for r := 0; r < perOwner; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", o))
			ad.InsertAttr("QDate", int64(1700000000+r%12)) // ties, broken by Rowid below
			ad.InsertAttr("Rowid", int64(n))
			ad.InsertAttrString("Payload", fmt.Sprintf("o%d-r%d", o, r))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			if aerr := arch.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	spec := TopKRowsSpec{
		GroupCols: []GroupCol{{Attr: "Owner"}},
		OrderBy: []TopKOrderTerm{
			{Attr: "QDate", Desc: true},
			{Attr: "Rowid", Desc: false},
		},
		K:     k,
		Attrs: []string{"Payload"},
	}

	flatten := func(groups []TopKRowsGroup) map[string]string {
		t.Helper()
		out := map[string]string{}
		for _, g := range groups {
			owner, verr := g.Values[0].StringValue()
			if verr != nil {
				t.Fatal(verr)
			}
			var cells []string
			for _, r := range g.Rows {
				p, perr := r[0].StringValue()
				if perr != nil {
					t.Fatal(perr)
				}
				cells = append(cells, p)
			}
			out[owner] = strings.Join(cells, ",")
		}
		return out
	}

	mg, err := mut.TopKRowsPerGroup("true", spec)
	if err != nil {
		t.Fatal(err)
	}
	ag, err := arch.TopKRowsPerGroup("true", spec)
	if err != nil {
		t.Fatalf("archive top-k: %v", err)
	}
	want, got := flatten(mg), flatten(ag)
	if len(got) != owners {
		t.Fatalf("archive returned %d groups, want %d", len(got), owners)
	}
	for owner, w := range want {
		if got[owner] != w {
			t.Errorf("owner %s: archive %q, mutable %q", owner, got[owner], w)
		}
	}

	// And the archive's wire rung must agree with its own value rung, as the mutable table's
	// does -- the shared implementation is only shared if both halves of it are.
	aw, err := arch.TopKRowsPerGroupWire("true", spec)
	if err != nil {
		t.Fatalf("archive wire top-k: %v", err)
	}
	if len(aw) != owners {
		t.Fatalf("archive wire rung returned %d groups, want %d", len(aw), owners)
	}
	for _, g := range aw {
		owner, verr := g.Values[0].StringValue()
		if verr != nil {
			t.Fatal(verr)
		}
		var cells []string
		for _, r := range decodeWireRows(t, g.Rows, []string{"Payload"}) {
			cells = append(cells, strings.Trim(r, "\""))
		}
		if cell := strings.Join(cells, ","); cell != want[owner] {
			t.Errorf("owner %s, archive wire rung: %q, want %q", owner, cell, want[owner])
		}
	}
}
