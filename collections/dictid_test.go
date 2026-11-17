package collections

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// registerTrained registers a dictionary trained from a distinguishable corpus and
// returns the id and codec. seed varies the corpus so two calls produce dictionaries
// that genuinely differ.
func registerTrained(t *testing.T, reg *dictReg, seed int) (uint32, Codec) {
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
	id, codec, err := reg.register(dict, NewZSTDCodec)
	if err != nil {
		t.Fatal(err)
	}
	return id, codec
}

// TestRegisteredDictionaryNamesItsSlot: the whole point. Before stamping, every
// dictionary carried Dictionary_ID 1, so two dictionaries were identical for the first
// sixteen bytes and nothing but the file NAME said which was which.
func TestRegisteredDictionaryNamesItsSlot(t *testing.T) {
	dictsDir := filepath.Join(t.TempDir(), "dicts")
	reg := newDictReg(identityCodec{})
	reg.dir = dictsDir

	id1, _ := registerTrained(t, reg, 1)
	id2, _ := registerTrained(t, reg, 2)
	if id1 == id2 {
		t.Fatalf("registry handed out the same id twice: %d", id1)
	}

	for _, id := range []uint32{id1, id2} {
		b, err := os.ReadFile(filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id)))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := dictSlotOf(b)
		if !ok {
			t.Fatalf("dicts/%d.zst names no slot at all", id)
		}
		if got != id {
			t.Errorf("dicts/%d.zst names slot %d; want %d", id, got, id)
		}
	}

	a, err := os.ReadFile(filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id1)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dictsDir, fmt.Sprintf("%d.zst", id2)))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a[:16], b[:16]) {
		t.Errorf("two dictionaries still share their first 16 bytes: %x", a[:16])
	}
}

// TestWrongDictionaryIsNamedNotJustRejected is the behavioural half. A wrong dictionary
// was already detected -- the frame's content checksum fails -- but it surfaced as an
// ordinary bad-record decode error, which the read path treats as a record to skip. With
// the frame naming its dictionary, the decoder can say that it does not HAVE that
// dictionary, which is a different and recoverable fact.
func TestWrongDictionaryIsNamedNotJustRejected(t *testing.T) {
	reg := newDictReg(identityCodec{})
	_, codecA := registerTrained(t, reg, 1)
	_, codecB := registerTrained(t, reg, 2)

	payload := []byte(`[ Owner="u1_3@pool.example.edu"; Cmd="/usr/bin/run-7.sh"; Args="--in /d/5/p00042 --seed 332598"; QDate=1790000042 ]`)
	frame := codecA.Compress(nil, payload)

	// The control: its own dictionary round-trips.
	back, err := codecA.Decompress(nil, frame)
	if err != nil {
		t.Fatalf("own dictionary failed to decompress: %v", err)
	}
	if !bytes.Equal(back, payload) {
		t.Fatal("round trip through the stamped dictionary changed the bytes")
	}

	_, err = codecB.Decompress(nil, frame)
	if err == nil {
		t.Fatal("the wrong dictionary decoded a frame it did not compress")
	}
	// It must name the dictionary as the problem. "checksum" would mean we are back to a
	// frame that looks like an ordinary damaged record.
	if !strings.Contains(strings.ToLower(err.Error()), "dictionary") {
		t.Errorf("wrong-dictionary error does not name the dictionary: %v", err)
	}
}

