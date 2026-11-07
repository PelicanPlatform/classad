package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The sidecar's exact-case run answers =?= and =!=, which do not fold case, beside the
// folded run that answers == and !=, which do. When every bucket is case-uniform and spelled
// in its folded form the two runs are the same bytes, and the exact one is written as a
// marker (exactRunMirrorsFolded). These tests hold the line that matters: the saving is
// taken ONLY where the runs really coincide, a corpus that spells one value three ways
// still gets case-sensitive answers, and a sidecar written before the marker existed is
// still read.

// ecFixture renders a job-like ad. Owner draws from spellings that differ only by case, so
// one folded bucket carries several exact spellings; Site is lower-case throughout, so its
// runs coincide. Two attributes in one corpus is the point: the decision is per attribute,
// and a corpus where one attribute mirrors and the other does not catches a writer that
// decides it for the whole segment.
type ecFixture struct {
	owners []string
	sites  []string
}

func (f ecFixture) adText(i int) string {
	return fmt.Sprintf(`[ ID=%d; Owner=%q; Site=%q; Memory=%d ]`,
		i, f.owners[i%len(f.owners)], f.sites[i%len(f.sites)], ((i%16)+1)*512)
}

// caseCollisions counts the folded values of f.owners that have more than one spelling. A
// fixture with none would make every =?= assertion below vacuous -- the exact run and the
// folded run would agree by accident -- so the tests assert on this before anything else.
func (f ecFixture) caseCollisions() int {
	byFold := map[string]map[string]bool{}
	for _, o := range f.owners {
		g := strings.ToLower(o)
		if byFold[g] == nil {
			byFold[g] = map[string]bool{}
		}
		byFold[g][o] = true
	}
	n := 0
	for _, spellings := range byFold {
		if len(spellings) > 1 {
			n++
		}
	}
	return n
}

// ecQueries are the four string operators over spellings that collide when folded, plus the
// probes that must find nothing. == and != fold; =?= and =!= do not.
var ecQueries = []string{
	`Owner == "alice"`,
	`Owner == "ALICE"`,
	`Owner != "alice"`,
	`Owner =?= "alice"`,
	`Owner =?= "Alice"`,
	`Owner =?= "ALICE"`,
	`Owner =?= "AlIcE"`, // spelled by nobody
	`Owner =!= "alice"`,
	`Owner =!= "Alice"`,
	`Owner =!= "bob"`,
	`Owner == "bob" && Memory > 4096`,
	`Site == "wisc"`,
	`Site == "WISC"`,
	`Site =?= "wisc"`,
	`Site =?= "WISC"`, // spelled by nobody
	`Site =!= "wisc"`,
	`Site != "osg"`,
}

// buildECArchive appends n ads of f to a fresh archive with Owner and Site categorical, with
// a small segment so several segments seal, and returns it with the source ads by ID.
func buildECArchive(t *testing.T, f ecFixture, n int, cat []string) (*Archive, map[int]*classad.ClassAd) {
	t.Helper()
	a, err := CreateArchive(ArchiveOptions{
		Dir: t.TempDir(), SegmentSize: 8 << 10,
		CategoricalAttrs: cat, ValueAttrs: []string{"Memory"},
	})
	if err != nil {
		t.Fatal(err)
	}
	src := map[int]*classad.ClassAd{}
	for i := 0; i < n; i++ {
		ad, perr := classad.Parse(f.adText(i))
		if perr != nil {
			t.Fatal(perr)
		}
		if aerr := a.Append(ad); aerr != nil {
			t.Fatal(aerr)
		}
		src[i] = ad
	}
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := a.SidecarSizes().Segments; got < 2 {
		t.Fatalf("%d sealed segments: a one-segment archive says nothing about the sealed layout", got)
	}
	return a, src
}

