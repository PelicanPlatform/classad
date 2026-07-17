package collections

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/RoaringBitmap/roaring/v2"
)

// TestReadIndexParity is the anchor for the shared planner logic: for every built segment,
// its in-RAM *segIndex and the *mmapSegIndex parsed from the sidecar it writes must return
// IDENTICAL covers / candidateOffsets / skipsPrefix / DNF-group results across a broad probe
// battery. If the two representations ever diverged (a skip the mmap tier got wrong, a
// candidate it missed), a sealed segment on the mmap tier would answer queries differently
// from the same data in RAM.
func TestReadIndexParity(t *testing.T) {
	t.Parallel()
	c := New(Options{
		Shards:           1,
		CategoricalAttrs: []string{"Owner", "State", "JobId"},
		ValueAttrs:       []string{"Memory", "Cpus"},
	})
	owners := []string{"alice", "bob", "carol", "dave"}
	states := []string{"Idle", "Running", "Held"}
	for i := 0; i < 8000; i++ { // JobId is unique -> high NDV -> trips the MPH in sealed segments
		ad := fmt.Sprintf(`[ Id=%d; Owner=%q; State=%q; JobId=%q; Memory=%d; Cpus=%d ]`,
			i, owners[i%len(owners)], states[i%len(states)], fmt.Sprintf("job-%d", i), (i%16+1)*512, i%8)
		if i%29 == 0 { // a Memory type exception
			ad = fmt.Sprintf(`[ Id=%d; Owner=%q; State=%q; JobId=%q; Memory="x"; Cpus=%d ]`,
				i, owners[i%len(owners)], states[i%len(states)], fmt.Sprintf("job-%d", i), i%8)
		}
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, ad)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	conj := []string{
		`Owner == "alice"`,
		`Owner != "bob"`,
		`Owner == "alice" || Owner == "carol"`,
		`Owner == "nobody"`, // bloom-absent -> skip
		`State == "Running" && Memory >= 4096`,
		`Memory > 2048 && Memory <= 6144`,
		`Memory >= 999999`, // out of range -> skip
		`Cpus == 4`,
		`JobId == "job-100"`,    // MPH path (present)
		`JobId == "job-999999"`, // MPH/absent
		`Owner isnt undefined`,
		`Owner is undefined`,
		`State == "Held" && Owner == "dave" && Cpus < 4`,
	}
	dnf := []string{
		`(Owner == "alice" && Memory >= 4096) || (State == "Held" && Cpus == 2)`,
		`(JobId == "job-1") || (JobId == "job-2") || (State == "Idle" && Memory < 1024)`,
	}

	segs := 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			live := seg.idx.Load()
			if live == nil {
				continue
			}
			path := filepath.Join(t.TempDir(), fmt.Sprintf("seg-%d.idx", seg.id))
			if err := writeSidecarIndex(path, live); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}
			data, closer, err := mapFile(path)
			if err != nil {
				t.Fatalf("map sidecar: %v", err)
			}
			mm, err := parseMmapSidecar(data)
			if err != nil {
				t.Fatalf("parse sidecar: %v", err)
			}
			segs++

			for _, qs := range conj {
				q, err := vm.Parse(qs)
				if err != nil {
					t.Fatalf("parse %q: %v", qs, err)
				}
				usable := c.planIndex(q.Probes())
				if live.covers(usable) != mm.covers(usable) {
					t.Errorf("seg %d %q: covers mismatch (live %v, mmap %v)", seg.id, qs, live.covers(usable), mm.covers(usable))
				}
				if live.skipsPrefix(usable) != mm.skipsPrefix(usable) {
					t.Errorf("seg %d %q: skipsPrefix mismatch (live %v, mmap %v)", seg.id, qs, live.skipsPrefix(usable), mm.skipsPrefix(usable))
				}
				if !bmEqual(live.candidateOffsets(usable), mm.candidateOffsets(usable)) {
					t.Errorf("seg %d %q: candidateOffsets mismatch", seg.id, qs)
				}
			}

			// catCanonicalValues must emit the same set from both tiers (matchmaking relies
			// on the complete distinct-value set per segment).
			for _, attr := range []string{"Owner", "State", "JobId"} {
				id, ok := c.intern.LookupID(attr)
				if !ok {
					continue
				}
				collect := func(ix readIndex) []string {
					var vs []string
					ix.catCanonicalValues(id, func(v string) bool { vs = append(vs, v); return true })
					sort.Strings(vs)
					return vs
				}
				lv, mv := collect(live), collect(mm)
				if len(lv) != len(mv) {
					t.Fatalf("seg %d %s: canonical count %d vs %d", seg.id, attr, len(lv), len(mv))
				}
				for i := range lv {
					if lv[i] != mv[i] {
						t.Errorf("seg %d %s: canonical[%d] %q vs %q", seg.id, attr, i, lv[i], mv[i])
					}
				}
			}

			for _, qs := range dnf {
				q, err := vm.Parse(qs)
				if err != nil {
					t.Fatalf("parse %q: %v", qs, err)
				}
				plan := q.ProbePlan()
				groups, prunable := c.planIndexGroups(plan)
				if !prunable {
					continue
				}
				if live.coversGroups(groups) != mm.coversGroups(groups) {
					t.Errorf("seg %d %q: coversGroups mismatch", seg.id, qs)
				}
				if !bmEqual(live.candidateOffsetsGroups(groups), mm.candidateOffsetsGroups(groups)) {
					t.Errorf("seg %d %q: candidateOffsetsGroups mismatch", seg.id, qs)
				}
			}
			_ = closer()
		}
	}
	if segs == 0 {
		t.Fatal("no segments compared")
	}
}

// TestSealedIndexBackings checks that both sealed-segment backings -- a file mmap and an
// anonymous mapping -- produce an index whose query results match the in-RAM segIndex. This
// is the step-4 substrate for flipping sealed segments to the mmap representation.
func TestSealedIndexBackings(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, CategoricalAttrs: []string{"Owner"}, ValueAttrs: []string{"Memory"}})
	for i := 0; i < 3000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)),
			mustAd(t, fmt.Sprintf(`[ Id=%d; Owner="u%d"; Memory=%d ]`, i, i%40, (i%16+1)*512))); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	queries := []string{`Owner == "u3"`, `Owner != "u3"`, `Memory >= 4096`, `Owner == "nope"`, `Memory >= 999999`}

	check := func(name string, mk func(si *segIndex) (*mmapSegIndex, func() error, error)) {
		segs := 0
		for _, sh := range c.shards {
			for _, seg := range sh.segs {
				if seg == nil {
					continue
				}
				live := seg.idx.Load()
				if live == nil {
					continue
				}
				mm, closer, err := mk(live)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				for _, qs := range queries {
					q, _ := vm.Parse(qs)
					u := c.planIndex(q.Probes())
					if !bmEqual(live.candidateOffsets(u), mm.candidateOffsets(u)) {
						t.Errorf("%s seg %d %q: candidate mismatch", name, seg.id, qs)
					}
				}
				_ = closer()
				segs++
			}
		}
		if segs == 0 {
			t.Fatalf("%s: no segments", name)
		}
	}

	check("file", func(si *segIndex) (*mmapSegIndex, func() error, error) {
		return sealedIndexFromFile(filepath.Join(t.TempDir(), "s.idx"), si)
	})
	check("anon", sealedIndexAnon)
}

func bmEqual(a, b *roaring.Bitmap) bool {
	if a == nil {
		a = roaring.New()
	}
	if b == nil {
		b = roaring.New()
	}
	return a.Equals(b)
}
