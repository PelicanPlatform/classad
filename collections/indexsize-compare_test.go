package collections

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Dedup-index route comparison. An HTCondor history archive needs a per-record
// "is this job already stored?" check. Two ways to index for it:
//
//	Route A: ValueAttrs{ClusterId, ProcId} + a composite over (ClusterId, ProcId).
//	         Dedup query: ClusterId == c && ProcId == p.
//	Route B: CategoricalAttrs{GlobalJobId}. Dedup query: GlobalJobId == "...".
//
// Both produce one distinct key per job. This measures what each costs on disk,
// against the baseline deployments actually run today (ClusterId alone).

// --- fixture -----------------------------------------------------------------

// idxFixture describes the generated history shape.
type idxFixture struct {
	n          int      // total records
	domFrac    int      // 1 record in domFrac is NOT in the dominant cluster
	tailProcs  int      // procs per ordinary (tail) cluster
	hosts      []string // submit hosts, assigned per cluster
	domCluster int
}

// globalJobID renders an HTCondor GlobalJobId: "<schedd>#<cluster>.<proc>#<QDate>".
// QDate is the cluster's submit time advanced in materialization waves of 1000 procs,
// so a big cluster's ids share a long prefix but not a single timestamp.
func (f idxFixture) globalJobID(cluster, proc int) string {
	host := f.hosts[cluster%len(f.hosts)]
	qdate := 1699000000 + cluster%100000 + (proc/1000)*37
	return fmt.Sprintf("%s#%d.%d#%d", host, cluster, proc, qdate)
}

