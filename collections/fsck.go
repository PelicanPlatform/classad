package collections

// Fsck inspects a persistent collection's files WITHOUT opening it, and reports what
// is intact, what is damaged, and what a repair could recover.
//
// It does not go through Open deliberately. Open is the thing that fails when a store
// is damaged, and it also rewrites state as it recovers (rebuilding directories,
// pruning dictionaries, reindexing). An operator reaching for fsck needs to learn what
// is there BEFORE anything touches it, so everything here is read-only: files are
// opened O_RDONLY and nothing is created, renamed or removed.
//
// The two things it finds that Open cannot report:
//
//   - Damage past the first bad record. Open's recovery walk stops at a failed CRC
//     ("the durable data ends here"), which is right for a torn tail and wrong for a
//     bit flip in the middle: every valid record after it is silently dropped. Fsck
//     resyncs past the damage and counts what is still readable, so the operator sees
//     "1 record is gone" instead of a segment that quietly came back short.
//   - Files Open ignores. Recovery matches "seg-<n>.d<dict>.dat" exactly and skips
//     everything else without a word, so a renamed segment is invisible -- and because
//     an unreferenced dictionary is deleted at the next clean open, an invisible
//     segment can lose the dictionary it needs to be read at all. Fsck names such
//     files.

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FsckDamage is one unreadable region of a segment.
type FsckDamage struct {
	// Offset is where the unreadable run begins, and Length how far it extends
	// before a record verified again. A run that reaches the end of the written
	// data is reported with Trailing set.
	Offset int
	Length int
	// Records is how many record slots the run covers, when that could be
	// determined; 0 when the framing itself was unreadable.
	Records int
	// Trailing marks damage that runs to the end of the segment. This is the
	// ordinary shape of a crash (a half-written record), not corruption, and a
	// repair discards it rather than trying to recover past it.
	Trailing bool
	// Reason names what failed: "crc" (framing intact, contents corrupt) or
	// "framing" (the record header itself was not usable).
	Reason string
}

// FsckSegment is one segment data file's census.
type FsckSegment struct {
	Path    string
	Size    int
	DictID  uint32 // the dictionary named in the file name
	Records int    // records that verified
	Bytes   int    // bytes those records occupy
	Damage  []FsckDamage
	// Sidecar reports the state of this segment's .idx: "absent", "ok", or a
	// rejection reason. A sidecar is derived state, so a bad one costs only the
	// rebuild -- it is never data loss.
	Sidecar string
	// maxSeq and supersede are scratch for the supersession check, folded into the
	// report once every segment has been walked.
	maxSeq    uint64
	supersede []FsckSupersede

	// ExtentTrusted is true when the sidecar's recorded extent was usable, which is
	// how Open avoids walking the segment. When it is true and damage was found, the
	// damage is INSIDE the trusted extent: Open will not notice it, and queries will
	// hit it one record at a time.
	ExtentTrusted bool
}

// FsckSupersede is a record whose supersededBySeq cannot be true.
//
// That field is one of the two the record checksum does NOT cover (it is rewritten in
// place when a later version supersedes this one), so corruption there is invisible to
// every other check. It matters because the field alone decides whether a record is
// live: a live record whose supersededBySeq is corrupted simply stops being returned.
// The key does not come back wrong -- it comes back missing, with nothing anywhere
// saying why.
type FsckSupersede struct {
	Path string // the segment holding the record
	Off  int    // its offset within that segment
	Key  string
	Seq  uint64 // the record's own commit sequence
	Sup  uint64 // the superseding sequence it claims
	Why  string // which invariant it breaks
}

// FsckStray is a file in a shard directory that recovery will silently ignore.
type FsckStray struct {
	Path string
	Why  string
}

