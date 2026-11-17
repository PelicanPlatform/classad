package collections

// Not losing the dictionary when the file stops being it.
//
// The file check (dictpin.go) has three outcomes and only one of them used to write. A
// file whose directory entry had gone was rewritten from the open descriptor; a file that
// had been REPLACED or CHANGED was reported and left strictly alone, because an operator
// restoring from a backup looks exactly like a replacement and clobbering it would destroy
// the thing being put back.
//
// Leaving it alone is still right. Leaving it at that was not. In that state the
// dictionary this store actually loaded exists in exactly two places -- this process's
// memory, and the descriptor it still holds -- and both go away when the process does.
// The store keeps serving happily until it restarts, which is precisely the eleven-day
// shape the whole exercise is about.
//
// So a discrepancy now also preserves. The true bytes are written to
// dicts/backup/<id>-<crc>.zst, beside the file that is no longer them rather than over it,
// and the fault says where they went.
//
// The name is the content. A backup named by its CRC cannot be written twice, so a fault
// that persists across many maintenance passes leaves one file rather than one per pass,
// and two different versions of a dictionary cannot collide. Before writing anything at
// all this looks for the same bytes in the places they might already be -- the backup
// directory, the attic, and the segment trailers -- because a dictionary already carried
// in three segments does not need a fourth copy in a directory.

import (
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync/atomic"
)

// dictBackupDir is where a dictionary goes when the file at its own path is no longer it.
// A subdirectory of dicts/, so it travels with the dictionaries in any copy, move or
// backup of the store -- and so that whatever took dicts/ takes this too, which is why it
// is a last resort behind the segment trailers rather than the first line.
const dictBackupDir = "backup"

// dictsPreserved counts dictionaries written to the backup directory because the file at
// their own path had been replaced or changed underneath the store.
var dictsPreserved atomic.Int64

// DictsPreserved reports how many dictionaries have been written aside because the file
// at their own path stopped being them.
func DictsPreserved() int64 { return dictsPreserved.Load() }

// dictBackupName is the content-addressed name of a preserved dictionary.
func dictBackupName(id, crc uint32) string {
	return fmt.Sprintf("%d-%08x.zst", id, crc)
}

// fileHasCRC reports whether path holds exactly len bytes with the given CRC-32C. It is
// how every "is this copy the same copy" question below is asked: a name, a length or a
// timestamp can agree while the bytes do not.
func fileHasCRC(path string, size int64, crc uint32) bool {
	st, err := os.Stat(path)
	if err != nil || st.Size() != size {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return crc32.Checksum(b, crcTable) == crc
}

// preserveDict makes sure the dictionary this store loaded still exists on disk after a
// fault showed the file at its own path is no longer it. It sets fault.Preserved to where
// those bytes can be found, which may be a copy that was already there.
//
// It searches before it writes, cheapest first: the backup directory (one stat and a read,
// and the answer for every pass after the first), then dicts/attic/, then the segment
// trailers. A dictionary already carried in three segments is preserved; adding a fourth
// copy in a directory the same sweep could take would buy nothing.
func (r *dictReg) preserveDict(id uint32, p *dictPin, fault *DictFault) {
	b := p.loaded()
	// r.dir is <collection>/dicts; the shards are its siblings.
	dictsDir := r.dir
	if dictsDir == "" {
		return
	}
	collDir := filepath.Dir(dictsDir)
	backup := filepath.Join(dictsDir, dictBackupDir, dictBackupName(id, p.crc))

	for _, cand := range []string{backup, filepath.Join(dictsDir, "attic", fmt.Sprintf("%d.zst", id))} {
		if fileHasCRC(cand, p.size, p.crc) {
			fault.Preserved = cand
			return
		}
	}
	for _, rep := range sweepDictReplicas(collDir, countShardDirs(collDir), map[uint32]bool{id: true}) {
		if crc32.Checksum(rep.dict, crcTable) == p.crc {
			fault.Preserved = rep.from
			return
		}
	}

	// Create the directory first rather than letting writeDictFile do it. That function
	// counts a missing parent as the dicts directory having vanished at runtime, which is
	// a real and serious signal; the backup directory is simply new the first time, and
	// bumping that counter here would cry wolf on every store that ever preserves one.
	if mkErr := os.MkdirAll(filepath.Dir(backup), 0o755); mkErr != nil {
		if fault.Err == nil {
			fault.Err = mkErr
		}
		return
	}
	if werr := writeDictFile(backup, b); werr != nil {
		if fault.Err == nil {
			fault.Err = werr
		}
		return
	}
	fault.Preserved = backup
	dictsPreserved.Add(1)
}