// adAt renders record i: a plausible completed-job history ad. Records interleave the
// dominant cluster with a long tail of ordinary clusters, which is how a real history
// file is ordered (by completion, not by cluster).
func (f idxFixture) adAt(i int) string {
	var cluster, proc int
	if i%f.domFrac == 0 {
		// tail: ordinary clusters of f.tailProcs procs each
		t := i / f.domFrac
		cluster = 2000000 + t/f.tailProcs
		proc = t % f.tailProcs
	} else {
		cluster = f.domCluster
		proc = i - i/f.domFrac
	}
	gjid := f.globalJobID(cluster, proc)
	owner := fmt.Sprintf("user%d", cluster%37)
	qdate := 1699000000 + cluster%100000 + (proc/1000)*37
	return fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; GlobalJobId="%s"; Owner="%s"; JobStatus=4; `+
		`ExitCode=%d; ExitBySignal=false; QDate=%d; JobStartDate=%d; CompletionDate=%d; `+
		`RemoteWallClockTime=%d.0; RemoteUserCpu=%d.0; RequestCpus=1; RequestMemory=%d; `+
		`RequestDisk=%d; ImageSize=%d; ResidentSetSize=%d; JobUniverse=5; NumJobStarts=1; `+
		`Cmd="/home/%s/run.sh"; Iwd="/home/%s/work/sub%d"; MachineAttrGLIDEIN_Site0="Site%d"; `+
		`BytesSent=%d; BytesRecvd=%d ]`,
		cluster, proc, gjid, owner,
		i%3, qdate, qdate+60, qdate+60+(i%7200),
		i%7200, (i%7200)/2, 1024*(1+i%4), 1048576*(1+i%8), 2048*(1+i%16), 1024*(1+i%12),
		owner, owner, cluster%50, i%14,
		100000+i%900000, 50000+i%400000)
}

// --- on-disk index accounting -------------------------------------------------

// idxPart is one index's measured on-disk bytes inside the sealed attribute sidecars.
type idxPart struct {
	kind    string
	bitmaps int64 // length-prefixed roaring payloads this index owns
	meta    int64 // its block: keys blob, offset arrays, bloom, MPH, segStats
}

func (p idxPart) total() int64 { return p.bitmaps + p.meta }

// idxBreakdown is a whole archive's sealed-sidecar bytes, attributed per index.
type idxBreakdown struct {
	segments int
	total    int64 // == SidecarSizes().MappedBytes
	overhead int64 // header, "all" bitmap, directory, CRC footer
	parts    map[string]*idxPart
}

// sidecarBreakdown attributes every byte of every sealed attribute sidecar to an index
// (or to overhead), and fails the test if the attribution does not reconcile to the
// total. The reconciliation is the point: it proves the per-index numbers below are the
// whole on-disk cost and not a subset of it.
func sidecarBreakdown(t *testing.T, c *Collection) idxBreakdown {
	t.Helper()
	out := idxBreakdown{parts: map[string]*idxPart{}}
	spec := c.spec.Load()
	nameOf := func(id uint32) string {
		if spec != nil && spec.inline {
			if n, ok := spec.names[id]; ok {
				return n
			}
		}
		if n, ok := c.intern.Name(id); ok {
			return n
		}
		return fmt.Sprintf("attr#%d", id)
	}
	part := func(name, kind string) *idxPart {
		p, ok := out.parts[name]
		if !ok {
			p = &idxPart{kind: kind}
			out.parts[name] = p
		}
		return p
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			mm := seg.msidx.Load()
			if mm == nil {
				continue
			}
			out.segments++
			data := mm.data
			out.total += int64(len(data))
			metaOff := int(binary.LittleEndian.Uint32(data[6:]))

			bmLen := func(off uint32) int64 { return 4 + int64(binary.LittleEndian.Uint32(data[off:])) }
			var bmSum int64
			// The "all" bitmap plus the fixed header and the CRC footer are not any one
			// index's cost.
			out.overhead += 10 + 4 + bmLen(mm.allOff)
			bmSum += bmLen(mm.allOff)

			type blk struct {
				off  uint32
				name string
				kind string
			}
			var blks []blk
			catIDs := make([]uint32, 0, len(mm.catDir))
			for id := range mm.catDir {
				catIDs = append(catIDs, id)
			}
			sort.Slice(catIDs, func(i, j int) bool { return catIDs[i] < catIDs[j] })
			valIDs := make([]uint32, 0, len(mm.valDir))
			for id := range mm.valDir {
				valIDs = append(valIDs, id)
			}
			sort.Slice(valIDs, func(i, j int) bool { return valIDs[i] < valIDs[j] })

			// Categorical blocks: own bitmaps are the folded postings, the exact-case
			// postings (which may alias a folded one), the exception set and the posted set.
			for _, id := range catIDs {
				off := mm.catDir[id]
				name := nameOf(id)
				blks = append(blks, blk{off, name, "categorical"})
				p := part(name, "categorical")
				seen := map[uint32]bool{}
				add := func(o uint32) {
					if !seen[o] {
						seen[o] = true
						n := bmLen(o)
						p.bitmaps += n
						bmSum += n
					}
				}
				add(binary.LittleEndian.Uint32(data[off:]))   // excOff
				add(binary.LittleEndian.Uint32(data[off+4:])) // postedOff
				n := binary.LittleEndian.Uint32(data[off+8:])
				bmBase := off + 12 + 4*(n+1)
				for j := uint32(0); j < n; j++ {
					add(binary.LittleEndian.Uint32(data[bmBase+4*j:]))
				}
				blobLen := binary.LittleEndian.Uint32(data[off+12+4*n:]) // keyOffs[n]
				exAt := bmBase + 4*n + blobLen
				exN := binary.LittleEndian.Uint32(data[exAt:])
				exBase := exAt + 4 + 4*(exN+1)
				for j := uint32(0); j < exN; j++ {
					add(binary.LittleEndian.Uint32(data[exBase+4*j:]))
				}
			}
			for _, id := range valIDs {
				off := mm.valDir[id]
				name := nameOf(id)
				blks = append(blks, blk{off, name, "value"})
				p := part(name, "value")
				seen := map[uint32]bool{}
				add := func(o uint32) {
					if !seen[o] {
						seen[o] = true
						n := bmLen(o)
						p.bitmaps += n
						bmSum += n
					}
				}
				add(binary.LittleEndian.Uint32(data[off:]))
				add(binary.LittleEndian.Uint32(data[off+4:]))
				n := binary.LittleEndian.Uint32(data[off+8:])
				bmBase := off + 12 + 8*n
				for j := uint32(0); j < n; j++ {
					add(binary.LittleEndian.Uint32(data[bmBase+4*j:]))
				}
			}
			comps := append([]mmapComposite(nil), mm.comps...)
			sort.Slice(comps, func(i, j int) bool { return comps[i].spec.key < comps[j].spec.key })
			for _, mc := range comps {
				parts := make([]string, len(mc.spec.ids))
				for k, id := range mc.spec.ids {
					parts[k] = nameOf(id)
				}
				name := strings.Join(parts, "+")
				off := mc.off
				blks = append(blks, blk{off, name, "composite"})
				p := part(name, "composite")
				seen := map[uint32]bool{}
				add := func(o uint32) {
					if !seen[o] {
						seen[o] = true
						nb := bmLen(o)
						p.bitmaps += nb
						bmSum += nb
					}
				}
				add(binary.LittleEndian.Uint32(data[off:])) // excOff
				n := binary.LittleEndian.Uint32(data[off+12:])
				bmBase := off + 16 + 4*(n+1)
				for j := uint32(0); j < n; j++ {
					add(binary.LittleEndian.Uint32(data[bmBase+4*j:]))
				}
			}
			// Blocks are written in directory order (cat by id, val by id, comp by key)
			// and are contiguous, so each one's extent is the gap to the next.
			if len(blks) == 0 {
				continue
			}
			end := len(data) - 4 // CRC footer
			for i, b := range blks {
				stop := end
				if i+1 < len(blks) {
					stop = int(blks[i+1].off)
				}
				part(b.name, b.kind).meta += int64(stop) - int64(b.off)
			}
			out.overhead += int64(blks[0].off) - int64(metaOff) // the meta directory
			if got, want := int64(metaOff), int64(10)+bmSum; got != want {
				t.Fatalf("bitmap region did not reconcile: metaOff=%d, header+bitmaps=%d", got, want)
			}
		}
		sh.mu.RUnlock()
	}
	var sum int64
	for _, p := range out.parts {
		sum += p.total()
	}
	if sum+out.overhead != out.total {
		t.Fatalf("sidecar attribution did not reconcile: parts=%d overhead=%d sum=%d total=%d",
			sum, out.overhead, sum+out.overhead, out.total)
	}
	return out
}

// dirBytes is every byte the archive directory holds (records, sidecars, everything).
func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return n
}

// --- the comparison -----------------------------------------------------------

type idxRoute struct {
	name string
	opts ArchiveOptions
}

type idxResult struct {
	name      string
	records   int
	segments  int
	sealedIdx int64 // SidecarSizes().MappedBytes
	liveIdx   int64 // IndexSizes().TotalBytes (the active, unsealed tail)
	dataBytes int64
	dir       int64
	mph       int64 // of sealedIdx, minimal-perfect-hash structures
	bloom     int64 // of sealedIdx, categorical bloom filters
	bd        idxBreakdown
}

func idxRoutes() []idxRoute {
	return []idxRoute{
		{"baseline", ArchiveOptions{ValueAttrs: []string{"ClusterId"}}},
		{"+ProcId", ArchiveOptions{ValueAttrs: []string{"ClusterId", "ProcId"}}},
		{"RouteA", ArchiveOptions{
			ValueAttrs:     []string{"ClusterId", "ProcId"},
			CompositeAttrs: [][]string{{"ClusterId", "ProcId"}},
		}},
		{"RouteB", ArchiveOptions{
			ValueAttrs:       []string{"ClusterId"},
			CategoricalAttrs: []string{"GlobalJobId"},
		}},
	}
}

func buildIdxArchive(t *testing.T, f idxFixture, r idxRoute) idxResult {
	t.Helper()
	dir := t.TempDir()
	opts := r.opts
	opts.Dir = dir
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatalf("%s: create archive: %v", r.name, err)
	}
	for i := 0; i < f.n; i++ {
		ad, perr := classad.Parse(f.adAt(i))
		if perr != nil {
			t.Fatalf("%s: parse record %d: %v", r.name, i, perr)
		}
		if aerr := a.AppendDeferred(ad); aerr != nil {
			t.Fatalf("%s: append %d: %v", r.name, i, aerr)
		}
	}
	if err := a.Flush(); err != nil {
		t.Fatalf("%s: flush: %v", r.name, err)
	}
	a.c.Reindex()

	// Preconditions. Every one of these is a way the measurement could be silently
	// measuring nothing.
	if got := a.Count(); got != f.n {
		t.Fatalf("%s: record count %d, want %d", r.name, got, f.n)
	}
	sc := a.SidecarSizes()
	if sc.Segments < 4 {
		t.Fatalf("%s: only %d sealed segments; a one-segment archive measures nothing about a sealed archive", r.name, sc.Segments)
	}
	cat, val := a.IndexedAttrs()
	wantCat := map[string]bool{}
	for _, n := range opts.CategoricalAttrs {
		wantCat[strings.ToLower(n)] = true
	}
	gotCat := map[string]bool{}
	for _, n := range cat {
		gotCat[strings.ToLower(n)] = true
	}
	for n := range wantCat {
		if !gotCat[n] {
			t.Fatalf("%s: categorical index %q not configured; got %v", r.name, n, cat)
		}
	}
	gotVal := map[string]bool{}
	for _, n := range val {
		gotVal[strings.ToLower(n)] = true
	}
	for _, n := range opts.ValueAttrs {
		if !gotVal[strings.ToLower(n)] {
			t.Fatalf("%s: value index %q not configured; got %v", r.name, n, val)
		}
	}
	bd := sidecarBreakdown(t, a.c)
	// A composite whose component is not separately indexed is refused SILENTLY by
	// newCompositeSpecs, which would make Route A look free. Prove it is in the sealed
	// sidecars, not merely requested.
	if len(opts.CompositeAttrs) > 0 {
		found := false
		for name, p := range bd.parts {
			if p.kind == "composite" {
				found = true
				if !strings.EqualFold(name, "ClusterId+ProcId") {
					t.Fatalf("%s: unexpected composite %q", r.name, name)
				}
			}
		}
		if !found {
			t.Fatalf("%s: composite (ClusterId,ProcId) is NOT in the sealed sidecars", r.name)
		}
	}
	for _, n := range opts.CategoricalAttrs {
		if p, ok := bd.parts[n]; !ok || p.total() == 0 {
			t.Fatalf("%s: categorical %q contributes no sealed sidecar bytes", r.name, n)
		}
	}
	is := a.IndexSizes()
	res := idxResult{
		name: r.name, records: a.Count(), segments: sc.Segments,
		sealedIdx: sc.MappedBytes, liveIdx: is.TotalBytes, dataBytes: is.DataBytes,
		mph: sc.MPHBytes, bloom: sc.BloomBytes, bd: bd,
	}
	if err := a.Close(); err != nil {
		t.Fatalf("%s: close: %v", r.name, err)
	}
	res.dir = dirBytes(t, dir)
	return res
}

func reportIdxRoutes(t *testing.T, label string, f idxFixture) {
	t.Helper()
	results := make([]idxResult, 0, 4)
	all := idxRoutes()
	for i := range all {
		results = append(results, buildIdxArchive(t, f, all[i]))
	}
	base := results[0]
	t.Logf("\n===== %s =====", label)
	t.Logf("fixture: n=%d, dominant cluster %d holds %d records (%.1f%%), %d tail clusters of %d procs, hosts=%v",
		f.n, f.domCluster, f.n-f.n/f.domFrac, 100*float64(f.n-f.n/f.domFrac)/float64(f.n),
		(f.n/f.domFrac+f.tailProcs-1)/f.tailProcs, f.tailProcs, f.hosts)
	t.Logf("example GlobalJobId: %q (%d bytes)", f.globalJobID(f.domCluster, 123456),
		len(f.globalJobID(f.domCluster, 123456)))
	t.Logf("%-10s %9s %7s %14s %14s %14s %14s %10s %10s", "config", "records", "segs", "sealedIdxB", "liveIdxB", "dirBytes", "dataBytes", "mphB", "bloomB")
	for _, r := range results {
		t.Logf("%-10s %9d %7d %14d %14d %14d %14d %10d %10d", r.name, r.records, r.segments, r.sealedIdx, r.liveIdx, r.dir, r.dataBytes, r.mph, r.bloom)
	}
	// The categorical key blob is raw, uncompressed, sorted text, and the exact-case run
	// holds a SECOND full copy of every key (it reuses the folded bitmap, not the folded
	// text). So Route B's block is predicted by arithmetic on the raw strings alone -- no
	// prefix sharing enters it. Predicting it to within the per-segment sketches is what
	// says the "these strings compress hard" premise does not hold here.
	var wantKeyText int64
	for i := 0; i < f.n; i++ {
		var cluster, proc int
		if i%f.domFrac == 0 {
			tj := i / f.domFrac
			cluster, proc = 2000000+tj/f.tailProcs, tj%f.tailProcs
		} else {
			cluster, proc = f.domCluster, i-i/f.domFrac
		}
		wantKeyText += int64(2*len(f.globalJobID(cluster, proc))) + 16
	}
	gotMeta := results[3].bd.parts["GlobalJobId"].meta
	t.Logf("RouteB GlobalJobId block meta=%d; 2*rawKeyText+16B/key predicts %d (%.1f%%); residual %d = bloom+MPH+segStats",
		gotMeta, wantKeyText, 100*float64(wantKeyText)/float64(gotMeta), gotMeta-wantKeyText)
	if gotMeta < wantKeyText {
		t.Fatalf("GlobalJobId block (%d) is SMALLER than the raw key text it should hold twice (%d): the layout assumption is wrong", gotMeta, wantKeyText)
	}
	t.Logf("")
	t.Logf("%-10s %16s %16s %16s", "config", "marg sealedIdx", "marg dirBytes", "B/record(sealed)")
	for _, r := range results[1:] {
		d := r.sealedIdx - base.sealedIdx
		t.Logf("%-10s %16d %16d %16.2f", r.name, d, r.dir-base.dir, float64(d)/float64(r.records))
	}
	t.Logf("")
	for _, r := range results {
		names := make([]string, 0, len(r.bd.parts))
		for n := range r.bd.parts {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Logf("  [%s] sealed sidecar total=%d overhead=%d", r.name, r.bd.total, r.bd.overhead)
		for _, n := range names {
			p := r.bd.parts[n]
			t.Logf("    %-22s %-12s total=%10d  bitmaps=%10d  meta=%10d  (%.2f B/rec)",
				n, p.kind, p.total(), p.bitmaps, p.meta, float64(p.total())/float64(r.records))
		}
	}
}

// TestIndexSizeDedupRoutes measures the on-disk index cost of the two dedup-index routes
// against the deployed baseline, on a skewed history fixture.
func TestIndexSizeDedupRoutes(t *testing.T) {
	if os.Getenv("CLASSAD_INDEXSIZE_MEASURE") == "" {
		t.Skip("set CLASSAD_INDEXSIZE_MEASURE=1 to run (builds eight multi-segment archives)")
	}
	n := 200000
	if s := os.Getenv("CLASSAD_INDEXSIZE_N"); s != "" {
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			t.Fatalf("CLASSAD_INDEXSIZE_N: %v", err)
		}
	}
	single := idxFixture{n: n, domFrac: 5, tailProcs: 5, domCluster: 1000000,
		hosts: []string{"submit-3.chtc.wisc.edu"}}
	multi := single
	multi.hosts = []string{
		"ap2001.chtc.wisc.edu", "ap2002.chtc.wisc.edu", "ap2003.chtc.wisc.edu",
		"submit-3.chtc.wisc.edu", "submit-4.chtc.wisc.edu", "ap40.uw.osg-htc.org",
		"ap21.uc.osg-htc.org", "login04.osgconnect.net",
	}
	reportIdxRoutes(t, "single submit host", single)
	reportIdxRoutes(t, "eight submit hosts", multi)
}

// --- secondary: dedup-probe latency ------------------------------------------

// idxProbe is one dedup lookup expressed for every route.
type idxProbe struct {
	cluster, proc int
	gjid          string
}

// idxProbes draws lookups that hit existing records: half in the dominant cluster (where
// the baseline's ClusterId-only index has to consider every record of that cluster), half
// in the long tail.
func idxProbes(f idxFixture, m int) (dominant, tail []idxProbe) {
	domN := f.n - f.n/f.domFrac
	tailN := f.n / f.domFrac
	for i := 0; i < m; i++ {
		// The dominant cluster's procs run 1..domN (record 0 goes to the tail), so a
		// probe for proc 0 would match nothing and time an empty scan instead of a lookup.
		p := 1 + (i*7919)%domN
		dominant = append(dominant, idxProbe{f.domCluster, p, f.globalJobID(f.domCluster, p)})
		tj := (i * 7919) % tailN
		c := 2000000 + tj/f.tailProcs
		p2 := tj % f.tailProcs
		tail = append(tail, idxProbe{c, p2, f.globalJobID(c, p2)})
	}
	return dominant, tail
}

func (r idxRoute) constraintFor(p idxProbe) string {
	if len(r.opts.CategoricalAttrs) > 0 {
		return fmt.Sprintf(`GlobalJobId == %q`, p.gjid)
	}
	return fmt.Sprintf("ClusterId == %d && ProcId == %d", p.cluster, p.proc)
}

// TestIndexSizeDedupLatency times the dedup probe under each route. Timing is the
// SECONDARY question here; the machine is shared, so the test reports the spread across
// rounds and flips arm order between them rather than reporting a single number.
func TestIndexSizeDedupLatency(t *testing.T) {
	if os.Getenv("CLASSAD_INDEXSIZE_MEASURE") == "" {
		t.Skip("set CLASSAD_INDEXSIZE_MEASURE=1 to run (builds three multi-segment archives)")
	}
	n := 100000
	if s := os.Getenv("CLASSAD_INDEXSIZE_N"); s != "" {
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			t.Fatalf("CLASSAD_INDEXSIZE_N: %v", err)
		}
	}
	f := idxFixture{n: n, domFrac: 5, tailProcs: 5, domCluster: 1000000,
		hosts: []string{"submit-3.chtc.wisc.edu"}}
	routes := idxRoutes()
	routes = []idxRoute{routes[0], routes[2], routes[3]} // baseline, RouteA, RouteB

	type arm struct {
		route idxRoute
		a     *Archive
	}
	arms := make([]arm, 0, len(routes))
	for _, r := range routes {
		dir := t.TempDir()
		opts := r.opts
		opts.Dir = dir
		a, err := CreateArchive(opts)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		for i := 0; i < f.n; i++ {
			ad, perr := classad.Parse(f.adAt(i))
			if perr != nil {
				t.Fatalf("parse: %v", perr)
			}
			if aerr := a.AppendDeferred(ad); aerr != nil {
				t.Fatalf("append: %v", aerr)
			}
		}
		if err := a.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
		a.c.Reindex()
		if a.SidecarSizes().Segments < 4 {
			t.Fatalf("%s: %d sealed segments", r.name, a.SidecarSizes().Segments)
		}
		arms = append(arms, arm{r, a})
	}
	defer func() {
		for _, ar := range arms {
			_ = ar.a.Close() //nolint:errcheck // measurement harness teardown
		}
	}()

	const m = 200
	dom, tail := idxProbes(f, m)
	// Every probe must actually find its record: a dedup check that silently matches
	// nothing would time an empty scan.
	for _, ar := range arms {
		for _, set := range [][]idxProbe{dom[:5], tail[:5]} {
			for _, p := range set {
				q, err := vm.Parse(ar.route.constraintFor(p))
				if err != nil {
					t.Fatalf("parse constraint: %v", err)
				}
				got := 0
				for range ar.a.QueryLimit(q, 1) {
					got++
				}
				if got != 1 {
					t.Fatalf("%s: probe %+v found %d records, want 1", ar.route.name, p, got)
				}
			}
		}
	}

	run := func(ar arm, probes []idxProbe) float64 {
		qs := make([]*vm.Query, len(probes))
		for i, p := range probes {
			q, err := vm.Parse(ar.route.constraintFor(p))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			qs[i] = q
		}
		start := time.Now()
		for _, q := range qs {
			for range ar.a.QueryLimit(q, 1) {
				break
			}
		}
		return float64(time.Since(start).Microseconds()) / float64(len(probes))
	}

	const rounds = 4
	domT := map[string][]float64{}
	tailT := map[string][]float64{}
	for r := 0; r < rounds; r++ {
		order := make([]int, len(arms))
		for i := range order {
			order[i] = i
		}
		if r%2 == 1 { // flip arm order between rounds
			for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}
		}
		for _, i := range order {
			domT[arms[i].route.name] = append(domT[arms[i].route.name], run(arms[i], dom))
			tailT[arms[i].route.name] = append(tailT[arms[i].route.name], run(arms[i], tail))
		}
	}
	t.Logf("dedup probe latency, us/query, %d queries per round, %d rounds (arm order flipped each round)", m, rounds)
	t.Logf("%-10s %-10s %10s %10s %10s", "config", "set", "min", "median", "max")
	for _, ar := range arms {
		for _, pair := range []struct {
			label string
			s     []float64
		}{{"dominant", domT[ar.route.name]}, {"tail", tailT[ar.route.name]}} {
			s := append([]float64(nil), pair.s...)
			sort.Float64s(s)
			t.Logf("%-10s %-10s %10.1f %10.1f %10.1f", ar.route.name, pair.label, s[0], s[len(s)/2], s[len(s)-1])
		}
	}
}