// ecMirrorCounts walks every sealed sidecar and reports, per categorical attribute name, how
// many of its blocks carry the mirror marker and how many write the run out. Reading the
// marker straight from the mapped bytes is deliberate: it is the only way to tell a block
// that took the saving from one that did not, since both answer queries the same way.
func ecMirrorCounts(t *testing.T, a *Archive) (mirrored, written map[string]int, version uint16) {
	t.Helper()
	mirrored, written = map[string]int{}, map[string]int{}
	spec := a.c.spec.Load()
	nameOf := func(id uint32) string {
		if spec != nil && spec.inline {
			if n, ok := spec.names[id]; ok {
				return strings.ToLower(n)
			}
		}
		n, _ := a.c.intern.Name(id)
		return strings.ToLower(n)
	}
	for _, sh := range a.c.shards {
		sh.mu.RLock()
		segs := slices.Clone(sh.segs)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil {
				continue
			}
			mm := seg.msidx.Load()
			if mm == nil {
				continue
			}
			version = mm.version
			for id, attrOff := range mm.catDir {
				name := nameOf(id)
				n, keyOffBase, _, _ := mm.catKeyBases(attrOff)
				exOff := keyOffBase + (n+1)*4 + n*4 + le32(mm.data, keyOffBase+n*4)
				if le32(mm.data, exOff) == exactRunMirrorsFolded {
					mirrored[name]++
				} else {
					written[name]++
				}
			}
		}
	}
	return mirrored, written, version
}

// ecCheckQueries runs every query against the archive and against a brute-force scan of the
// source ads, and reports any disagreement as rows, not bytes.
func ecCheckQueries(t *testing.T, a *Archive, src map[int]*classad.ClassAd, label string) {
	t.Helper()
	for _, qs := range ecQueries {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatalf("parse %q: %v", qs, err)
		}
		got := archiveQueryIDs(t, a, qs)
		want := bruteIDs(src, q)
		if !equalInts(got, want) {
			t.Errorf("%s: %q returned %d rows, want %d\n got=%v\nwant=%v",
				label, qs, len(got), len(want), got, want)
		}
	}
}

// TestSidecarExactRunMirrorIsPerAttribute: on a corpus whose Owner is spelled three ways and
// whose Site is lower-case throughout, Site's blocks take the marker and Owner's do not, and
// every string probe still answers exactly what a full scan answers.
func TestSidecarExactRunMirrorIsPerAttribute(t *testing.T) {
	t.Parallel()
	f := ecFixture{
		owners: []string{"alice", "Alice", "ALICE", "bob", "Carol", "dave"},
		sites:  []string{"wisc", "osg", "chtc"},
	}
	if n := f.caseCollisions(); n == 0 {
		t.Fatalf("fixture has no value spelled two ways: every =?= assertion here would be vacuous")
	}
	a, src := buildECArchive(t, f, 600, []string{"Owner", "Site"})
	defer a.Close()

	mirrored, written, version := ecMirrorCounts(t, a)
	if mirrored["owner"] != 0 {
		t.Errorf("Owner mirrors the folded run in %d sealed blocks, but it is spelled %d ways per bucket",
			mirrored["owner"], f.caseCollisions()+1)
	}
	if written["owner"] == 0 {
		t.Fatalf("no sealed Owner block at all (mirrored=%v written=%v): nothing was measured",
			mirrored, written)
	}
	if written["site"] != 0 || mirrored["site"] == 0 {
		t.Errorf("Site is lower-case throughout: want every block mirrored, got mirrored=%d written=%d",
			mirrored["site"], written["site"])
	}
	if version < 12 {
		t.Errorf("a sidecar carrying the mirror marker declares version %d; the marker is a v12 layout", version)
	}
	ecCheckQueries(t, a, src, "mixed-case corpus")
}

// TestSidecarExactRunMirrorAllLowerCase: a corpus with no upper case anywhere mirrors every
// categorical block, and still answers =?= and =!= as a full scan does -- including the
// probes carrying upper case, which must find nothing in a run of folded keys.
func TestSidecarExactRunMirrorAllLowerCase(t *testing.T) {
	t.Parallel()
	f := ecFixture{
		owners: []string{"alice", "bob", "carol", "dave"},
		sites:  []string{"wisc", "osg", "chtc"},
	}
	a, src := buildECArchive(t, f, 600, []string{"Owner", "Site"})
	defer a.Close()
	mirrored, written, _ := ecMirrorCounts(t, a)
	if len(written) != 0 {
		t.Errorf("every value is lower-case, yet %v blocks wrote the exact run out", written)
	}
	if mirrored["owner"] == 0 || mirrored["site"] == 0 {
		t.Fatalf("no mirrored block for an attribute (mirrored=%v): nothing was measured", mirrored)
	}
	ecCheckQueries(t, a, src, "all-lower-case corpus")
}

