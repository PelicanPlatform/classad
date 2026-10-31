package collections

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// payloadFixtureOpts are the options every payload-version test opens its store with. Group schemas
// are on, so the payloads carry group blocks as well as base blocks: the version covers both.
func payloadFixtureOpts(dir string) Options {
	// No remnant dictionary: these cases are about versions 0 and 1, whatever the default is. A payload
	// that carries one is version 2, covered by the remnant dictionary tests.
	return Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: 4, GroupStabilityRuns: 1,
		RemnantDictionary: -1}
}

// payloadFixtureAds is how many ads the fixture writes: enough for several sealed segments.
const payloadFixtureAds = 2000

// payloadFixture writes a store whose sealed segments are columnarized, closes it, and returns every
// ad it held as read BEFORE columnarization -- the whole-record form, which is the reference every
// later read is checked against. It fails rather than skips when nothing columnarizes: a payload the
// build cannot read back is refused at columnarization, so a skip would hide exactly that.
func payloadFixture(t *testing.T, dir string) []string {
	t.Helper()
	c, err := Open(payloadFixtureOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := range payloadFixtureAds {
		lead := ""
		if i < 3 {
			lead = fmt.Sprintf("Aardvark=%d; ", i) // below the presence floor: shifts the id space
		}
		cid := fmt.Sprintf("%d", i)
		if i%50 == 1 {
			cid = `"cluster-as-text"` // a type exception, so it lands in a cold tail
		}
		text := fmt.Sprintf(`[ %sClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d ]`,
			lead, cid, i%10, i%7, i%6, (i%16)*1024)
		if i%211 == 5 {
			// An expression in a schema'd field: it stays in the remnant.
			text = fmt.Sprintf(`[ %sClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=ProcId*512+7 ]`,
				lead, cid, i%10, i%7, i%6)
		}
		if i%3 == 0 {
			// The bundle that becomes a group schema; RanExit occasionally escapes.
			exit := fmt.Sprintf("%d", i%3)
			if i%150 == 0 {
				exit = `"aborted"`
			}
			text = fmt.Sprintf(`[ %sClusterId=%s; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d; RanStart=%d; RanEnd=%d; RanHost="h%d"; RanExit=%s ]`,
				lead, cid, i%10, i%7, i%6, (i%16)*1024, i, i+10, i%50, exit)
		}
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	want := readAll(t, c, "true")
	sort.Strings(want)
	if len(want) != payloadFixtureAds {
		t.Fatalf("fixture reads %d ads, want %d", len(want), payloadFixtureAds)
	}
	// A dictionary interns the sealed segments, so the payloads are dictionary-keyed -- the shape a
	// production table is in. A small sample keeps training cheap.
	if _, err := c.RetrainDict(256); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		c.Close()
		t.Fatal("schema scan did not enable")
	}
	segs, groups := columnarCensus(c)
	interned := 0
	for _, seg := range c.shards[0].segs {
		if seg != nil && seg.columnarized() && seg.dict.Load() != nil {
			interned++
		}
	}
	t.Logf("%d columnarized segments (%d interned), %d group blocks", segs, interned, groups)
	if segs == 0 {
		c.Close()
		t.Fatal("no segment was columnarized")
	}
	if groups == 0 {
		c.Close()
		t.Fatal("no columnarized segment carries a group block")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return want
}

// columnarCensus counts a collection's columnarized segments and the group blocks they carry.
func columnarCensus(c *Collection) (segs, groups int) {
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cn := seg.colNative.Load(); cn != nil {
				segs++
				groups += len(cn.seg.groups)
			}
		}
	}
	return segs, groups
}

// assertReadsEveryAd opens dir and requires every ad to read back exactly as want, from columnarized
// segments. Returns the number of columnarized segments seen.
func assertReadsEveryAd(t *testing.T, dir string, want []string) int {
	t.Helper()
	c, err := Open(payloadFixtureOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	segs, groups := columnarCensus(c)
	if segs == 0 || groups == 0 {
		t.Fatalf("reopened with %d columnarized segments and %d group blocks; the payloads were not read", segs, groups)
	}
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg != nil && seg.colDamaged.Load() {
				t.Errorf("segment %s was marked damaged", seg.path)
			}
		}
	}
	got := readAll(t, c, "true")
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("read %d ads, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ad %d differs:\n  got  %s\n  want %s", i, got[i], want[i])
		}
	}
	return segs
}

// segmentFiles lists the segment data files under dir.
func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*", "seg-*.dat"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

