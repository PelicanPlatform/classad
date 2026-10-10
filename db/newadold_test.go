package db

import (
	"testing"
)

// TestNewClassAdOldThenAmendKeepsTheAd: an ad created from old-ClassAd text and amended in the
// same transaction must commit whole. The dbrpc server creates ads through NewClassAdOld, so a
// client that creates and then sets an attribute in one transaction hit this; the commit used to
// store only the amended attribute, because the amendment was taken as a patch over a stored base
// that did not exist.
func TestNewClassAdOldThenAmendKeepsTheAd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		amend func(t *testing.T, tx *Txn)
		want  map[string]bool // attribute -> must be present
	}{
		{"set", func(t *testing.T, tx *Txn) {
			if err := tx.SetAttribute("1.0", "ExitCode", "0"); err != nil {
				t.Fatal(err)
			}
		}, map[string]bool{"Owner": true, "JobStatus": true, "ClusterId": true, "ExitCode": true}},
		{"delete", func(t *testing.T, tx *Txn) {
			tx.DeleteAttribute("1.0", "JobStatus")
		}, map[string]bool{"Owner": true, "JobStatus": false, "ClusterId": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat, err := OpenCatalog(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer cat.Close()
			d, err := cat.CreateTable("jobs")
			if err != nil {
				t.Fatal(err)
			}
			tx := d.Begin()
			if !tx.NewClassAdOld("1.0", "Owner = \"alice\"\nJobStatus = 1\nClusterId = 1\n") {
				t.Fatal("NewClassAdOld declined a plain ad; the test would not exercise the wire-native path")
			}
			tc.amend(t, tx)
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			ad, ok := d.LookupClassAd("1.0")
			if !ok {
				t.Fatal("ad missing after commit")
			}
			for attr, present := range tc.want {
				if _, got := ad.Lookup(attr); got != present {
					t.Errorf("%s present=%v, want %v; committed ad: %s", attr, got, present, ad)
				}
			}
		})
	}
}