// TestSidecarExactRunMirrorSavesKeyBytes: the same corpus, capitalized. Capitalizing the
// first letter of every value leaves cardinality, record lengths, segment packing and every
// posting bitmap alone and changes exactly one thing -- the exact spellings stop matching
// their folded keys, so the marker cannot be used. The difference in sealed sidecar bytes is
// therefore the second copy of the keys and nothing else, which is what this measures.
func TestSidecarExactRunMirrorSavesKeyBytes(t *testing.T) {
	t.Parallel()
	lower := ecFixture{
		owners: []string{"alice", "bob", "carol", "dave"},
		sites:  []string{"wisc", "osg", "chtc"},
	}
	upper := ecFixture{owners: make([]string, len(lower.owners)), sites: make([]string, len(lower.sites))}
	capitalize := func(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
	for i, o := range lower.owners {
		upper.owners[i] = capitalize(o)
	}
	for i, s := range lower.sites {
		upper.sites[i] = capitalize(s)
	}

	const n = 600
	a, aSrc := buildECArchive(t, lower, n, []string{"Owner", "Site"})
	defer a.Close()
	b, bSrc := buildECArchive(t, upper, n, []string{"Owner", "Site"})
	defer b.Close()
	// Both arms answer what a full scan answers. The capitalized arm is the one that has to
	// be watched: every one of its buckets is case-uniform, so a detector that asked only
	// "one spelling per bucket?" would mirror it, and `Owner =?= "alice"` would then match
	// the rows spelled "Alice".
	ecCheckQueries(t, a, aSrc, "lower-case arm")
	ecCheckQueries(t, b, bSrc, "capitalized arm")

	mirrored, written, _ := ecMirrorCounts(t, a)
	if len(written) != 0 || len(mirrored) == 0 {
		t.Fatalf("lower-case arm did not mirror (mirrored=%v written=%v)", mirrored, written)
	}
	upMirrored, upWritten, _ := ecMirrorCounts(t, b)
	if len(upMirrored) != 0 || len(upWritten) == 0 {
		t.Fatalf("capitalized arm mirrored (mirrored=%v written=%v): the arms differ in more than the marker",
			upMirrored, upWritten)
	}
	as, bs := a.SidecarSizes(), b.SidecarSizes()
	if as.Segments != bs.Segments {
		t.Fatalf("arms sealed %d vs %d segments; they are not comparable", as.Segments, bs.Segments)
	}
	// Each written-out run costs its keys' text plus a key offset and a bitmap offset per
	// key, plus the trailing key offset and the count word. Every segment here sees every
	// value (the fixture cycles through them far faster than a segment fills), so this is
	// not an estimate: it is the byte count the two arms must differ by.
	var want int64
	for _, blocks := range []struct {
		vals []string
		n    int
	}{{lower.owners, upWritten["owner"]}, {lower.sites, upWritten["site"]}} {
		per := int64(4) // the trailing exactKeyOff[exactN]
		for _, v := range blocks.vals {
			per += int64(len(v)) + 8
		}
		want += per * int64(blocks.n)
	}
	got := bs.MappedBytes - as.MappedBytes
	if got <= 0 {
		t.Fatalf("mirroring saved %d bytes (lower=%d capitalized=%d): no saving at all",
			got, as.MappedBytes, bs.MappedBytes)
	}
	if got != want {
		t.Errorf("saving %d bytes, the key text and its offsets account for %d: the difference is not the second copy",
			got, want)
	}
	t.Logf("mirrored=%d capitalized=%d saved=%d bytes over %d segments (key text + offsets predict %d)",
		as.MappedBytes, bs.MappedBytes, got, as.Segments, want)
}

// ageSidecarsToV11 rewrites every sealed sidecar with the layout version the builder carried
// before the mirror marker existed. The bytes are already exactly what that builder wrote --
// the fallback branch is its serialization, unchanged -- so only the version word (and the
// CRC over it) has to move. It fails rather than lie if any block took the marker, since
// those bytes would be a v12 layout wearing a v11 version.
func ageSidecarsToV11(t *testing.T, a *Archive) int {
	t.Helper()
	c := a.c
	spec := c.spec.Load()
	aged := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := slices.Clone(sh.segs)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg.msidx.Load() == nil {
				continue
			}
			attr, err := buildSidecarIndex(buildSegIndex(c, seg, seg.used, spec))
			if err != nil {
				t.Fatal(err)
			}
			binary.LittleEndian.PutUint16(attr[4:], 11)
			binary.LittleEndian.PutUint32(attr[len(attr)-4:], crc32.Checksum(attr[:len(attr)-4], crcTable))
			container := buildSegmentSidecar(attr, c.segKeyIndex(seg), c.colBlobPreserve(seg),
				buildZoneBlob(c.zonesForSeg(sh, seg), c.intern.Name), seg.used, seg.dictRecOff(),
				seg.colRecOff(), encodeDamage(seg.damaged()))
			if !c.installSidecar(sh, seg, snapshotPath(seg), container) {
				t.Fatal("installSidecar failed")
			}
			mm := seg.msidx.Load()
			if mm == nil || mm.version != 11 {
				t.Fatal("aged sidecar not adopted as v11")
			}
			aged++
		}
	}
	return aged
}

