package collections

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// openCounts is what one Open reported about damage.
type openCounts struct {
	diag    OpenIndexDiag
	skipped int64 // DamagedRecordsSkipped rose by this
	past    int64 // RecordsPastDamage rose by this
}

// openCounting opens opts and returns the collection with what the open reported about damage.
func openCounting(t *testing.T, opts Options) (*Collection, openCounts) {
	t.Helper()
	var oc openCounts
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { oc.diag = d }
	defer func() { OpenIndexDiagHook = prev }()
	skipped, past := DamagedRecordsSkipped(), RecordsPastDamage()
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	oc.skipped, oc.past = DamagedRecordsSkipped()-skipped, RecordsPastDamage()-past
	return c, oc
}

// requireReads reads every key of orig from c: each reads exactly as written, or is one of missing.
// A missing key's read must count it unless the open did (openCounted). A scan must return exactly
// the ads of the keys that read.
func requireReads(t *testing.T, c *Collection, orig map[string]string, missing map[string]bool, openCounted bool) {
	t.Helper()
	kc := checkEveryKey(t, c, orig)
	got := map[string]bool{}
	for _, k := range kc.missing {
		got[k] = true
		if !missing[k] {
			t.Errorf("key %s did not read", k)
		}
	}
	for k := range missing {
		if !got[k] {
			t.Errorf("key %s read, though its record is damaged", k)
		}
	}
	if len(kc.uncounted) > 0 && !openCounted {
		t.Errorf("%d missing keys were not counted: %v", len(kc.uncounted), kc.uncounted)
	}
	if n, _ := checkScanAds(t, c, orig); n != len(orig)-len(missing) {
		t.Errorf("a scan returned %d ads, want %d", n, len(orig)-len(missing))
	}
}

// requireAggregates checks a count, a group count and a numeric aggregate against the ads that read.
// On a columnarized store these answer from the columns, which have a row for a damaged record too.
func requireAggregates(t *testing.T, c *Collection, orig map[string]string, missing map[string]bool) {
	t.Helper()
	wantCount, wantN, wantSum := 0, 0, int64(0)
	wantGroups := map[int64]int{}
	for k, s := range orig {
		if missing[k] {
			continue
		}
		ad, err := classad.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := ad.EvaluateAttrInt("JobStatus"); ok && v == 3 {
			wantCount++
		}
		if v, ok := ad.EvaluateAttrInt("ProcId"); ok {
			wantN++
			wantSum += v
		}
		if v, ok := ad.EvaluateAttrInt("JobStatus"); ok {
			wantGroups[v]++
		}
	}
	if n, ok := c.CountConstraint("JobStatus == 3"); !ok || n != wantCount {
		t.Errorf("CountConstraint = %d, %v; want %d", n, ok, wantCount)
	}
	if st, ok := c.NumStatsQuery(nil, "ProcId"); !ok || st.N != wantN || int64(st.Sum) != wantSum {
		t.Errorf("NumStatsQuery(ProcId) = %+v, %v; want n=%d sum=%d", st, ok, wantN, wantSum)
	}
	groups, ok := c.GroupCountAll("JobStatus")
	if !ok {
		t.Fatal("GroupCountAll did not answer")
	}
	gotGroups := map[int64]int{}
	for _, g := range groups {
		v, _ := g.Value.IntValue()
		gotGroups[v] += g.Count
	}
	if fmt.Sprint(gotGroups) != fmt.Sprint(wantGroups) {
		t.Errorf("GroupCountAll(JobStatus) = %v, want %v", gotGroups, wantGroups)
	}
}

