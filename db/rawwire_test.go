package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections"
)

// TestUpdateOldVisibleInQueryRawAndWatch proves the wire-native ingest path goes
// through the same storage, query, and change-log/watch machinery as a
// transactional Put -- so a collector using UpdateOld still gets correct queries
// AND a working watch feed (which HA replication rides on).
func TestUpdateOldVisibleInQueryRawAndWatch(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	if err := d.UpdateOld("a", `Name = "a"`+"\n"+`State = "Idle"`); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateOld("b", `Name = "b"`+"\n"+`State = "Busy"`); err != nil {
		t.Fatal(err)
	}

	// Query sees both.
	seq, err := d.Query("true")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if n != 2 {
		t.Fatalf("Query after UpdateOld = %d, want 2", n)
	}

	// QueryRaw yields wire-form ads (non-empty expression lines) and honors the
	// constraint.
	rseq, err := d.QueryRaw(`State == "Idle"`)
	if err != nil {
		t.Fatal(err)
	}
	raw := 0
	for ra := range rseq {
		raw++
		if len(ra.Exprs) == 0 {
			t.Fatal("QueryRaw yielded a RawAd with no expression lines")
		}
	}
	if raw != 1 {
		t.Fatalf("QueryRaw Idle = %d, want 1", raw)
	}

	// Watch from the start replays both as upserts -- the proof UpdateOld reaches
	// the change log (and thus the HA commit stream), not just the key directory.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wseq, err := d.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	upserts := 0
	for ev := range wseq {
		if ev.Kind == WatchUpsert {
			upserts++
		}
		if upserts >= 2 {
			break
		}
	}
	if upserts < 2 {
		t.Fatalf("Watch replayed %d upserts, want 2 (UpdateOld did not reach the change log)", upserts)
	}
}