// TestOpenRefusesRelabelledDictionary: swapping two dictionary files is the failure this
// is for. Both files are valid dictionaries, both load, and fsck sees nothing wrong with
// either -- so before stamping the store opened and then silently skipped every record in
// the segments naming them.
func TestOpenRefusesRelabelledDictionary(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	dicts := filepath.Join(dir, "dicts")
	one, two := filepath.Join(dicts, "1.zst"), filepath.Join(dicts, "2.zst")
	tmp := filepath.Join(dicts, "swap.tmp")
	for _, step := range [][2]string{{one, tmp}, {two, one}, {tmp, two}} {
		if err := os.Rename(step[0], step[1]); err != nil {
			t.Fatal(err)
		}
	}

	_, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err == nil {
		t.Fatal("Open accepted a store whose dictionary files had been swapped")
	}
	if !errors.Is(err, ErrDictIDMismatch) {
		t.Fatalf("error does not unwrap to ErrDictIDMismatch: %v", err)
	}
	var mm *DictIDMismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("error is not a *DictIDMismatchError: %v", err)
	}
	if mm.Slot == mm.Found {
		t.Errorf("mismatch reports slot == found (%d)", mm.Slot)
	}
	if !strings.Contains(err.Error(), mm.Path) {
		t.Errorf("message does not name the file: %v", err)
	}

	// And putting them back is the whole recovery.
	for _, step := range [][2]string{{one, tmp}, {two, one}, {tmp, two}} {
		if rerr := os.Rename(step[0], step[1]); rerr != nil {
			t.Fatal(rerr)
		}
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open after putting the dictionaries back: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestOpenAcceptsLegacyDictionary: every dictionary written before stamping carries
// Dictionary_ID 1 whatever slot it is in, so refusing on a mismatch there would refuse
// every store that already exists. Such a dictionary is unverifiable, not wrong, and is
// counted so an operator can see how much of a store still predates stamping.
func TestOpenAcceptsLegacyDictionary(t *testing.T) {
	dir, _ := buildTwoDictStore(t)
	path := filepath.Join(dir, "dicts", "2.zst")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(b[dictIDOff:], legacyDictID) // as an older build wrote it
	if werr := os.WriteFile(path, b, 0o644); werr != nil {
		t.Fatal(werr)
	}

	before := DictsUnverifiable()
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatalf("Open refused a store holding a pre-stamping dictionary: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
	if got := DictsUnverifiable(); got <= before {
		t.Errorf("DictsUnverifiable did not move (%d -> %d)", before, got)
	}
}

// TestStampDictID covers the guards. Stamping something that is not a dictionary, or
// stamping id 0 (the base codec, which has no dictionary and no file, and which the
// format reads as "no dictionary id"), must fail rather than produce a file that looks
// right and decodes nothing.
func TestStampDictID(t *testing.T) {
	good := testDictHeader("payload")
	out, err := stampDictID(good, 9)
	if err != nil {
		t.Fatalf("stamping a valid dictionary: %v", err)
	}
	if slot, ok := dictSlotOf(out); !ok || slot != 9 {
		t.Errorf("stamped dictionary names slot (%d, %v); want (9, true)", slot, ok)
	}
	// Registry ids start at 1 and legacyDictID IS 1, so the bias is what keeps the first
	// dictionary of every store from being stamped with the value that means "unstamped".
	one, err := stampDictID(good, 1)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := dictIDOf(one); raw == legacyDictID {
		t.Error("dictionary 1 was stamped with the pre-stamping sentinel")
	}
	if slot, ok := dictSlotOf(one); !ok || slot != 1 {
		t.Errorf("dictionary 1 names slot (%d, %v); want (1, true)", slot, ok)
	}
	if slot, ok := dictSlotOf(good); ok {
		t.Errorf("an unstamped dictionary names slot %d; want no answer at all", slot)
	}
	if id, _ := dictIDOf(good); id != legacyDictID {
		t.Error("stampDictID wrote through to the caller's slice")
	}
	if !bytes.Equal(out[8:], good[8:]) {
		t.Error("stampDictID changed bytes outside the id field")
	}

	for _, tc := range []struct {
		name string
		b    []byte
		id   uint32
	}{
		{"id zero", good, 0},
		{"id past what can be stamped", good, ^uint32(0)},
		{"not a dictionary", []byte("this is not a zstd dictionary"), 3},
		{"too short", []byte{0x37, 0xA4}, 3},
		{"empty", nil, 3},
	} {
		if _, err := stampDictID(tc.b, tc.id); err == nil {
			t.Errorf("%s: stampDictID succeeded", tc.name)
		}
	}
}

// TestDictIDOf pins the parse against the format: magic first, then the id.
func TestDictIDOf(t *testing.T) {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, dictMagic)
	binary.LittleEndian.PutUint32(b[4:], 0x01020304)
	if id, ok := dictIDOf(b); !ok || id != 0x01020304 {
		t.Errorf("dictIDOf = (%#x, %v); want (0x01020304, true)", id, ok)
	}
	if b[0] != 0x37 || b[1] != 0xA4 || b[2] != 0x30 || b[3] != 0xEC {
		t.Errorf("dictMagic does not serialize as 37 A4 30 EC: % x", b[:4])
	}
	for _, bad := range [][]byte{nil, {0x37, 0xA4, 0x30}, make([]byte, 16)} {
		if _, ok := dictIDOf(bad); ok {
			t.Errorf("dictIDOf accepted % x", bad)
		}
	}
}
