package dbrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// testPairPersistent is testPair over a PERSISTENT (inline-names) db -- the
// mode wire-form rows exist for -- with the connection's privilege selectable.
func testPairPersistent(t *testing.T, includePrivate bool) (*Client, func()) {
	t.Helper()
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: includePrivate}) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

func seedWireAds(t *testing.T, c *Client, n int) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		key := "slot" + string(rune('a'+i%26)) + "." + string(rune('0'+i/26))
		if err := tx.NewClassAd(ctx, key,
			"MyType = \"Machine\"\nName = \""+key+"\"\nState = \"Unclaimed\"\nCpus = 8\nMemory = 16384\nClaimId = \"<secret-"+key+">\""); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// renderRows collects a QueryRawWireStream result rendered at the edge, as
// name->value maps keyed by Name.
func renderRows(t *testing.T, c *Client, table, constraint string, attrs []string, redact bool) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	var buf []byte
	var offs []int
	err := c.QueryRawWireStream(context.Background(), table, constraint, attrs, 0, redact, func(row []byte) bool {
		var ok bool
		buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Fatal("render failed on a wire row")
		}
		m := map[string]string{}
		for i := 0; i+1 < len(offs); i++ {
			name, val, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
			if !found {
				t.Fatalf("malformed expr %q", buf[offs[i]:offs[i+1]])
			}
			m[name] = val
		}
		out[strings.Trim(m["Name"], `"`)] = m
		return true
	})
	if err != nil {
		t.Fatalf("QueryRawWireStream: %v", err)
	}
	return out
}

// TestQueryRawWireStream verifies the batched wire-row stream end to end:
// projection, source-side redaction (both requested and privilege-forced), and
// multi-frame batching under a tiny budget.
func TestQueryRawWireStream(t *testing.T) {
	c, cleanup := testPairPersistent(t, true) // privileged, like the collector
	defer cleanup()
	const n = 60
	seedWireAds(t, c, n)

	// Force many small frames so batch reassembly is exercised.
	old := WireBatchBudget
	WireBatchBudget = 512
	defer func() { WireBatchBudget = old }()

	proj := renderRows(t, c, DefaultTable, "", []string{"Name", "Cpus"}, false)
	if len(proj) != n {
		t.Fatalf("projected wire stream returned %d ads, want %d", len(proj), n)
	}
	for name, m := range proj {
		// Name, Cpus projected; MyType always ships (lifted by the renderer, so
		// not in the expr map).
		if m["Cpus"] != "8" || len(m) != 2 {
			t.Fatalf("%s: projected row %v, want exactly Name+Cpus", name, m)
		}
	}

	// Privileged + redact requested: ClaimId stripped at the source.
	red := renderRows(t, c, DefaultTable, "", nil, true)
	for name, m := range red {
		if _, leak := m["ClaimId"]; leak {
			t.Fatalf("%s: redact-requested wire row leaked ClaimId", name)
		}
		if m["Memory"] != "16384" {
			t.Fatalf("%s: whole-ad redacted row lost Memory: %v", name, m)
		}
	}

	// Privileged, no redact: private attributes flow (the collector's PVT path).
	full := renderRows(t, c, DefaultTable, "", nil, false)
	if _, ok := full["slota.0"]["ClaimId"]; !ok {
		t.Fatal("privileged unredacted wire row missing ClaimId")
	}

	// Constraint pushdown still applies.
	some := renderRows(t, c, DefaultTable, `Cpus == 8 && Name == "slota.0"`, []string{"Name"}, false)
	if len(some) != 1 {
		t.Fatalf("constrained wire stream returned %d ads, want 1", len(some))
	}
}

// TestQueryRawWireUnprivilegedAlwaysRedacts: a connection served without
// IncludePrivate is redacted regardless of what the client requests.
func TestQueryRawWireUnprivilegedAlwaysRedacts(t *testing.T) {
	c, cleanup := testPairPersistent(t, false)
	defer cleanup()
	seedWireAds(t, c, 3)
	full := renderRows(t, c, DefaultTable, "", nil, false) // redact NOT requested
	for name, m := range full {
		if _, leak := m["ClaimId"]; leak {
			t.Fatalf("%s: unprivileged wire row leaked ClaimId", name)
		}
	}
}

