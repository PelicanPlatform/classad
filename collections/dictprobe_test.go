package collections

import (
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fingerprintedStore builds the carrier fixture and runs a census, which records a
// fingerprint in every sealed segment that has room for one.
func fingerprintedStore(t *testing.T) (dir, segPath string) {
	t.Helper()
	dir = buildCarrierStore(t)
	c := openCarrier(t, dir, 1)
	c.EnsureDictReplicas()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "0"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		var n uint64
		var d uint32
		if _, serr := fmt.Sscanf(e.Name(), "seg-%d.d%d.dat", &n, &d); serr != nil || d != 1 {
			continue
		}
		p := filepath.Join(dir, "0", e.Name())
		if tr, ok := readSegTrailerFile(p); ok {
			if _, _, _, has := tr.dictFingerprint(); has {
				return dir, p
			}
		}
	}
	// NOT a skip. buildCarrierStore reliably produces sealed segments under dictionary 1,
	// so "none was fingerprinted" means the census did not record them -- which is the
	// thing these tests exist to catch, and a skip would report it as nothing at all.
	t.Fatal("the census fingerprinted no segment under dictionary 1")
	return "", ""
}

// TestSegmentRecordsWhichDictionaryItUsed: the file name says WHICH dictionary, and since
// stamping so does every frame -- but neither says which BYTES. That is what the
// fingerprint adds, and it is the only thing on disk that can notice the file under an id
// is no longer the dictionary those records were compressed with.
func TestSegmentRecordsWhichDictionaryItUsed(t *testing.T) {
	dir, segPath := fingerprintedStore(t)
	dict, err := os.ReadFile(filepath.Join(dir, "dicts", "1.zst"))
	if err != nil {
		t.Fatal(err)
	}

	tr, ok := readSegTrailerFile(segPath)
	if !ok {
		t.Fatal("no trailer")
	}
	id, length, crc, has := tr.dictFingerprint()
	if !has {
		t.Fatal("the segment records no fingerprint")
	}
	if id != 1 {
		t.Errorf("fingerprint names dictionary %d; want 1", id)
	}
	if length != uint32(len(dict)) {
		t.Errorf("fingerprint length %d; want %d", length, len(dict))
	}
	if crc != crc32.Checksum(dict, crcTable) {
		t.Error("fingerprint checksum is not the dictionary's")
	}
	// Recording one must not disturb the records it describes.
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("Open after fingerprinting: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestFsckCatchesADictionaryThatIsNotTheOneRecorded is the case neither signal catches
// alone and nothing caught before. The file carries the RIGHT id and the WRONG bytes:
// loadDicts accepts it, the file check accepts it, and every record silently stops
// decoding. Bitrot reaches this state with nobody's help.
func TestFsckCatchesADictionaryThatIsNotTheOneRecorded(t *testing.T) {
	dir, _ := fingerprintedStore(t)
	// A different dictionary, stamped as if it were number 1. Training it through a
	// registry with its own directory is the straightforward way to get the bytes.
	otherDir := filepath.Join(t.TempDir(), "dicts")
	reg := newDictReg(identityCodec{})
	reg.dir = otherDir
	otherID, _ := registerTrained(t, reg, 77)
	raw, err := os.ReadFile(filepath.Join(otherDir, fmt.Sprintf("%d.zst", otherID)))
	if err != nil {
		t.Fatal(err)
	}
	imposter, err := stampDictID(raw, 1)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "dicts", "1.zst")
	if werr := os.WriteFile(path, imposter, 0o644); werr != nil {
		t.Fatal(werr)
	}

	// The control: everything that looks at the file in isolation is happy with it.
	if why := fsckDictFile(path); why != "" {
		t.Fatalf("control: the file check rejected the imposter (%s); the point is that it does not", why)
	}

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.DictChecks) == 0 {
		t.Fatal("neither check noticed a dictionary that is not the one the records used")
	}
	found := false
	for _, c := range r.DictChecks {
		if c.DictID != 1 || !c.Recorded {
			continue
		}
		found = true
		if c.Matches {
			t.Error("the fingerprint matched an imposter")
		}
		if c.ProbeFailures == 0 {
			t.Errorf("the probe decoded %d records with the wrong dictionary", c.Probed)
		}
		if v := c.Verdict(); !strings.Contains(v, "not the one these records were written with") {
			t.Errorf("the verdict does not name the dictionary as the problem: %s", v)
		}
	}
	if !found {
		t.Fatalf("no check reported dictionary 1: %+v", r.DictChecks)
	}
	if out := r.String(); !strings.Contains(out, "DICTIONARY MISMATCH") {
		t.Errorf("the report does not surface it:\n%s", out)
	}
}