// payloadOffset returns the offset of the columnar payload record in a segment file's bytes.
func payloadOffset(data []byte) (int, bool) {
	for off := 0; off+recHeaderSize <= len(data); {
		total := int(recTotalLen(data, uint32(off)))
		if total <= 0 || off+total > len(data) {
			break
		}
		if recIsCol(data, uint32(off)) && recIsMarker(data, uint32(off)) {
			return off, true
		}
		off += total
	}
	return 0, false
}

// rewritePayload replaces the payload record at off with one carrying blob, re-framed and
// re-checksummed exactly as appendCol writes it. The payload must be the segment's last record (it
// is: columnarizeSegment appends it last), so a shorter blob leaves only unwritten space behind it.
func rewritePayload(t *testing.T, data []byte, off int, blob []byte) {
	t.Helper()
	oldTotal := int(recTotalLen(data, uint32(off)))
	if !allZero(data[off+oldTotal:]) {
		t.Fatal("the payload is not the segment's last record")
	}
	blob = append([]byte(nil), blob...) // it may alias data
	rl := recordLen(0, len(blob))
	if off+rl > len(data) {
		t.Fatal("a rewritten payload does not fit")
	}
	clear(data[off+recTotalLenOff : off+oldTotal])
	b := data[off : off+rl]
	binary.LittleEndian.PutUint32(b[recTotalLenOff:], uint32(rl))
	binary.LittleEndian.PutUint32(b[recKeyLenOff:], markerFlag|colFlag)
	binary.LittleEndian.PutUint32(b[recKeyOff:], uint32(len(blob)))
	copy(b[recKeyOff+4:], blob)
	crcOff := recKeyOff + 4 + len(blob)
	binary.LittleEndian.PutUint32(b[crcOff:], recCRC(b, crcOff))
}

// editPayloads applies edit to the payload of every columnarized segment file under dir, writing
// back only the files it changed. edit returns the replacement blob, or nil to leave one alone.
// Returns the paths changed.
func editPayloads(t *testing.T, dir string, edit func(path string, blob []byte) []byte) []string {
	t.Helper()
	var changed []string
	for _, p := range segmentFiles(t, dir) {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		off, ok := payloadOffset(data)
		if !ok {
			continue
		}
		nb := edit(p, recAd(data, uint32(off)))
		if nb == nil {
			continue
		}
		rewritePayload(t, data, off, nb)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		changed = append(changed, p)
	}
	return changed
}

// TestColPayloadVersions runs the on-disk version cases against copies of one columnarized store,
// so the fixture is built once.
func TestColPayloadVersions(t *testing.T) {
	base := t.TempDir()
	want := payloadFixture(t, base)
	t.Run("current", func(t *testing.T) { testColPayloadCurrentVersionRoundTrip(t, copyStore(t, base), want) })
	t.Run("legacy", func(t *testing.T) { testColPayloadLegacyUnversionedReads(t, copyStore(t, base), want) })
	t.Run("future", func(t *testing.T) { testColPayloadFutureVersionRefused(t, copyStore(t, base), want) })
	t.Run("future-damaged", func(t *testing.T) { testColPayloadFutureVersionRepairLeavesDamagedSegment(t, copyStore(t, base)) })
}

