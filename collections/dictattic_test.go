package collections

// The scenario this guards: a segment file's name gets mangled (a restore, a
// half-finished move). Recovery ignores it silently, so its dictionary looks
// unreferenced -- and pruning used to DELETE that dictionary at the next clean open.
// The bytes were fine and the name was repairable, but without the dictionary nothing
// could ever decode them again.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestPruneRetiresDictionaryInsteadOfDeleting is the fix, tested where the decision
// is actually made. Pruning must never unlink a dictionary: the segments that need it
// may simply be invisible right now (a mangled file name), and the bytes are only
// recoverable while the dictionary still exists.
func TestPruneRetiresDictionaryInsteadOfDeleting(t *testing.T) {
	dir := t.TempDir()
	dictsDir := filepath.Join(dir, "dicts")
	if err := os.MkdirAll(dictsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	base := identityCodec{}
	reg := newDictReg(base)
	reg.dir = dictsDir

	mk := func(seed int) (Codec, uint32) {
		t.Helper()
		var samples [][]byte
		for i := 0; i < 1200; i++ {
			samples = append(samples, []byte(fmt.Sprintf(
				`[ Owner="u%d_%d@pool.example.edu"; Cmd="/usr/bin/run-%d.sh"; Args="--in /d/%d/p%05d --seed %d"; QDate=%d ]`,
				seed, i%97, i%41, i%29, i, i*7919, 1790000000+i)))
		}
		dict, err := TrainDictSize(samples, 1<<14)
		if err != nil {
			t.Skipf("dictionary training unavailable: %v", err)
		}
		codec, err := NewZSTDCodec(dict)
		if err != nil {
			t.Fatal(err)
		}
		id, err := reg.register(codec, dict)
		if err != nil {
			t.Fatal(err)
		}
		return codec, id
	}

	keepMe, keepID := mk(1)
	_, dropID := mk(2)
	t.Logf("registered dictionaries %d (kept) and %d (pruned)", keepID, dropID)

	for _, id := range []uint32{keepID, dropID} {
		if _, err := os.Stat(filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id))); err != nil {
			t.Fatalf("dictionary %d was not written: %v", id, err)
		}
	}

	removed := reg.prune(func(c Codec) bool { return c == keepMe })
	t.Logf("prune removed ids %v from the registry", removed)

	if _, err := os.Stat(filepath.Join(dictsDir, fmt.Sprintf("%d.zst", keepID))); err != nil {
		t.Errorf("the kept dictionary was disturbed: %v", err)
	}
	gone := filepath.Join(dictsDir, fmt.Sprintf("%d.zst", dropID))
	if _, err := os.Stat(gone); !os.IsNotExist(err) {
		t.Errorf("pruned dictionary should have left dicts/: %v", err)
	}
	retired := filepath.Join(dictsDir, "attic", fmt.Sprintf("%d.zst", dropID))
	if _, err := os.Stat(retired); err != nil {
		t.Fatalf("pruned dictionary %d was DESTROYED, not retired: %v\n"+
			"Any segment still naming it is now permanently undecodable.", dropID, err)
	}
	t.Logf("dictionary %d retired to dicts/attic/", dropID)
}

// TestAtticDictionaryStillDecodes: retiring is only worth anything if the file is
// still usable. Move it back and it must decode what it encoded.
func TestAtticDictionaryStillDecodes(t *testing.T) {
	dir := t.TempDir()
	dictsDir := filepath.Join(dir, "dicts")
	if err := os.MkdirAll(dictsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var samples [][]byte
	for i := 0; i < 1200; i++ {
		samples = append(samples, []byte(fmt.Sprintf(
			`[ Owner="u%d@pool.example.edu"; Cmd="/usr/bin/run-%d.sh"; Args="--in /d/%d/p%05d"; QDate=%d ]`,
			i%97, i%41, i%29, i, 1790000000+i)))
	}
	dict, err := TrainDictSize(samples, 1<<14)
	if err != nil {
		t.Skipf("dictionary training unavailable: %v", err)
	}
	codec, err := NewZSTDCodec(dict)
	if err != nil {
		t.Fatal(err)
	}
	reg := newDictReg(identityCodec{})
	reg.dir = dictsDir
	id, err := reg.register(codec, dict)
	if err != nil {
		t.Fatal(err)
	}
	plain := samples[42]
	frame := codec.Compress(nil, plain)

	reg.prune(func(Codec) bool { return false })

	raw, err := os.ReadFile(filepath.Join(dictsDir, "attic", fmt.Sprintf("%d.zst", id)))
	if err != nil {
		t.Fatalf("retired dictionary unreadable: %v", err)
	}
	restored, err := NewZSTDCodec(raw)
	if err != nil {
		t.Fatalf("retired dictionary did not rebuild a codec: %v", err)
	}
	got, err := restored.Decompress(nil, frame)
	if err != nil {
		t.Fatalf("retired dictionary could not decode what it encoded: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatal("retired dictionary decoded to the wrong bytes")
	}
	t.Log("a retired dictionary still decodes its segments after being moved back")
}