// TestQueryRawInline is the point of the inline-RawAd support: a PERSISTENT db
// (inline-encoded storage) must yield wire-form RawAds from QueryRaw, decoded
// straight from the stored records with no AST -- previously QueryRaw bailed on
// inline collections.
func TestQueryRawInline(t *testing.T) {
	d, err := Open(t.TempDir()) // persistent -> inline encoding
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	if err := d.UpdateOld("a", "MyType = \"Machine\"\nName = \"slot1\"\nState = \"Idle\"\nCpus = 8"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateOld("b", "MyType = \"Machine\"\nName = \"slot2\"\nState = \"Busy\""); err != nil {
		t.Fatal(err)
	}

	// Match-all (ScanRaw path).
	all, err := d.QueryRaw("true")
	if err != nil {
		t.Fatal(err)
	}
	n, sawSlot1 := 0, false
	for ra := range all {
		n++
		if ra.MyType != "Machine" {
			t.Fatalf("inline RawAd MyType = %q, want Machine", ra.MyType)
		}
		joined := ""
		for _, e := range ra.Exprs {
			joined += string(e) + "\n"
		}
		if strings.Contains(joined, "MyType") {
			t.Fatalf("MyType leaked into Exprs (should be the RawAd.MyType field):\n%s", joined)
		}
		if strings.Contains(joined, `Name = "slot1"`) {
			sawSlot1 = true
			if !strings.Contains(joined, "Cpus = 8") || !strings.Contains(joined, `State = "Idle"`) {
				t.Fatalf("inline RawAd for slot1 missing rendered attrs:\n%s", joined)
			}
		}
	}
	if n != 2 {
		t.Fatalf("inline QueryRaw(true) yielded %d, want 2", n)
	}
	if !sawSlot1 {
		t.Fatal("inline QueryRaw did not render slot1's attributes")
	}

	// Constrained (indexed/scan QueryRaw path).
	idle, err := d.QueryRaw(`State == "Idle"`)
	if err != nil {
		t.Fatal(err)
	}
	m := 0
	for range idle {
		m++
	}
	if m != 1 {
		t.Fatalf("inline QueryRaw(Idle) = %d, want 1", m)
	}
}

// TestUpdateOldEncryptedFallback checks the encryption guard: on an encrypted DB,
// UpdateOld must take the parse+Put path so data is sealed at rest -- not the
// wire-native fast path, whose encoder does not seal. The reopen with a different
// pool key must fail, proving the ad was written encrypted rather than plaintext.
func TestUpdateOldEncryptedFallback(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenConfig(Config{Dir: dir, PoolKeys: []KEK{poolKey("POOL")}, EncryptedAttrs: []string{"Secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateOld("a", `Name = "a"`+"\n"+`Secret = "top-secret"`); err != nil {
		t.Fatal(err)
	}
	ad, ok := d.LookupClassAd("a")
	if !ok {
		t.Fatal("ad missing after encrypted UpdateOld")
	}
	if s, _ := ad.EvaluateAttrString("Secret"); s != "top-secret" {
		t.Fatalf("Secret = %q, want top-secret (encrypted fallback failed)", s)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with the wrong pool key: must fail (the data key can't be unwrapped),
	// which proves the store was actually encrypted.
	if bad, err := OpenConfig(Config{Dir: dir, PoolKeys: []KEK{poolKey("WRONG")}, EncryptedAttrs: []string{"Secret"}}); err == nil {
		_ = bad.Close()
		t.Fatal("reopen with wrong key succeeded: the ad was stored in plaintext (UpdateOld leaked past encryption)")
	}
}

// TestQueryRawProjectedFromSeq covers the DB-level pagination: pages cover the
// matching set exactly once, the constraint and projection are applied to every
// page, and a resume from the returned cursor picks up where the last page
// stopped.
func TestQueryRawProjectedFromSeq(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	const n = 250
	for i := 0; i < n; i++ {
		owner := "alice"
		if i%2 == 1 {
			owner = "bob"
		}
		ad := fmt.Sprintf("MyType = \"Job\"\nClusterId = %d\nProcId = 0\nOwner = %q\nCmd = \"/bin/true\"", i, owner)
		if err := d.UpdateOld(fmt.Sprintf("k%d", i), ad); err != nil {
			t.Fatal(err)
		}
	}

	var (
		cursor SeqCursor
		seen   = map[int]int{}
		pages  int
	)
	for {
		rows, page, err := d.QueryRawProjectedFromSeq(`Owner == "alice"`, []string{"ClusterId", "Owner"}, cursor, 16)
		if err != nil {
			t.Fatal(err)
		}
		for ra := range rows {
			var cluster int
			owner := ""
			for _, e := range ra.Exprs {
				name, val, _ := strings.Cut(string(e), " = ")
				switch name {
				case "ClusterId":
					if _, err := fmt.Sscanf(val, "%d", &cluster); err != nil {
						t.Fatalf("ClusterId %q does not parse: %v", val, err)
					}
				case "Owner":
					owner = strings.Trim(val, `"`)
				case "Cmd", "ProcId":
					t.Fatalf("projection kept %s, which was not requested", name)
				}
			}
			if owner != "alice" {
				t.Fatalf("constraint let through Owner %q", owner)
			}
			seen[cluster]++
		}
		pages++
		if !page.More {
			break
		}
		cursor = page.Next
		if pages > 100 {
			t.Fatal("pagination did not terminate")
		}
	}

	if pages < 2 {
		t.Fatalf("expected several pages, got %d", pages)
	}
	if len(seen) != n/2 {
		t.Fatalf("paged over %d distinct jobs, want %d", len(seen), n/2)
	}
	for cluster, count := range seen {
		if count != 1 {
			t.Errorf("cluster %d seen %d times across pages, want 1", cluster, count)
		}
		if cluster%2 != 0 {
			t.Errorf("cluster %d does not match the constraint", cluster)
		}
	}
}

// TestQueryRawWireInMemoryTable is the RAM table's half of the relay contract: QueryRawWire
// must serve an in-memory table, and the rows it serves must render to exactly what the text
// path returns for the same query -- expression-valued attributes included, since those are
// where the two record encodings actually differ (an interned record spells the attribute
// references INSIDE an expression as ids too, not just the attribute keys).
//
// It fails loudly on the empty result, which is the failure mode this path invites: a relay
// scan that yields nothing is indistinguishable from a query that matched nothing.
func TestQueryRawWireInMemoryTable(t *testing.T) {
	d, err := Open("") // in-memory -> interned encoding
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if d.Persistent() {
		t.Fatal("Open(\"\") produced a persistent table: this test would not cover the RAM path")
	}

	ads := map[string]string{
		"a": "MyType = \"Machine\"\nName = \"slotA\"\nState = \"Unclaimed\"\nCpus = 8\n" +
			"ClaimId = \"<secret>\"\n" +
			"Requirements = (TARGET.RequestCpus <= Cpus) && (MY.State =?= \"Unclaimed\")\n" +
			"Rank = strcat(Name, \"/\", State)\nNested = [Inner = Cpus * 2]\nPicked = Nested.Inner",
		"b": "MyType = \"Machine\"\nName = \"slotB\"\nState = \"Claimed\"\nCpus = 4\n" +
			"Requirements = Cpus > 2\nRank = -Cpus",
	}
	for k, text := range ads {
		if err := d.UpdateOld(k, text); err != nil {
			t.Fatal(err)
		}
	}

	collect := func(constraint string, proj []string, redact bool) map[string]map[string]string {
		t.Helper()
		seq, err := d.QueryRawWire(constraint, proj, redact)
		if err != nil {
			t.Fatalf("QueryRawWire: %v", err)
		}
		out := map[string]map[string]string{}
		var buf []byte
		var offs []int
		for row := range seq {
			var ok bool
			buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
			if !ok {
				t.Fatal("RenderRawAdInline failed on a shipped row")
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
		}
		return out
	}
	collectText := func(constraint string, proj []string, redact bool) map[string]map[string]string {
		t.Helper()
		seq, err := d.QueryRawProjected(constraint, proj, redact)
		if err != nil {
			t.Fatalf("QueryRawProjected: %v", err)
		}
		out := map[string]map[string]string{}
		for ra := range seq {
			m := map[string]string{}
			for _, e := range ra.Exprs {
				name, val, found := strings.Cut(string(e), " = ")
				if !found {
					t.Fatalf("malformed expr %q", e)
				}
				m[name] = val
			}
			out[strings.Trim(m["Name"], `"`)] = m
		}
		return out
	}

	cases := []struct {
		name       string
		constraint string
		proj       []string
		redact     bool
	}{
		{"whole-ad", "true", nil, false},
		{"whole-ad-redacted", "true", nil, true},
		{"projected", "true", []string{"Name", "Requirements", "Rank", "Picked", "Nested"}, false},
		{"constrained", `Cpus == 8`, []string{"Name", "Requirements"}, false},
		{"projected-redacted", "true", []string{"Name", "ClaimId"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := collectText(tc.constraint, tc.proj, tc.redact)
			got := collect(tc.constraint, tc.proj, tc.redact)
			if len(got) == 0 {
				t.Fatal("QueryRawWire returned no rows for an in-memory table")
			}
			if len(got) != len(want) {
				t.Fatalf("wire returned %d ads, text %d", len(got), len(want))
			}
			for name, attrs := range want {
				g, ok := got[name]
				if !ok {
					t.Errorf("%s: missing from the wire result", name)
					continue
				}
				if len(g) != len(attrs) {
					t.Errorf("%s: wire %v != text %v", name, g, attrs)
					continue
				}
				for a, v := range attrs {
					if g[a] != v {
						t.Errorf("%s.%s: wire %q != text %q", name, a, g[a], v)
					}
				}
			}
		})
	}

	// Redaction is applied at the source, not by the renderer: the private value's bytes must
	// not be in the row at all. And the entitled read must still carry it, or the check above
	// would pass for the trivial reason that nothing ships.
	seq, err := d.QueryRawWire("true", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for row := range seq {
		if strings.Contains(string(row), "<secret>") {
			t.Fatal("a redacted wire row carries the private value bytes")
		}
	}
	seq, err = d.QueryRawWire("true", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	entitled := false
	for row := range seq {
		if strings.Contains(string(row), "<secret>") {
			entitled = true
		}
	}
	if !entitled {
		t.Fatal("an entitled whole-ad wire row dropped the private attribute")
	}
}
