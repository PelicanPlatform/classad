package collections

// Getting a dictionary back out of the segments that need it.
//
// Copies of each dictionary ride in segment trailers (dictreplica.go). This is the other
// half: the paths that go looking for one when dicts/ no longer has it.
//
// Nothing here runs in the ordinary case. Open asks only when its coverage check has
// already reported a dictionary missing, which is the whole point of that check being
// cheap -- a healthy store pays one readdir per shard and never opens a segment to ask
// about dictionaries at all.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// dictsRecoveredFromSegments counts dictionaries restored to dicts/ from a copy found in a
// segment trailer, process-wide since start. A non-zero count means the primary copy was
// lost and the redundancy earned its keep -- which is worth an operator's attention even
// though the store opened.
var dictsRecoveredFromSegments atomic.Int64

// DictsRecoveredFromSegments reports how many dictionaries have been restored from a
// segment trailer because dicts/ no longer held them.
func DictsRecoveredFromSegments() int64 { return dictsRecoveredFromSegments.Load() }

// foundReplica is a copy of a dictionary read out of a segment's trailer.
type foundReplica struct {
	dict []byte
	from string // the segment file it came out of
}

// sweepDictReplicas reads the trailer of every segment file under dir looking for copies
// of the dictionaries in want. It stops once it has one of each.
//
// It reads trailers, not segments: two short ReadAts per file and no mapping. want is
// consulted per file, so a sweep for one dictionary across a store that has already
// answered costs nothing more than the directory walk.
//
// A copy is accepted only if it is a usable dictionary AND names the id it was filed
// under -- a segment carrying a dictionary stamped 7 under the label 3 is reporting a
// confusion, not a copy, and writing it into dicts/3.zst would make that confusion
// permanent. A copy still carrying legacyDictID says nothing about which id it is and is
// taken at its label, exactly as loadDicts takes such a file.
func sweepDictReplicas(dir string, shards int, want map[uint32]bool) map[uint32]foundReplica {
	found := map[uint32]foundReplica{}
	if len(want) == 0 {
		return found
	}
	for s := 0; s < shards && len(found) < len(want); s++ {
		shardDir := filepath.Join(dir, fmt.Sprintf("%d", s))
		entries, err := os.ReadDir(shardDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if len(found) == len(want) {
				break
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".dat") {
				continue
			}
			path := filepath.Join(shardDir, e.Name())
			tr, ok := readSegTrailerFile(path)
			if !ok {
				continue
			}
			for _, id := range tr.dictReplicaIDs() {
				if !want[id] {
					continue
				}
				if _, already := found[id]; already {
					continue
				}
				dict, has := tr.dictReplica(id)
				if !has || !usableDictReplica(dict, id) {
					continue
				}
				found[id] = foundReplica{dict: append([]byte(nil), dict...), from: path}
			}
		}
	}
	return found
}

// usableDictReplica reports whether bytes found in a trailer are a dictionary this store
// can use for id. See sweepDictReplicas for why the id has to agree.
func usableDictReplica(dict []byte, id uint32) bool {
	if _, ok := dictIDOf(dict); !ok {
		return false
	}
	// A stamped dictionary must name this slot. One that is not stamped names no slot at
	// all and is taken at its label, exactly as loadDicts takes such a file.
	if slot, named := dictSlotOf(dict); named && slot != id {
		return false
	}
	codec, err := NewZSTDCodec(dict)
	return err == nil && codec != nil
}

// restoreDictsFromSegments writes back every missing dictionary it can find a copy of,
// registers it, and returns how many it restored.
//
// It is called from Open with the store half-built: the dictionaries are loaded, nothing
// is mapped, and the coverage check has just said which ids segments name and the registry
// lacks. Restoring here rather than refusing is right because the copy is the dictionary,
// proven by its own id and by building a codec from it before a byte is written.
func (c *Collection) restoreDictsFromSegments(dir string, missing []uint32) int {
	want := make(map[uint32]bool, len(missing))
	for _, id := range missing {
		if id != 0 {
			want[id] = true
		}
	}
	found := sweepDictReplicas(dir, len(c.shards), want)
	if len(found) == 0 {
		return 0
	}
	ids := make([]uint32, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	dictsDir := filepath.Join(dir, "dicts")
	n := 0
	for _, id := range ids {
		rep := found[id]
		path := filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id))
		if err := writeDictFile(path, rep.dict); err != nil {
			continue
		}
		codec, err := NewZSTDCodec(rep.dict)
		if err != nil {
			continue
		}
		var pin *dictPin
		if p, _, perr := pinDictFile(path); perr == nil {
			pin = p
		}
		c.dicts.install(id, codec, pin)
		dictsRecoveredFromSegments.Add(1)
		n++
		if c.onDictFault != nil {
			c.onDictFault(&DictFault{
				ID: id, Path: path, Kind: DictFaultMissing, Repaired: true, From: rep.from,
			})
		}
	}
	return n
}

// countShardDirs counts the numbered shard directories under dir.
func countShardDirs(dir string) int {
	n := 0
	for {
		st, err := os.Stat(filepath.Join(dir, fmt.Sprintf("%d", n)))
		if err != nil || !st.IsDir() {
			return n
		}
		n++
	}
}

// surveyTrailers walks every segment file's trailer, counting the dictionary copies each
// carries and collecting the damage it finds. One pass answers both, because both questions
// are asked of the same two short reads per segment.
//
// It reads only each replica section's four-byte id, never the dictionaries themselves, so
// a whole-store census costs a trailer read per segment and nothing more.
func surveyTrailers(dir string) (map[uint32]int, []FsckTrailer) {
	counts := map[uint32]int{}
	var damage []FsckTrailer
	for s := 0; ; s++ {
		shardDir := filepath.Join(dir, fmt.Sprintf("%d", s))
		entries, err := os.ReadDir(shardDir)
		if err != nil {
			return counts, damage
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".dat") {
				continue
			}
			path := filepath.Join(shardDir, e.Name())
			tr, state := readSegTrailerState(path)
			switch state {
			case segTrailerNone:
				continue
			case segTrailerDamaged:
				damage = append(damage, FsckTrailer{
					Path: path,
					Why:  "a trailer was written and its directory does not verify",
				})
				continue
			}
			if len(tr.damaged) > 0 {
				damage = append(damage, FsckTrailer{Path: path, Sections: tr.damaged})
			}
			for _, id := range tr.dictReplicaIDs() {
				counts[id]++
			}
		}
	}
}

// fsckDictLabels lists the dictionary files stored under an id they do not name. A file
// still carrying legacyDictID says nothing about where it belongs and is never reported:
// every dictionary written before stamping carries it, so calling those mislabelled would
// flag every store that predates it.
func fsckDictLabels(dictsDir string) []FsckDictLabel {
	var out []FsckDictLabel
	for _, f := range readDictFileFacts(dictsDir) {
		if f.mislabelled() {
			out = append(out, FsckDictLabel{Path: f.path, FiledAs: f.filedAs, Names: f.names})
		}
	}
	return out
}