// TestQueryRawWireServesMemoryTable: a RAM table serves wire rows too. Its records are keyed
// by interned ids rather than inline names, which is a difference in how a name is SPELLED in
// the stored bytes, not in what is stored -- so the row is assembled by rewriting those ids
// back to names, and the relay no longer refuses.
//
// The trap this test guards is the one the refusal used to guard: a relay scan that yields
// nothing looks exactly like a query that matched nothing. So it asserts the wire rows against
// the TEXT rows of the same table, attribute by attribute, and fails if the wire stream comes
// back empty.
func TestQueryRawWireServesMemoryTable(t *testing.T) {
	c, cleanup := catServerPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateTableInMemory(ctx, "scratch"); err != nil {
		t.Fatal(err)
	}
	tx, err := c.BeginTable(ctx, "scratch")
	if err != nil {
		t.Fatal(err)
	}
	// An expression-valued attribute is the interesting case: an interned record spells the
	// attribute references inside it as ids, so a row that shipped those bytes verbatim would
	// render into a DIFFERENT expression at the far edge, or fail to render at all.
	ads := map[string]string{
		"k1": "MyType = \"Machine\"\nName = \"one\"\nCpus = 4\n" +
			"Requirements = (TARGET.RequestCpus <= Cpus) && (State =?= \"Unclaimed\")\n" +
			"State = \"Unclaimed\"\nRank = strcat(Name, \"/\", State)",
		"k2": "MyType = \"Machine\"\nName = \"two\"\nCpus = 8\n" +
			"Requirements = MY.Cpus > 4\nState = \"Claimed\"\nRank = -Cpus",
	}
	for k, text := range ads {
		if err := tx.NewClassAd(ctx, k, text); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The text stream is the reference.
	texts, err := c.QueryRawTable(ctx, "scratch", "true", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) != len(ads) {
		t.Fatalf("text stream returned %d rows, want %d", len(texts), len(ads))
	}
	want := map[string]map[string]string{}
	for _, raw := range texts {
		m := map[string]string{}
		for _, line := range strings.Split(raw, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			name, val, found := strings.Cut(line, " = ")
			if !found {
				t.Fatalf("malformed text expr %q", line)
			}
			// The wire renderer LIFTS the type fields out of the expression list, so they are
			// not comparable as attributes.
			if strings.EqualFold(name, "MyType") || strings.EqualFold(name, "TargetType") {
				continue
			}
			m[name] = val
		}
		want[strings.Trim(m["Name"], `"`)] = m
	}

	got := renderRows(t, c, "scratch", "", nil, false)
	if len(got) == 0 {
		t.Fatal("the wire stream returned no rows for a RAM table")
	}
	if len(got) != len(want) {
		t.Fatalf("wire stream returned %d rows, text stream %d", len(got), len(want))
	}
	for name, attrs := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%s: missing from the wire stream", name)
			continue
		}
		for a, v := range attrs {
			if g[a] != v {
				t.Errorf("%s.%s: wire %q != text %q", name, a, g[a], v)
			}
		}
		if len(g) != len(attrs) {
			t.Errorf("%s: wire row has %d attributes, text row %d", name, len(g), len(attrs))
		}
	}

	// Projection and constraint pushdown work on a RAM table like any other.
	some := renderRows(t, c, "scratch", `Cpus == 8`, []string{"Name", "Requirements"}, false)
	if len(some) != 1 {
		t.Fatalf("constrained wire stream returned %d rows, want 1", len(some))
	}
	if r := some["two"]["Requirements"]; r != want["two"]["Requirements"] {
		t.Errorf("projected Requirements = %q, want %q", r, want["two"]["Requirements"])
	}

	// A persistent table on the same server still serves wire rows.
	if err := c.CreateTable(ctx, "durable"); err != nil {
		t.Fatal(err)
	}
	tx, err = c.BeginTable(ctx, "durable")
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.NewClassAd(ctx, "k1", "Name = \"two\"\nCpus = 8"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows := 0
	if err := c.QueryRawWireStream(ctx, "durable", "true", nil, 0, false, func([]byte) bool {
		rows++
		return true
	}); err != nil {
		t.Fatalf("persistent table: %v", err)
	}
	if rows != 1 {
		t.Errorf("persistent table returned %d wire rows, want 1", rows)
	}
}

// TestQueryRawWireServesView is the RAM path's other consumer: a materialized view's backing
// store is an in-memory db, so the view started serving wire rows the moment a RAM table
// could. Its rows must agree with its own text rows -- a view's ads are built by the
// aggregator rather than written by a client, so nothing else here covers that shape.
func TestQueryRawWireServesView(t *testing.T) {
	f := newMXFixture(t)
	defer f.close()
	ctx := context.Background()

	texts, err := f.c.QueryRawTable(ctx, mxView, "true", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) == 0 {
		t.Fatal("the view has no rows; this test would prove nothing")
	}
	want := map[string]string{}
	for _, line := range strings.Split(texts[0], "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, val, found := strings.Cut(line, " = ")
		if !found {
			t.Fatalf("malformed text expr %q", line)
		}
		if strings.EqualFold(name, "MyType") || strings.EqualFold(name, "TargetType") {
			continue
		}
		want[name] = val
	}

	rows := 0
	got := map[string]string{}
	var buf []byte
	var offs []int
	if err := f.c.QueryRawWireStream(ctx, mxView, "true", nil, 0, false, func(row []byte) bool {
		rows++
		var ok bool
		buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Error("render failed on a view's wire row")
			return false
		}
		for i := 0; i+1 < len(offs); i++ {
			name, val, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
			if !found {
				t.Errorf("malformed expr %q", buf[offs[i]:offs[i+1]])
				return false
			}
			got[name] = val
		}
		return true
	}); err != nil {
		t.Fatalf("QueryRawWireStream on a view: %v", err)
	}
	if rows != len(texts) {
		t.Fatalf("view wire stream returned %d rows, text stream %d", rows, len(texts))
	}
	if len(got) != len(want) {
		t.Fatalf("view wire row %v != text row %v", got, want)
	}
	for name, val := range want {
		if got[name] != val {
			t.Errorf("view %s: wire %q != text %q", name, got[name], val)
		}
	}
}
