package db

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func TestSetPrimaryKeyPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPrimaryKey([]string{"Day", "Owner"}); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got := d2.PrimaryKey(); strings.Join(got, ",") != "Day,Owner" {
		t.Fatalf("primary key after reopen = %v, want [Day Owner]", got)
	}
}

// A later index change must not erase the declaration. saveIndexConfig rebuilds the whole
// persisted struct from live state on every save, and the collection has no notion of a
// primary key, so the declaration survives only because deriveIndexConfig carries it
// through. Without that it is dropped by the next AddIndex -- silently, and the table
// then refuses every INSERT ... SELECT. This is the test that fails if the threading goes.
func TestPrimaryKeySurvivesLaterIndexChange(t *testing.T) {
	dir := t.TempDir()
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPrimaryKey([]string{"Day", "Owner"}); err != nil {
		t.Fatal(err)
	}
	// Any operation that persists the index config; AddIndex is the common one.
	d.AddIndex([]string{"Owner"}, []string{"RequestMemory"})
	d.AddHotAttrs("RequestCpus")
	d.Close()

	d2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got := d2.PrimaryKey(); strings.Join(got, ",") != "Day,Owner" {
		t.Fatalf("primary key after an index change = %v, want [Day Owner]", got)
	}
}

func TestSetPrimaryKeyRefusals(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	cases := []struct {
		name string
		cols []string
		want string
	}{
		{"private attribute", []string{"Owner", "ClaimId"}, "private"},
		{"not an attribute name", []string{"COUNT(*)"}, "not a valid attribute name"},
		{"duplicate column", []string{"Owner", "owner"}, "named twice"},
		{"empty name", []string{"Owner", "  "}, "empty attribute name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := d.SetPrimaryKey(tc.cols)
			if err == nil {
				t.Fatalf("SetPrimaryKey(%v) succeeded, want a refusal", tc.cols)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SetPrimaryKey(%v) = %v, want it to mention %q", tc.cols, err, tc.want)
			}
		})
	}
}

func TestDeriveKeyUsesTheGroupingRelation(t *testing.T) {
	cols := []string{"Day", "Owner"}
	// 1 and 1.0 are ONE group, so they must be one key: the derivation borrows
	// GroupValueText precisely so that a grouped row's key matches its group.
	intDay := mustAd(t, "Day = 1\nOwner = \"alice\"")
	realDay := mustAd(t, "Day = 1.0\nOwner = \"alice\"")
	k1, err := DeriveKey(intDay, cols)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := DeriveKey(realDay, cols)
	if err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatalf("1 and 1.0 derived different keys: %q vs %q", k1, k2)
	}

	// The string "4" and the number 4 are NOT one group, so they must not be one key.
	numAd := mustAd(t, "Day = 4\nOwner = \"alice\"")
	strAd := mustAd(t, "Day = \"4\"\nOwner = \"alice\"")
	kn, err := DeriveKey(numAd, cols)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := DeriveKey(strAd, cols)
	if err != nil {
		t.Fatal(err)
	}
	if kn == ks {
		t.Fatalf("the number 4 and the string \"4\" derived the same key %q", kn)
	}
}

func TestDeriveKeyRefusesUndefinedColumn(t *testing.T) {
	ad := mustAd(t, "Owner = \"alice\"")
	_, err := DeriveKey(ad, []string{"Day", "Owner"})
	if err == nil {
		t.Fatal("DeriveKey with a missing key attribute succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "Day") || !strings.Contains(err.Error(), "undefined") {
		t.Fatalf("error %v should name the undefined column", err)
	}
}

func TestDeriveKeyRefusesWithoutDeclaration(t *testing.T) {
	ad := mustAd(t, "Owner = \"alice\"")
	if _, err := DeriveKey(ad, nil); err == nil {
		t.Fatal("DeriveKey with no declared key succeeded, want a refusal")
	}
}

// Two rows deriving one key would be written, counted as two, and leave one row behind.
// The batch is refused whole instead.
func TestDeriveKeysRefusesCollidingRows(t *testing.T) {
	ads := []*classad.ClassAd{
		mustAd(t, "Day = 1\nOwner = \"alice\"\nJobs = 10"),
		mustAd(t, "Day = 1\nOwner = \"alice\"\nJobs = 20"),
	}
	_, err := DeriveKeys(ads, []string{"Day", "Owner"})
	if err == nil {
		t.Fatal("DeriveKeys on two rows with one key succeeded, want a refusal")
	}
	for _, want := range []string{"rows 1 and 2", "coarser"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v should mention %q", err, want)
		}
	}
}

func TestDeriveKeysDistinctRows(t *testing.T) {
	ads := []*classad.ClassAd{
		mustAd(t, "Day = 1\nOwner = \"alice\""),
		mustAd(t, "Day = 1\nOwner = \"bob\""),
		mustAd(t, "Day = 2\nOwner = \"alice\""),
	}
	keys, err := DeriveKeys(ads, []string{"Day", "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("got %d keys, want 3", len(keys))
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("duplicate key %q in %v", k, keys)
		}
		seen[k] = true
	}
}
