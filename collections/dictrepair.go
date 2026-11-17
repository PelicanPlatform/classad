package collections

// Putting the dictionaries right.
//
// Fsck says what is wrong and never writes; Repair writes. Everything here follows the
// same rules the segment repair does: nothing is destroyed, nothing is clobbered, and a
// dry run is a faithful rehearsal rather than an estimate.
//
// Two things can be put right without opening the store.
//
// A dictionary filed under the wrong id can be RE-FILED, which is possible for the first
// time now that a dictionary names itself (see dictid.go). Before stamping, nothing on
// disk knew which file belonged where, and two swapped files were indistinguishable from
// two correct ones. A swap is the shape this actually takes, so the re-filing goes through
// temporary names: moving 3 onto 7 while 7 is still there would either clobber it or
// refuse, and both are wrong when the two simply want to exchange places.
//
// A dictionary that is MISSING can be restored from any surviving copy -- dicts/backup/,
// dicts/attic/, or a segment trailer. Open already restores from a trailer by itself, so
// this is for the operator working on a store they would rather not start yet, and for the
// two sources Open does not consult.
//
// What is NOT attempted: a segment whose trailer will not read is left exactly as it is.
// The length that says where its arena ends lives in the foot that failed its checksum, so
// truncating back to the arena means guessing where the records stop, and guessing low
// destroys them. The damage is reported and the segment simply never carries a copy again.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RepairDict is one dictionary file a repair run put right.
type RepairDict struct {
	ID uint32
	// Path is where the dictionary is now, or where it would go in a dry run.
	Path string
	// From is where it came from: the name it was wrongly filed under, or the copy it
	// was restored from.
	From string
	// Action is "re-filed" or "restored".
	Action string
}

// dictFileFacts is what a repair needs to know about one file in dicts/.
type dictFileFacts struct {
	path    string
	filedAs uint32
	names   uint32 // the slot the dictionary itself names; meaningful only when named
	named   bool   // false: unreadable, or written before stamping and naming no slot
	usable  bool
}

// readDictFileFacts reads dictsDir, skipping subdirectories (attic, backup) and anything
// not named for an id.
func readDictFileFacts(dictsDir string) []dictFileFacts {
	entries, err := os.ReadDir(dictsDir)
	if err != nil {
		return nil
	}
	var out []dictFileFacts
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var id uint32
		if _, serr := fmt.Sscanf(e.Name(), "%d.zst", &id); serr != nil {
			continue
		}
		f := dictFileFacts{path: filepath.Join(dictsDir, e.Name()), filedAs: id}
		if b, rerr := os.ReadFile(f.path); rerr == nil {
			if slot, named := dictSlotOf(b); named {
				f.names = slot
				f.named = true
				f.usable = usableDictReplica(b, slot)
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].filedAs < out[j].filedAs })
	return out
}

// mislabelled reports whether this file is filed under an id that is not the one it
// names. A dictionary written before stamping names no slot and is never re-filed: moving
// it on that basis would be inventing an answer.
func (f dictFileFacts) mislabelled() bool {
	return f.usable && f.named && f.names != f.filedAs
}

// repairDictLabels re-files every dictionary stored under an id it does not name.
//
// Phase one moves each of them aside to a temporary name, phase two places each at the id
// it names if that slot is free, and phase three puts anything that could not be placed
// back where it was. Going through temporaries is what makes a straight SWAP work, which
// is the case this exists for; leaving no temporary behind is what makes a partial run
// safe to repeat.
func repairDictLabels(dictsDir string, opts RepairOptions, r *RepairReport) {
	files := readDictFileFacts(dictsDir)
	taken := map[uint32]bool{}
	var moving []dictFileFacts
	for _, f := range files {
		if f.mislabelled() {
			moving = append(moving, f)
			continue
		}
		taken[f.filedAs] = true // a correctly filed dictionary keeps its slot
	}
	if len(moving) == 0 {
		return
	}
	if opts.DryRun {
		for _, f := range moving {
			if !taken[f.names] {
				r.Dicts = append(r.Dicts, RepairDict{
					ID:   f.names,
					Path: filepath.Join(dictsDir, fmt.Sprintf("%d.zst", f.names)),
					From: f.path, Action: "re-filed",
				})
				taken[f.names] = true
			}
		}
		return
	}

	type staged struct {
		tmp  string
		from string
		id   uint32
	}
	var pending []staged
	for i, f := range moving {
		tmp := filepath.Join(dictsDir, fmt.Sprintf(".repair-%d.zst", i))
		if err := os.Rename(f.path, tmp); err != nil {
			continue
		}
		pending = append(pending, staged{tmp: tmp, from: f.path, id: f.names})
	}
	for _, p := range pending {
		dst := filepath.Join(dictsDir, fmt.Sprintf("%d.zst", p.id))
		if taken[p.id] {
			continue
		}
		if err := os.Rename(p.tmp, dst); err != nil {
			continue
		}
		taken[p.id] = true
		r.Dicts = append(r.Dicts, RepairDict{ID: p.id, Path: dst, From: p.from, Action: "re-filed"})
	}
	// Anything still staged could not be placed -- its slot is held by a dictionary that
	// is correctly filed there. Put it back rather than leaving a temporary nobody reads.
	for _, p := range pending {
		if _, err := os.Stat(p.tmp); err == nil {
			_ = os.Rename(p.tmp, p.from) //nolint:errcheck // best effort: a file left under .repair-N is inert either way, and the next run retries
		}
	}
	_ = syncDir(dictsDir) //nolint:errcheck // the renames have landed; a failed sync costs durability of the NAMES, not correctness
}

