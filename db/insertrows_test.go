package db

import (
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// rowCopyFixture builds a source table of job-shaped ads and an empty target keyed by key.
func rowCopyFixture(t *testing.T, n int, key ...string) (src, dst *DB) {
	t.Helper()
	src, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	tx := src.Begin()
	for i := 0; i < n; i++ {
		ad := mustAd(t, fmt.Sprintf(
			"Jid = %d\nOwner = %q\nRequestCpus = %d\nRequirements = RequestCpus > 1",
			i, fmt.Sprintf("u%d", i%3), i+1))
		tx.NewClassAd(fmt.Sprintf("job%d", i), ad)
	}
	if commitErr := tx.Commit(); commitErr != nil {
		t.Fatal(commitErr)
	}
	dst, err = Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	if len(key) > 0 {
		if keyErr := dst.SetPrimaryKey(key); keyErr != nil {
			t.Fatal(keyErr)
		}
	}
	return src, dst
}

// scan is the source sequence a copy reads, the way the server opens one.
func rowScan(t *testing.T, src *DB, constraint string) iter.Seq[*classad.ClassAd] {
	t.Helper()
	seq, err := src.Query(constraint)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestCopyWholeAds(t *testing.T) {
	src, dst := rowCopyFixture(t, 5, "Jid")
	n, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("copied %d rows, want 5", n)
	}
	if got := len(dst.Keys()); got != 5 {
		t.Fatalf("target holds %d rows, want 5", got)
	}
	// The key is the derived one, not the source's storage key.
	if _, ok := dst.LookupClassAd("3"); !ok {
		t.Fatalf("expected a row at the derived key 3, keys are %v", dst.Keys())
	}
}

// A copy is idempotent: the second run replaces the first run's rows rather than adding a
// second copy of each. This is the property the declared key exists to buy.
func TestCopyReRunConverges(t *testing.T) {
	src, dst := rowCopyFixture(t, 5, "Jid")
	for run := 1; run <= 2; run++ {
		n, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{}, nil)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if n != 5 {
			t.Fatalf("run %d copied %d rows, want 5", run, n)
		}
		if got := len(dst.Keys()); got != 5 {
			t.Fatalf("after run %d the target holds %d rows, want 5", run, got)
		}
	}
}

func TestCopyProjectionRenames(t *testing.T) {
	src, dst := rowCopyFixture(t, 3, "Id")
	n, err := dst.CopyRowsInto(rowScan(t, src, "true"),
		CopySpec{Attrs: []string{"Jid", "Owner"}, Names: []string{"Id", "User"}},
		CopyLimits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("copied %d rows, want 3", n)
	}
	ad, ok := dst.LookupClassAd("1")
	if !ok {
		t.Fatalf("no row at derived key 1, keys are %v", dst.Keys())
	}
	if v := ad.EvaluateAttr("User"); !v.IsString() {
		t.Fatalf("User is %v, want a string", v)
	}
	// Only the projected columns are stored.
	if ad.Size() != 2 {
		t.Fatalf("row has %d attributes, want 2", ad.Size())
	}
	if _, present := ad.Lookup("RequestCpus"); present {
		t.Fatal("RequestCpus was not projected but is present")
	}
}

// A projection copies an attribute AS STORED. Evaluating it instead would flatten a stored
// expression into whatever it happened to mean with the other columns dropped, which is the
// copy quietly rewriting the data it was asked to move.
func TestCopyKeepsStoredExpressions(t *testing.T) {
	src, dst := rowCopyFixture(t, 2, "Jid")
	n, err := dst.CopyRowsInto(rowScan(t, src, "true"),
		CopySpec{Attrs: []string{"Jid", "Requirements"}, Names: []string{"Jid", "Requirements"}},
		CopyLimits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("copied %d rows, want 2", n)
	}
	ad, ok := dst.LookupClassAd("0")
	if !ok {
		t.Fatal("no row at derived key 0")
	}
	expr, present := ad.Lookup("Requirements")
	if !present {
		t.Fatal("Requirements missing")
	}
	// RequestCpus was NOT projected, so an evaluating copy would have stored `false` or an
	// error. The expression survived instead.
	if got := expr.String(); !strings.Contains(got, "RequestCpus") {
		t.Fatalf("Requirements stored as %q, want the expression as written", got)
	}
}