// copyStore copies a closed store's directory tree into a fresh temporary directory.
func copyStore(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// testColPayloadCurrentVersionRoundTrip: what columnarization writes today carries the header at the
// current version, and reopens to exactly the ads that went in.
func testColPayloadCurrentVersionRoundTrip(t *testing.T, dir string, want []string) {

	n := 0
	editPayloads(t, dir, func(p string, blob []byte) []byte {
		n++
		if len(blob) < colPayloadHdr ||
			binary.LittleEndian.Uint32(blob) != colPayloadMagic ||
			binary.LittleEndian.Uint32(blob[4:]) != colPayloadVersion {
			t.Errorf("%s: payload begins % x, want the version %d header", filepath.Base(p), blob[:8], colPayloadVersion)
		}
		return nil
	})
	if n == 0 {
		t.Fatal("no payload on disk")
	}
	segs := assertReadsEveryAd(t, dir, want)
	if segs != n {
		t.Errorf("%d columnarized segments published, %d payloads on disk", segs, n)
	}

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range r.Segments {
		s := &r.Segments[i]
		if s.Columnar && (s.ColumnarVersion != colPayloadVersion || s.ColumnarUnsupported) {
			t.Errorf("%s: fsck reports version %d unsupported=%v", filepath.Base(s.Path), s.ColumnarVersion, s.ColumnarUnsupported)
		}
	}
}

// testColPayloadLegacyUnversionedReads is the compatibility promise: every payload written before the
// header existed is the only copy of its values, and must keep reading -- as version 0, across
// reopens, untouched.
//
// The legacy files are the fixture's own payloads with the header stripped, which is byte for byte
// what the unversioned writer produced: version 1's body is version 0's payload unchanged
// (TestColPayloadVersion1BodyIsLegacyPayload holds that line).
func testColPayloadLegacyUnversionedReads(t *testing.T, dir string, want []string) {
	legacy := map[string][]byte{}
	changed := editPayloads(t, dir, func(p string, blob []byte) []byte {
		body, version, err := splitColPayload(blob)
		if err != nil || version != colPayloadVersion {
			t.Fatalf("%s: fixture payload version %d err %v", filepath.Base(p), version, err)
		}
		legacy[p] = append([]byte(nil), body...)
		return body
	})
	if len(changed) == 0 {
		t.Fatal("no payload to make legacy")
	}

	for round := range 2 { // open, close, and open again: nothing on the read path may upgrade or lose it
		before := ColNativeCRCFailures()
		if segs := assertReadsEveryAd(t, dir, want); segs != len(changed) {
			t.Errorf("round %d: %d columnarized segments published, %d legacy payloads on disk", round, segs, len(changed))
		}
		if d := ColNativeCRCFailures() - before; d != 0 {
			t.Errorf("round %d: %d legacy payloads were refused", round, d)
		}
	}
	editPayloads(t, dir, func(p string, blob []byte) []byte {
		if !bytes.Equal(blob, legacy[p]) {
			t.Errorf("%s: the legacy payload changed on disk across reopens", filepath.Base(p))
		}
		return nil
	})

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for i := range r.Segments {
		s := &r.Segments[i]
		if !s.Columnar {
			continue
		}
		seen++
		if s.ColumnarVersion != 0 || s.ColumnarUnsupported {
			t.Errorf("%s: fsck reports version %d unsupported=%v for a legacy payload",
				filepath.Base(s.Path), s.ColumnarVersion, s.ColumnarUnsupported)
		}
	}
	if seen != len(changed) {
		t.Errorf("fsck saw %d payloads, want %d", seen, len(changed))
	}
}

// testColPayloadFutureVersionRefused: a payload a newer build wrote is intact data this build cannot
// interpret. It must not be treated as damage -- that path lets scans skip its records silently --
// and nothing may rewrite or drop it.
func testColPayloadFutureVersionRefused(t *testing.T, dir string, want []string) {

	future := uint32(colPayloadMaxVersion + 1)
	var target string
	editPayloads(t, dir, func(p string, blob []byte) []byte {
		if target != "" {
			return nil
		}
		target = p
		nb := append([]byte(nil), blob...)
		binary.LittleEndian.PutUint32(nb[4:], future)
		return nb
	})
	if target == "" {
		t.Fatal("no payload to forge")
	}
	original := snapshotFiles(t, dir)

	crc, unsup := ColNativeCRCFailures(), ColNativeUnsupportedVersions()
	c, err := Open(payloadFixtureOpts(dir))
	if err == nil {
		c.Close()
		t.Fatal("Open accepted a collection holding a payload this build cannot read")
	}
	if !errors.Is(err, ErrUnsupportedColumnarVersion) {
		t.Fatalf("Open failed with %v, want ErrUnsupportedColumnarVersion", err)
	}
	if !strings.Contains(err.Error(), filepath.Base(target)) || !strings.Contains(err.Error(), fmt.Sprintf("version %d", future)) {
		t.Errorf("the error does not name the segment and its version: %v", err)
	}
	if ColNativeCRCFailures() != crc {
		t.Error("an unsupported version was counted as a checksum failure")
	}
	if ColNativeUnsupportedVersions() == unsup {
		t.Error("the unsupported version was not counted")
	}
	assertFilesUnchanged(t, original, "the refused Open")

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + r.String())
	found := false
	for i := range r.Segments {
		s := &r.Segments[i]
		if s.Path != target {
			if s.ColumnarUnsupported {
				t.Errorf("%s: reported unsupported, but only %s was forged", s.Path, target)
			}
			continue
		}
		found = true
		if !s.Columnar || !s.ColumnarUnsupported || s.ColumnarVersion != future {
			t.Errorf("fsck: columnar=%v unsupported=%v version=%d, want an unsupported version %d finding",
				s.Columnar, s.ColumnarUnsupported, s.ColumnarVersion, future)
		}
		if len(s.Damage) != 0 {
			t.Errorf("fsck reported the forged segment as damaged: %+v", s.Damage)
		}
	}
	if !found {
		t.Fatal("fsck did not report the forged segment")
	}
	if !strings.Contains(r.String(), "UNSUPPORTED columnar payload format version") {
		t.Error("the fsck report does not say so")
	}

	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unsupported) != 1 || rep.Unsupported[0] != target {
		t.Errorf("repair Unsupported = %v, want [%s]", rep.Unsupported, target)
	}
	assertFilesUnchanged(t, original, "Repair")

	// Nothing was lost: put the version back and every ad reads.
	editPayloads(t, dir, func(p string, blob []byte) []byte {
		if p != target {
			return nil
		}
		nb := append([]byte(nil), blob...)
		binary.LittleEndian.PutUint32(nb[4:], colPayloadVersion)
		return nb
	})
	assertReadsEveryAd(t, dir, want)
}

