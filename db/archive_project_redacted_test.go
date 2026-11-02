package db

import (
	"fmt"
	"iter"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestArchiveQueryProjectRedacted pins the archive's redacted projected scan: the same read, the
// same rows, with the private attributes withheld.
//
// The pair matters more than either half. A read surface that serves both table kinds reaches for
// QueryProjectRedacted on a mutable table and had nothing to reach for on an archive -- so it would
// either refuse unprivileged peers on history or serve them the entitled read. The second is the
// mistake the paged projection op actually made, so the archive variant is tested as the entitled
// one's twin, against the same records, rather than on its own.
func TestArchiveQueryProjectRedacted(t *testing.T) {
	const n = 60
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nOwner = \"u%d\"\nClaimId = \"secret-claim-%d\"\nRequestMemory = %d",
			i, i%4, i, 512*(1+i%3)))
		if perr != nil {
			t.Fatal(perr)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}

	attrs := []string{"ClusterId", "Owner", "ClaimId", "RequestMemory"}
	read := func(redacted bool) (rows int, claims int) {
		t.Helper()
		var it iter.Seq[[]classad.Value]
		var e error
		if redacted {
			it, e = a.QueryProjectRedacted("true", attrs)
		} else {
			it, e = a.QueryProject("true", attrs)
		}
		if e != nil {
			t.Fatal(e)
		}
		for vals := range it {
			rows++
			// Every row must still carry the attributes that are not private: redaction
			// withholds values, it does not drop rows or blank the rest of the projection.
			if vals[0].IsUndefined() || vals[1].IsUndefined() || vals[3].IsUndefined() {
				t.Fatalf("redacted=%v: a non-private projected attribute came back UNDEFINED: %v",
					redacted, vals)
			}
			if !vals[2].IsUndefined() {
				claims++
			}
		}
		return rows, claims
	}

	entitledRows, entitledClaims := read(false)
	if entitledRows != n {
		t.Fatalf("entitled projected scan returned %d rows, want %d", entitledRows, n)
	}
	// The premise: without it, a redacted scan returning no claims proves nothing.
	if entitledClaims != n {
		t.Fatalf("entitled projected scan read ClaimId in %d of %d rows; the premise fails, so the "+
			"redacted arm below is not measuring redaction", entitledClaims, n)
	}

	redRows, redClaims := read(true)
	if redRows != entitledRows {
		t.Errorf("redacted projected scan returned %d rows, entitled %d: redaction withholds "+
			"values, not rows", redRows, entitledRows)
	}
	if redClaims != 0 {
		t.Fatalf("the redacted projected scan served ClaimId in %d of %d rows", redClaims, redRows)
	}
}