// SELECT * and a projection naming every column must agree, which is only true because the
// projection does not evaluate.
func TestCopyProjectionIsSubsetOfStar(t *testing.T) {
	src, star := rowCopyFixture(t, 3, "Jid")
	if _, err := star.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{}, nil); err != nil {
		t.Fatal(err)
	}
	named, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = named.Close() })
	if err := named.SetPrimaryKey([]string{"Jid"}); err != nil {
		t.Fatal(err)
	}
	cols := []string{"Jid", "Owner", "RequestCpus", "Requirements"}
	if _, err := named.CopyRowsInto(rowScan(t, src, "true"),
		CopySpec{Attrs: cols, Names: cols}, CopyLimits{}, nil); err != nil {
		t.Fatal(err)
	}
	for _, k := range star.Keys() {
		a, _ := star.LookupClassAd(k)
		b, ok := named.LookupClassAd(k)
		if !ok {
			t.Fatalf("key %s missing from the named projection", k)
		}
		for _, c := range cols {
			av, _ := a.Lookup(c)
			bv, _ := b.Lookup(c)
			if av.String() != bv.String() {
				t.Fatalf("key %s attribute %s: star has %q, projection has %q",
					k, c, av.String(), bv.String())
			}
		}
	}
}

func TestCopyRefusesWithoutPrimaryKey(t *testing.T) {
	src, dst := rowCopyFixture(t, 2)
	_, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{}, nil)
	if err == nil || !strings.Contains(err.Error(), "declares no primary key") {
		t.Fatalf("error = %v, want a no-primary-key refusal", err)
	}
}

// The key that does not identify a source row is the silent-loss case: N rows written, one
// surviving, counted as N.
func TestCopyRefusesCollidingKeys(t *testing.T) {
	src, dst := rowCopyFixture(t, 6, "Owner") // 6 jobs over 3 owners
	_, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{}, nil)
	if err == nil || !strings.Contains(err.Error(), "both derive primary key") {
		t.Fatalf("error = %v, want a colliding-key refusal", err)
	}
	if got := len(dst.Keys()); got != 0 {
		t.Fatalf("target holds %d rows after a refused copy, want 0", got)
	}
}

func TestCopyRefusesProjectionMissingTheKey(t *testing.T) {
	src, dst := rowCopyFixture(t, 3, "Jid")
	_, err := dst.CopyRowsInto(rowScan(t, src, "true"),
		CopySpec{Attrs: []string{"Owner"}, Names: []string{"Owner"}}, CopyLimits{}, nil)
	if err == nil || !strings.Contains(err.Error(), "do not include Jid") {
		t.Fatalf("error = %v, want a key-coverage refusal naming Jid", err)
	}
}

// SELECT * cannot be checked for key coverage statically -- what an ad carries is known per
// row -- so the per-row derivation has to catch it.
func TestCopyStarRefusesRowMissingTheKey(t *testing.T) {
	src, dst := rowCopyFixture(t, 2, "Absent")
	_, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{}, nil)
	if err == nil || !strings.Contains(err.Error(), `"Absent" is undefined`) {
		t.Fatalf("error = %v, want an undefined-key refusal", err)
	}
	if got := len(dst.Keys()); got != 0 {
		t.Fatalf("target holds %d rows after a refused copy, want 0", got)
	}
}

func TestCopyRefusesPastRowLimit(t *testing.T) {
	src, dst := rowCopyFixture(t, 10, "Jid")
	_, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{MaxRows: 4}, nil)
	if err == nil || !strings.Contains(err.Error(), "more than 4 rows match") {
		t.Fatalf("error = %v, want a row-limit refusal", err)
	}
	if got := len(dst.Keys()); got != 0 {
		t.Fatalf("target holds %d rows after a refused copy, want 0", got)
	}
}

func TestCopyRefusesPastByteLimit(t *testing.T) {
	src, dst := rowCopyFixture(t, 10, "Jid")
	_, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{MaxBytes: 700}, nil)
	if err == nil || !strings.Contains(err.Error(), "would hold about") {
		t.Fatalf("error = %v, want a byte-limit refusal", err)
	}
	if got := len(dst.Keys()); got != 0 {
		t.Fatalf("target holds %d rows after a refused copy, want 0", got)
	}
}