// testColPayloadFutureVersionRepairLeavesDamagedSegment: Repair would ordinarily rewrite a segment
// with damage mid-file. One whose payload it cannot read stays exactly as it is.
func testColPayloadFutureVersionRepairLeavesDamagedSegment(t *testing.T, dir string) {

	var target string
	editPayloads(t, dir, func(p string, blob []byte) []byte {
		if target != "" {
			return nil
		}
		target = p
		nb := append([]byte(nil), blob...)
		binary.LittleEndian.PutUint32(nb[4:], colPayloadMaxVersion+1)
		return nb
	})
	if target == "" {
		t.Fatal("no payload to forge")
	}
	offs := recordOffsets(t, target)
	if len(offs) < 4 {
		t.Fatal("segment too small")
	}
	flipBit(t, target, offs[1]+recKeyOff+2) // a remnant, not the payload
	original := snapshotFiles(t, dir)

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	needs := false
	for i := range r.Segments {
		s := &r.Segments[i]
		if s.Path == target {
			needs = segmentNeedsRepair(*s)
		}
	}
	if !needs {
		t.Fatal("the damage did not register; the test proves nothing")
	}
	rep, err := Repair(dir, RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rep.Segments {
		if s.Path == target {
			t.Errorf("repair rewrote the segment: %+v", s)
		}
	}
	if len(rep.Unsupported) != 1 || rep.Unsupported[0] != target {
		t.Errorf("repair Unsupported = %v, want [%s]", rep.Unsupported, target)
	}
	assertFilesUnchanged(t, original, "Repair")
	if _, err := os.Stat(filepath.Join(dir, quarantineDir)); err == nil {
		t.Error("repair quarantined something")
	}
}

// snapshotFiles reads every segment data file under dir.
func snapshotFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, p := range segmentFiles(t, dir) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out[p] = b
	}
	return out
}

// assertFilesUnchanged requires the segment data files under dir to be exactly snap.
func assertFilesUnchanged(t *testing.T, snap map[string][]byte, after string) {
	t.Helper()
	for p, want := range snap {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("after %s: %s: %v", after, filepath.Base(p), err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("after %s: %s changed", after, filepath.Base(p))
		}
	}
}

