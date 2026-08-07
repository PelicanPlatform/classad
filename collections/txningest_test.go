package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Txn.PutOld encodes old-ClassAd text straight to the stored wire form, so the buffered
// write holds bytes rather than an *classad.ClassAd. Everything that reads the buffer has
// to cope with that -- and anything that tests `ad != nil` silently skips the write instead
// of failing, so these tests exercise each reader deliberately.

// putOldText is one ad in the old-ClassAd form a socket delivers.
const putOldText = `ClusterId = 42
Owner = "alice"
JobStatus = 2
RequestMemory = 2048
Ratio = 1.5
Held = false
Args = {1, 2, 3}
Meta = [ a = 1 ]
Doubled = RequestMemory * 2
Note = "a \"quoted\" word"`

// TestPutOldMatchesParsedPut is the equivalence this whole path rests on: an ad ingested as
// text must be stored exactly as the same ad parsed and Put would be, value for value.
func TestPutOldMatchesParsedPut(t *testing.T) {
	fast := New(Options{Shards: 2})
	defer fast.Close()
	slow := New(Options{Shards: 2})
	defer slow.Close()

	tx := fast.Begin()
	if !tx.PutOld([]byte("k1"), putOldText) {
		t.Fatal("PutOld declined a plain ad")
	}
	if res := tx.Commit(); len(res.Conflicts) != 0 {
		t.Fatalf("conflicts on a fresh key: %v", res.Conflicts)
	}

	ref, err := classad.ParseOld(putOldText)
	if err != nil {
		t.Fatal(err)
	}
	tx2 := slow.Begin()
	tx2.Put([]byte("k1"), ref)
	tx2.Commit()

	got, ok := fast.Get([]byte("k1"))
	if !ok {
		t.Fatal("ad missing after PutOld + Commit")
	}
	want, ok := slow.Get([]byte("k1"))
	if !ok {
		t.Fatal("ad missing from the reference collection")
	}
	if !got.Equal(want) {
		t.Errorf("wire-ingested ad differs from the parsed one:\n got  = %s\n want = %s",
			got.String(), want.String())
	}
	// The expression attribute must still evaluate against its sibling.
	if v, ok := got.EvaluateAttrNumber("Doubled"); !ok || v != 4096 {
		t.Errorf("Doubled = %v (ok=%v), want 4096", v, ok)
	}
}

// TestPutOldReadYourWrites covers the point lookup: a buffered wire write has to be visible
// to Get inside the same transaction, which means materializing it on demand.
func TestPutOldReadYourWrites(t *testing.T) {
	c := New(Options{Shards: 2})
	defer c.Close()

	tx := c.Begin()
	if !tx.PutOld([]byte("k1"), putOldText) {
		t.Fatal("PutOld declined")
	}
	ad, ok := tx.Get([]byte("k1"))
	if !ok {
		t.Fatal("Get inside the transaction did not see its own PutOld")
	}
	if owner, _ := ad.EvaluateAttrString("Owner"); owner != "alice" {
		t.Errorf("Owner = %q, want alice", owner)
	}
	tx.Commit()
}

// TestPutOldVisibleToTransactionalQuery covers the buffered-write overlay, which scans the
// transaction's writes rather than looking one up. It tested `ad != nil` and therefore
// skipped every wire write -- a query inside the transaction quietly returned fewer rows.
func TestPutOldVisibleToTransactionalQuery(t *testing.T) {
	c := New(Options{Shards: 2})
	defer c.Close()

	tx := c.Begin()
	for i := 0; i < 3; i++ {
		text := fmt.Sprintf("ClusterId = %d\nOwner = \"alice\"\nJobStatus = 2", i)
		if !tx.PutOld([]byte(fmt.Sprintf("k%d", i)), text) {
			t.Fatal("PutOld declined")
		}
	}
	q, err := vm.Parse(`Owner == "alice"`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	tx.forEachBufferedMatch(q, func(string, *classad.ClassAd) bool { n++; return true })
	if n != 3 {
		t.Errorf("the transactional query overlay saw %d buffered rows, want 3", n)
	}
	tx.Commit()
}

// TestPutOldComposesWithSetAttribute pins the read-modify-write inside one transaction: the
// wire write must materialize so the attribute is added to the ad it wrote, not to nothing.
func TestPutOldComposesWithSetAttribute(t *testing.T) {
	c := New(Options{Shards: 2})
	defer c.Close()

	tx := c.Begin()
	if !tx.PutOld([]byte("k1"), putOldText) {
		t.Fatal("PutOld declined")
	}
	ad, ok := tx.Get([]byte("k1"))
	if !ok {
		t.Fatal("Get did not see the PutOld")
	}
	ad.InsertAttr("Extra", 7)
	tx.Put([]byte("k1"), ad)
	tx.Commit()

	got, ok := c.Get([]byte("k1"))
	if !ok {
		t.Fatal("ad missing after commit")
	}
	if v, ok := got.EvaluateAttrNumber("Extra"); !ok || v != 7 {
		t.Errorf("Extra = %v (ok=%v), want 7", v, ok)
	}
	if owner, _ := got.EvaluateAttrString("Owner"); owner != "alice" {
		t.Errorf("Owner = %q, want the original alice -- the read-modify-write lost the base ad", owner)
	}
}

// TestPutOldConflictDetection checks that the wire path is still a transactional write: a
// key changed by another committer since this transaction's snapshot must conflict, not
// silently overwrite.
func TestPutOldConflictDetection(t *testing.T) {
	c := New(Options{Shards: 2})
	defer c.Close()

	base, err := classad.ParseOld("ClusterId = 1\nOwner = \"orig\"")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put([]byte("k1"), base); err != nil {
		t.Fatal(err)
	}

	tx := c.Begin()
	if _, ok := tx.Get([]byte("k1")); !ok { // take the snapshot
		t.Fatal("base ad missing")
	}
	if !tx.PutOld([]byte("k1"), "ClusterId = 1\nOwner = \"txn\"") {
		t.Fatal("PutOld declined")
	}
	// Another committer wins the race.
	other, _ := classad.ParseOld("ClusterId = 1\nOwner = \"other\"")
	if err := c.Put([]byte("k1"), other); err != nil {
		t.Fatal(err)
	}
	res := tx.Commit()
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflicts = %v, want the one key that raced", res.Conflicts)
	}
	got, _ := c.Get([]byte("k1"))
	if owner, _ := got.EvaluateAttrString("Owner"); owner != "other" {
		t.Errorf("Owner = %q, want other -- the conflicted wire write must not have applied", owner)
	}
}

