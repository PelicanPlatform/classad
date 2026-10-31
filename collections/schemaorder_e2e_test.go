package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The fixture's attributes, written in this order so the first process's intern ids ascend with
// it: W, X, Y, Z are all int/4 under the first-derived schema, so that schema lays them out in
// exactly this order.
func orderAdText(i int) string {
	return fmt.Sprintf("[ Status=%d; W=%d; X=%d; Y=%d; Z=%d; Owner=\"user%d\"; Secret=\"tok-%d\" ]",
		i%6+1, 100000+i%1000, 200000+i%1000, 300000+i%1000, 400000+i%1000, i%7, i)
}

func orderAdWant(i int) map[string]int64 {
	return map[string]int64{
		"Status": int64(i%6 + 1),
		"W":      int64(100000 + i%1000),
		"X":      int64(200000 + i%1000),
		"Y":      int64(300000 + i%1000),
		"Z":      int64(400000 + i%1000),
	}
}

const orderAdCount = 3000

func checkOrderAds(t *testing.T, c *Collection, when string) int {
	t.Helper()
	bad := 0
	for i := range orderAdCount {
		ad, ok := c.Get([]byte(fmt.Sprintf("%d.0", i)))
		if !ok {
			t.Fatalf("%s: record %d missing", when, i)
		}
		for k, want := range orderAdWant(i) {
			got, ok := ad.EvaluateAttrInt(k)
			if !ok || got != want {
				bad++
				if bad <= 6 {
					t.Errorf("%s: rec %d %s = %d (ok=%v), want %d", when, i, k, got, ok, want)
				}
			}
		}
	}
	return bad
}

// TestStaleSegmentReadsAfterRederiveAndReopen is the production incident: a base-schema re-derive
// whose convergence is budgeted leaves two schemas on disk, and the next open reads the segments
// that still hold the OLD one under the NEW one's field order.
func TestStaleSegmentReadsAfterRederiveAndReopen(t *testing.T) {
	staleSegmentRederiveCase(t, nil)
}

// The same, with encryption at rest -- the configuration the affected archive runs under. An
// encrypted collection columnarizes exactly like any other (a sealed value is never a column), so
// its sealed segments carry their own schema in the segment the same way; this asserts that the
// decode is no more and no less correct there.
func TestStaleSegmentReadsAfterRederiveAndReopenEncrypted(t *testing.T) {
	_, dataKey := deriveDataKey(t)
	staleSegmentRederiveCase(t, func(o *Options) {
		o.DataKey = dataKey
		o.EncryptedAttrs = []string{"Secret"}
	})
}

func staleSegmentRederiveCase(t *testing.T, tune func(*Options)) {
	t.Helper()
	dir := t.TempDir()
	opts := func(budget int) Options {
		o := Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, ColumnarSegmentBudget: budget}
		if tune != nil {
			tune(&o)
		}
		return o
	}

	// --- process A: write and columnarize under the first-derived schema.
	c, err := Open(opts(0))
	if err != nil {
		t.Fatal(err)
	}
	for i := range orderAdCount {
		ad, err := classad.Parse(orderAdText(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c.RetrainDict(0)
	if !c.BuildAndEnableSchemaScan(4096, 4) {
		c.Close()
		t.Skip("schema scan did not enable")
	}
	for c.ColumnarizeSealed() > 0 {
	}
	if bad := checkOrderAds(t, c, "process A"); bad != 0 {
		t.Fatalf("process A: %d mismatches", bad)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// --- process B: adopt a re-derived schema that moves Z out of the int/4 group, and converge
	// ONE segment under it (the production state: a budgeted convergence, 843 segments behind).
	c2, err := Open(opts(1))
	if err != nil {
		t.Fatal(err)
	}
	st := c2.schemaScan.Load()
	if st == nil {
		c2.Close()
		t.Skip("no schema adopted on reopen")
	}
	fields := make([]adField, 0, len(st.schema.fields))
	for _, f := range st.schema.fields {
		nf := adField{id: f.id, kind: f.kind, width: f.width, unsigned: f.unsigned}
		if n, _ := c2.schemaFieldName(f.id); n == "Z" && nf.kind == akInt {
			nf.width = 2 // a width change, as the production re-derive made five of
		}
		fields = append(fields, nf)
	}
	if !c2.installSchemaScan(layoutSchema(fields), nil) {
		t.Fatal("install failed")
	}
	n := c2.recolumnarizeStale()
	t.Logf("converged %d segment(s)", n)
	if bad := checkOrderAds(t, c2, "process B"); bad != 0 {
		t.Errorf("process B: %d mismatches", bad)
	}
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}

	// --- process C: the next restart. Every segment must still read its own values.
	c3, err := Open(opts(0))
	if err != nil {
		t.Fatal(err)
	}
	defer c3.Close()
	if bad := checkOrderAds(t, c3, "process C"); bad != 0 {
		t.Errorf("process C (after reopen): %d of %d attribute reads wrong", bad, orderAdCount*5)
	}
}
