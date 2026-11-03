package db

import (
	"os"
	"path/filepath"
	"testing"
)

// TestChooseBaseCodec: a fresh store defaults to ZSTD; a store that already holds data
// (legacy identity default, no recorded choice) keeps identity; a recorded choice wins.
func TestChooseBaseCodec(t *testing.T) {
	// In-memory: always ZSTD.
	if c := chooseBaseCodec(""); c == nil || c.Name() == "identity" {
		t.Errorf("in-memory base codec = %v, want zstd", c)
	}

	// Fresh dir: ZSTD, and the choice is persisted.
	fresh := t.TempDir()
	if c := chooseBaseCodec(fresh); c == nil || c.Name() == "identity" {
		t.Errorf("fresh dir base codec = %v, want zstd", c)
	}
	if b, _ := os.ReadFile(filepath.Join(fresh, baseCodecFile)); string(b) != "zstd" {
		t.Errorf("fresh dir basecodec file = %q, want zstd", b)
	}
	// Reopen (choice recorded) stays ZSTD.
	if c := chooseBaseCodec(fresh); c == nil || c.Name() == "identity" {
		t.Errorf("reopened fresh dir base codec = %v, want zstd", c)
	}

	// Legacy dir: already has shard data but no recorded choice -> keep identity.
	legacy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacy, "0"), 0o755); err != nil { // simulate prior segments
		t.Fatal(err)
	}
	if c := chooseBaseCodec(legacy); c != nil {
		t.Errorf("legacy dir base codec = %v, want identity (nil)", c)
	}
	if b, _ := os.ReadFile(filepath.Join(legacy, baseCodecFile)); string(b) != "identity" {
		t.Errorf("legacy dir basecodec file = %q, want identity", b)
	}
}

// TestChooseBaseCodecIdentityWhenChoiceCannotBePersisted: the marker file is the only record of
// which codec id-0 records were written under, and a reopen that does not find it falls back to
// identity. A store that cannot write the marker must therefore not be compressed, or it comes
// back as a store that cannot decode its own records.
func TestChooseBaseCodecIdentityWhenChoiceCannotBePersisted(t *testing.T) {
	dir := t.TempDir()
	// A directory squatting on the marker's name makes both the read and the write fail, on
	// every platform and whatever the process's privileges are.
	if err := os.Mkdir(filepath.Join(dir, baseCodecFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if c := chooseBaseCodec(dir); c != nil {
		t.Fatalf("chose %q for a store whose codec choice could not be recorded; a reopen finds no marker and decodes those records as identity", c.Name())
	}
}