// TestPutOldMaintainsOrderedIndex covers the other reader of the buffered object: ordered
// indexes are maintained from the ad at commit, so a wire write has to materialize there.
func TestPutOldMaintainsOrderedIndex(t *testing.T) {
	c := New(Options{
		Shards: 2,
		Ordered: []OrderSpec{{
			Partition: "Owner",
			Where:     "JobStatus == 1",
			Keys:      []SortKey{{Expr: "JobPrio", Desc: true}},
		}},
	})
	defer c.Close()

	tx := c.Begin()
	for i, prio := range []int{5, 20, 10} {
		text := fmt.Sprintf("Owner = \"alice\"\nJobStatus = 1\nJobPrio = %d\nJob = \"j%d\"", prio, i)
		if !tx.PutOld([]byte(fmt.Sprintf("j%d", i)), text) {
			t.Fatal("PutOld declined")
		}
	}
	tx.Commit()

	var got []string
	for r := range c.Ordered(0, classad.NewStringValue("alice"), OrderCursor{}) {
		j, _ := r.Ad.EvaluateAttrString("Job")
		got = append(got, j)
	}
	want := []string{"j1", "j2", "j0"} // JobPrio 20, 10, 5
	if len(got) != len(want) {
		t.Fatalf("ordered partition = %v, want %v -- wire writes must reach the ordered index", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ordered partition = %v, want %v", got, want)
			break
		}
	}
}

// TestPutOldDeclinesWhereItCannotBeFaithful pins the cases the caller must handle by
// parsing: an encrypted collection (the streaming encoder does not seal) and malformed
// text. Declining is the contract -- a silent wrong encoding would not be.
func TestPutOldDeclinesWhereItCannotBeFaithful(t *testing.T) {
	plain := New(Options{Shards: 1})
	defer plain.Close()
	tx := plain.Begin()
	if tx.PutOld([]byte("bad"), "this is not = = an ad {{{") {
		t.Error("PutOld accepted malformed text; it must decline so the caller reports the parse error")
	}
	tx.Commit()

	if !mmapSupported {
		t.Skip("encrypted collections need the persistent path")
	}
	_, dataKey := deriveDataKey(t)
	enc, err := Open(Options{
		Shards:         1,
		Dir:            t.TempDir(),
		DataKey:        dataKey,
		EncryptedAttrs: []string{"ClaimId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	etx := enc.Begin()
	if etx.PutOld([]byte("k1"), "ClaimId = \"secret\"\nOwner = \"alice\"") {
		t.Error("PutOld accepted an encrypted collection; the streaming encoder cannot seal")
	}
	etx.Commit()
}

// TestPutOldDuplicateAttributeDefers covers the shape the streaming encoder hands back to
// the reference parser: a repeated attribute name, where streaming would keep the first and
// the parser keeps the last. Either PutOld declines, or it stores the parser's answer --
// what it must not do is store the other one.
func TestPutOldDuplicateAttributeDefers(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()

	const dup = "Owner = \"first\"\nJobStatus = 1\nOwner = \"second\""
	tx := c.Begin()
	if !tx.PutOld([]byte("k1"), dup) {
		t.Skip("PutOld declined the duplicate outright, which is also correct")
	}
	tx.Commit()

	ref, err := classad.ParseOld(dup)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Get([]byte("k1"))
	if !ok {
		t.Fatal("ad missing")
	}
	if !got.Equal(ref) {
		t.Errorf("duplicate-attribute ad stored as %s, want the reference parser's %s",
			got.String(), ref.String())
	}
}
