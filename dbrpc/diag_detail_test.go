package dbrpc

import (
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// detailOnlyFields are the Diagnostics fields a BASIC read deliberately leaves unmeasured, each
// with the reason it costs more than the rest of the report put together. Everything else must
// come back identical from both reads -- a basic report that quietly disagreed with a full one
// about how many ads or segments a table holds would be worse than a slow one.
var detailOnlyFields = map[string]string{
	"Suggestions": "decodes a sample of records to profile their attributes",
	"DropSuggestions": "decodes a sample of records to profile their attributes (mutable only; " +
		"an archive is never told which index to drop)",
	"IndexSizes": "walks every live segment's heap postings",
	"Detail":     "the discriminator itself",
}

// TestDiagnosticsBasicAgreesWithFull is the contract that makes a basic read safe to print: every
// field it does measure matches the full read exactly, and the ones it does not are left zero
// rather than filled with a cheaper, differently-wrong number.
//
// Reflective on purpose, like TestDiagnosticsFieldsPopulatedForBothKinds: a fixed field list would
// go stale the moment someone adds a field to diagCommon, and a field added to the detail branch
// without a reason here is exactly the drift worth catching.
func TestDiagnosticsBasicAgreesWithFull(t *testing.T) {
	for _, kind := range []string{"mutable", "archive"} {
		t.Run(kind, func(t *testing.T) {
			full, basic := diagBothWays(t, kind)
			if !full.Detail {
				t.Error("the full read does not report itself as detailed")
			}
			if basic.Detail {
				t.Error("the basic read reports itself as detailed")
			}
			rt := reflect.TypeOf(full)
			fv, bv := reflect.ValueOf(full), reflect.ValueOf(basic)
			compared := 0
			for i := 0; i < rt.NumField(); i++ {
				name := rt.Field(i).Name
				if name == "SidecarSizes" {
					continue // compared field by field below: the totals carry over, the breakdown does not
				}
				if _, skip := detailOnlyFields[name]; skip {
					if !bv.Field(i).IsZero() {
						t.Errorf("%s: basic read measured %s, which it is documented not to", kind, name)
					}
					continue
				}
				if !reflect.DeepEqual(fv.Field(i).Interface(), bv.Field(i).Interface()) {
					t.Errorf("%s: %s differs between the full and basic reads:\n full:  %+v\n basic: %+v",
						kind, name, fv.Field(i).Interface(), bv.Field(i).Interface())
				}
				compared++
			}
			// Without this the loop above passes just as well over a struct whose fields are all
			// zero on both sides, which is the shape every vacuous version of this test had.
			if compared < 8 {
				t.Errorf("%s: only %d fields compared; the fixture is not exercising the report", kind, compared)
			}
			// The sidecar totals are the whole point of SidecarBytes: a basic read must report the
			// same count and the same bytes, and only the structural breakdown may go missing.
			if full.SidecarSizes.Segments != basic.SidecarSizes.Segments ||
				full.SidecarSizes.MappedBytes != basic.SidecarSizes.MappedBytes {
				t.Errorf("%s: sidecar totals differ: full %+v, basic %+v", kind, full.SidecarSizes, basic.SidecarSizes)
			}
			if full.SidecarSizes.Segments == 0 || full.SidecarSizes.MappedBytes == 0 {
				t.Fatalf("%s: no sealed sidecars in the fixture; the sidecar comparison is vacuous", kind)
			}
			if basic.SidecarSizes.MPHBytes != 0 || basic.SidecarSizes.BloomBytes != 0 {
				t.Errorf("%s: basic read measured the sidecar breakdown: %+v", kind, basic.SidecarSizes)
			}
			if full.SidecarSizes.MPHBytes == 0 && full.SidecarSizes.BloomBytes == 0 {
				t.Errorf("%s: the full read reports no MPH or bloom bytes, so the breakdown this test "+
					"claims to distinguish is not being measured at all", kind)
			}
			// Likewise for the sampled half: if the fixture stopped producing suggestions, the
			// detailOnlyFields check above would hold vacuously.
			if !suggests(full.Suggestions, demandAttr) {
				t.Errorf("%s: the full read suggests no index on %s, so the basic read's empty "+
					"Suggestions proves nothing", kind, demandAttr)
			}
		})
	}
}

// diagBothWays builds one persistent table of the given kind with sealed, indexed sidecars and
// recorded query demand, then reads its diagnostics both ways off the same server.
func diagBothWays(t *testing.T, kind string) (full, basic Diagnostics) {
	t.Helper()
	root := t.TempDir()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: filepath.Join(root, "cat")})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	t.Cleanup(func() { s.Close(); cat.Close() })

	// A small segment size so segments actually SEAL into sidecars, and indexes so there is
	// something in them to break down. With the 8 MiB default nothing seals and every sidecar
	// comparison below reads zero against zero.
	const ads = 4000
	var build func(detail bool) ([]byte, error)
	var query func(string) (iter.Seq[*classad.ClassAd], error)
	switch kind {
	case "mutable":
		d, dErr := db.OpenConfig(db.Config{
			Dir: filepath.Join(root, "mutable"), SegmentSize: 1 << 16,
			CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"RequestMemory"},
		})
		if dErr != nil {
			t.Fatal(dErr)
		}
		t.Cleanup(func() { d.Close() })
		tx := d.Begin()
		for i := 0; i < ads; i++ {
			if !tx.NewClassAdOld(fmt.Sprintf("k%d", i), parityAd(i)) {
				t.Fatalf("staging ad %d failed", i)
			}
		}
		if commitErr := tx.Commit(); commitErr != nil {
			t.Fatal(commitErr)
		}
		d.Reindex()
		query = d.Query
		build = func(detail bool) ([]byte, error) { return s.diagJSON(d, detail) }
	case "archive":
		arch, aErr := cat.CreateArchiveTable("history", db.ArchiveConfig{
			SegmentSize: 1 << 16, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"RequestMemory"},
		})
		if aErr != nil {
			t.Fatal(aErr)
		}
		for i := 0; i < ads; i++ {
			if appendErr := arch.AppendOld(parityAd(i)); appendErr != nil {
				t.Fatal(appendErr)
			}
		}
		query = arch.Query
		build = func(detail bool) ([]byte, error) { return s.archiveDiagJSON(arch, detail) }
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	demandQueries(t, kind, query)

	// Full FIRST, so a basic read cannot be credited with state the full one happened to warm.
	full = mustDiag(t, func() ([]byte, error) { return build(true) })
	basic = mustDiag(t, func() ([]byte, error) { return build(false) })
	return full, basic
}