// FsckReport is the whole collection's census.
type FsckReport struct {
	Dir      string
	Segments []FsckSegment
	Strays   []FsckStray
	// DictsPresent are the dictionary ids with a file in dicts/, and DictsMissing
	// the ids some segment names but which have no file -- those segments cannot be
	// decoded at all until the dictionary is restored.
	DictsPresent []uint32
	DictsMissing []uint32
	// DictsInAttic are missing dictionaries whose file is sitting in dicts/attic/,
	// retired by pruning. Those segments ARE recoverable: move the file back.
	DictsInAttic []uint32
	// Supersede lists records whose supersededBySeq is impossible. Each one is a key
	// that may silently not be returned.
	Supersede []FsckSupersede
	// MaxSeq is the highest commit sequence a record could legitimately name: the
	// shard commit sequences recorded in the directory snapshots when available,
	// otherwise the highest sequence written into any record.
	MaxSeq uint64
	// CommitSeqKnown is true when a directory snapshot supplied the real commit
	// sequence. Without one, MaxSeq is only a floor -- a delete advances the commit
	// sequence without writing a record, so a legitimate tombstone can name a
	// sequence higher than any record's -- and the "superseded by a commit that
	// never happened" check is skipped rather than reporting healthy deletes.
	CommitSeqKnown bool
}

// Totals sums the per-segment counts.
func (r *FsckReport) Totals() (records, damagedRuns, damagedRecords int) {
	for _, s := range r.Segments {
		records += s.Records
		for _, d := range s.Damage {
			if d.Trailing {
				continue // a torn tail is not loss; it was never committed
			}
			damagedRuns++
			damagedRecords += d.Records
		}
	}
	return
}

// String renders the report the way an operator wants to read it.
func (r *FsckReport) String() string {
	var b strings.Builder
	rec, runs, lost := r.Totals()
	fmt.Fprintf(&b, "fsck %s\n", r.Dir)
	fmt.Fprintf(&b, "  %d segment(s), %d record(s) readable\n", len(r.Segments), rec)
	if runs == 0 {
		b.WriteString("  no damage found\n")
	} else {
		fmt.Fprintf(&b, "  %d damaged run(s), %d record(s) unreadable\n", runs, lost)
	}
	for _, s := range r.Segments {
		if len(s.Damage) == 0 && s.Sidecar == "ok" {
			continue
		}
		fmt.Fprintf(&b, "  %s: %d records, sidecar %s\n", filepath.Base(s.Path), s.Records, s.Sidecar)
		for _, d := range s.Damage {
			kind := "DAMAGE"
			if d.Trailing {
				kind = "torn tail"
			}
			count := fmt.Sprintf("%d record(s)", d.Records)
			if d.Records == 0 {
				count = "record count unknown (framing lost)"
			}
			fmt.Fprintf(&b, "    %s at %d (+%d bytes, %s, %s)\n",
				kind, d.Offset, d.Length, count, d.Reason)
		}
		if s.ExtentTrusted && len(s.Damage) > 0 {
			b.WriteString("    note: inside the sidecar's trusted extent -- a normal open will not notice\n")
		}
	}
	if n := len(r.Supersede); n > 0 {
		fmt.Fprintf(&b, "  %d record(s) with an impossible supersededBySeq "+
			"(highest commit seen: %d):\n", n, r.MaxSeq)
		for _, f := range r.Supersede {
			fmt.Fprintf(&b, "    %s@%d key=%q seq=%d supersededBy=%d -- %s\n",
				filepath.Base(f.Path), f.Off, f.Key, f.Seq, f.Sup, f.Why)
		}
		b.WriteString("    That field is not covered by the record checksum, and it alone\n")
		b.WriteString("    decides whether a record is live, so such a key may simply not be\n")
		b.WriteString("    returned. Repair is a JUDGEMENT CALL and fsck will not make it: a\n")
		b.WriteString("    corrupted tombstone is indistinguishable from a real delete, so\n")
		b.WriteString("    restoring the record could un-delete data someone removed.\n")
	}
	for _, s := range r.Strays {
		fmt.Fprintf(&b, "  IGNORED %s: %s\n", s.Path, s.Why)
	}
	attic := make(map[uint32]bool, len(r.DictsInAttic))
	for _, id := range r.DictsInAttic {
		attic[id] = true
	}
	for _, id := range r.DictsMissing {
		if attic[id] {
			fmt.Fprintf(&b, "  RECOVERABLE dictionary %d: in dicts/attic/%d.zst -- "+
				"move it back to dicts/ to read those segments\n", id, id)
			continue
		}
		fmt.Fprintf(&b, "  MISSING dictionary %d: segments naming it cannot be decoded\n", id)
	}
	return b.String()
}

