package dbrpc

import (
	"context"
	"errors"
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

// TestQueryRawWireRefusesMemoryTable is the guard on the trap this relay sets: a table
// that cannot produce self-contained rows yields NO rows, which on the wire is
// indistinguishable from a query that matched nothing. A consumer that switched to this
// stream would silently return an empty result for every RAM table. It must refuse
// instead, before any row, so the caller falls back to the text stream.
func TestQueryRawWireRefusesMemoryTable(t *testing.T) {
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
	if err := tx.NewClassAd(ctx, "k1", "Name = \"one\"\nCpus = 4"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// The row is really there over the text stream.
	texts, err := c.QueryRawTable(ctx, "scratch", "true", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(texts) != 1 {
		t.Fatalf("text stream returned %d rows, want the one that was inserted", len(texts))
	}

	rows := 0
	err = c.QueryRawWireStream(ctx, "scratch", "true", nil, 0, false, func([]byte) bool {
		rows++
		return true
	})
	if !errors.Is(err, ErrRawWireUnsupported) {
		t.Errorf("err = %v, want ErrRawWireUnsupported", err)
	}
	if rows != 0 {
		t.Errorf("delivered %d rows before refusing; the refusal must precede any row", rows)
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
	rows = 0
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
