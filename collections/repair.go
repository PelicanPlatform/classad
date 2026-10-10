package collections

// Repair rewrites damaged segments, keeping every record that still verifies.
//
// Fsck says what is wrong; Repair makes the store openable again without the loss
// that a plain Open would take. Open's recovery walk stops at the first bad record --
// correct for a torn tail, and for a bit flip in the middle it silently drops every
// valid record after it. Repair copies the good records into a fresh segment file, so
// what follows the damage comes back.
//
// Three rules it does not break:
//
//   - The original is never destroyed. It moves to quarantine/, because an operator
//     who disagrees with what repair kept needs the bytes to argue with. Deleting a
//     quarantined file is a separate, explicit decision.
//   - Derived state that encodes record OFFSETS is removed, not updated. Rewriting a
//     segment moves every record, so its index sidecar and its shard's directory
//     snapshot now describe positions that no longer hold anything. Both are pure
//     caches that rebuild on the next open; leaving a stale one is how a repair turns
//     a readable store into a wrong one.
//   - Nothing is written until the replacement is complete and fsynced. A crash
//     mid-repair leaves either the original or the finished replacement, never a
//     half-written segment.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// quarantineDir is where originals go, under the collection root. Open never looks
// at it (it addresses shard directories by number), and Fsck skips it by name.
const quarantineDir = "quarantine"

// RepairOptions controls a repair run.
type RepairOptions struct {
	// DryRun reports what would change and writes nothing. Everything else is
	// identical, including reading and verifying every record, so a dry run is a
	// faithful rehearsal rather than an estimate.
	DryRun bool
}

// RepairSegment is what happened to one segment file.
type RepairSegment struct {
	Path           string
	RecordsKept    int
	RecordsDropped int
	BytesBefore    int
	BytesAfter     int
	// Quarantined is where the original was moved, or "" for a dry run.
	Quarantined string
	// DroppedSidecar and DroppedSnapshot record the derived files removed because
	// they describe offsets this rewrite invalidated.
	DroppedSidecar  bool
	DroppedSnapshot bool
}

// RepairReport is the outcome of a repair run.
type RepairReport struct {
	Dir      string
	DryRun   bool
	Segments []RepairSegment
	// Untouched counts segments that needed nothing: either undamaged, or damaged
	// only at the tail, which is an interrupted write rather than corruption.
	Untouched int
}

// Totals sums the records kept and dropped across every rewritten segment.
func (r *RepairReport) Totals() (kept, dropped int) {
	for _, s := range r.Segments {
		kept += s.RecordsKept
		dropped += s.RecordsDropped
	}
	return
}

// String renders the report for an operator.
func (r *RepairReport) String() string {
	var b strings.Builder
	kept, dropped := r.Totals()
	verb := "repaired"
	if r.DryRun {
		verb = "would repair"
	}
	fmt.Fprintf(&b, "%s %s\n", verb, r.Dir)
	if len(r.Segments) == 0 {
		fmt.Fprintf(&b, "  nothing to do (%d segment(s) need no repair)\n", r.Untouched)
		return b.String()
	}
	fmt.Fprintf(&b, "  %d segment(s) rewritten, %d left alone\n", len(r.Segments), r.Untouched)
	fmt.Fprintf(&b, "  %d record(s) kept, %d unreadable record(s) dropped\n", kept, dropped)
	for _, s := range r.Segments {
		fmt.Fprintf(&b, "  %s: kept %d, dropped %d (%d -> %d bytes)\n",
			filepath.Base(s.Path), s.RecordsKept, s.RecordsDropped, s.BytesBefore, s.BytesAfter)
		if s.Quarantined != "" {
			fmt.Fprintf(&b, "    original kept at %s\n", s.Quarantined)
		}
	}
	return b.String()
}

// Repair scans dir with Fsck's walk and rewrites every segment holding damage that a
// plain open would not survive cleanly. It returns what it did (or, for a dry run,
// what it would do).
func Repair(dir string, opts RepairOptions) (*RepairReport, error) {
	scan, err := Fsck(dir)
	if err != nil {
		return nil, err
	}
	rep := &RepairReport{Dir: dir, DryRun: opts.DryRun}

	for _, seg := range scan.Segments {
		if !segmentNeedsRepair(seg) {
			rep.Untouched++
			continue
		}
		done, err := repairSegment(seg.Path, opts)
		if err != nil {
			return rep, fmt.Errorf("repairing %s: %w", seg.Path, err)
		}
		rep.Segments = append(rep.Segments, *done)
	}
	return rep, nil
}

