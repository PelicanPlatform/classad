package collections

// Holding on to the dictionary files.
//
// A compression dictionary is read once, at Open, and then lives in memory for as long as
// the process does. Nothing reads the file again, which is why a dictionary directory can
// vanish underneath a running store and cost nothing until the next restart -- at which
// point every segment written under every dictionary in it is unreadable. That is not a
// hypothetical: it is what happened to a production archive, eleven days before anything
// noticed.
//
// So the store keeps hold of what it loaded:
//
//   - The DESCRIPTOR stays open. The inode survives an unlink, so whatever removes the
//     directory entry does not take the bytes with it; the window in which a dictionary is
//     really gone begins when this process closes the file, not when the reaper runs. It
//     also gives a stable identity -- fstat's inode number -- to compare the path against
//     later, which is what turns "a file is there" into "the file I loaded is there".
//   - A CRC-32C of the contents. A ZSTD dictionary carries no checksum of its own (the
//     header is magic, Dictionary_ID, entropy tables), so without one there is no way to
//     tell a file that was rewritten from one that was not.
//
// Neither of these PREVENTS the loss. systemd-tmpfiles and tmpwatch do not consult open
// descriptors, and nothing here can stop them. What they buy is that the loss is noticed
// while the process is healthy and the bytes are still in hand, instead of at the next
// restart, and that the ordinary case -- the entry was removed, nothing else changed -- is
// repaired from the descriptor rather than mourned.
//
// The cost is one file descriptor and one CRC per live dictionary: single digits of each.
// (Segments deliberately do the opposite and release their descriptor after mapping, since
// a large store holds thousands of them; see newMmapSegment.)

import (
	"fmt"
	"hash/crc32"
	"os"
	"sync/atomic"
	"time"
)

// dictVerifyInterval bounds how often a maintenance pass re-checks the dictionary files.
// The check is a stat and a read per dictionary -- a few hundred KB -- which is nothing
// next to a merge but is not free next to a Rotate that runs on every seal. An interval
// makes the cost per unit of TIME rather than per pass, so a busy store pays no more than
// an idle one. VerifyDictFiles ignores it.
const dictVerifyInterval = time.Minute

// DictFaultKind says what was found wrong with a dictionary file.
type DictFaultKind int

const (
	// DictFaultMissing: the directory entry is gone. The store still holds the bytes
	// through its open descriptor, so this is the one kind that repairs itself.
	DictFaultMissing DictFaultKind = iota + 1
	// DictFaultReplaced: something else occupies the path -- a different inode. The file
	// is NOT overwritten: a restore from a backup looks exactly like this, and clobbering
	// it would destroy the thing that was being put back.
	DictFaultReplaced
	// DictFaultChanged: the same file, different contents. Also not overwritten, for the
	// same reason.
	DictFaultChanged
	// DictFaultMismatch: the dictionary file is not the one a segment's records were
	// written with, according to the fingerprint that segment recorded at seal
	// (segtrailer.go). Unlike the three above this is not about the file CHANGING under a
	// running store -- it is noticed at Open, against a dictionary this process has only
	// just loaded, and it is the one state in which every record of that segment decodes
	// to nothing while everything else about the store looks correct.
	DictFaultMismatch
)

func (k DictFaultKind) String() string {
	switch k {
	case DictFaultMissing:
		return "missing"
	case DictFaultReplaced:
		return "replaced"
	case DictFaultChanged:
		return "changed"
	case DictFaultMismatch:
		return "not the dictionary these records were written with"
	}
	return fmt.Sprintf("DictFaultKind(%d)", int(k))
}