// TestDiagnosticsFlagsWordIsOptional pins the compatibility direction of the flags word: a client
// that predates it sends a frame ending after the table name, and must still get the WHOLE report.
// The saving is the new client's to ask for -- defaulting an absent word to basic would silently
// empty .suggest and .indexes for every client that has not been rebuilt.
//
// Driven at the frame level because the Client cannot produce the old shape: it always sends the
// word now, which is exactly why nothing else would notice this breaking.
func TestDiagnosticsFlagsWordIsOptional(t *testing.T) {
	s, arch := diagDetailServer(t)
	full := mustDiag(t, func() ([]byte, error) { return s.archiveDiagJSON(arch, true) })

	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"no flags word (an older client)", putStr(req(1, opDiag), "history")},
		{"flags word asking for detail", putI32(putStr(req(1, opDiag), "history"), int32(diagFlagDetail))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := diagOverTheWire(t, s, tc.frame)
			if len(d.Suggestions) != len(full.Suggestions) || d.IndexSizes.TotalBytes != full.IndexSizes.TotalBytes {
				t.Errorf("answered with a partial report: %d suggestions and %d index bytes, "+
					"want %d and %d", len(d.Suggestions), d.IndexSizes.TotalBytes,
					len(full.Suggestions), full.IndexSizes.TotalBytes)
			}
			if d.SidecarSizes.BloomBytes == 0 {
				t.Error("answered without the sidecar breakdown")
			}
		})
	}

	t.Run("unknown flag is refused", func(t *testing.T) {
		frame := putI32(putStr(req(1, opDiag), "history"), int32(diagFlagsKnown<<1))
		_, status, _, ok := respHeader(s.handle(nil, 1, opDiag, mustReqBody(t, frame), true, true))
		if !ok || status == stOK {
			t.Errorf("a request carrying an unimplemented flag was served (status %d); it must be "+
				"refused rather than answered without what it asked for", status)
		}
	})
}

// diagDetailServer builds a catalog server holding one sealed, indexed archive named "history",
// with query demand recorded against it.
func diagDetailServer(t *testing.T) (*Server, *db.ArchiveTable) {
	t.Helper()
	cat, err := db.OpenCatalogConfig(db.CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	t.Cleanup(func() { s.Close(); cat.Close() })
	arch, err := cat.CreateArchiveTable("history", db.ArchiveConfig{
		SegmentSize: 1 << 16, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"RequestMemory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4000; i++ {
		if appendErr := arch.AppendOld(parityAd(i)); appendErr != nil {
			t.Fatal(appendErr)
		}
	}
	demandQueries(t, "archive", arch.Query)
	return s, arch
}

// diagOverTheWire runs one request frame through the server's own dispatch and decodes the
// Diagnostics out of the reply, without the Client -- which is the point, since the Client can
// no longer produce a frame that omits the flags word.
func diagOverTheWire(t *testing.T, s *Server, frame []byte) Diagnostics {
	t.Helper()
	_, status, body, ok := respHeader(s.handle(nil, 1, opDiag, mustReqBody(t, frame), true, true))
	if !ok {
		t.Fatal("unparseable response frame")
	}
	if status != stOK {
		t.Fatalf("request refused: status %d (%s)", status, body.str())
	}
	var d Diagnostics
	if err := json.Unmarshal([]byte(body.str()), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// mustReqBody strips the request header off a frame, leaving the body reader handle() expects.
func mustReqBody(t *testing.T, frame []byte) *reader {
	t.Helper()
	_, _, body, ok := reqHeader(frame)
	if !ok {
		t.Fatal("unparseable request frame")
	}
	return body
}
