package collections

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A columnar-format bump must cost a BLOCK rebuild, not the accelerator.
//
// v5 changed the block payload, so readColSection rejects every v4 sidecar -- correct, since the payload
// cannot be read. But the derived schema was only ever recovered from a section that loaded, so rejecting the
// section threw the schema away too, and `.schema` reported "off — no derived schema" on every table after
// upgrading. Not a cold accelerator: no accelerator, until something re-derived a schema.
//
// Simulated by writing a sidecar and then poking its version, which is exactly what an older build's file
// looks like to this one.
func TestSchemaSurvivesSectionVersionBump(t *testing.T) {
	dir := t.TempDir()
	build := func() {
		c := New(Options{Shards: 1, SegmentSize: 1 << 16, Dir: dir})
		defer c.Close()
		for i := 0; i < 3000; i++ {
			src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nOwner = \"user%d\"",
				i, i%10, 1+i%5, 1024+(i%32)*512, i%8)
			if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, src)); err != nil {
				t.Fatal(err)
			}
		}
		for _, e := range []string{"ProcId >= 0", "JobStatus >= 0", "RequestMemory >= 0"} {
			q, err := vm.Parse(e)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 20; i++ {
				for range c.Query(q) {
				}
			}
		}
		if !c.BuildAndEnableSchemaScan(4000, 4) {
			t.Skip("no sealed segments to sample")
		}
		c.Reindex() // seals the sidecars, writing the col section
	}
	build()

	// Reopen: the schema must come back, which is the baseline the assertion below rests on.
	c := New(Options{Shards: 1, SegmentSize: 1 << 16, Dir: dir})
	if info := c.SchemaScanInfo(); !info.Enabled {
		c.Close()
		t.Skip("schema did not persist even at the current version; nothing to compare against")
	}
	fields := len(c.SchemaScanInfo().Schema)
	c.Close()

	// Now make every col section look like it came from a build with a different BLOCK format, which is what
	// a version bump does. The schema prefix is untouched, because a bump does not change it.
	bumped := corruptColSectionVersions(t, dir)
	if bumped == 0 {
		t.Skip("no col section found on disk to age")
	}

	c2 := New(Options{Shards: 1, SegmentSize: 1 << 16, Dir: dir})
	defer c2.Close()
	info := c2.SchemaScanInfo()
	if !info.Enabled {
		t.Fatalf("after a block-format bump the accelerator is OFF (%d sections aged); the schema is still "+
			"readable in those sections and should have been adopted so the blocks could rebuild", bumped)
	}
	if got := len(info.Schema); got != fields {
		t.Errorf("recovered %d schema fields after the bump, want %d", got, fields)
	}
	// And it must answer correctly while the blocks are absent -- a schema with no blocks must not be
	// mistaken for a loaded accelerator, or a scan would iterate zero blocks and skip every record.
	q, err := vm.Parse("ProcId >= 0")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range c2.Query(q) {
		n++
	}
	if n != 3000 {
		t.Errorf("scan saw %d records after the bump, want 3000 -- a schema without blocks was treated as a "+
			"loaded accelerator and records were skipped", n)
	}
}

// corruptColSectionVersions rewrites the VERSION field of every columnar section in dir's sidecars, making
// them look like they came from a build with a different block format. Returns how many it changed.
//
// It edits the version in place rather than constructing an old file, because the header's CRC covers the
// BODY -- so an aged section stays internally valid, which is precisely the state a real version bump leaves
// on disk and the state the recovery path has to handle.
func corruptColSectionVersions(t *testing.T, dir string) int {
	t.Helper()
	var magic [4]byte
	binary.LittleEndian.PutUint32(magic[:], colSectionMagic)
	changed := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hit := false
		for i := 0; i+colSectionHdr <= len(b); i++ {
			if !bytes.Equal(b[i:i+4], magic[:]) {
				continue
			}
			if binary.LittleEndian.Uint16(b[i+4:]) != colSectionVersion {
				continue
			}
			binary.LittleEndian.PutUint16(b[i+4:], colSectionVersion-1) // an older block format
			hit = true
			changed++
		}
		if hit {
			return os.WriteFile(path, b, 0o644)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("aging sidecars: %v", err)
	}
	return changed
}