// requireWatchReplay replays a watch from empty and requires an upsert for exactly the keys that read,
// each with its ad as written.
func requireWatchReplay(t *testing.T, c *Collection, orig map[string]string, missing map[string]bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := c.Watch(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for ev := range events {
		if ev.Kind == WatchSynced {
			break
		}
		if ev.Kind != WatchUpsert {
			continue
		}
		k := string(ev.Key)
		if want, ok := orig[k]; !ok || ev.Ad.String() != want {
			t.Fatalf("watch replayed %s as %s", k, ev.Ad)
		}
		seen[k] = true
	}
	for k := range orig {
		if seen[k] == missing[k] {
			t.Errorf("watch replay: key %s seen=%v, damaged=%v", k, seen[k], missing[k])
		}
	}
}

// keyedOffsets returns the offsets of a segment file's keyed records, walking it by length.
func keyedOffsets(t *testing.T, path string) (data []byte, offs []int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range recordOffsets(t, path) {
		if !recIsMarker(data, uint32(o)) {
			offs = append(offs, o)
		}
	}
	return data, offs
}

// damageKey flips a bit in the key of the record at off and returns the key as written.
func damageKey(t *testing.T, path string, data []byte, off int) string {
	t.Helper()
	k := string(recKey(data, uint32(off)))
	flipBit(t, path, off+recKeyOff+len(k)-1)
	return k
}

// removeFile removes path, which must exist.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// craftRecord returns a whole record image with a valid checksum, as a segment would hold it.
func craftRecord(seq uint64, key, ad []byte) []byte {
	b := make([]byte, recordLen(len(key), len(ad)))
	binary.LittleEndian.PutUint64(b[recSeqOff:], seq)
	binary.LittleEndian.PutUint64(b[recSupOff:], seqMax)
	binary.LittleEndian.PutUint32(b[recNextSegOff:], noSeg)
	binary.LittleEndian.PutUint32(b[recTotalLenOff:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[recKeyLenOff:], uint32(len(key)))
	copy(b[recKeyOff:], key)
	adLenOff := recKeyOff + len(key)
	binary.LittleEndian.PutUint32(b[adLenOff:], uint32(len(ad)))
	copy(b[adLenOff+4:], ad)
	crcOff := adLenOff + 4 + len(ad)
	binary.LittleEndian.PutUint32(b[crcOff:], recCRC(b, crcOff))
	return b
}

// damageTotal flips a high bit of the totalLen of the record at off, so it points far past the
// segment, and returns the record's key.
func damageTotal(t *testing.T, path string, data []byte, off int) string {
	t.Helper()
	flipBits(t, path, off+recTotalLenOff+2, 0x10)
	return string(recKey(data, uint32(off)))
}

// sidecarDamage returns the damage set a segment's sidecar records, or nil.
func sidecarDamage(t *testing.T, path string) *damageSet {
	t.Helper()
	dm := readSidecarTrailer(path + ".idx").damage
	return dm
}

// TestRecoverPastDamage: with no sidecar to supply its extent, Open's recovery walk stopped at the first
// record failing its checksum. In a columnarized segment that left the dictionary and payload unread, so
// 1475 of 2000 ads came back. The payload names every record's offset, which proves the framing past the
// damage: Open now continues, sets the damaged records aside, and reads everything else. Every key must
// read exactly as written or, for a damaged record, be missing and counted.
func TestRecoverPastDamage(t *testing.T) {
	base := t.TempDir()
	payloadFixture(t, base)
	orig := getEveryKey(t, payloadFixtureOpts(base), payloadFixtureAds)

	// setup copies the fixture, damages it, and removes the target segment's sidecar.
	setup := func(t *testing.T, damage func(path string, data []byte, offs []int) map[string]bool) (string, string, map[string]bool) {
		dir := copyStore(t, base)
		target := firstColumnarSegment(t, dir)
		data, offs := keyedOffsets(t, target)
		missing := damage(target, data, offs)
		removeFile(t, target+".idx")
		return dir, target, missing
	}

	t.Run("remnant", func(t *testing.T) {
		dir, target, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			return map[string]bool{damageKey(t, p, data, offs[len(offs)/2]): true}
		})
		r, err := Fsck(dir)
		if err != nil {
			t.Fatal(err)
		}
		st := fsckSegmentOf(t, r, target).OpenStop
		if st == nil || !st.Recovered || st.Damaged != 1 || st.Records != 0 || st.Loaded == 0 {
			t.Errorf("fsck OpenStop = %+v, want recovered past 1 damaged record", st)
		}
		if !strings.Contains(r.String(), "the framing past it is proven") {
			t.Errorf("the fsck report does not say the open continues:\n%s", r.String())
		}
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if oc.skipped != 1 || oc.past != 0 {
			t.Errorf("DamagedRecordsSkipped rose by %d and RecordsPastDamage by %d, want 1 and 0", oc.skipped, oc.past)
		}
		if ds := oc.diag.DamageStops; len(ds) != 1 || !ds[0].Recovered || ds[0].Path != target || ds[0].Damaged != 1 {
			t.Errorf("OpenIndexDiag.DamageStops = %+v, want one recovered stop in %s", ds, target)
		}
		requireReads(t, c, orig, missing, true)
		requireAggregates(t, c, orig, missing)
	})

	t.Run("two-records", func(t *testing.T) {
		dir, _, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			return map[string]bool{
				damageKey(t, p, data, offs[len(offs)/3]):   true,
				damageKey(t, p, data, offs[2*len(offs)/3]): true,
			}
		})
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if oc.skipped != 2 || oc.past != 0 {
			t.Errorf("DamagedRecordsSkipped rose by %d and RecordsPastDamage by %d, want 2 and 0", oc.skipped, oc.past)
		}
		requireReads(t, c, orig, missing, true)
		requireAggregates(t, c, orig, missing)
	})

	t.Run("last-record", func(t *testing.T) {
		// The last row's damage ends where the dictionary begins, not where its totalLen says.
		dir, _, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			return map[string]bool{damageTotal(t, p, data, offs[len(offs)-1]): true}
		})
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if oc.skipped != 1 {
			t.Errorf("DamagedRecordsSkipped rose by %d, want 1", oc.skipped)
		}
		requireReads(t, c, orig, missing, true)
	})

	t.Run("total-length", func(t *testing.T) {
		// A damaged totalLen points far past the segment; every walk that stepped by it would stop
		// there. The payload's rows say where the record really ends.
		dir, _, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			return map[string]bool{damageTotal(t, p, data, offs[len(offs)/2]): true}
		})
		opts := payloadFixtureOpts(dir)
		opts.WatchHistory = 64
		c, oc := openCounting(t, opts)
		defer c.Close()
		if oc.skipped != 1 || oc.past != 0 {
			t.Errorf("DamagedRecordsSkipped rose by %d and RecordsPastDamage by %d, want 1 and 0", oc.skipped, oc.past)
		}
		requireReads(t, c, orig, missing, true)
		requireAggregates(t, c, orig, missing)
		requireWatchReplay(t, c, orig, missing)
	})

	t.Run("first-record", func(t *testing.T) {
		// A damaged seq in a segment's first record would misplace the whole segment in append
		// order, which is read from that seq.
		var order []string
		{
			c, err := Open(payloadFixtureOpts(base))
			if err != nil {
				t.Fatal(err)
			}
			for _, seg := range c.shards[0].segs {
				order = append(order, filepath.Base(seg.path))
			}
			c.Close()
		}
		dir, _, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			flipBits(t, p, offs[0]+recSeqOff+6, 0x01)
			return map[string]bool{string(recKey(data, uint32(offs[0]))): true}
		})
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if oc.skipped != 1 {
			t.Errorf("DamagedRecordsSkipped rose by %d, want 1", oc.skipped)
		}
		var got []string
		for _, seg := range c.shards[0].segs {
			got = append(got, filepath.Base(seg.path))
		}
		if fmt.Sprint(got) != fmt.Sprint(order) {
			t.Errorf("segment order %v, want %v", got, order)
		}
		requireReads(t, c, orig, missing, true)
	})

	t.Run("no-read-verification", func(t *testing.T) {
		// A damaged record is refused by the damage set itself, not only by read verification.
		dir, _, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			o := offs[len(offs)/2]
			flipRecordBody(t, p, o)
			return map[string]bool{string(recKey(data, uint32(o))): true}
		})
		opts := payloadFixtureOpts(dir)
		opts.DisableReadVerification = true
		c, _ := openCounting(t, opts)
		defer c.Close()
		requireReads(t, c, orig, missing, true)
		requireAggregates(t, c, orig, missing)
	})

	t.Run("forged-record", func(t *testing.T) {
		// A whole record image with a valid checksum inside a damaged record -- an ad can hold one.
		// A resync finds it, and it is no row of the payload, so the framing is not proven: the
		// walk stops at the damage, and the forged record is never loaded.
		var inSeg map[string]bool
		dir, _, _ := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			inSeg = keysOfSegment(t, p)
			for i := len(offs) / 2; i+1 < len(offs); i++ {
				o := offs[i]
				fake := craftRecord(1, []byte("forged"), nil)
				if offs[i+1]-o < len(fake)+8 {
					continue
				}
				copy(data[o+8:], fake)
				if err := os.WriteFile(p, data, 0o644); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			t.Fatal("no record large enough to hold a forged one")
			return nil
		})
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if ds := oc.diag.DamageStops; len(ds) != 1 || ds[0].Recovered || oc.past == 0 || oc.skipped != 0 {
			t.Errorf("DamageStops %+v, RecordsPastDamage +%d, DamagedRecordsSkipped +%d; want a counted stop",
				ds, oc.past, oc.skipped)
		}
		if _, ok := c.Get([]byte("forged")); ok {
			t.Fatal("the forged record was loaded")
		}
		requireReads(t, c, orig, inSeg, true)
	})

	t.Run("rows-do-not-tile", func(t *testing.T) {
		// A payload whose rows do not account for every byte before it does not prove the framing,
		// even when every record that verifies is a row: here one row names the next record instead
		// of the damaged one, and nothing would delimit the damage.
		dir, _, _ := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			k := len(offs) / 2
			damageKey(t, p, data, offs[k])
			damageKey(t, p, data, offs[k+5]) // a second, properly delimited damaged row
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			pay, _ := payloadOffset(b)
			blob := append([]byte(nil), recAd(b, uint32(pay))...)
			cs, err := unmarshalColPayload(blob, identityCodec{}, fsckInterner())
			if err != nil {
				t.Fatal(err)
			}
			rows := make([]uint32, cs.offsLen())
			for i := range rows {
				rows[i] = cs.offAt(i)
			}
			at := bytes.Index(blob, packU32s(rows))
			if at < 0 || int(rows[k]) != offs[k] {
				t.Fatal("cannot find the payload's offset map")
			}
			binary.LittleEndian.PutUint32(blob[at+4*k:], rows[k+1])
			rewritePayload(t, b, pay, blob)
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return nil
		})
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if ds := oc.diag.DamageStops; len(ds) != 1 || ds[0].Recovered {
			t.Errorf("DamageStops %+v, want a stop that is not recovered", ds)
		}
	})

	t.Run("written-after-payload", func(t *testing.T) {
		// The payload must be the segment's last record; with anything written after it, it does not
		// prove where the segment ends.
		dir, _, _ := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			damageKey(t, p, data, offs[len(offs)/2])
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			pay, _ := payloadOffset(b)
			end := pay + int(recTotalLen(b, uint32(pay)))
			flipBits(t, p, end+recTotalLenOff, 0x08)
			return nil
		})
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if ds := oc.diag.DamageStops; len(ds) != 1 || ds[0].Recovered {
			t.Errorf("DamageStops %+v, want a stop that is not recovered", ds)
		}
	})

	t.Run("dictionary", func(t *testing.T) {
		// The framing is proven, but the records are keyed by a dictionary that does not verify, so
		// none of them can be read; each read says so.
		var inSeg map[string]bool
		dir, target, _ := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			for _, o := range recordOffsets(t, p) {
				if recIsDict(data, uint32(o)) {
					flipBit(t, p, o+recKeyOff+4+16)
				}
			}
			inSeg = keysOfSegment(t, p)
			return nil
		})
		unusable := SegDictsUnusable()
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if ds := oc.diag.DamageStops; len(ds) != 1 || !ds[0].Recovered || ds[0].Path != target || !ds[0].Dictionary {
			t.Errorf("OpenIndexDiag.DamageStops = %+v, want a recovered stop at the dictionary", ds)
		}
		if SegDictsUnusable() == unusable {
			t.Error("the damaged dictionary was not refused")
		}
		requireReads(t, c, orig, inSeg, false)
	})

	t.Run("reopen", func(t *testing.T) {
		// The damage set goes into the sidecar written at Close. The next open trusts the extent
		// and still skips the damaged record, without walking the segment.
		dir, target, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			return map[string]bool{damageTotal(t, p, data, offs[len(offs)/2]): true}
		})
		c, _ := openCounting(t, payloadFixtureOpts(dir))
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if dm := sidecarDamage(t, target); dm.len() != 1 {
			t.Fatalf("the sidecar records %d damaged spans, want 1", dm.len())
		}
		r, err := Fsck(dir)
		if err != nil {
			t.Fatal(err)
		}
		if s := fsckSegmentOf(t, r, target); !s.ExtentTrusted || s.SidecarDamage != 1 || s.OpenStop != nil {
			t.Errorf("fsck: trusted %v, sidecar damage %d, open stop %+v; want a trusted extent recording 1 span",
				s.ExtentTrusted, s.SidecarDamage, s.OpenStop)
		}
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if len(oc.diag.DamageStops) != 0 || oc.skipped != 1 {
			t.Errorf("reopen: %d damage stops, DamagedRecordsSkipped +%d; want none and 1", len(oc.diag.DamageStops), oc.skipped)
		}
		requireReads(t, c, orig, missing, true)
		requireAggregates(t, c, orig, missing)
	})

	t.Run("compact", func(t *testing.T) {
		// Compaction (here forced by Rewrite) leaves the damaged record out, and the rewritten store
		// carries no damage.
		dir, _, missing := setup(t, func(p string, data []byte, offs []int) map[string]bool {
			return map[string]bool{damageTotal(t, p, data, offs[len(offs)/2]): true}
		})
		c, _ := openCounting(t, payloadFixtureOpts(dir))
		dropped := DamagedRecordsDropped()
		if n := c.Rewrite(); n != len(orig)-len(missing) {
			t.Fatalf("Rewrite rewrote %d ads, want %d", n, len(orig)-len(missing))
		}
		if d := DamagedRecordsDropped() - dropped; d != 1 {
			t.Errorf("DamagedRecordsDropped rose by %d, want 1", d)
		}
		requireReads(t, c, orig, missing, true)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c, oc := openCounting(t, payloadFixtureOpts(dir))
		defer c.Close()
		if oc.skipped != 0 || len(oc.diag.DamageStops) != 0 {
			t.Errorf("after compaction: DamagedRecordsSkipped +%d, %d damage stops; want none", oc.skipped, len(oc.diag.DamageStops))
		}
		requireReads(t, c, orig, missing, true)
	})

	// A damaged record whose header names a live key in another segment, with a newer seq. Rebuilding
	// the directory picks each key's newest record and supersedes the rest on disk, so believing this
	// one would retire the live record. collide damages the target segment that way.
	collide := func(t *testing.T, dir string) (target, live string, missing map[string]bool) {
		target = firstColumnarSegment(t, dir)
		data, offs := keyedOffsets(t, target)
		o := offs[len(offs)/2]
		own := string(recKey(data, uint32(o)))
		inSeg := keysOfSegment(t, target)
		for k := range orig {
			if !inSeg[k] && len(k) == len(own) {
				live = k
				break
			}
		}
		if live == "" {
			t.Fatal("no key of the same length outside the target segment")
		}
		copy(data[o+recKeyOff:], live)
		binary.LittleEndian.PutUint64(data[o+recSeqOff:], 1<<40)
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
		// Without the directory snapshot the directory is rebuilt from the records.
		removeFile(t, filepath.Join(filepath.Dir(target), dirSnapName))
		return target, live, map[string]bool{own: true}
	}
	for _, sidecar := range []bool{true, false} {
		t.Run(fmt.Sprintf("collision/sidecar=%v", sidecar), func(t *testing.T) {
			dir := copyStore(t, base)
			target, live, missing := collide(t, dir)
			if !sidecar {
				removeFile(t, target+".idx")
			}
			c, oc := openCounting(t, payloadFixtureOpts(dir))
			if oc.skipped != 1 {
				t.Errorf("DamagedRecordsSkipped rose by %d, want 1", oc.skipped)
			}
			if ad, ok := c.Get([]byte(live)); !ok || ad.String() != orig[live] {
				t.Fatalf("live key %s reads %v, %v after a damaged record claimed it", live, ad, ok)
			}
			requireReads(t, c, orig, missing, true)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if dm := sidecarDamage(t, target); dm.len() != 1 {
				t.Errorf("the sidecar records %d damaged spans, want 1", dm.len())
			}
			// Once more without the snapshot: the directory is rebuilt again, from the persisted set.
			removeFile(t, filepath.Join(filepath.Dir(target), dirSnapName))
			c, _ = openCounting(t, payloadFixtureOpts(dir))
			defer c.Close()
			requireReads(t, c, orig, missing, true)
		})
	}
}