// DictFault is one dictionary file that is no longer what the store loaded. It implements
// error so it can be handed to Options.OnDictFault, and carries the detail so a caller
// that wants to act on the kind can reach it with errors.As.
type DictFault struct {
	ID   uint32
	Path string
	Kind DictFaultKind
	// Repaired is true when a missing file was rewritten from the open descriptor. The
	// records were never at risk while this process lived; the next one would have lost
	// them.
	Repaired bool
	// Preserved is where the dictionary this store loaded can still be found, after a
	// fault showed the file at its own path is no longer it. It may be a copy this check
	// wrote (dicts/backup/) or one it found already there -- the attic, or a segment
	// carrying a replica. Empty when nothing was preserved and nothing needed to be.
	Preserved string
	// From is the segment file a repaired copy came out of, when the dictionary was not
	// rewritten from the open descriptor but found in a segment trailer at Open. Empty
	// otherwise.
	From string
	// Err is why a repair was not possible, or why the check itself could not finish.
	Err error
}

func (f *DictFault) Error() string {
	switch {
	case f.Kind == DictFaultMismatch:
		return fmt.Sprintf("collections: dictionary file %s is not the one %s was written with -- "+
			"its recorded fingerprint does not match -- so every record in that segment will fail to decode; "+
			"run Fsck, which probes the records as well as comparing the fingerprint", f.Path, f.From)
	case f.Kind == DictFaultMissing && f.Repaired && f.From != "":
		return fmt.Sprintf("collections: dictionary file %s was missing and was restored from the copy in %s; the primary copy was lost while this store was not running", f.Path, f.From)
	case f.Kind == DictFaultMissing && f.Repaired:
		return fmt.Sprintf("collections: dictionary file %s had been removed and was rewritten from the open descriptor; something outside this process is deleting dictionaries", f.Path)
	case f.Kind == DictFaultMissing:
		return fmt.Sprintf("collections: dictionary file %s has been removed and could not be rewritten (%v); every segment naming dictionary %d becomes unreadable at the next open", f.Path, f.Err, f.ID)
	case f.Err != nil:
		return fmt.Sprintf("collections: dictionary file %s: %s: %v", f.Path, f.Kind, f.Err)
	case f.Preserved != "":
		return fmt.Sprintf("collections: dictionary file %s was %s underneath this store; it is no longer the dictionary %d was loaded from, which is kept at %s", f.Path, f.Kind, f.ID, f.Preserved)
	default:
		return fmt.Sprintf("collections: dictionary file %s was %s underneath this store; it is no longer the dictionary %d was loaded from, and this process holds the only copy", f.Path, f.Kind, f.ID)
	}
}

func (f *DictFault) Unwrap() error { return f.Err }

var (
	dictFilesMissing  atomic.Int64
	dictFilesReplaced atomic.Int64
	dictFilesChanged  atomic.Int64
	dictFilesRestored atomic.Int64
)

// DictFilesMissing counts dictionary files found gone by a maintenance check, process-wide
// since start. Nothing in this package removes one, so any count at all means something
// outside the process is deleting them. See DictsDirRecreated, which counts the coarser
// case of the whole directory going.
func DictFilesMissing() int64 { return dictFilesMissing.Load() }

// DictFilesReplaced counts dictionary files found to be a different file than the one
// loaded (a different inode at the same path).
func DictFilesReplaced() int64 { return dictFilesReplaced.Load() }

// DictFilesChanged counts dictionary files found with contents that no longer match what
// was loaded from them.
func DictFilesChanged() int64 { return dictFilesChanged.Load() }

// DictFilesRestored counts missing dictionary files rewritten from the descriptor the
// store still held open.
func DictFilesRestored() int64 { return dictFilesRestored.Load() }

// dictPin is what the store keeps of the file a dictionary was loaded from: the bytes, the
// open descriptor, and a fingerprint.
//
// The BYTES are the authoritative copy, and holding them is not redundant with holding the
// descriptor. A descriptor survives an unlink, so it still reads the dictionary when the
// directory entry is removed -- but a file rewritten IN PLACE keeps its inode, and the
// descriptor then reads the new contents like everybody else. That is the one case where
// the dictionary is genuinely gone from disk, and it is exactly the case where having it
// still in hand decides whether it can be put somewhere safe. The cost is ~112 KB per live
// dictionary, single digits of them.
//
// The descriptor still earns its place: fstat gives a stable identity for what was loaded,
// which is what tells a file that was REPLACED from one that was merely changed.
type dictPin struct {
	path string
	f    *os.File
	// dict is the dictionary as loaded. Never written to after pinDictFile returns.
	dict   []byte
	ino    uint64
	hasIno bool
	size   int64
	crc    uint32
}

