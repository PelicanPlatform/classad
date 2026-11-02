package collections

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// What withholding the attributes the sealing policy names costs a reader not entitled to sealed
// values on a mutable table whose values are all sealed already -- the steady state, where it changes
// no answer. History-shaped ads in a persistent store with a key and EncryptedAttrs = Cmd, Args; each
// redacted read is timed over the same store, rounds interleaved with the entitled reads, medians
// reported one per line as "keyless-withhold: <read> rows=<n> <median-ms>" so two builds can be compared:
//
//	MUTABLE_KEYLESS_BENCH=1 go test -run TestMutableKeylessReadCost -v .
//	MUTABLE_KEYLESS_BENCH=1 MUTABLE_KEYLESS_BENCH_N=50000 MUTABLE_KEYLESS_BENCH_ROUNDS=9 go test ...
func TestMutableKeylessReadCost(t *testing.T) {
	if os.Getenv("MUTABLE_KEYLESS_BENCH") == "" {
		t.Skip("set MUTABLE_KEYLESS_BENCH=1 to measure")
	}
	n := envInt("MUTABLE_KEYLESS_BENCH_N", 20000)
	rounds := envInt("MUTABLE_KEYLESS_BENCH_ROUNDS", 7)
	ads := historyBenchAds(t, n)
	c, err := Open(Options{Dir: t.TempDir(), Shards: 4, DataKey: sealedRefKey(),
		EncryptedAttrs: []string{"Cmd", "Args"}, SegmentSize: 1 << 22})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tx := c.Begin()
	for i, ad := range ads {
		tx.Put([]byte(fmt.Sprintf("k%d", i)), ad)
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("put conflicted")
	}
	internAll(c)
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()

	sel := mustQuery(t, `Owner == "user3" && RemoteWallClockTime > 2600`)
	all := mustQuery(t, `true`)
	proj := []string{"ClusterId", "ProcId", "Owner", "Cmd", "RemoteWallClockTime"}
	reads := []struct {
		name string
		run  func() int
	}{
		{"QueryRedacted(selective)", func() int { return countSeq(c.QueryRedacted(sel)) }},
		{"QueryRedacted(all)", func() int { return countSeq(c.QueryRedacted(all)) }},
		{"QueryProjectRedacted(all)", func() int { return countSeq(c.QueryProjectRedacted(all, proj)) }},
		{"QueryRawWire(redact,proj)", func() int { return countSeq(c.QueryRawWire(all, proj, true)) }},
		{"ScanRawWire(redact,whole)", func() int { return countSeq(c.ScanRawWire(nil, true)) }},
		{"ScanRawRedacted", func() int { return countSeq(c.ScanRawRedacted()) }},
		{"CountQueryRedacted", func() int { n, _ := c.CountQueryRedacted(sel); return n }},
		{"NumStatsQueryRedacted", func() int { s, _ := c.NumStatsQueryRedacted(nil, "RemoteWallClockTime"); return s.N }},
		{"Query(entitled,selective)", func() int { return countSeq(c.Query(sel)) }},
	}
	samples := make([][]time.Duration, len(reads))
	counts := make([]int, len(reads))
	for r := 0; r < rounds; r++ {
		for j := range reads {
			i := j
			if r%2 == 1 {
				i = len(reads) - 1 - j
			}
			start := time.Now()
			counts[i] = reads[i].run()
			samples[i] = append(samples[i], time.Since(start))
		}
	}
	for i, rd := range reads {
		s := samples[i]
		sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
		t.Logf("keyless-withhold: %s rows=%d %.3f", rd.name, counts[i], float64(s[len(s)/2].Microseconds())/1000)
	}
}
