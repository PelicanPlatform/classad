package db

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func mustParse(t *testing.T, s string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// SetAttribute and DeleteAttribute buffer a PATCH instead of reading the stored ad, so the
// transaction now has to compose sequences that a read-modify-write got right for free. These
// pin the ones that broke. They run with delta records OFF, because the patch path is taken
// unconditionally -- a regression here hits every existing user, not just delta-mode tables.

func openMem(t *testing.T) *DB {
	t.Helper()
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestTxnDeleteThenSetSameAttribute(t *testing.T) {
	d := openMem(t)
	tx := d.Begin()
	tx.NewClassAd("1.0", mustParse(t, `[ A = 1; B = 2 ]`))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = d.Begin()
	tx.DeleteAttribute("1.0", "A")
	if err := tx.SetAttribute("1.0", "A", "99"); err != nil {
		t.Fatal(err)
	}
	// Read-your-writes must already show the re-set value.
	if v, ok := tx.LookupAttr("1.0", "A"); !ok || v != "99" {
		t.Fatalf("read-your-writes A = %q (present %v), want 99", v, ok)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ad, ok := d.LookupClassAd("1.0")
	if !ok {
		t.Fatal("key missing")
	}
	if v, _ := ad.EvaluateAttrInt("A"); v != 99 {
		t.Fatalf("A = %d, want 99: the delete was applied after the set", v)
	}
	if v, _ := ad.EvaluateAttrInt("B"); v != 2 {
		t.Fatalf("B = %d, want 2", v)
	}
}

func TestTxnDestroyThenSet(t *testing.T) {
	d := openMem(t)
	tx := d.Begin()
	tx.NewClassAd("2.0", mustParse(t, `[ Old = 1 ]`))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = d.Begin()
	tx.DestroyClassAd("2.0")
	if err := tx.SetAttribute("2.0", "New", "2"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ad, ok := d.LookupClassAd("2.0")
	if !ok {
		t.Fatal("key missing entirely; the SetAttribute should have re-created it")
	}
	if _, present := ad.Lookup("Old"); present {
		t.Fatal("Old survived a DestroyClassAd: the destroy was dropped")
	}
	if v, _ := ad.EvaluateAttrInt("New"); v != 2 {
		t.Fatalf("New = %d, want 2", v)
	}
}

func TestTxnDeleteThenSetDifferentAttribute(t *testing.T) {
	d := openMem(t)
	tx := d.Begin()
	tx.NewClassAd("3.0", mustParse(t, `[ A = 1; B = 2; C = 3 ]`))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx = d.Begin()
	tx.DeleteAttribute("3.0", "A")
	if err := tx.SetAttribute("3.0", "B", "42"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ad, _ := d.LookupClassAd("3.0")
	if _, present := ad.Lookup("A"); present {
		t.Fatal("A survived its delete")
	}
	if v, _ := ad.EvaluateAttrInt("B"); v != 42 {
		t.Fatalf("B = %d, want 42", v)
	}
	if v, _ := ad.EvaluateAttrInt("C"); v != 3 {
		t.Fatalf("C = %d, want 3 (untouched attribute lost)", v)
	}
}