// repairMissingDicts writes back every dictionary a segment file names and dicts/ does not
// have, from the first surviving copy it finds: dicts/backup/, then dicts/attic/, then a
// segment trailer. Every candidate must name the id it is being restored as and must build
// a codec before a byte is written -- a copy that disagrees about which dictionary it is
// would make the confusion permanent.
func repairMissingDicts(dir string, opts RepairOptions, r *RepairReport) {
	dictsDir := filepath.Join(dir, "dicts")
	have := map[uint32]bool{0: true}
	for _, f := range readDictFileFacts(dictsDir) {
		if f.usable {
			have[f.filedAs] = true
		}
	}
	want := map[uint32]bool{}
	for s := 0; ; s++ {
		shardDir := filepath.Join(dir, fmt.Sprintf("%d", s))
		entries, err := os.ReadDir(shardDir)
		if err != nil {
			break
		}
		for _, e := range entries {
			if id, ok := recoveredSegmentDictID(e.Name()); ok && !have[id] {
				want[id] = true
			}
		}
	}
	if len(want) == 0 {
		return
	}

	ids := make([]uint32, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	trailers := sweepDictReplicas(dir, countShardDirs(dir), want)
	for _, id := range ids {
		dict, from := findSurvivingDict(dictsDir, id, trailers)
		if dict == nil {
			continue
		}
		dst := filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id))
		if !opts.DryRun {
			if err := writeDictFile(dst, dict); err != nil {
				continue
			}
		}
		r.Dicts = append(r.Dicts, RepairDict{ID: id, Path: dst, From: from, Action: "restored"})
	}
}

// findSurvivingDict looks for a usable copy of a dictionary, cheapest source first.
func findSurvivingDict(dictsDir string, id uint32, trailers map[uint32]foundReplica) (dict []byte, from string) {
	backupDir := filepath.Join(dictsDir, dictBackupDir)
	if entries, err := os.ReadDir(backupDir); err == nil {
		prefix := fmt.Sprintf("%d-", id)
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".zst") {
				continue
			}
			p := filepath.Join(backupDir, e.Name())
			if b, rerr := os.ReadFile(p); rerr == nil && usableDictReplica(b, id) {
				return b, p
			}
		}
	}
	attic := filepath.Join(dictsDir, "attic", fmt.Sprintf("%d.zst", id))
	if b, err := os.ReadFile(attic); err == nil && usableDictReplica(b, id) {
		return b, attic
	}
	if rep, ok := trailers[id]; ok && usableDictReplica(rep.dict, id) {
		return rep.dict, rep.from
	}
	return nil, ""
}

// repairDicts is the dictionary half of a repair run: re-file what is in the wrong place,
// then restore what is missing. The order matters -- a dictionary re-filed into its proper
// slot is no longer missing, and restoring first would put a second copy of it there.
func repairDicts(dir string, opts RepairOptions, r *RepairReport) {
	dictsDir := filepath.Join(dir, "dicts")
	if _, err := os.Stat(dictsDir); err != nil {
		return // an identity-codec collection has no dictionaries
	}
	repairDictLabels(dictsDir, opts, r)
	repairMissingDicts(dir, opts, r)
	sort.SliceStable(r.Dicts, func(i, j int) bool { return r.Dicts[i].ID < r.Dicts[j].ID })
}