// Fsck examines the collection rooted at dir. It never writes, and it returns a
// report even when parts of the store are unreadable -- an error means fsck could not
// look, not that the store is bad.
func Fsck(dir string) (*FsckReport, error) {
	r := &FsckReport{Dir: dir}

	present, err := fsckDicts(filepath.Join(dir, "dicts"))
	if err != nil {
		return nil, err
	}
	r.DictsPresent = present
	have := make(map[uint32]bool, len(present))
	for _, id := range present {
		have[id] = true
	}
	have[0] = true // id 0 is the base codec, reconstructed from Options, never a file

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("fsck: reading %s: %w", dir, err)
	}
	missing := map[uint32]bool{}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "dicts" || e.Name() == quarantineDir {
			continue
		}
		shardDir := filepath.Join(dir, e.Name())
		files, err := os.ReadDir(shardDir)
		if err != nil {
			return nil, fmt.Errorf("fsck: reading %s: %w", shardDir, err)
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			path := filepath.Join(shardDir, f.Name())
			var n uint64
			var dictID uint32
			switch {
			case strings.HasSuffix(f.Name(), ".idx"), f.Name() == dirSnapName:
				continue // sidecars and the directory snapshot: derived, checked per segment
			case !strings.HasSuffix(f.Name(), ".dat"):
				r.Strays = append(r.Strays, FsckStray{path,
					"not a segment data file; recovery ignores it"})
				continue
			}
			if _, err := fmt.Sscanf(f.Name(), "seg-%d.d%d.dat", &n, &dictID); err != nil {
				r.Strays = append(r.Strays, FsckStray{path,
					"does not match seg-<n>.d<dict>.dat; recovery ignores it, and the " +
						"dictionary it needs may be pruned at the next open"})
				continue
			}
			seg, err := fsckSegment(path, dictID)
			if err != nil {
				r.Strays = append(r.Strays, FsckStray{path, err.Error()})
				continue
			}
			if !have[dictID] {
				missing[dictID] = true
			}
			r.Segments = append(r.Segments, *seg)
		}
	}
	inAttic, err := fsckDicts(filepath.Join(dir, "dicts", "attic"))
	if err != nil {
		return nil, err
	}
	retired := make(map[uint32]bool, len(inAttic))
	for _, id := range inAttic {
		retired[id] = true
	}
	for id := range missing {
		r.DictsMissing = append(r.DictsMissing, id)
		if retired[id] {
			r.DictsInAttic = append(r.DictsInAttic, id)
		}
	}
	sort.Slice(r.DictsInAttic, func(i, j int) bool { return r.DictsInAttic[i] < r.DictsInAttic[j] })
	sort.Slice(r.DictsMissing, func(i, j int) bool { return r.DictsMissing[i] < r.DictsMissing[j] })

	// Fold in the supersession findings. The "commit that never happened" ones were
	// collected against a running maximum, so a later segment may have raised it past
	// them; re-check against the whole store's maximum before reporting any of them.
	for _, seg := range r.Segments {
		if seg.maxSeq > r.MaxSeq {
			r.MaxSeq = seg.maxSeq
		}
	}
	// A clean Close leaves each shard's commit sequence in its directory snapshot.
	// That is the only true upper bound: deletes advance it without writing records,
	// so the highest record sequence is a floor and nothing more.
	if cs, ok := fsckCommitSeq(dir); ok {
		r.CommitSeqKnown = true
		if cs > r.MaxSeq {
			r.MaxSeq = cs
		}
	}
	for _, seg := range r.Segments {
		for _, f := range seg.supersede {
			if f.Sup > f.Seq {
				// "Never happened" is only decidable against a real commit sequence.
				if !r.CommitSeqKnown || f.Sup <= r.MaxSeq {
					continue
				}
			}
			r.Supersede = append(r.Supersede, f)
		}
	}
	sort.Slice(r.Supersede, func(i, j int) bool {
		if r.Supersede[i].Path != r.Supersede[j].Path {
			return r.Supersede[i].Path < r.Supersede[j].Path
		}
		return r.Supersede[i].Off < r.Supersede[j].Off
	})
	if len(r.Supersede) > fsckSupersedeMax {
		r.Supersede = r.Supersede[:fsckSupersedeMax]
	}
	sort.Slice(r.Segments, func(i, j int) bool { return r.Segments[i].Path < r.Segments[j].Path })
	return r, nil
}

