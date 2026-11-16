package collections

// Recovering a store whose compression dictionaries are gone.
//
// Open refuses such a store (see verifyDictCoverage), which is right: the records are
// intact and a restored dictionary brings every one of them back, so the loss is
// recoverable right up until something writes believing those segments are gone. But a
// refusal alone leaves an operator with a daemon that will not start and no supported way
// forward, and the procedure they then invent by hand is the one below -- move the
// undecodable segments aside, drop the directory snapshots that describe them, start.
//
// The order of preference is not negotiable:
//
//  1. If the dictionary is in dicts/attic/, RESTORE it. Pruning retires dictionaries
//     there precisely so this is possible, and a restore costs nothing and loses nothing.
//     QuarantineDictless refuses to run while any missing dictionary is recoverable that
//     way, because quarantining instead would discard records it could have kept.
//  2. Otherwise quarantine, which keeps the bytes. A segment under quarantine/ is out of
//     the store but not destroyed: if the dictionary turns up later -- a backup, another
//     replica, a filesystem undelete -- the segment can be moved back and read.
//
// Nothing here deletes anything.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// DictlessOptions controls a QuarantineDictless pass.
type DictlessOptions struct {
	// DryRun reports what would move and writes nothing. Everything else is identical,
	// so a dry run is a faithful rehearsal rather than an estimate.
	DryRun bool
	// Force runs even when a missing dictionary is sitting in dicts/attic/. The default
	// refuses, because restoring that file keeps records this pass would set aside.
	Force bool
}

// DictlessSegment is one segment file the pass moved, or would move.
type DictlessSegment struct {
	Path   string // where it was
	DictID uint32 // the dictionary it names and the store does not have
	// Quarantined is where it went, or "" for a dry run.
	Quarantined string
	// Sidecars are the derived files moved with it (.idx, .kidx), by name.
	Sidecars []string
}

// DictlessReport is the outcome of a pass.
type DictlessReport struct {
	Dir     string
	DryRun  bool
	Moved   []DictlessSegment
	Missing []uint32 // the dictionary ids involved, ascending
	// Recoverable are missing dictionaries whose file is in dicts/attic/. Unless Force
	// was set, the pass does nothing while this is non-empty.
	Recoverable []uint32
	// DroppedSnapshots lists the shard directory snapshots removed. They record record
	// OFFSETS for segments that are no longer there, and they rebuild on the next open;
	// leaving a stale one is how a readable store becomes a wrong one.
	DroppedSnapshots []string
}

