package db

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// putAd is a test helper: store key=ad, failing on error.
func putAd(t *testing.T, d *DB, key, text string) {
	t.Helper()
	ad, err := classad.ParseOld(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	if err := d.Put(key, ad); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func TestDeleteWhereConstraint(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	putAd(t, d, "a", `Name = "a"`+"\n"+`State = "Idle"`)
	putAd(t, d, "b", `Name = "b"`+"\n"+`State = "Claimed"`)
	putAd(t, d, "c", `Name = "c"`+"\n"+`State = "Idle"`)

	n, err := d.DeleteWhere(`State == "Idle"`)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("DeleteWhere removed %d, want 2", n)
	}
	if d.Len() != 1 {
		t.Fatalf("Len = %d, want 1", d.Len())
	}
	if _, ok := d.LookupClassAd("b"); !ok {
		t.Fatal("Claimed ad b should have survived")
	}
	if _, ok := d.LookupClassAd("a"); ok {
		t.Fatal("Idle ad a should have been removed")
	}
}

// TestDeleteWhereExpiry expresses the collector's expiry sweep as a DeleteWhere
// constraint (now > LastHeardFrom + lifetime) and checks stale ads go while a
// fresh one stays -- the single-primitive expiry the collector backends use.
func TestDeleteWhereExpiry(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	const now = 1_000_000
	putAd(t, d, "stale1", `Name = "stale1"`+"\n"+`LastHeardFrom = 900000`+"\n"+`ClassAdLifetime = 60`) // 900060 < now
	putAd(t, d, "stale2", `Name = "stale2"`+"\n"+`LastHeardFrom = 999000`)                             // no lifetime -> default 900 -> 999900 < now
	putAd(t, d, "fresh", `Name = "fresh"`+"\n"+`LastHeardFrom = 999950`+"\n"+`ClassAdLifetime = 900`)  // 1000850 > now

	// The collector builds exactly this constraint each sweep.
	constraint := fmt.Sprintf(`%d > LastHeardFrom + ifThenElse(ClassAdLifetime =!= undefined, ClassAdLifetime, 900)`, now)
	n, err := d.DeleteWhere(constraint)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expiry removed %d, want 2 (the two stale ads)", n)
	}
	if _, ok := d.LookupClassAd("fresh"); !ok {
		t.Fatal("fresh ad should have survived expiry")
	}
}

func TestPutOverwriteAndDelete(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	putAd(t, d, "k", `Name = "k"`+"\n"+`V = 1`)
	putAd(t, d, "k", `Name = "k"`+"\n"+`V = 2`) // overwrite
	ad, ok := d.LookupClassAd("k")
	if !ok {
		t.Fatal("k missing after put")
	}
	if v, _ := ad.EvaluateAttrInt("V"); v != 2 {
		t.Fatalf("V = %d after overwrite, want 2", v)
	}

	present, err := d.Delete("k")
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("Delete reported k absent, want present")
	}
	if present, err := d.Delete("k"); err != nil || present {
		t.Fatalf("second Delete: present=%v err=%v, want false,nil", present, err)
	}
}

// TestDeleteWhereConcurrentWriters runs DeleteWhere while writers churn the match
// set, exercising the optimistic-retry / self-healing loop. It asserts the sweep
// converges (no error, no panic) and never removes an ad that does not match.
func TestDeleteWhereConcurrentWriters(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	const nAds = 400
	for i := 0; i < nAds; i++ {
		putAd(t, d, fmt.Sprintf("slot%d", i), fmt.Sprintf(`Name = "slot%d"`+"\n"+`State = "Idle"`, i))
	}

	var wg sync.WaitGroup
	// Writers flip some ads to Claimed (out of the match set) concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < nAds; i += 3 {
			putAd(t, d, fmt.Sprintf("slot%d", i), fmt.Sprintf(`Name = "slot%d"`+"\n"+`State = "Claimed"`, i))
		}
	}()
	// Concurrent sweep of the Idle ads.
	wg.Add(1)
	var sweepErr error
	go func() {
		defer wg.Done()
		_, sweepErr = d.DeleteWhere(`State == "Idle"`)
	}()
	wg.Wait()
	if sweepErr != nil {
		t.Fatalf("concurrent DeleteWhere: %v", sweepErr)
	}

	// A final sweep removes any Idle ads still present; afterwards no Idle ad
	// remains, and every surviving ad is Claimed (never wrongly deleted).
	if _, err := d.DeleteWhere(`State == "Idle"`); err != nil {
		t.Fatal(err)
	}
	d.ForEach(func(ad *classad.ClassAd) bool {
		if s, _ := ad.EvaluateAttrString("State"); s != "Claimed" {
			t.Fatalf("surviving ad has State=%q, want Claimed", s)
		}
		return true
	})
}
