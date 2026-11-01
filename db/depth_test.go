package db

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// chain is the n-term expression `a0 || a1 || ...`, which reaches depth n-1: wire.MaxDepth+1 terms
// is the longest an attribute may hold.
func chain(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString(" || ")
		}
		fmt.Fprintf(&b, "a%d", i)
	}
	return b.String()
}

const (
	limitTerms = wire.MaxDepth + 1
	overTerms  = limitTerms + 1
)

func chainAd(t *testing.T, n int) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse("[Id = 1; Requirements = " + chain(n) + "]")
	if err != nil {
		t.Fatal(err)
	}
	return ad
}

// mustTooDeep fails unless err is the too-deep refusal, by every name it goes by, naming
// Requirements.
func mustTooDeep(t *testing.T, what string, err error) {
	t.Helper()
	for _, target := range []error{ErrTooDeep, collections.ErrTooDeep, wire.ErrTooDeep} {
		if !errors.Is(err, target) {
			t.Fatalf("%s: err = %v, want ErrTooDeep", what, err)
		}
	}
	var de *wire.DepthError
	if !errors.As(err, &de) || de.Attr != "Requirements" || de.Depth != overTerms-1 {
		t.Fatalf("%s: err = %v, want Requirements at depth %d", what, err, overTerms-1)
	}
}

// TestWritesRefuseTooDeep: every DB write path refuses a too-deep value as ErrTooDeep and leaves the
// key as it was; SetAttribute refuses at once, without buffering, and the rest of its transaction
// commits.
func TestWritesRefuseTooDeep(t *testing.T) {
	d, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if err = d.Put("k", chainAd(t, limitTerms)); err != nil {
		t.Fatalf("Put at the limit: %v", err)
	}
	err = d.Put("k", chainAd(t, overTerms))
	mustTooDeep(t, "Put", err)
	var ue *UnappliedError
	if !errors.As(err, &ue) || len(ue.Keys) != 1 || ue.Keys[0] != "k" || !strings.Contains(err.Error(), "too-deep") {
		t.Fatalf("Put: err = %v, want an UnappliedError for k saying too-deep", err)
	}
	if got, ok := d.LookupClassAd("k"); !ok || !strings.HasSuffix(mustLookup(t, got, "Requirements"), fmt.Sprintf("a%d)", limitTerms-1)) {
		t.Fatal("a refused Put changed k")
	}

	tx := d.Begin()
	tx.NewClassAd("j", chainAd(t, 2))
	mustTooDeep(t, "SetAttribute", tx.SetAttribute("j", "Requirements", chain(overTerms)))
	mustTooDeep(t, "SetAttribute (stored key)", tx.SetAttribute("k", "Requirements", chain(overTerms)))
	if err := tx.SetAttribute("j", "X", "1"); err != nil {
		t.Fatal(err)
	}
	if v, ok := tx.LookupAttr("j", "Requirements"); !ok || v != "(a0 || a1)" {
		t.Fatalf("a refused SetAttribute was buffered: Requirements = %q", v)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit after a refused SetAttribute: %v", err)
	}
	j, _ := d.LookupClassAd("j")
	if x, _ := j.EvaluateAttrInt("X"); x != 1 {
		t.Fatal("the transaction's other writes did not commit")
	}

	mustTooDeep(t, "UpdateOld", d.UpdateOld("o", "Id = 1\nRequirements = "+chain(overTerms)+"\n"))
	if _, ok := d.LookupClassAd("o"); ok {
		t.Fatal("a refused UpdateOld stored the ad")
	}
}

// TestArchiveAppendRefusesTooDeep: both archive append paths refuse a too-deep ad.
func TestArchiveAppendRefusesTooDeep(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	a, err := cat.CreateArchiveTable("history", ArchiveConfig{})
	if err != nil {
		t.Fatal(err)
	}
	mustTooDeep(t, "Append", a.Append(chainAd(t, overTerms)))
	mustTooDeep(t, "AppendOld", a.AppendOld("Id = 1\nRequirements = "+chain(overTerms)+"\n"))
	if err := a.AppendOld("Id = 1\nRequirements = " + chain(limitTerms) + "\n"); err != nil {
		t.Fatalf("AppendOld at the limit: %v", err)
	}
}

func mustLookup(t *testing.T, ad *classad.ClassAd, name string) string {
	t.Helper()
	e, ok := ad.Lookup(name)
	if !ok {
		t.Fatalf("no %s", name)
	}
	return e.String()
}