// TestFsckBlamesTheFingerprintWhenTheRecordsDecode is the other half of asking twice. A
// fingerprint that disagrees while every sampled record decodes means the FINGERPRINT is
// the untrustworthy one, and saying so is the difference between a useful report and one
// that sends an operator after a dictionary that is perfectly fine.
func TestFsckBlamesTheFingerprintWhenTheRecordsDecode(t *testing.T) {
	dir, segPath := fingerprintedStore(t)
	used := 0
	func() {
		c := openCarrier(t, dir, 1)
		sh := c.shards[0]
		sh.mu.RLock()
		for _, s := range sh.segs {
			if s != nil && s.path == segPath {
				used = s.used
			}
		}
		sh.mu.RUnlock()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	// Doctor the recorded checksum, leaving the dictionary itself alone.
	if err := updateSegTrailer(segPath, used, func(have []segTrailerSection) []segTrailerSection {
		out := have[:0]
		for _, s := range have {
			if s.id != segSecDictFingerprint {
				out = append(out, s)
			}
		}
		return append(out, segTrailerSection{
			id: segSecDictFingerprint, body: encodeDictFingerprint(1, 999, 0xBADBAD),
		})
	}); err != nil {
		t.Fatal(err)
	}

	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.DictChecks {
		if c.Path != segPath {
			continue
		}
		if c.Matches || !c.Recorded {
			t.Fatalf("expected a recorded fingerprint that does not match: %+v", c)
		}
		if c.ProbeFailures != 0 {
			t.Errorf("the records did not decode, so this is not the case under test: %+v", c)
		}
		if v := c.Verdict(); !strings.Contains(v, "FINGERPRINT is the untrustworthy one") {
			t.Errorf("the verdict blames the wrong thing: %s", v)
		}
		return
	}
	t.Fatalf("the doctored fingerprint was not reported: %+v", r.DictChecks)
}

// TestFsckIsQuietWhenBothChecksAgree is the control. Two checks that both pass say nothing
// worth printing, and a report that lists every healthy segment is one nobody reads.
func TestFsckIsQuietWhenBothChecksAgree(t *testing.T) {
	dir, _ := fingerprintedStore(t)
	r, err := Fsck(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.DictChecks) != 0 {
		t.Errorf("a healthy store reported %d mismatch(es): %+v", len(r.DictChecks), r.DictChecks)
	}
	if strings.Contains(r.String(), "DICTIONARY MISMATCH") {
		t.Errorf("the report cries wolf:\n%s", r.String())
	}
}

// TestProbeKeepsTheLargestSamples pins the rule the probe depends on. An INCOMPRESSIBLE
// record decodes correctly under any dictionary, because it never back-references one, so
// sampling at random proves nothing. The largest compressed ads are the ones that leaned
// on the dictionary hardest.
func TestProbeKeepsTheLargestSamples(t *testing.T) {
	var have [][]byte
	for _, n := range []int{10, 500, 20, 4000, 30, 1000, 40} {
		have = keepLargestProbe(have, bytes.Repeat([]byte("x"), n))
	}
	if len(have) != dictProbeSamples {
		t.Fatalf("kept %d samples; want %d", len(have), dictProbeSamples)
	}
	sizes := map[int]bool{}
	for _, b := range have {
		sizes[len(b)] = true
	}
	for _, want := range []int{4000, 1000, 500} {
		if !sizes[want] {
			t.Errorf("dropped a %d byte sample for a smaller one: %v", want, sizes)
		}
	}
	for _, tooSmall := range []int{10, 20, 30} {
		if sizes[tooSmall] {
			t.Errorf("kept a %d byte sample over a larger one: %v", tooSmall, sizes)
		}
	}
	if got := keepLargestProbe(nil, nil); len(got) != 0 {
		t.Error("an empty ad was kept as a sample")
	}
	// Several samples, not one: a single decode that succeeds is not evidence.
	if dictProbeSamples < 2 {
		t.Error("one sample cannot distinguish a working dictionary from an incompressible record")
	}
	if probed, failures := runDictProbe(nil, have); probed != 0 || failures != 0 {
		t.Errorf("probed with no dictionary: %d/%d", failures, probed)
	}
}

// imposterDictionary trains a different dictionary and stamps it as if it were id, which
// is the one state nothing caught before: the right id, the right magic, loads cleanly,
// and is not the dictionary any of the records were compressed with.
func imposterDictionary(t *testing.T, id uint32) []byte {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dicts")
	reg := newDictReg(identityCodec{})
	reg.dir = dir
	otherID, _ := registerTrained(t, reg, 77)
	raw, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d.zst", otherID)))
	if err != nil {
		t.Fatal(err)
	}
	out, err := stampDictID(raw, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestOpenReportsADictionaryThatIsNotTheOneRecorded: recovery already reads the trailer,
// so asking whether the dictionary it just loaded is the one these records were written
// with costs a comparison. Open does not refuse on it -- one signal is not enough to keep
// a daemon down -- but staying silent would mean serving the segment as if it were empty,
// which is the failure this whole mechanism exists to end.
func TestOpenReportsADictionaryThatIsNotTheOneRecorded(t *testing.T) {
	dir, _ := fingerprintedStore(t)
	if werr := os.WriteFile(filepath.Join(dir, "dicts", "1.zst"), imposterDictionary(t, 1), 0o644); werr != nil {
		t.Fatal(werr)
	}
	before := DictFingerprintMismatches()

	var reported []error
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 20,
		OnDictFault: func(e error) { reported = append(reported, e) },
	})
	if err != nil {
		t.Fatalf("Open refused rather than reported: %v", err)
	}
	defer c.Close()

	if DictFingerprintMismatches() <= before {
		t.Error("the mismatch was not counted")
	}
	if len(reported) == 0 {
		t.Fatal("Open served a store whose dictionary is not the one its records used, silently")
	}
	var df *DictFault
	if !errors.As(reported[0], &df) || df.Kind != DictFaultMismatch {
		t.Fatalf("the fault does not name the mismatch: %v", reported[0])
	}
	if df.From == "" || !strings.HasSuffix(df.From, ".dat") {
		t.Errorf("the fault does not name the segment that recorded it: %+v", df)
	}
	msg := reported[0].Error()
	for _, want := range []string{"1.zst", ".dat", "fail to decode", "Fsck"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message does not mention %q: %s", want, msg)
		}
	}
}

