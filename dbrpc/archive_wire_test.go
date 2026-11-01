package dbrpc

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// Wire-form rows from an ARCHIVE.
//
// This op refused an archive, which made `history` -- the table most queries are about -- the
// only table a consumer could not read without the render-and-reparse round trip. Downstream
// that round trip measured at roughly two fifths of a read's CPU and 86 allocations a row, and
// it bought nothing: an archive's records already carry inline names, so it is the storage form
// wire rows suit best.
//
// The refusal was not a bug in the op so much as a kind never wired to it, which is why the
// tests here are mostly about AGREEMENT: a new source for an existing relay has to produce what
// the established source produces, or consumers that trusted one will read the other wrong.

const (
	archWireClaim = "claim-9f3a2-private-by-name"
	archWireN     = 50
)

// seedWireArchive creates "history" and appends archWireN records, newest last.
//
// SegmentSize is small deliberately: with the default 8 MiB a fixture this size stays in ONE
// UNSEALED segment, and then the fixture rather than the code decides which read path runs.
func seedWireArchive(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		ValueAttrs:  []string{"ClusterId"},
		ZoneAttrs:   []string{"CompletionDate"},
		SegmentSize: 64 << 10,
	}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for i := 0; i < archWireN; i++ {
		ad := fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nJobStatus = 4\n"+
			"Cmd = \"/bin/run%d\"\nCompletionDate = %d\nResidentSetSize = %d\n"+
			"MemoryUsage = (ResidentSetSize + 1023) / 1024\nClaimId = \"%s-%d\"",
			i, i%10, i, 1700000000+i, 90000+i, archWireClaim, i)
		if err := c.ArchiveAppend(ctx, "history", ad); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}
}

// wireRows renders a wire-row stream into one name->value map per row, in arrival order.
func wireRows(t *testing.T, c *Client, constraint string, attrs []string, limit int, redact, refs bool) []map[string]string {
	t.Helper()
	var out []map[string]string
	var buf []byte
	var offs []int
	collect := func(row []byte) bool {
		var ok bool
		buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Fatal("render failed on an archive wire row")
		}
		m := map[string]string{}
		for i := 0; i+1 < len(offs); i++ {
			name, val, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
			if !found {
				t.Fatalf("malformed expr %q", buf[offs[i]:offs[i+1]])
			}
			m[name] = val
		}
		out = append(out, m)
		return true
	}
	var err error
	if refs {
		err = c.QueryRawWireRefsStream(context.Background(), "history", constraint, attrs, limit, redact, collect)
	} else {
		err = c.QueryRawWireStream(context.Background(), "history", constraint, attrs, limit, redact, collect)
	}
	if err != nil {
		t.Fatalf("wire stream (refs=%v): %v", refs, err)
	}
	return out
}

// TestArchiveRawWireOverRPC is the regression: this op answered
// "QueryRawWire is not served from an archive" for every archive table.
func TestArchiveRawWireOverRPC(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{IncludePrivate: true})
	defer cleanup()
	seedWireArchive(t, c)

	rows := wireRows(t, c, "true", []string{"Owner", "ClusterId"}, 0, false, false)
	if len(rows) != archWireN {
		t.Fatalf("archive wire stream returned %d rows, want %d", len(rows), archWireN)
	}
	for i, m := range rows {
		if _, ok := m["ClusterId"]; !ok {
			t.Fatalf("row %d missing the projected ClusterId: %v", i, m)
		}
		// The projection has to reach the archive's own walk, not be applied after.
		if _, ok := m["Cmd"]; ok {
			t.Errorf("row %d carries unselected Cmd: %v", i, m)
		}
		if _, ok := m["JobStatus"]; ok {
			t.Errorf("row %d carries unselected JobStatus: %v", i, m)
		}
	}
}

// TestArchiveRawWireIsNewestFirstWithLimit: the archive's two defining read properties. A
// relay that returned the right rows in the wrong order, or read all of them and sliced, would
// pass a row-count check and be wrong in the way that matters for history.
func TestArchiveRawWireIsNewestFirstWithLimit(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{IncludePrivate: true})
	defer cleanup()
	seedWireArchive(t, c)

	rows := wireRows(t, c, "true", []string{"ClusterId"}, 3, false, false)
	if len(rows) != 3 {
		t.Fatalf("LIMIT 3 returned %d rows", len(rows))
	}
	for i, want := range []string{"49", "48", "47"} {
		if rows[i]["ClusterId"] != want {
			t.Errorf("row %d ClusterId = %q, want %q (newest-first order wrong)",
				i, rows[i]["ClusterId"], want)
		}
	}
}