// TestSidecarV11ExactRunStillRead: a sidecar from before the marker -- every exact spelling
// written out, including the ones whose text repeats their folded key -- is still read, and
// answers every probe exactly as the current layout does. The corpus is mixed-case so that
// the builder writes the full run for Owner, which is what a v11 builder wrote for every
// attribute; the archive is then aged to v11 and queried again.
func TestSidecarV11ExactRunStillRead(t *testing.T) {
	t.Parallel()
	f := ecFixture{
		owners: []string{"alice", "Alice", "ALICE", "bob", "carol", "dave"},
		sites:  []string{"wisc", "osg", "chtc"},
	}
	if f.caseCollisions() == 0 {
		t.Fatalf("fixture has no value spelled two ways: Owner would mirror and nothing v11-shaped would be written")
	}
	// Owner only: Site is lower-case and would take the marker, which a v11 sidecar cannot
	// carry.
	a, src := buildECArchive(t, f, 600, []string{"Owner"})
	defer a.Close()
	mirrored, written, version := ecMirrorCounts(t, a)
	if len(mirrored) != 0 {
		t.Fatalf("a block took the marker (%v): these bytes are not a v11 layout", mirrored)
	}
	if written["owner"] == 0 {
		t.Fatalf("no sealed Owner block (written=%v)", written)
	}
	if version < 12 {
		t.Fatalf("archive wrote version %d, want the current layout before aging", version)
	}
	want := map[string][]int{}
	for _, qs := range ecQueries {
		if strings.Contains(qs, "Site") {
			continue
		}
		want[qs] = archiveQueryIDs(t, a, qs)
	}

	if n := ageSidecarsToV11(t, a); n == 0 {
		t.Fatal("aged no sidecars")
	}
	_, agedWritten, agedVersion := ecMirrorCounts(t, a)
	if agedVersion != 11 {
		t.Fatalf("aged sidecar reports version %d, want 11", agedVersion)
	}
	if agedWritten["owner"] == 0 {
		t.Fatalf("aged sidecar has no written-out Owner run")
	}
	// The lower-case buckets' text lives in the v11 exact run, not in the folded one. Prove
	// the reader is searching that run: its key offsets are not the folded run's.
	for _, sh := range a.c.shards {
		sh.mu.RLock()
		segs := slices.Clone(sh.segs)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg.msidx.Load() == nil {
				continue
			}
			mm := seg.msidx.Load()
			for _, attrOff := range mm.catDir {
				_, foldedKeyOffBase, _, _ := mm.catKeyBases(attrOff)
				_, exKeyOffBase, _, _, _ := mm.catExactRun(attrOff)
				if exKeyOffBase == foldedKeyOffBase {
					t.Fatalf("v11 block resolved to the folded run; the exact run was not read")
				}
				if _, ok := mm.catFindExact(attrOff, "bob"); !ok {
					t.Errorf("v11 exact run does not hold the lower-case spelling %q it wrote out", "bob")
				}
			}
		}
	}

	for qs, w := range want {
		got := archiveQueryIDs(t, a, qs)
		if !equalInts(got, w) {
			t.Errorf("after aging to v11, %q returned %d rows, want %d\n got=%v\nwant=%v",
				qs, len(got), len(w), got, w)
		}
	}
	src2 := map[int]*classad.ClassAd{}
	for id, ad := range src {
		src2[id] = ad
	}
	for qs := range want {
		q, err := vm.Parse(qs)
		if err != nil {
			t.Fatal(err)
		}
		if got, brute := archiveQueryIDs(t, a, qs), bruteIDs(src2, q); !equalInts(got, brute) {
			t.Errorf("v11 %q: got %d rows, full scan says %d", qs, len(got), len(brute))
		}
	}
}

