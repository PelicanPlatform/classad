package collections

import "testing"

// TestComputedColumnUnderWidenedPolicy: a computed value written into its column before the policy
// sealed an attribute it reads is a function of that attribute's plaintext. MemDouble = RequestMemory * 2
// is computed into MemDouble's slot; once RequestMemory is sealed, a reader not entitled to sealed values
// must read MemDouble as its expression evaluates without RequestMemory -- undefined -- through every
// path: the counts, the value zones that prune on the column, the aggregates, and the evaluating
// projection, which substitutes computed values for an entitled reader only. After a migration certifies
// the store, nothing derived from RequestMemory is computed any more.
func TestComputedColumnUnderWidenedPolicy(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	opts := sealedParityOpts(t.TempDir())
	opts.EncryptedAttrs = nil
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sealedPut(t, c, 0, sealedParityN)
	internAll(c)
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	memID, _ := c.intern.LookupID("MemDouble")
	inColumn := func() int {
		n := 0
		for _, sh := range c.shards {
			sh.mu.RLock()
			for _, seg := range sh.segs {
				if seg == nil {
					continue
				}
				cs := seg.colblk.Load()
				if cs == nil {
					continue
				}
				for _, b := range cs.blocks {
					idx, ok := b.schema.byID[memID]
					if !ok {
						continue
					}
					if cb := b.computedBits(idx); cb != nil {
						for k := 0; k < b.n; k++ {
							if testBit(cb, k) {
								n++
							}
						}
					}
				}
			}
			sh.mu.RUnlock()
		}
		return n
	}
	if inColumn() == 0 {
		t.Fatal("fixture: no MemDouble value is computed into its column")
	}
	projected := func(redact bool) (defined int) {
		proj := c.QueryProject
		if redact {
			proj = c.QueryProjectRedacted
		}
		for row := range proj(mustParseQ(t, "ProcId >= 0"), []string{"MemDouble"}) {
			if !row[0].IsUndefined() {
				defined++
			}
		}
		return defined
	}
	if n := projected(true); n == 0 {
		t.Fatal("fixture: the redacted projection reads no MemDouble before the widen")
	}
	if err := c.SetEncryptedAttrs([]string{"RequestMemory"}); err != nil {
		t.Fatal(err)
	}
	if n := projected(true); n != 0 {
		t.Errorf("after the widen the redacted projection read %d MemDouble values", n)
	}
	if n := projected(false); n == 0 {
		t.Error("after the widen the entitled projection reads no MemDouble")
	}
	for _, f := range []string{"MemDouble > 2000", "MemDouble > 1000000", "MemDouble =!= undefined"} {
		q := mustParseQ(t, f)
		if n, ok := c.vectorEvalCountAs(q, true); ok && n != 0 {
			t.Errorf("after the widen vectorEvalCount(redact)(%s) = %d, want 0", f, n)
		}
		if n, ok := c.CountQueryRedacted(q); ok && n != 0 {
			t.Errorf("after the widen CountQueryRedacted(%s) = %d, want 0", f, n)
		}
	}
	// Grouped aggregates read a computed field through the constraint, a per-aggregate FILTER or a
	// filtered COUNT(*): each must count no record by a computed MemDouble.
	groups := []GroupSpec{{Attr: "JobStatus"}}
	if gs, ok := c.GroupStatsConstraintAggsRedacted("MemDouble > 2000", groups, []GroupAggCol{{Attr: "ProcId"}}, nil); ok {
		for _, g := range gs {
			if g.Count != 0 {
				t.Errorf("after the widen a redacted GROUP BY constrained on MemDouble counted %d records", g.Count)
			}
		}
	}
	if gs, ok := c.GroupStatsAllAggsRedacted(groups, []GroupAggCol{{Attr: "ProcId", Filter: "MemDouble > 2000"}},
		[]string{"MemDouble > 2000"}); ok {
		for _, g := range gs {
			if len(g.Stats) > 0 && g.Stats[0].N != 0 {
				t.Errorf("after the widen a redacted aggregate FILTERed on MemDouble read %d values", g.Stats[0].N)
			}
			if len(g.CountOf) > 0 && g.CountOf[0] != 0 {
				t.Errorf("after the widen a redacted COUNT(*) FILTERed on MemDouble counted %d records", g.CountOf[0])
			}
		}
	}
	if c.MigrateSealedAttrs(0) == 0 {
		t.Fatal("the migration rewrote nothing")
	}
	if !c.computedTrusted() {
		t.Fatal("a clean migration did not certify the store")
	}
	if n := inColumn(); n != 0 {
		t.Errorf("after the migration %d MemDouble values derived from the sealed RequestMemory are in the column", n)
	}
	if n := projected(true); n != 0 {
		t.Errorf("after the migration the redacted projection read %d MemDouble values", n)
	}
	if n := projected(false); n == 0 {
		t.Error("after the migration the entitled projection reads no MemDouble")
	}
}