// TestDamageInTrustedExtent: a sealed segment whose sidecar supplies its extent is not walked at open,
// and a damaged record inside it was read by header wherever a walk passed it. When the directory is
// rebuilt it walks every record: a walk misframed by a damaged totalLen is reframed from the sidecar's
// key index, the damaged record is set aside, and the sidecar records it for the next open. A rewrite
// checks every record before copying it, so a damaged one is left out rather than given a fresh
// checksum.
func TestDamageInTrustedExtent(t *testing.T) {
	base := t.TempDir()
	buildInternedStore(t, base)
	orig := getEveryKey(t, internedStoreOpts(base), internedAds)

	// damage copies the store and damages the middle record of its first interned segment.
	damage := func(t *testing.T, how func(path string, data []byte, off int) string) (string, string, map[string]bool) {
		dir := copyStore(t, base)
		target, _ := firstInternedSegment(t, dir)
		data, offs := keyedOffsets(t, target)
		return dir, target, map[string]bool{how(target, data, offs[len(offs)/2]): true}
	}
	// keysAre requires c's keys to be orig's less missing: no damaged key, and none it was copied under.
	keysAre := func(t *testing.T, c *Collection, missing map[string]bool) {
		t.Helper()
		for _, k := range c.Keys() {
			if _, ok := orig[k]; !ok || missing[k] {
				t.Errorf("Keys holds %q, which was not written or is damaged", k)
			}
		}
	}

	t.Run("total-length", func(t *testing.T) {
		dir, target, missing := damage(t, func(p string, data []byte, o int) string { return damageTotal(t, p, data, o) })
		removeFile(t, filepath.Join(filepath.Dir(target), dirSnapName))
		opts := internedStoreOpts(dir)
		c, oc := openCounting(t, opts)
		if oc.skipped != 1 || oc.past != 0 {
			t.Errorf("DamagedRecordsSkipped rose by %d and RecordsPastDamage by %d, want 1 and 0", oc.skipped, oc.past)
		}
		requireReads(t, c, orig, missing, true)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if dm := sidecarDamage(t, target); dm.len() != 1 {
			t.Fatalf("the sidecar records %d damaged spans, want 1", dm.len())
		}
		// The next open restores the directory snapshot and walks nothing; the sidecar says what to skip.
		c, oc = openCounting(t, opts)
		defer c.Close()
		if oc.skipped != 1 {
			t.Errorf("reopen: DamagedRecordsSkipped rose by %d, want 1", oc.skipped)
		}
		requireReads(t, c, orig, missing, true)
	})

	t.Run("total-length-snapshot", func(t *testing.T) {
		// With the directory snapshot the open walks no records, so nothing finds the damage before a
		// scan does. A scan it cuts short must say so, and the next open must walk the records.
		dir, target, missing := damage(t, func(p string, data []byte, o int) string { return damageTotal(t, p, data, o) })
		c, _ := openCounting(t, internedStoreOpts(dir))
		before := readSkipsOf("framing")
		n := 0
		for range c.Scan() {
			n++
		}
		t.Logf("a scan returned %d of %d ads; framing losses counted: %d", n, len(orig)-len(missing), readSkipsOf("framing")-before)
		if n < len(orig)-len(missing) && readSkipsOf("framing") == before {
			t.Errorf("a scan returned %d of %d ads and counted no lost framing", n, len(orig)-len(missing))
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c, oc := openCounting(t, internedStoreOpts(dir))
		defer c.Close()
		if oc.skipped != 1 {
			t.Errorf("reopen: DamagedRecordsSkipped rose by %d, want 1", oc.skipped)
		}
		if dm := sidecarDamage(t, target); dm.len() != 1 {
			t.Errorf("the sidecar records %d damaged spans, want 1", dm.len())
		}
		requireReads(t, c, orig, missing, true)
	})

	t.Run("key-length", func(t *testing.T) {
		// A key length running past the segment: a walk that read the key without checking the CRC
		// sliced past the mapping and panicked.
		dir, target, missing := damage(t, func(p string, data []byte, o int) string {
			flipBits(t, p, o+recKeyLenOff+3, 0x01)
			return string(recKey(data, uint32(o)))
		})
		c, _ := openCounting(t, internedStoreOpts(dir))
		requireReads(t, c, orig, missing, true)
		keysAre(t, c, missing)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		removeFile(t, filepath.Join(filepath.Dir(target), dirSnapName))
		c, _ = openCounting(t, internedStoreOpts(dir))
		defer c.Close()
		requireReads(t, c, orig, missing, true)
	})

	t.Run("rewrite", func(t *testing.T) {
		dir, _, missing := damage(t, func(p string, data []byte, o int) string { return damageKey(t, p, data, o) })
		c, _ := openCounting(t, internedStoreOpts(dir))
		defer c.Close()
		dropped := DamagedRecordsDropped()
		c.Rewrite()
		if d := DamagedRecordsDropped() - dropped; d != 1 {
			t.Errorf("DamagedRecordsDropped rose by %d, want 1", d)
		}
		requireReads(t, c, orig, missing, true)
		keysAre(t, c, missing)
	})

	t.Run("columnarize", func(t *testing.T) {
		dir, _, missing := damage(t, func(p string, data []byte, o int) string { return damageKey(t, p, data, o) })
		c, _ := openCounting(t, internedStoreOpts(dir))
		defer c.Close()
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("schema scan did not enable")
		}
		if segs, _ := columnarCensus(c); segs == 0 {
			t.Fatal("no segment was columnarized")
		}
		requireReads(t, c, orig, missing, true)
		keysAre(t, c, missing)
	})
}