// segmentNeedsRepair reports whether rewriting this segment would recover anything.
//
// Damage confined to the tail is NOT repaired: a half-written record at the end is
// what an interrupted write looks like, the data was never committed, and Open
// already handles it exactly right by stopping there. Rewriting would churn the file
// for no gain.
func segmentNeedsRepair(s FsckSegment) bool {
	for _, d := range s.Damage {
		if !d.Trailing {
			return true
		}
	}
	return false
}

// repairSegment rewrites one segment file, keeping the records that verify.
func repairSegment(path string, opts RepairOptions) (*RepairSegment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := &RepairSegment{Path: path, BytesBefore: len(data)}

	// Collect the good records in order. Records are copied verbatim: their bytes
	// (including the CRC that proved them good) are exactly what goes into the new
	// file, so a repair can only ever drop data, never alter it.
	kept := make([]byte, 0, len(data))
	for off := 0; off+recHeaderSize <= len(data); {
		total := int(recTotalLen(data, uint32(off)))
		framed := total > 0 && total%8 == 0 && off+total <= len(data)
		if framed && recVerifyCRC(data, uint32(off)) {
			kept = append(kept, data[off:off+total]...)
			out.RecordsKept++
			off += total
			continue
		}
		if framed {
			out.RecordsDropped++
			off += total
			continue
		}
		next := repairResync(data, off)
		if next < 0 {
			break
		}
		out.RecordsDropped++ // at least one; the framing is gone so the count is a floor
		off = next
	}
	// Keep the file its original size. A segment is preallocated and mmapped at that
	// size, and the active one is still appended to; the walk stops at the first zero
	// totalLen, so trailing zeroes read as "unwritten", which is what they are.
	out.BytesAfter = len(kept)
	if len(kept) > len(data) { // cannot happen; a rewrite only ever shrinks
		return nil, fmt.Errorf("internal: repaired segment grew from %d to %d bytes", len(data), len(kept))
	}
	replacement := make([]byte, len(data))
	copy(replacement, kept)

	if opts.DryRun {
		return out, nil
	}

	// Write the replacement beside the original and make it durable BEFORE anything
	// is moved, so a crash here leaves the original untouched.
	tmp := path + ".repair"
	if err := writeFileSyncAt(tmp, replacement); err != nil {
		return nil, err
	}

	// Preserve the original under quarantine/<shard>/<name>.
	shardDir := filepath.Dir(path)
	qdir := filepath.Join(filepath.Dir(shardDir), quarantineDir, filepath.Base(shardDir))
	if err := os.MkdirAll(qdir, 0o755); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	quarantined := filepath.Join(qdir, filepath.Base(path))
	if err := os.Rename(path, quarantined); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	out.Quarantined = quarantined

	if err := os.Rename(tmp, path); err != nil {
		// Put the original back rather than leaving the segment absent.
		_ = os.Rename(quarantined, path)
		_ = os.Remove(tmp)
		return nil, err
	}

	// Every record moved, so anything recording offsets now points at the wrong
	// bytes. Both of these are caches that rebuild on the next open; a stale one
	// would turn a readable store into a wrong one.
	if err := os.Remove(path + ".idx"); err == nil {
		out.DroppedSidecar = true
	}
	if err := os.Remove(filepath.Join(shardDir, dirSnapName)); err == nil {
		out.DroppedSnapshot = true
	}
	return out, nil
}

// repairResync finds the next offset whose record frames and verifies, mirroring
// Fsck's resync so repair keeps exactly what fsck reported as readable.
func repairResync(data []byte, from int) int {
	for off := from + 8; off+recHeaderSize <= len(data); off += 8 {
		total := int(recTotalLen(data, uint32(off)))
		if total <= 0 || total%8 != 0 || off+total > len(data) {
			continue
		}
		if recVerifyCRC(data, uint32(off)) {
			return off
		}
	}
	return -1
}

// writeFileSyncAt writes b to path and fsyncs it, so the caller may rely on the file
// being complete once this returns.
func writeFileSyncAt(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