// TestColPayloadMagicCannotBeLegacy pins the property the whole scheme rests on: no legacy payload
// begins with the magic, so a legacy payload is never mistaken for a versioned one (or the reverse).
//
// A legacy payload begins with its schema's field count, a little-endian u32. readAdSchema refuses
// more than adSchemaMaxFields, so any legacy payload that ever read has a zero fourth byte; the
// magic's is not. A payload whose count exceeded the cap was never readable, and still cannot match:
// that many fields (10 bytes each at the least) do not fit a record whose payload length is a u32.
func TestColPayloadMagicCannotBeLegacy(t *testing.T) {
	var m [4]byte
	binary.LittleEndian.PutUint32(m[:], colPayloadMagic)
	if string(m[:]) != "COLN" {
		t.Errorf("magic is % x on disk, want \"COLN\"", m)
	}
	if m[3] == 0 || colPayloadMagic <= adSchemaMaxFields {
		t.Fatalf("magic %#x could be a readable legacy field count (cap %#x)", colPayloadMagic, adSchemaMaxFields)
	}
	const minFieldBytes = 4 + 2 + 2 + 2 // empty name, kind, width, unsigned
	if uint64(colPayloadMagic)*minFieldBytes <= uint64(^uint32(0)) {
		t.Fatalf("a legacy payload of %#x fields could fit a record", colPayloadMagic)
	}

	// Real legacy payloads, built by the unversioned writer, across schema widths.
	c := New(Options{Shards: 1})
	for _, nf := range []int{1, 2, 7, 64, 300} {
		var sb strings.Builder
		for f := range nf {
			fmt.Fprintf(&sb, "A%d=%d\n", f, f)
		}
		var wires [][]byte
		for range 16 {
			wires = append(wires, c.encodeAd(mustAdOld(t, sb.String()).AST()))
		}
		s := buildAdSchema(wires, adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: true})
		blk := encodeColumnarBlock(s, encodeRows(s, wires), resolveColLayout(s, nil), identityCodec{}, nil)
		legacy := marshalColSegment(oneBlockColSeg(blk, make([]uint32, blk.n)), c.intern.Name)
		if binary.LittleEndian.Uint32(legacy) != uint32(len(s.fields)) {
			t.Fatalf("%d fields: a legacy payload begins % x, not its field count", nf, legacy[:4])
		}
		body, version, err := splitColPayload(legacy)
		if err != nil || version != 0 || !bytes.Equal(body, legacy) {
			t.Errorf("%d fields: legacy payload read as version %d (err %v)", nf, version, err)
		}
	}
	// And every count a readable legacy payload can begin with.
	for _, n := range []uint32{0, 1, 0xff, 0x100, 0xffff, 0x10000, adSchemaMaxFields} {
		b := binary.LittleEndian.AppendUint32(nil, n)
		b = append(b, 0, 0, 0, 0)
		if _, version, err := splitColPayload(b); version != 0 || err != nil {
			t.Errorf("a legacy payload beginning with count %#x read as version %d (err %v)", n, version, err)
		}
	}
}

// TestColPayloadVersion1BodyIsLegacyPayload: version 1 added the header and nothing else, which is
// what lets one body parser serve both -- and what makes the legacy fixture above faithful.
func TestColPayloadVersion1BodyIsLegacyPayload(t *testing.T) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := range 64 {
		wires = append(wires, c.encodeAd(mustAdOld(t, fmt.Sprintf("Cpus=%d\nMemory=%d\nArch=\"X86_64\"\nExtra=%d", 1+i%8, 1024+i, i*100000)).AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.80, Fit: 0.90, Strings: true})
	blk := encodeColumnarBlock(s, encodeRows(s, wires), resolveColLayout(s, hotHalf(s)), identityCodec{}, nil)
	cs := oneBlockColSeg(blk, make([]uint32, blk.n))

	v1 := marshalColPayload(cs, c.intern.Name)
	legacy := marshalColSegment(cs, c.intern.Name)
	if len(v1) != colPayloadHdr+len(legacy) || !bytes.Equal(v1[colPayloadHdr:], legacy) {
		t.Fatal("the version 1 body differs from the legacy payload")
	}
	for name, blob := range map[string][]byte{"v1": v1, "legacy": legacy} {
		got, err := unmarshalColPayload(blob, identityCodec{}, c.intern.Intern)
		if err != nil || got == nil || got.blocks[0].n != blk.n {
			t.Errorf("%s: unmarshalColPayload = %v, %v", name, got, err)
		}
	}
	// The downgrade property: a build that predates the header reads the magic as a field count
	// beyond its cap and rejects the payload outright rather than misreading it.
	if unmarshalColSegment(v1, identityCodec{}, c.intern.Intern) != nil {
		t.Error("an unversioned reader accepted a versioned payload")
	}
}

// TestSplitColPayloadHeaders covers the header cases a writer never produces: each is refused, and
// only a short header is called malformed.
func TestSplitColPayloadHeaders(t *testing.T) {
	hdr := func(v uint32) []byte {
		b := binary.LittleEndian.AppendUint32(nil, colPayloadMagic)
		return append(binary.LittleEndian.AppendUint32(b, v), 1, 2, 3)
	}
	for _, v := range []uint32{0, colPayloadMaxVersion + 1, 0xffffffff} {
		if _, got, err := splitColPayload(hdr(v)); !errors.Is(err, ErrUnsupportedColumnarVersion) || got != v {
			t.Errorf("version %d: got version %d, err %v; want ErrUnsupportedColumnarVersion", v, got, err)
		}
	}
	if body, v, err := splitColPayload(hdr(colPayloadVersion)); err != nil || v != colPayloadVersion || !bytes.Equal(body, []byte{1, 2, 3}) {
		t.Errorf("current version: body % x, version %d, err %v", body, v, err)
	}
	if _, _, err := splitColPayload(hdr(colPayloadVersion)[:6]); !errors.Is(err, errColPayloadMalformed) {
		t.Errorf("a short header: err %v, want errColPayloadMalformed", err)
	}
}