// TestArchiveRawWireAgreesWithTextProjection is the oracle.
//
// The text projection op has served archives all along, so it is the established answer; this
// op is a new source for the same relay. If the two disagree on which attributes a row carries
// or on a value, a consumer that switches for the speed reads different data -- which is the
// failure a row count cannot see.
func TestArchiveRawWireAgreesWithTextProjection(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{IncludePrivate: true})
	defer cleanup()
	seedWireArchive(t, c)
	ctx := context.Background()

	const where = "ClusterId >= 40"
	proj := []string{"Owner", "ClusterId", "CompletionDate"}

	textRows, err := c.QueryRawProject(ctx, "history", where, proj, 0)
	if err != nil {
		t.Fatalf("QueryRawProject: %v", err)
	}
	wire := wireRows(t, c, where, proj, 0, false, false)
	if len(wire) != len(textRows) {
		t.Fatalf("wire returned %d rows, text %d", len(wire), len(textRows))
	}
	if len(wire) == 0 {
		t.Fatal("the constraint matched nothing; this test would prove nothing")
	}

	for i, raw := range textRows {
		ad, perr := classad.ParseOld(raw)
		if perr != nil {
			t.Fatalf("parse text row %q: %v", raw, perr)
		}
		for _, attr := range proj {
			want := ad.EvaluateAttr(attr).String()
			got := wire[i][attr]
			// The text row renders a string with quotes, as the wire render does, so
			// compare the rendered forms after stripping both.
			if strings.Trim(got, `"`) != strings.Trim(want, `"`) {
				t.Errorf("row %d %s: wire %q, text %q", i, attr, got, want)
			}
		}
		// Same attribute SET, not just the same values for the ones asked about.
		var textNames []string
		for _, name := range ad.GetAttributes() {
			if !strings.EqualFold(name, "MyType") && !strings.EqualFold(name, "TargetType") {
				textNames = append(textNames, strings.ToLower(name))
			}
		}
		var wireNames []string
		for name := range wire[i] {
			if !strings.EqualFold(name, "MyType") && !strings.EqualFold(name, "TargetType") {
				wireNames = append(wireNames, strings.ToLower(name))
			}
		}
		sort.Strings(textNames)
		sort.Strings(wireNames)
		if strings.Join(textNames, ",") != strings.Join(wireNames, ",") {
			t.Fatalf("row %d attribute sets differ: wire %v, text %v", i, wireNames, textNames)
		}
	}
}

// TestArchiveRawWireRefsCarriesReferences: the refs variant exists for a consumer that
// EVALUATES what it receives, and an archive consumer is exactly that -- the Arrow encoder.
// MemoryUsage is written over ResidentSetSize, which nobody selects, so the plain op strips it
// and the expression evaluates to undefined.
//
// Both halves are asserted. Checking only that refs works would pass against an op that always
// chased references, which would make the plain form silently wrong for a relay reproducing
// HTCondor's projection protocol.
func TestArchiveRawWireRefsCarriesReferences(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{IncludePrivate: true})
	defer cleanup()
	seedWireArchive(t, c)

	plain := wireRows(t, c, "ClusterId == 7", []string{"MemoryUsage"}, 0, false, false)
	if len(plain) != 1 {
		t.Fatalf("plain: %d rows, want 1", len(plain))
	}
	if _, ok := plain[0]["ResidentSetSize"]; ok {
		t.Error("the plain wire op carried a reference it was not asked for")
	}

	refs := wireRows(t, c, "ClusterId == 7", []string{"MemoryUsage"}, 0, false, true)
	if len(refs) != 1 {
		t.Fatalf("refs: %d rows, want 1", len(refs))
	}
	if _, ok := refs[0]["ResidentSetSize"]; !ok {
		t.Fatalf("the refs wire op dropped the sibling MemoryUsage is written over: %v", refs[0])
	}
	// And the point of carrying it: the expression evaluates to a number.
	ad, perr := classad.ParseOld("MemoryUsage = " + refs[0]["MemoryUsage"] +
		"\nResidentSetSize = " + refs[0]["ResidentSetSize"])
	if perr != nil {
		t.Fatal(perr)
	}
	if v, ok := ad.EvaluateAttrInt("MemoryUsage"); !ok || v != (90007+1023)/1024 {
		t.Errorf("MemoryUsage evaluated to %d (ok=%v), want %d", v, ok, (90007+1023)/1024)
	}
}

// TestArchiveRawWireRedactsPrivateAttributes: enabling a kind on a relay op must not widen what
// it discloses. An unprivileged session's rows are searched for the secret AS BYTES, because a
// decoded-value check would miss a value carried under another name.
func TestArchiveRawWireRedactsPrivateAttributes(t *testing.T) {
	for _, tc := range []struct {
		name           string
		includePrivate bool
		wantSecret     bool
	}{
		{"unprivileged", false, false},
		// The privileged case is the control: without it, a redaction that stripped
		// everything would pass.
		{"privileged", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Two connections to ONE catalog: the archive is seeded over a
			// privileged one, because a read-only session cannot create or append,
			// and read back over a session with the privilege under test. One
			// connection cannot do both, and a second catalog would not be the
			// same data.
			cat, err := db.OpenCatalog(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			srv := NewServerCatalog(cat)
			defer srv.Close()

			wconn, wsrv := netPipe()
			go func() { _ = srv.ServeConnOpts(wsrv, ServeOptions{IncludePrivate: true}) }()
			writer := NewClient(wconn)
			seedWireArchive(t, writer)
			writer.Close()

			opts := ServeOptions{ReadOnly: true}
			if tc.includePrivate {
				opts = ServeOptions{IncludePrivate: true}
			}
			rconn, rsrv := netPipe()
			go func() { _ = srv.ServeConnOpts(rsrv, opts) }()
			c := NewClient(rconn)
			defer c.Close()

			found := false
			rows := 0
			// redact=false in the REQUEST: an unprivileged session must redact
			// anyway, so asking for the entitled read is the adversarial ask.
			err = c.QueryRawWireStream(context.Background(), "history", "true",
				[]string{"Owner", "ClaimId"}, 0, false, func(row []byte) bool {
					rows++
					if bytes.Contains(row, []byte(archWireClaim)) {
						found = true
					}
					return true
				})
			if err != nil {
				t.Fatalf("QueryRawWireStream: %v", err)
			}
			if rows != archWireN {
				t.Fatalf("%d rows, want %d", rows, archWireN)
			}
			if found != tc.wantSecret {
				t.Errorf("private attribute present = %v, want %v", found, tc.wantSecret)
			}
		})
	}
}
