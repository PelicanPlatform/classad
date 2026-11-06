package db

import (
	"bytes"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestSystemKeyHiddenFromClientButDurable verifies the db-level guarantees for reserved
// system keys: they are excluded from Query and are neither returned nor removed by
// DeleteWhere, yet remain retrievable by explicit LookupClassAd, enumerable via
// ForEachSystemAd, and durable across a snapshot/restore cycle.
func TestSystemKeyHiddenFromClientButDurable(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	putAd(t, d, "a", `State = "Idle"`)
	putAd(t, d, "b", `State = "Idle"`)
	putAd(t, d, "c", `State = "Claimed"`)

	sysKey := SystemKey("idem/op-42")
	if !IsSystemKey(sysKey) {
		t.Fatalf("SystemKey did not produce a system key: %q", sysKey)
	}
	if putErr := d.Put(sysKey, mustAd(t, "Marker = 1\nState = \"Idle\"")); putErr != nil {
		t.Fatal(putErr)
	}

	// Query(true) returns only the normal ads (never the system record), even though
	// the system record's State matches the constraint.
	{
		seq, qerr := d.Query("true")
		if qerr != nil {
			t.Fatal(qerr)
		}
		count := 0
		for ad := range seq {
			count++
			if _, ok := ad.Lookup("Marker"); ok {
				t.Error("Query(true) returned the system record")
			}
		}
		if count != 3 {
			t.Errorf("Query(true) returned %d ads, want 3", count)
		}
	}

	// DeleteWhere(true) removes only the normal ads and spares the system record.
	n, err := d.DeleteWhere("true")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("DeleteWhere(true) removed %d, want 3 (the system record must be spared)", n)
	}
	if _, ok := d.LookupClassAd(sysKey); !ok {
		t.Fatal("DeleteWhere(true) deleted the system record")
	}
	// Len counts what a reader is shown, and the system record is not among that -- it is
	// the one thing Query above did not return and DeleteWhere did not remove. StoredLen is
	// where it is still counted, because it is still stored.
	if d.Len() != 0 {
		t.Errorf("Len = %d, want 0 (the system record is not a row a reader sees)", d.Len())
	}
	if d.StoredLen() != 1 {
		t.Errorf("StoredLen = %d, want 1 (the system record is still stored)", d.StoredLen())
	}

	// ForEachSystemAd yields exactly the system record.
	{
		seen := 0
		d.ForEachSystemAd(func(key string, ad *classad.ClassAd) bool {
			if !IsSystemKey(key) {
				t.Errorf("ForEachSystemAd exposed non-system key %q", key)
			}
			if key == sysKey {
				seen++
			}
			return true
		})
		if seen != 1 {
			t.Errorf("ForEachSystemAd yielded %d matching records, want 1", seen)
		}
	}

	// Durability: the system record survives a snapshot/restore round-trip.
	var snap bytes.Buffer
	if err := d.Snapshot(&snap); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := d.Restore(bytes.NewReader(snap.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, ok := d.LookupClassAd(sysKey); !ok {
		t.Fatal("system record lost across snapshot/restore")
	}
	sysAfter := 0
	d.ForEachSystemAd(func(key string, _ *classad.ClassAd) bool {
		if key == sysKey {
			sysAfter++
		}
		return true
	})
	if sysAfter != 1 {
		t.Errorf("after restore ForEachSystemAd yielded %d matching records, want 1", sysAfter)
	}
}