// pinDictFile opens path, fingerprints it, and keeps the descriptor. It returns the
// contents too, so the one read serves both the caller that needs the dictionary and the
// fingerprint that will outlive it.
func pinDictFile(path string) (*dictPin, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	b := make([]byte, fi.Size())
	if _, err := f.ReadAt(b, 0); err != nil && len(b) > 0 {
		f.Close()
		return nil, nil, err
	}
	ino, hasIno := fileIno(fi)
	return &dictPin{
		path: path, f: f, dict: b, ino: ino, hasIno: hasIno,
		size: fi.Size(), crc: crc32.Checksum(b, crcTable),
	}, b, nil
}

func (p *dictPin) close() {
	if p != nil && p.f != nil {
		p.f.Close()
	}
}

// loaded returns the dictionary as this store loaded it. No I/O, and no failure mode:
// whatever has happened to the file, these are the bytes the running codecs were built
// from.
func (p *dictPin) loaded() []byte { return p.dict }

// fileMatches reports whether the file behind the descriptor still holds what was loaded
// from it. It reads through the DESCRIPTOR rather than the path, which is sound only
// because the caller has just confirmed the inode still matches -- same inode, same bytes,
// and no second open to race against.
func (p *dictPin) fileMatches() (bool, error) {
	b := make([]byte, p.size)
	if _, err := p.f.ReadAt(b, 0); err != nil && len(b) > 0 {
		return false, err
	}
	return crc32.Checksum(b, crcTable) == p.crc, nil
}

// verify compares the path against what was pinned. ok is false when nothing is wrong.
//
// The content check reads through the pinned DESCRIPTOR, not the path, which is sound
// only because the inode was just confirmed to match -- same inode, same bytes, and no
// second open to race against. Where the platform has no inode to compare (see fileIno) a
// replacement is invisible and only a content change is caught.
func (p *dictPin) verify() (DictFault, bool) {
	fault := DictFault{ID: 0, Path: p.path}
	fi, err := os.Stat(p.path)
	if err != nil {
		if !os.IsNotExist(err) {
			fault.Kind, fault.Err = DictFaultChanged, err
			return fault, true
		}
		fault.Kind = DictFaultMissing
		return fault, true
	}
	if p.hasIno {
		if ino, ok := fileIno(fi); ok && ino != p.ino {
			fault.Kind = DictFaultReplaced
			return fault, true
		}
	}
	if fi.Size() != p.size {
		fault.Kind = DictFaultChanged
		return fault, true
	}
	switch same, err := p.fileMatches(); {
	case err != nil:
		fault.Kind, fault.Err = DictFaultChanged, err
		return fault, true
	case !same:
		fault.Kind = DictFaultChanged
		return fault, true
	}
	return DictFault{}, false
}

// verifyFiles checks every pinned dictionary file and rewrites the ones whose directory
// entry has gone. It returns one fault per file found wrong; an empty result is the
// healthy answer.
//
// The I/O runs off the registry lock: the lock protects the pin map, and a stat that
// blocks on a wedged filesystem must not hold up a commit that only wants to look up a
// codec.
func (r *dictReg) verifyFiles() []DictFault {
	r.mu.Lock()
	ids := make([]uint32, 0, len(r.pins))
	pins := make([]*dictPin, 0, len(r.pins))
	for id, p := range r.pins {
		ids = append(ids, id)
		pins = append(pins, p)
	}
	r.mu.Unlock()

	var faults []DictFault
	for i, p := range pins {
		fault, bad := p.verify()
		if !bad {
			continue
		}
		fault.ID = ids[i]
		switch fault.Kind {
		case DictFaultMissing:
			dictFilesMissing.Add(1)
			r.restorePin(ids[i], p, &fault)
			if !fault.Repaired {
				// The rewrite did not happen, so the dictionary is still only in this
				// process. Preserving may not work either -- a read-only filesystem
				// fails both -- but it may also find the bytes already safe somewhere,
				// which turns "you have until the next restart" into "it is in segment X".
				r.preserveDict(ids[i], p, &fault)
			}
		case DictFaultReplaced:
			dictFilesReplaced.Add(1)
			r.preserveDict(ids[i], p, &fault)
		case DictFaultChanged:
			dictFilesChanged.Add(1)
			r.preserveDict(ids[i], p, &fault)
		}
		faults = append(faults, fault)
	}
	return faults
}

