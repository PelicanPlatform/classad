package db

import (
	"context"
	"strings"
	"testing"
	"time"
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