// TestRecoverPastDamageNonColumnar: a segment that is not columnarized has no payload to prove its
// framing. Without a sidecar there is no proof at all, and the walk still stops at the damage and
// counts what it leaves. A sidecar whose extent is not trusted still lists every record's offset in
// its key index, and that is proof enough.
func TestRecoverPastDamageNonColumnar(t *testing.T) {
	base := t.TempDir()
	buildInternedStore(t, base)
	orig := getEveryKey(t, internedStoreOpts(base), internedAds)

	damage := func(t *testing.T) (string, string, string) {
		dir := copyStore(t, base)
		target, _ := firstInternedSegment(t, dir)
		data, offs := keyedOffsets(t, target)
		o := offs[len(offs)/2]
		flipRecordBody(t, target, o)
		return dir, target, string(recKey(data, uint32(o)))
	}

	t.Run("no-sidecar", func(t *testing.T) {
		dir, target, _ := damage(t)
		inSeg := keysOfSegment(t, target)
		removeFile(t, target+".idx")
		c, oc := openCounting(t, internedStoreOpts(dir))
		defer c.Close()
		if ds := oc.diag.DamageStops; len(ds) != 1 || ds[0].Recovered || oc.past == 0 || oc.skipped != 0 {
			t.Errorf("DamageStops %+v, RecordsPastDamage +%d, DamagedRecordsSkipped +%d; want a counted stop",
				ds, oc.past, oc.skipped)
		}
		// The dictionary is past the stop, so the records before it fail to read too (counted).
		requireReads(t, c, orig, inSeg, true)
	})

	t.Run("untrusted-extent", func(t *testing.T) {
		dir, target, key := damage(t)
		// Zero the extent the trailer records: the sidecar is no longer trusted for it.
		b, err := os.ReadFile(target + ".idx")
		if err != nil {
			t.Fatal(err)
		}
		if binary.LittleEndian.Uint32(b[len(b)-4:]) != sidecarContainerMagicV4 {
			t.Fatal("the fixture's sidecar is not a v4 container")
		}
		binary.LittleEndian.PutUint64(b[len(b)-sidecarTrailerLenV4+16:], 0)
		if err := os.WriteFile(target+".idx", b, 0o644); err != nil {
			t.Fatal(err)
		}
		c, oc := openCounting(t, internedStoreOpts(dir))
		defer c.Close()
		if ds := oc.diag.DamageStops; len(ds) != 1 || !ds[0].Recovered || oc.past != 0 || oc.skipped != 1 {
			t.Errorf("DamageStops %+v, RecordsPastDamage +%d, DamagedRecordsSkipped +%d; want a recovered stop",
				ds, oc.past, oc.skipped)
		}
		requireReads(t, c, orig, map[string]bool{key: true}, true)
	})
}