// TestIndexSizeExactCaseMirror measures what the exact-case run costs on a history-shaped
// archive keyed on GlobalJobId. Each arm differs from the one above it in one byte per key
// -- a capitalized host letter -- so cardinality, postings and sketches are identical and
// the whole difference between the arms is the run. The last arm capitalizes one host of
// eight: an attribute where most keys ARE their folded spelling but a minority are not,
// which the all-or-nothing marker deliberately does not help. Gated like the rest of the
// sizing harness.
func TestIndexSizeExactCaseMirror(t *testing.T) {
	if os.Getenv("CLASSAD_INDEXSIZE_MEASURE") == "" {
		t.Skip("set CLASSAD_INDEXSIZE_MEASURE=1 to run (builds four multi-segment archives)")
	}
	n := 200000
	if s := os.Getenv("CLASSAD_INDEXSIZE_N"); s != "" {
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
			t.Fatalf("CLASSAD_INDEXSIZE_N: %v", err)
		}
	}
	routeB := idxRoutes()[3]
	one := idxFixture{n: n, domFrac: 5, tailProcs: 5, domCluster: 1000000,
		hosts: []string{"submit-3.chtc.wisc.edu"}}
	oneMixed := one
	oneMixed.mixedHosts = 1
	eight := one
	eight.hosts = []string{
		"ap2001.chtc.wisc.edu", "ap2002.chtc.wisc.edu", "ap2003.chtc.wisc.edu",
		"submit-3.chtc.wisc.edu", "submit-4.chtc.wisc.edu", "ap40.uw.osg-htc.org",
		"ap21.uc.osg-htc.org", "login04.osgconnect.net"}
	eightOneMixed := eight
	eightOneMixed.mixedHosts = 1

	t.Logf("%-20s %9s %7s %14s %14s %16s %10s %12s", "corpus", "records", "segs", "sealedIdxB",
		"gjidMeta", "gjidB/record", "foldedKey%", "avgKeyLen")
	for _, f := range []struct {
		label string
		fx    idxFixture
	}{
		{"1 host, lower", one},
		{"1 host, capitalized", oneMixed},
		{"8 hosts, lower", eight},
		{"8 hosts, 1 capital", eightOneMixed},
	} {
		// How many of this corpus\'s keys are already spelled as their folded form, and how
		// long they are: the two numbers a per-key back-reference scheme would be sized on.
		folded, keyLen := 0, 0
		for i := 0; i < f.fx.n; i++ {
			var cluster, proc int
			if i%f.fx.domFrac == 0 {
				tj := i / f.fx.domFrac
				cluster, proc = 2000000+tj/f.fx.tailProcs, tj%f.fx.tailProcs
			} else {
				cluster, proc = f.fx.domCluster, i-i/f.fx.domFrac
			}
			k := f.fx.globalJobID(cluster, proc)
			keyLen += len(k)
			if k == strings.ToLower(k) {
				folded++
			}
		}
		r := buildIdxArchive(t, f.fx, routeB)
		p := r.bd.parts["GlobalJobId"]
		t.Logf("%-20s %9d %7d %14d %14d %16.2f %9.1f%% %12.1f", f.label, r.records, r.segments,
			r.sealedIdx, p.meta, float64(p.total())/float64(r.records),
			100*float64(folded)/float64(f.fx.n), float64(keyLen)/float64(f.fx.n))
	}
}
