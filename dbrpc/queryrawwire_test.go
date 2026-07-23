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