// TestDamageSidecarFormat: the damage set round-trips through a v5 sidecar, a container a pre-v5 reader
// refuses (so it walks the segment rather than trust an extent without the set), and a set that is
// malformed or does not fit its segment is refused.
func TestDamageSidecarFormat(t *testing.T) {
	dm := newDamageSet([]damageSpan{{64, 128}, {16, 32}})
	got, ok := decodeDamage(encodeDamage(dm))
	if !ok || fmt.Sprint(got.spans) != fmt.Sprint([]damageSpan{{16, 32}, {64, 128}}) {
		t.Fatalf("round trip = %v, %v", got, ok)
	}
	if end, ok := got.skip(20); !ok || end != 32 {
		t.Errorf("skip(20) = %d, %v; want 32", end, ok)
	}
	if got.covers(32) || !got.covers(16) || got.covers(8) {
		t.Error("covers is wrong at a span's edges")
	}
	bad := encodeDamage(dm)
	bad[9] ^= 1
	if _, ok := decodeDamage(bad); ok {
		t.Error("a damage set with a bad checksum decoded")
	}
	overlap := encodeDamage(&damageSet{spans: []damageSpan{{16, 64}, {32, 72}}})
	if _, ok := decodeDamage(overlap); ok {
		t.Error("overlapping spans decoded")
	}

	key := []byte("key-section")
	v5 := buildSegmentSidecar([]byte("attr"), key, nil, nil, 4096, 8, 0, encodeDamage(dm))
	if sc, isV5 := splitSidecarV5(v5); !sc.ok || !isV5 || !bytes.Equal(sc.key, key) || sc.used != 4096 || sc.dictRec != 8 {
		t.Fatalf("v5 split: %+v, v5=%v", sc, isV5)
	}
	// A pre-v5 reader finds its container magic in the last four bytes of a trailer of its own
	// length, and none of them is there.
	tail := binary.LittleEndian.Uint32(v5[len(v5)-4:])
	for _, m := range []uint32{sidecarContainerMagic, sidecarContainerMagicV2, sidecarContainerMagicV3, sidecarContainerMagicV4} {
		if tail == m {
			t.Errorf("a v5 container ends in the pre-v5 magic %#x", m)
		}
	}
	if _, k, _, _, ok := splitSegmentSidecar(v5); !ok || !bytes.Equal(k, key) {
		t.Error("splitSegmentSidecar does not read a v5 container")
	}
	path := filepath.Join(t.TempDir(), "seg.idx")
	if err := os.WriteFile(path, v5, 0o644); err != nil {
		t.Fatal(err)
	}
	if f := readSidecarTrailer(path); f.used != 4096 || f.dictRec != 8 || f.damage.len() != 2 {
		t.Errorf("readSidecarTrailer = %d, %d, %v", f.used, f.dictRec, f.damage)
	}
	if v4 := buildSegmentSidecar([]byte("attr"), key, nil, nil, 4096, 8, 0, nil); binary.LittleEndian.Uint32(v4[len(v4)-4:]) != sidecarContainerMagicV4 {
		t.Error("a segment without damage no longer writes a v4 container")
	}

	// The key-index proof refuses an index that does not describe the segment: one whose hashes come
	// from another hasher here, as an index left over from some other segment's records would.
	{
		dir := t.TempDir()
		buildInternedStore(t, dir)
		target, _ := firstInternedSegment(t, dir)
		seg, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		used := readSidecarTrailer(target + ".idx").used
		own, err := parseKeyIndex(buildKeyIndex(seg, used, fnvHasher{}, nil, 0))
		if err != nil {
			t.Fatal(err)
		}
		if spans, ok := keyIndexDamage(seg, used, own, fnvHasher{}, nil); !ok || len(spans) != 0 {
			t.Fatalf("the segment's own key index: %v, %v; want no damage", spans, ok)
		}
		other, err := parseKeyIndex(buildKeyIndex(seg, used, otherHasher{}, nil, 0))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := keyIndexDamage(seg, used, other, fnvHasher{}, nil); ok {
			t.Error("a key index that does not describe the segment proved its framing")
		}
	}

	// A set must end where a record that verifies begins (or at the extent).
	data := make([]byte, 256)
	if damageFits(data, 128, newDamageSet([]damageSpan{{0, 64}})) {
		t.Error("a span ending on no record fits")
	}
	if !damageFits(data, 128, newDamageSet([]damageSpan{{0, 64}, {64, 128}})) {
		t.Error("adjacent spans reaching the extent do not fit")
	}
	if damageFits(data, 128, newDamageSet([]damageSpan{{64, 136}})) {
		t.Error("a span past the extent fits")
	}
}

