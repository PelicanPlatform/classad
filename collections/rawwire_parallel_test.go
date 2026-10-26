package collections

import (
	"fmt"
	"strings"
	"testing"
)

// wireRowAttrs renders a wire-form row to attribute -> expression text, the shape a consumer
// actually sees (RenderRawAdInline lifts MyType/TargetType out of the entries).
func wireRowAttrs(t *testing.T, row []byte) (map[string]string, string) {
	t.Helper()
	buf, offs, myType, _, ok := RenderRawAdInline(row, nil, nil)
	if !ok {
		t.Fatal("RenderRawAdInline failed on a shipped row")
	}
	m := make(map[string]string)
	for i := 0; i+1 < len(offs); i++ {
		expr := string(buf[offs[i]:offs[i+1]])
		name, val, found := strings.Cut(expr, " = ")
		if !found {
			t.Fatalf("unparsable rendered expression %q", expr)
		}
		m[name] = val
	}
	return m, myType
}

// TestRawWireInternedParallelMatchesSerial pins the projected wire scan's two implementations --
// the parallel fan-out and the serial fallback -- to each other AND to the values the ads were
// written with, on an INTERNED store.
//
// Interned is the state a long-running store is in (maintenance retrains on an interval), and it is
// the state where the scan converts records to inline names before subsetting them. That conversion
// is narrowed to the projection first, in the segment's own id space, and the narrowing is a
// shortcut around a correct-but-slow path: get it wrong and the row comes back missing an attribute
// the caller asked for, which no "did it return rows" assertion would catch. Hence the independent
// oracle -- comparing the two scans alone would be satisfied by both dropping the same attribute,
// since they now share the narrowing.
//
// The encrypted variant covers the sealed-value case the narrowing deliberately skips, and pins the
// data key being passed to the row assembler: without it the row does not merely come back with the
// value still sealed, it fails to assemble at all and is dropped from the result.
func TestRawWireInternedParallelMatchesSerial(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	cases := []struct {
		name     string
		dataKey  []byte
		proj     []string
		redact   bool
		columnar bool
	}{
		{"plaintext", nil, []string{"ClusterId", "ProcId", "JobStatus", "Owner", "RequestMemory"}, true, false},
		{"encrypted", key, []string{"ClusterId", "ProcId", "JobStatus", "Owner", "Note"}, false, false},
		// Columnarized: the serial scan then builds the row from the columns the projection plan
		// names, a second place a projected row can come back missing an attribute.
		{"columnar", nil, []string{"ClusterId", "ProcId", "JobStatus", "Owner", "RequestMemory"}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 16}
			if tc.dataKey != nil {
				opts.DataKey = tc.dataKey
				opts.EncryptedAttrs = []string{"Note"}
			}
			c, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.parallelMinBytes = 0 // fan out even though the corpus is small

			// Enough attributes per ad that narrowing to five of them is the whole point, and
			// enough ads to fill several segments.
			const n = 1500
			want := make(map[string]map[string]string, n)
			for i := 0; i < n; i++ {
				var b strings.Builder
				fmt.Fprintf(&b, `[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=%d; JobStatus=%d; Owner="user%d"; RequestMemory=%d; Note="note-%d"`,
					i, i%10, 1+i%5, i%7, (1+i%8)*1024, i)
				for f := 0; f < 20; f++ {
					fmt.Fprintf(&b, `; Filler%02d="filler value %d-%d"`, f, i, f)
				}
				b.WriteString(" ]")
				k := fmt.Sprintf("%d.%d", i, i%10)
				if err := c.Put([]byte(k), mustAd(t, b.String())); err != nil {
					t.Fatal(err)
				}
				want[k] = map[string]string{
					"ClusterId":     fmt.Sprint(i),
					"ProcId":        fmt.Sprint(i % 10),
					"JobStatus":     fmt.Sprint(1 + i%5),
					"Owner":         fmt.Sprintf("%q", fmt.Sprintf("user%d", i%7)),
					"RequestMemory": fmt.Sprint((1 + i%8) * 1024),
					"Note":          fmt.Sprintf("%q", fmt.Sprintf("note-%d", i)),
				}
			}
			if _, err := c.RetrainDict(500); err != nil {
				t.Fatal(err)
			}
			interned, total := 0, 0
			for _, sh := range c.shards {
				sh.mu.Lock()
				for _, s := range sh.segs {
					if s == nil || s.used == 0 {
						continue
					}
					total++
					if s.dict.Load() != nil {
						interned++
					}
				}
				sh.mu.Unlock()
			}
			if interned == 0 {
				t.Fatalf("RetrainDict interned no segments (%d live): the path under test is not covered", total)
			}
			if tc.columnar {
				if !c.BuildAndEnableSchemaScan(4096, 8) {
					t.Fatal("no columnar schema derived")
				}
				c.ColumnarizeSealed()
				c.Reindex()
				if columnarSegmentsForTest(c) == 0 {
					t.Fatal("no segment columnarized: the path under test is not covered")
				}
			}

			// The parallel scan, driven directly so the test knows the fan-out engaged rather than
			// silently measuring the serial fallback twice.
			sel := c.newWireSubsetSelector(tc.proj, tc.redact)
			var par []map[string]string
			engaged := c.runParallelWireScan(sel, sel.neededCount(), func(row []byte) bool {
				attrs, myType := wireRowAttrs(t, row)
				if myType != "Job" {
					t.Errorf("parallel row lost MyType: got %q", myType)
				}
				par = append(par, attrs)
				return true
			})
			if !engaged {
				t.Skip("parallel wire scan did not engage (too few workers available)")
			}

			var ser []map[string]string
			for row := range c.QueryRawWire(mustParseQ(t, "true"), tc.proj, tc.redact) {
				attrs, myType := wireRowAttrs(t, row)
				if myType != "Job" {
					t.Errorf("serial row lost MyType: got %q", myType)
				}
				ser = append(ser, attrs)
			}

			index := func(rows []map[string]string) map[string]map[string]string {
				m := make(map[string]map[string]string, len(rows))
				for _, r := range rows {
					m[r["ClusterId"]+"."+r["ProcId"]] = r
				}
				return m
			}
			pm, sm := index(par), index(ser)
			if len(pm) != n || len(sm) != n {
				t.Fatalf("row counts: parallel %d (%d distinct), serial %d (%d distinct), want %d",
					len(par), len(pm), len(ser), len(sm), n)
			}
			for k, exp := range want {
				for _, got := range []struct {
					path string
					row  map[string]string
				}{{"parallel", pm[k]}, {"serial", sm[k]}} {
					if got.row == nil {
						t.Fatalf("%s scan dropped key %s", got.path, k)
					}
					if len(got.row) != len(tc.proj) {
						t.Fatalf("%s scan row %s has attributes %v, want exactly %v", got.path, k, got.row, tc.proj)
					}
					for _, a := range tc.proj {
						if got.row[a] != exp[a] {
							t.Fatalf("%s scan row %s: %s = %q, want %q", got.path, k, a, got.row[a], exp[a])
						}
					}
				}
			}
		})
	}
}
