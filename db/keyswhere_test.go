package db

import (
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestKeysWhere returns the storage keys of matching rows, including rows with no key attribute,
// and stops early when the caller breaks.
func TestKeysWhere(t *testing.T) {
	d, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	put := func(key, text string) {
		ad, err := classad.ParseOld(text)
		if err != nil {
			t.Fatal(err)
		}
		tx := d.Begin()
		tx.NewClassAd(key, ad)
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	put("0O.-1", `MyType = "Owner"`)
	put("0O.-2", `MyType = "Owner"`)
	put("1.0", `MyType = "Job"`)

	collect := func(constraint string) []string {
		seq, err := d.KeysWhere(constraint)
		if err != nil {
			t.Fatalf("KeysWhere(%q): %v", constraint, err)
		}
		var out []string
		for k := range seq {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}

	if got := collect(`MyType == "Owner"`); len(got) != 2 || got[0] != "0O.-1" || got[1] != "0O.-2" {
		t.Errorf("KeysWhere(Owner) = %v, want [0O.-1 0O.-2]", got)
	}
	if got := collect(`true`); len(got) != 3 {
		t.Errorf("KeysWhere(true) matched %d, want 3", len(got))
	}
	if got := collect(`MyType == "Nope"`); len(got) != 0 {
		t.Errorf("KeysWhere(no-match) = %v, want empty", got)
	}

	// A bad constraint errors eagerly (before any scan).
	if _, err := d.KeysWhere(`MyType ==`); err == nil {
		t.Error("KeysWhere with a malformed constraint should error")
	}

	// Early break stops the scan.
	seq, _ := d.KeysWhere(`true`)
	n := 0
	for range seq {
		n++
		break
	}
	if n != 1 {
		t.Errorf("early break visited %d keys, want 1", n)
	}
}
