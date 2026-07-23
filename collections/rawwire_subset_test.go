package collections

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// collectWireRendered runs the wire-form relay end-to-end in miniature:
// ScanRawWire rows (assembled subsets) rendered at the "edge" by
// RenderRawAdInline, materialized into copies.
func collectWireRendered(t *testing.T, c *Collection, projection []string, redact bool) []RawAd {
	t.Helper()
	var out []RawAd
	var buf []byte
	var offs []int
	for row := range c.ScanRawWire(projection, redact) {
		var mt, tt string
		var ok bool
		buf, offs, mt, tt, ok = RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Fatal("RenderRawAdInline failed on a shipped row")
		}
		cp := RawAd{MyType: mt, TargetType: tt}
		for i := 0; i+1 < len(offs); i++ {
			cp.Exprs = append(cp.Exprs, append([]byte(nil), buf[offs[i]:offs[i+1]]...))
		}
		out = append(out, cp)
	}
	return out
}

// TestScanRawWireRoundTrip verifies the relay path -- subset assembly at the
// store, old-ClassAd render at the far edge -- produces exactly what the
// direct projected/redacted text scans produce, for a projected, a redacted
// and a whole-ad selection on a persistent (inline) collection.
func TestScanRawWireRoundTrip(t *testing.T) {
	t.Parallel()
	c := projTestCollectionInline(t)

	cases := []struct {
		name   string
		proj   []string
		redact bool
	}{
		{"projected", []string{"Name", "State", "Cpus", "NoSuchAttr"}, false},
		{"projected-redacted", []string{"Name", "ClaimId"}, true},
		{"whole-ad", nil, false},
		{"whole-ad-redacted", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := adsByName(t, collectProjected(c, tc.proj, false, tc.redact))
			got := adsByName(t, collectWireRendered(t, c, tc.proj, tc.redact))
			if len(got) != len(want) {
				t.Fatalf("wire relay returned %d ads, direct scan %d", len(got), len(want))
			}
			for name, attrs := range want {
				g := got[name]
				if len(g) != len(attrs) {
					t.Errorf("%s: wire %v != direct %v", name, g, attrs)
					continue
				}
				for a, v := range attrs {
					if g[a] != v {
						t.Errorf("%s.%s: wire %q != direct %q", name, a, g[a], v)
					}
				}
			}
		})
	}

	// The redacted whole-ad row must not leak ClaimId even before rendering:
	// scan the raw row bytes for the secret value.
	for row := range c.ScanRawWire(nil, true) {
		if strings.Contains(string(row), "<secret>") {
			t.Fatal("redacted wire row carries the private value bytes")
		}
	}
}

// BenchmarkScanRawWireZSTD isolates the db-side wire-subset scan on a
// ZSTD-compressed persistent store (the htcondordb production configuration):
// the per-record decompress dominates, bounding what any relay above it can see.
func BenchmarkScanRawWireZSTD(b *testing.B) {
	sample := loadCorpus(b)
	const n = 2000
	c := populatePersistent(b, sample, n, b.TempDir())
	defer c.Close()
	if _, err := c.RetrainDict(n); err != nil {
		b.Fatal(err)
	}
	proj := []string{"Name", "Machine", "OpSys", "Arch", "State", "Activity",
		"LoadAvg", "Memory", "Cpus", "EnteredCurrentActivity", "MyCurrentTime", "TotalSlots"}
	c.AddHotAttrs(append(append([]string{}, proj...), "MyType", "TargetType")...)
	keys := c.Keys()
	for _, k := range keys {
		if ad, ok := c.Get([]byte(k)); ok {
			if err := c.Put([]byte(k), ad); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cnt := 0
		for range c.ScanRawWire(proj, true) {
			cnt++
		}
	}
	b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
}

// TestHotPrefixDecodable verifies the hot-first physical layout end to end: a
// record encoded with a hot set can be TRUNCATED to a small prefix and still
// serve a hot-covered subset, identical to the subset from the full record --
// through both the AST encoder (Put) and the text-ingest StreamEncoder
// (UpdateOld), the path most records arrive through.
func TestHotPrefixDecodable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, HotAttrs: []string{"Name", "State", "Cpus", "MyType", "TargetType"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// AST path.
	ad, err := classad.Parse(projTestAdTexts[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put([]byte("ast"), ad); err != nil {
		t.Fatal(err)
	}
	// Text-ingest (StreamEncoder) path.
	if err := c.UpdateOld([]OldAdUpdate{{Key: []byte("txt"),
		Text: "MyType = \"Machine\"\nTargetType = \"Job\"\nName = \"slot1@txt\"\nState = \"Claimed\"\nCpus = 4\nMemory = 8192\nLoadAvg = 0.5\nArch = \"X86_64\"\nOpSys = \"LINUX\"\n"}}); err != nil {
		t.Fatal(err)
	}

	sel := c.newWireSubsetSelector([]string{"Name", "State", "Cpus"}, false)
	needed := sel.neededCount()
	var sc wire.SubsetScratch
	checked := 0
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		forEachVisible(s0, wins, func(rec []byte, codec Codec) bool {
			full, ferr := codec.Decompress(nil, rec)
			if ferr != nil {
				t.Fatal(ferr)
			}
			want, ok := wire.AppendAdSubsetInlineHotFirst(nil, wire.Ad(full), sel.keep, needed, nil, &sc)
			if !ok {
				t.Fatal("subset failed on the full record")
			}
			// The whole selection must be servable from a small physical prefix.
			const prefix = 512
			if len(full) > prefix {
				got, gok := wire.AppendAdSubsetInlineHotFirst(nil, wire.Ad(full[:prefix]), sel.keep, needed, nil, &sc)
				if !gok {
					t.Fatalf("subset not servable from a %dB prefix (hot region not front-loaded?)", prefix)
				}
				if string(got) != string(want) {
					t.Fatal("prefix-served subset differs from full-record subset")
				}
			}
			checked++
			return true
		})
		releaseWindows(wins)
	}
	if checked < 2 {
		t.Fatalf("checked %d records, want both encode paths", checked)
	}
}