// TestOpenIsQuietWhenTheDictionaryIsTheRecordedOne is the control: this runs on every
// segment of every open, so a false positive would be a log line per segment forever.
func TestOpenIsQuietWhenTheDictionaryIsTheRecordedOne(t *testing.T) {
	dir, _ := fingerprintedStore(t)
	var reported []error
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 20,
		OnDictFault: func(e error) { reported = append(reported, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(reported) != 0 {
		t.Errorf("a healthy store reported %d fault(s): %v", len(reported), reported)
	}
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// segUsedOf reports a sealed segment's written extent, which a trailer rewrite needs.
func segUsedOf(t *testing.T, dir, segPath string) int {
	t.Helper()
	c := openCarrier(t, dir, 1)
	used := 0
	sh := c.shards[0]
	sh.mu.RLock()
	for _, s := range sh.segs {
		if s != nil && s.path == segPath {
			used = s.used
		}
	}
	sh.mu.RUnlock()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return used
}

// TestOpenIsQuietAboutASegmentThatRecordedNothing is the case every store that predates
// fingerprints is in, and the one a false positive would be worst in: the check runs on
// every segment of every open, so reading "nothing recorded" as "does not match" would
// emit a fault per segment, forever, on stores where nothing is wrong. A channel that
// cries wolf on every open is one nobody reads when it finally has something to say.
//
// A segment with a trailer but no fingerprint is not hypothetical: it is any segment
// sealed before this change that has since been given a dictionary replica.
func TestOpenIsQuietAboutASegmentThatRecordedNothing(t *testing.T) {
	dir, segPath := fingerprintedStore(t)
	used := segUsedOf(t, dir, segPath)

	// Strip the fingerprint, keeping everything else the trailer holds.
	if err := updateSegTrailer(segPath, used, func(have []segTrailerSection) []segTrailerSection {
		out := have[:0]
		for _, s := range have {
			if s.id != segSecDictFingerprint {
				out = append(out, s)
			}
		}
		return out
	}); err != nil {
		t.Fatal(err)
	}
	tr, ok := readSegTrailerFile(segPath)
	if !ok {
		t.Fatal("setup: the segment lost its trailer entirely")
	}
	if _, _, _, has := tr.dictFingerprint(); has {
		t.Fatal("setup: the fingerprint was not stripped")
	}

	var reported []error
	c, err := Open(Options{
		Dir: dir, Shards: 1, SegmentSize: 1 << 20,
		OnDictFault: func(e error) { reported = append(reported, e) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(reported) != 0 {
		t.Errorf("a segment that recorded no fingerprint was reported as a mismatch: %v", reported)
	}
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}
