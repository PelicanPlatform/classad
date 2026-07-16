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