// A cancelled copy writes nothing rather than committing however far it reached.
func TestCopyCancelledWritesNothing(t *testing.T) {
	src, dst := rowCopyFixture(t, 10, "Jid")
	seen := 0
	_, err := dst.CopyRowsInto(rowScan(t, src, "true"), CopySpec{}, CopyLimits{}, func() bool {
		seen++
		return seen > 3
	})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("error = %v, want a cancellation", err)
	}
	if got := len(dst.Keys()); got != 0 {
		t.Fatalf("target holds %d rows after a cancelled copy, want 0", got)
	}
}

func TestCopyEmptySourceWritesNothing(t *testing.T) {
	src, dst := rowCopyFixture(t, 3, "Jid")
	n, err := dst.CopyRowsInto(rowScan(t, src, "Owner == \"nobody\""), CopySpec{}, CopyLimits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("copied %d rows from an empty scan, want 0", n)
	}
}

func TestCopyConstraintSelectsRows(t *testing.T) {
	src, dst := rowCopyFixture(t, 9, "Jid") // owners u0, u1, u2 round-robin
	n, err := dst.CopyRowsInto(rowScan(t, src, `Owner == "u1"`), CopySpec{}, CopyLimits{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("copied %d rows, want 3", n)
	}
}

func TestCopySpecValidate(t *testing.T) {
	cases := []struct {
		name string
		spec CopySpec
		want string
	}{
		{"star takes no names", CopySpec{Names: []string{"A"}}, "takes no column names"},
		{"length mismatch", CopySpec{Attrs: []string{"A", "B"}, Names: []string{"A"}}, "but 1 target names"},
		{"bad target", CopySpec{Attrs: []string{"A"}, Names: []string{"COUNT(*)"}}, "needs an alias"},
		{"bad source", CopySpec{Attrs: []string{"a+b"}, Names: []string{"A"}}, "not a valid attribute name"},
		{"duplicate target", CopySpec{Attrs: []string{"A", "B"}, Names: []string{"X", "x"}}, "named twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if err := (CopySpec{}).Validate(); err != nil {
		t.Fatalf("SELECT * should validate: %v", err)
	}
}

// A private target name is refused: a projection renames, so it is the one shape that can
// move a value out from under the private-attribute convention in a single line.
func TestCopyRefusesPrivateTargetName(t *testing.T) {
	err := CopySpec{Attrs: []string{"A"}, Names: []string{"ClaimId"}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "private attribute") {
		t.Fatalf("error = %v, want a private-attribute refusal", err)
	}
}

func TestCheckSealedCopy(t *testing.T) {
	cases := []struct {
		name     string
		src, dst []string
		cols     map[string]string
		want     string
	}{
		{"nothing sealed", nil, nil, nil, ""},
		{"star, sealed both ends", []string{"Secret"}, []string{"Secret"}, nil, ""},
		{"star, bare target", []string{"Secret"}, nil, nil, "Secret is encrypted at rest"},
		{"column not copied", []string{"Secret"}, nil, map[string]string{"other": "Other"}, ""},
		{"copied under its own name", []string{"Secret"}, nil,
			map[string]string{"secret": "Secret"}, "Secret is encrypted"},
		{"renamed out from under the seal", []string{"Secret"}, []string{"Secret"},
			map[string]string{"secret": "Plain"}, "Secret (as Plain)"},
		{"renamed into a sealed column", []string{"Secret"}, []string{"Plain"},
			map[string]string{"secret": "Plain"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSealedCopy(tc.src, tc.dst, tc.cols)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestCopyLimitsDefaults(t *testing.T) {
	got := CopyLimits{}.withDefaults()
	if got != DefaultCopyLimits() {
		t.Fatalf("zero limits = %+v, want the defaults %+v", got, DefaultCopyLimits())
	}
	// A caller's own limit survives; the other is filled.
	got = CopyLimits{MaxRows: 7}.withDefaults()
	if got.MaxRows != 7 || got.MaxBytes != DefaultCopyLimits().MaxBytes {
		t.Fatalf("limits = %+v, want MaxRows 7 and the default MaxBytes", got)
	}
}
