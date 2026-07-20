package collections

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteDictFileRecreatesMissingDir is the regression guard for the retrain
// failure "open .../dicts/N.zst: no such file or directory": if the dicts
// directory has gone missing at runtime, writing a dictionary must recreate it
// rather than fail with ENOENT.
func TestWriteDictFileRecreatesMissingDir(t *testing.T) {
	dir := t.TempDir()
	// dicts/ does not exist (simulating it having disappeared after Open).
	path := filepath.Join(dir, "dicts", "6.zst")
	if err := writeDictFile(path, []byte("dict-bytes")); err != nil {
		t.Fatalf("writeDictFile must recreate the missing dir, got: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "dict-bytes" {
		t.Fatalf("dict not written after dir recreate: %q err=%v", got, err)
	}
}

// TestRegisterRecreatesMissingDir checks the register path (the caller in
// RetrainDict) self-heals the missing dir and registers the codec, and that a
// failed write does NOT register a codec whose file is missing.
func TestRegisterRecreatesMissingDir(t *testing.T) {
	dir := t.TempDir()
	dictsDir := filepath.Join(dir, "dicts")
	r := newDictReg(identityCodec{})
	r.dir = dictsDir
	// dicts/ absent -> register must recreate it and persist the file.
	c := &countingCodec{}
	id, err := r.register(c, []byte("d"))
	if err != nil {
		t.Fatalf("register should self-heal the missing dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dictsDir, "1.zst")); err != nil {
		t.Errorf("dict file not written (id=%d): %v", id, err)
	}
	if got, ok := r.idFor(c); !ok || got != id {
		t.Errorf("codec not registered after successful write (id=%d ok=%v)", got, ok)
	}

	// Now make the write fail: put a regular file where the dicts dir must be, on a
	// fresh registry, so MkdirAll fails -> the codec must NOT be registered.
	dir2 := t.TempDir()
	blocker := filepath.Join(dir2, "dicts")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r2 := newDictReg(identityCodec{})
	r2.dir = blocker // MkdirAll(blocker) fails: it is a file
	c2 := &countingCodec{}
	if _, err := r2.register(c2, []byte("d")); err == nil {
		t.Fatal("register must fail when the dict file cannot be written")
	}
	if _, ok := r2.idFor(c2); ok {
		t.Error("a codec whose dict write failed must not be registered (recovery hazard)")
	}
}

// countingCodec is a distinct Codec value for registry tests (identity behavior).
type countingCodec struct{}

func (countingCodec) Name() string                       { return "test-counting" }
func (countingCodec) Compress(dst, src []byte) []byte    { return append(dst, src...) }
func (countingCodec) Decompress(dst, src []byte) ([]byte, error) { return append(dst, src...), nil }