// String renders the report the way an operator wants to read it.
func (r *DictlessReport) String() string {
	var b []byte
	add := func(format string, a ...any) { b = append(b, fmt.Sprintf(format, a...)...) }
	what := "quarantine"
	if r.DryRun {
		what = "quarantine (dry run)"
	}
	add("%s %s\n", what, r.Dir)
	if len(r.Recoverable) > 0 {
		add("  REFUSED: dictionar%s %v %s in dicts/attic/\n",
			plural2(len(r.Recoverable), "y", "ies"), r.Recoverable,
			plural2(len(r.Recoverable), "is", "are"))
		add("    Move the file(s) back into dicts/ and reopen: every record in the\n")
		add("    segments naming them comes back. Quarantining would set those records\n")
		add("    aside instead. Pass Force to quarantine anyway.\n")
		return string(b)
	}
	add("  %d segment(s) naming missing dictionar%s %v\n",
		len(r.Moved), plural2(len(r.Missing), "y", "ies"), r.Missing)
	for _, s := range r.Moved {
		if r.DryRun {
			add("    would move %s (dict %d)\n", filepath.Base(s.Path), s.DictID)
			continue
		}
		add("    moved %s -> %s\n", filepath.Base(s.Path), s.Quarantined)
	}
	for _, p := range r.DroppedSnapshots {
		add("  dropped %s (rebuilt on the next open)\n", p)
	}
	if len(r.Moved) > 0 && !r.DryRun {
		add("  The segments are under %s/, not deleted: if the dictionary is found,\n", quarantineDir)
		add("  move them back and they are readable again.\n")
	}
	return string(b)
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// QuarantineDictless moves every segment file naming a dictionary the store does not have
// into quarantine/, and drops the directory snapshot of each shard it touched, so the
// store opens again without them.
//
// It never deletes: the segments keep their bytes under quarantine/, which Open does not
// address (it reads shard directories by number) and Fsck skips by name. If the dictionary
// turns up later the segments can be moved back.
//
// It refuses while any missing dictionary is sitting in dicts/attic/, since restoring that
// file keeps records this pass would otherwise set aside. Force overrides.
func QuarantineDictless(dir string, opts DictlessOptions) (*DictlessReport, error) {
	r := &DictlessReport{Dir: dir, DryRun: opts.DryRun}

	have, err := dictFilesIn(filepath.Join(dir, "dicts"))
	if err != nil {
		return nil, err
	}
	have[0] = true // the base codec is reconstructed from Options, never a file
	attic, err := dictFilesIn(filepath.Join(dir, "dicts", "attic"))
	if err != nil {
		return nil, err
	}

	shardDirs, err := numberedShardDirs(dir)
	if err != nil {
		return nil, err
	}

	missing := map[uint32]bool{}
	recoverable := map[uint32]bool{}
	type candidate struct {
		shardDir string
		name     string
		dictID   uint32
	}
	var cands []candidate
	for _, shardDir := range shardDirs {
		entries, rerr := os.ReadDir(shardDir)
		if rerr != nil {
			return nil, fmt.Errorf("quarantine: reading %s: %w", shardDir, rerr)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			id, ok := dictIDFromSegmentName(e.Name())
			if !ok || have[id] {
				continue
			}
			missing[id] = true
			if attic[id] {
				recoverable[id] = true
			}
			cands = append(cands, candidate{shardDir, e.Name(), id})
		}
	}
	r.Missing = sortedDictIDs(missing)
	r.Recoverable = sortedDictIDs(recoverable)
	if len(r.Recoverable) > 0 && !opts.Force {
		return r, nil // nothing moved: the better recovery is available
	}
	if len(cands) == 0 {
		return r, nil
	}

	touchedShards := map[string]bool{}
	for _, c := range cands {
		seg := DictlessSegment{Path: filepath.Join(c.shardDir, c.name), DictID: c.dictID}
		if opts.DryRun {
			r.Moved = append(r.Moved, seg)
			touchedShards[c.shardDir] = true
			continue
		}
		qdir := filepath.Join(dir, quarantineDir, filepath.Base(c.shardDir))
		if mkErr := os.MkdirAll(qdir, 0o755); mkErr != nil {
			return nil, mkErr
		}
		dst := filepath.Join(qdir, c.name)
		if mvErr := os.Rename(seg.Path, dst); mvErr != nil {
			return nil, fmt.Errorf("quarantine: moving %s: %w", seg.Path, mvErr)
		}
		seg.Quarantined = dst
		// The sidecars describe this segment's records and are worthless without it,
		// but they move rather than being removed: they are part of the evidence an
		// operator may want if the dictionary is recovered.
		for _, suffix := range []string{".idx", ".kidx"} {
			if mvErr := os.Rename(seg.Path+suffix, dst+suffix); mvErr == nil {
				seg.Sidecars = append(seg.Sidecars, c.name+suffix)
			}
		}
		r.Moved = append(r.Moved, seg)
		touchedShards[c.shardDir] = true
	}

	// A directory snapshot names record offsets inside segments that are no longer in the
	// shard. It is a cache that rebuilds on the next open, and a stale one turns a
	// readable store into a wrong one -- the same rule Repair follows.
	for _, shardDir := range sortedShardDirs(touchedShards) {
		snap := filepath.Join(shardDir, dirSnapName)
		if opts.DryRun {
			if _, statErr := os.Stat(snap); statErr == nil {
				r.DroppedSnapshots = append(r.DroppedSnapshots, snap)
			}
			continue
		}
		if rmErr := os.Remove(snap); rmErr == nil {
			r.DroppedSnapshots = append(r.DroppedSnapshots, snap)
		}
	}
	return r, nil
}

// dictFilesIn lists the dictionary ids with a file in dictsDir. A missing directory is
// not an error: it means no dictionary was ever trained, or the directory is gone, and
// both leave the caller with an empty set.
func dictFilesIn(dictsDir string) (map[uint32]bool, error) {
	out := map[uint32]bool{}
	entries, err := os.ReadDir(dictsDir)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("quarantine: reading %s: %w", dictsDir, err)
	}
	for _, e := range entries {
		var id uint32
		if _, serr := fmt.Sscanf(e.Name(), "%d.zst", &id); serr != nil {
			continue
		}
		out[id] = true
	}
	return out, nil
}

// numberedShardDirs lists the shard directories of a store, which are named by number.
// Addressing them this way is what keeps dicts/ and quarantine/ out of every walk.
func numberedShardDirs(dir string) ([]string, error) {
	var out []string
	for i := 0; ; i++ {
		shardDir := filepath.Join(dir, fmt.Sprintf("%d", i))
		if st, err := os.Stat(shardDir); err != nil || !st.IsDir() {
			break
		}
		out = append(out, shardDir)
	}
	if len(out) == 0 {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("quarantine: %w", err)
		}
	}
	return out, nil
}

func sortedDictIDs(set map[uint32]bool) []uint32 {
	out := make([]uint32, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortedShardDirs(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