// TestMergeLeavesDamageOut: merging sealed archive segments decodes each record and writes it afresh,
// with a fresh checksum. A damaged record copied that way would stop looking damaged, so the merge
// checks every record first and leaves a damaged one out; a segment whose recovery stopped at damage,
// with records past it that only Repair can recover, is not merged at all.
func TestMergeLeavesDamageOut(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	base := t.TempDir()
	a := buildMergeArchive(t, base, 500)
	before := allClusterIDs(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	opts := ArchiveOptions{Dir: base, SegmentSize: 1 << 12}

	// damage flips a bit in the stored ad of the middle record of the second segment the merge takes.
	// Archive records have no key, so the ad is what is damaged.
	damage := func(t *testing.T, dir string) string {
		a, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
		if err != nil {
			t.Fatal(err)
		}
		target := sealedRun(t, a, 3)[1].path
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		_, offs := keyedOffsets(t, target)
		flipRecordBody(t, target, offs[len(offs)/2])
		return target
	}
	// oneDropped requires got to be before with exactly one record left out, the rest in order.
	oneDropped := func(t *testing.T, got []int64) {
		t.Helper()
		if len(got) != len(before)-1 {
			t.Fatalf("%d records, want %d", len(got), len(before)-1)
		}
		skipped := 0
		for i, j := 0, 0; i < len(before); i++ {
			if j < len(got) && got[j] == before[i] {
				j++
				continue
			}
			skipped++
		}
		if skipped != 1 {
			t.Fatalf("the records are not the originals less one, in order (%d differ)", skipped)
		}
	}

	t.Run("trusted-extent", func(t *testing.T) {
		dir := copyStore(t, base)
		damage(t, dir)
		opts := opts
		opts.Dir = dir
		a, err := OpenArchive(opts)
		if err != nil {
			t.Fatal(err)
		}
		run := sealedRun(t, a, 3)
		dropped := DamagedRecordsDropped()
		a.c.maintMu.Lock()
		ok := a.c.mergeSegments(a.c.shards[0], run)
		a.c.maintMu.Unlock()
		if !ok {
			t.Fatal("the merge was refused")
		}
		if d := DamagedRecordsDropped() - dropped; d != 1 {
			t.Errorf("DamagedRecordsDropped rose by %d, want 1", d)
		}
		oneDropped(t, allClusterIDs(t, a))
		if cerr := a.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		a, err = OpenArchive(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		oneDropped(t, allClusterIDs(t, a))
	})

	t.Run("stopped-walk", func(t *testing.T) {
		dir := copyStore(t, base)
		target := damage(t, dir)
		removeFile(t, target+".idx")
		opts := opts
		opts.Dir = dir
		a, err := OpenArchive(opts)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		refused := RewritesRefusedDamaged()
		run := sealedRun(t, a, 3)
		a.c.maintMu.Lock()
		ok := a.c.mergeSegments(a.c.shards[0], run)
		a.c.maintMu.Unlock()
		if ok || RewritesRefusedDamaged() == refused {
			t.Fatal("a segment holding records past a stopped walk was merged")
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("the damaged segment is gone: %v", err)
		}
	})
}

// otherHasher hashes keys differently from the default, for an index that is not the segment's.
type otherHasher struct{}

func (otherHasher) Hash(key []byte) uint64 { return fnvHasher{}.Hash(key) ^ 0x9e3779b97f4a7c15 }