// fsckDicts lists the dictionary ids that have a file on disk.
func fsckDicts(dictsDir string) ([]uint32, error) {
	entries, err := os.ReadDir(dictsDir)
	if os.IsNotExist(err) {
		return nil, nil // an identity-codec collection has no dicts directory
	}
	if err != nil {
		return nil, fmt.Errorf("fsck: reading %s: %w", dictsDir, err)
	}
	var ids []uint32
	for _, e := range entries {
		var id uint32
		if _, err := fmt.Sscanf(e.Name(), "%d.zst", &id); err == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// fsckSegment walks one segment file, resyncing past damage instead of stopping at
// it, and reports every readable record and every unreadable run.
func fsckSegment(path string, dictID uint32) (*FsckSegment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read: %w", err)
	}
	s := &FsckSegment{Path: path, Size: len(data), DictID: dictID}

	// Report the sidecar's state, and whether Open would trust its extent -- damage
	// inside a trusted extent is damage a normal open will not see.
	if extent, _ := readSidecarTrailer(path + ".idx"); extent > 0 {
		s.Sidecar = "ok"
		if extent <= len(data) && recExtentEndsCleanly(data, extent) {
			s.ExtentTrusted = true
		} else {
			s.Sidecar = "extent does not match the segment; recovery will rewalk"
		}
	} else if _, err := os.Stat(path + ".idx"); os.IsNotExist(err) {
		s.Sidecar = "absent"
	} else {
		s.Sidecar = "unreadable trailer; recovery will rewalk and rebuild"
	}

	// Walk the segment. A record whose FRAMING is intact can be stepped over exactly,
	// even when its contents fail the CRC -- so a corrupt record costs one record, not
	// the rest of the file. Only when the framing itself is unusable do we fall back to
	// scanning for the next offset that verifies.
	var run *FsckDamage
	for off := 0; off+recHeaderSize <= len(data); {
		total := int(recTotalLen(data, uint32(off)))
		framed := total > 0 && total%8 == 0 && off+total <= len(data)

		if framed && recVerifyCRC(data, uint32(off)) {
			run = nil // a good record ends any run of damage
			s.Records++
			s.Bytes += total
			noteSupersede(s, data, off)
			off += total
			continue
		}
		if framed {
			// Contents corrupt, framing fine: count exactly one record and step over it,
			// merging into the current run so consecutive bad records read as one region.
			if run == nil {
				s.Damage = append(s.Damage, FsckDamage{Offset: off, Reason: "crc"})
				run = &s.Damage[len(s.Damage)-1]
			}
			run.Length += total
			run.Records++
			off += total
			continue
		}
		// Framing unusable: the rest is unreachable by stepping, so scan for a resync
		// point. Anything before it is lost, and its record count is unknowable.
		run = nil
		off = fsckResync(s, data, off)
		if off < 0 {
			break
		}
	}

	// Damage with nothing but zeroes after it is the LAST written record, which is
	// what an interrupted write leaves behind: the record never committed, and Open
	// already does the right thing by stopping there.
	//
	// A genuinely corrupt last record is indistinguishable from a torn one -- both are
	// a bad record followed by unwritten space -- so this treats the ambiguous case as
	// the benign one. The cost of being wrong is one uncommitted record; the cost of
	// the other choice is rewriting the active segment on every run forever.
	if n := len(s.Damage); n > 0 {
		last := &s.Damage[n-1]
		if end := last.Offset + last.Length; end <= len(data) && allZero(data[end:]) {
			last.Trailing = true
		}
	}
	return s, nil
}

// fsckResync scans forward from a bad record for the next offset that frames and
// verifies, records the gap, and returns that offset. It returns -1 when nothing
// verifies before the end of the file, having recorded the run as trailing.
//
// Records are 8-byte aligned (recAlign), so candidate offsets step by 8 rather than
// by 1: an eighth of the work, and an offset that is not 8-aligned could not have
// been a record start anyway. A candidate is accepted only if its CRC verifies, which
// is what makes this safe -- a 32-bit checksum over a plausible header and body is not
// something random bytes produce.
func fsckResync(s *FsckSegment, data []byte, from int) int {
	start := from
	skipped := 0
	for off := from + 8; off+recHeaderSize <= len(data); off += 8 {
		total := int(recTotalLen(data, uint32(off)))
		if total <= 0 || total%8 != 0 || off+total > len(data) {
			continue
		}
		if !recVerifyCRC(data, uint32(off)) {
			continue
		}
		// The framing is gone, so the record count in this gap is genuinely unknown.
		// Report 0 rather than inventing a number from the byte length.
		_ = skipped
		s.Damage = append(s.Damage, FsckDamage{
			Offset: start, Length: off - start, Records: 0, Reason: "framing",
		})
		return off
	}
	// Nothing verified again. If the rest of the file is zeroes this is simply the
	// unwritten tail of a preallocated segment, not damage worth reporting.
	if allZero(data[start:]) {
		return -1
	}
	s.Damage = append(s.Damage, FsckDamage{
		Offset: start, Length: len(data) - start, Records: 0, Trailing: true, Reason: "framing",
	})
	return -1
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// fsckSupersedeMax bounds how many impossible supersededBySeq records are kept. The
// point is to name the problem and some of its keys, not to materialize a list as long
// as the damage.
const fsckSupersedeMax = 64

// noteSupersede records a verified record's sequence numbers for the supersession
// check, and keeps the ones that are already impossible on their own terms.
//
// Two invariants need no knowledge of the rest of the store:
//
//   - A record cannot be superseded by a commit at or before the one that wrote it.
//   - A marker record carries no supersession at all.
//
// The third -- superseded by a commit that never happened -- needs the highest sequence
// in the store, which is only known once every segment has been walked, so candidates
// above the running maximum are kept provisionally and filtered at the end.
func noteSupersede(s *FsckSegment, data []byte, off int) {
	if recIsMarker(data, uint32(off)) {
		return
	}
	seq := recSeq(data, uint32(off))
	if seq > s.maxSeq {
		s.maxSeq = seq
	}
	sup := recSuperseded(data, uint32(off))
	if sup == seqMax {
		return // current: the ordinary case
	}
	switch {
	case sup <= seq:
		s.supersede = append(s.supersede, FsckSupersede{
			Path: s.Path, Off: off, Key: string(recKey(data, uint32(off))), Seq: seq, Sup: sup,
			Why: "superseded at or before the commit that wrote it",
		})
	case sup > s.maxSeq:
		// Provisional: a later segment may raise the maximum past this. Re-checked
		// against the whole store's maximum in Fsck.
		s.supersede = append(s.supersede, FsckSupersede{
			Path: s.Path, Off: off, Key: string(recKey(data, uint32(off))), Seq: seq, Sup: sup,
			Why: "superseded by a commit that never happened",
		})
	}
	if len(s.supersede) > fsckSupersedeMax {
		s.supersede = s.supersede[:fsckSupersedeMax]
	}
}

// fsckCommitSeq reads the highest shard commit sequence recorded in the directory
// snapshots under dir, and whether any snapshot was found.
//
// A snapshot is written on a clean Close and consumed by the next open, so it is
// present exactly when the database was shut down cleanly and not reopened since --
// which is the state an operator running fsck is usually in. After a crash there is
// none, and the supersession upper bound is simply unavailable.
//
// Only the header is read, and only after the trailing CRC verifies, so a corrupt
// snapshot contributes nothing rather than a wild bound.
func fsckCommitSeq(dir string) (uint64, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	var max uint64
	var found bool
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "dicts" || e.Name() == quarantineDir {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name(), dirSnapName))
		if err != nil || len(data) < 4+4+4+8 {
			continue
		}
		body := data[:len(data)-4]
		if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
			continue
		}
		if binary.LittleEndian.Uint32(body[0:]) != dirSnapMagic ||
			binary.LittleEndian.Uint32(body[4:]) != dirSnapVersion {
			continue
		}
		if cs := binary.LittleEndian.Uint64(body[8:]); cs > max {
			max = cs
		}
		found = true
	}
	return max, found
}
