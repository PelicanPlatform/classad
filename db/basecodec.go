package db

import (
	"os"
	"path/filepath"

	"github.com/PelicanPlatform/classad/collections"
)

// The base (dictionary id 0) codec is baked into every record written under it -- a
// persistent store's id-0 segments must be decoded with the same codec they were written
// with. So we cannot simply flip the default: an existing store created with the identity
// codec would fail to decode if reopened with ZSTD. chooseBaseCodec therefore defaults
// NEW stores to ZSTD (compression on from the first write) while PRESERVING the codec an
// existing store was created with, persisting the choice in <dir>/basecodec so it is
// stable across reopens. RetrainDict still layers a trained dictionary (id > 0) on top.
const baseCodecFile = "basecodec"

// chooseBaseCodec returns the base codec for a store at dir (empty = in-memory). New
// stores get ZSTD; a store that already holds data keeps identity (the historical
// default) unless a persisted choice says otherwise.
func chooseBaseCodec(dir string) collections.Codec {
	zstd := func() collections.Codec {
		c, err := collections.NewZSTDCodec(nil)
		if err != nil {
			return nil // fall back to the collection default (identity)
		}
		return c
	}
	if dir == "" {
		return zstd() // in-memory: always compress
	}
	_ = os.MkdirAll(dir, 0o755) // ensure the dir exists so the choice can be persisted
	path := filepath.Join(dir, baseCodecFile)
	if b, err := os.ReadFile(path); err == nil {
		if string(b) == "identity" {
			return nil
		}
		return zstd()
	}
	// No recorded choice: an existing store (already has shard data written under the old
	// identity default) keeps identity; a fresh store gets ZSTD. Persist it either way.
	if hasShardData(dir) {
		_ = os.WriteFile(path, []byte("identity"), 0o644)
		return nil
	}
	_ = os.WriteFile(path, []byte("zstd"), 0o644)
	return zstd()
}

// hasShardData reports whether dir already holds a prior store's segment data (shard 0's
// subdirectory exists once Open has written segments there).
func hasShardData(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "0"))
	return err == nil && fi.IsDir()
}