// restorePin rewrites a vanished dictionary file from the descriptor still holding its
// bytes, and re-pins the result. Re-pinning matters: the rewrite is a new inode, and a pin
// left naming the old one would report every later pass as a replacement.
//
// A failed re-pin keeps the old pin rather than dropping it. The descriptor still reads
// the dictionary, which is the thing worth keeping; the worst it costs is a spurious
// fault next time.
func (r *dictReg) restorePin(id uint32, p *dictPin, fault *DictFault) {
	if werr := writeDictFile(p.path, p.loaded()); werr != nil {
		fault.Err = werr
		return
	}
	fault.Repaired = true
	dictFilesRestored.Add(1)
	fresh, _, err := pinDictFile(p.path)
	if err != nil {
		return
	}
	r.mu.Lock()
	if cur, ok := r.pins[id]; ok && cur == p {
		r.pins[id] = fresh
		r.mu.Unlock()
		p.close()
		return
	}
	r.mu.Unlock()
	fresh.close()
}

// setPin installs (or replaces) the pin for a dictionary id.
func (r *dictReg) setPin(id uint32, p *dictPin) {
	r.mu.Lock()
	old := r.pins[id]
	r.pins[id] = p
	r.mu.Unlock()
	old.close()
}

// dropPin closes and forgets a dictionary's pin. Pruning calls it: a dictionary retired to
// the attic is SUPPOSED to leave dicts/, and a pin left behind would report that as a file
// someone deleted and put it straight back.
func (r *dictReg) dropPin(id uint32) {
	r.mu.Lock()
	p := r.pins[id]
	delete(r.pins, id)
	r.mu.Unlock()
	p.close()
}

// closePins releases every held descriptor. Called from Close.
func (r *dictReg) closePins() {
	r.mu.Lock()
	pins := make([]*dictPin, 0, len(r.pins))
	for id, p := range r.pins {
		pins = append(pins, p)
		delete(r.pins, id)
	}
	r.mu.Unlock()
	for _, p := range pins {
		p.close()
	}
}

// VerifyDictFiles checks every dictionary file against what the store loaded, rewrites
// any whose directory entry has gone, and reports what it found. An empty result is the
// healthy answer.
//
// Maintenance calls this on its own (see beginMaint), rate-limited by
// dictVerifyInterval; this entry point runs the check immediately and is what a daemon
// with its own schedule, or an operator with a question, should use.
//
// Every fault is also delivered to Options.OnDictFault as it is found, so a caller that
// wants to log rather than poll does not have to call this at all.
func (c *Collection) VerifyDictFiles() []DictFault {
	c.lastDictVerify.Store(time.Now().UnixNano())
	return c.reportDictFaults(c.dicts.verifyFiles())
}

// maybeVerifyDictFiles runs the check unless one ran within dictVerifyInterval. The
// compare-and-swap is what keeps two concurrent maintenance passes from both deciding it
// is their turn.
func (c *Collection) maybeVerifyDictFiles() {
	now := time.Now().UnixNano()
	last := c.lastDictVerify.Load()
	if now-last < int64(dictVerifyInterval) {
		return
	}
	if !c.lastDictVerify.CompareAndSwap(last, now) {
		return
	}
	c.reportDictFaults(c.dicts.verifyFiles())
}

// reportDictFaults hands each fault to the callback, if there is one.
func (c *Collection) reportDictFaults(faults []DictFault) []DictFault {
	if c.onDictFault == nil {
		return faults
	}
	for i := range faults {
		c.onDictFault(&faults[i])
	}
	return faults
}
