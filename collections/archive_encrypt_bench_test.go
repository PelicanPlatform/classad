package collections

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"
)

// What sealing costs an archive, measured three ways over the same history-shaped ads:
//
//	nokey    no data key -- how every archive was stored before archives were sealed
//	private  a key and no explicit set: only private attributes are sealed (these ads hold none)
//	sealed   a key and EncryptedAttrs = Cmd, Args: two strings sealed in every record
//
// Each read is timed entitled and, on the keyed arms, redacted. Arms are interleaved and their order
// alternates every round; medians are reported. It is a measurement, not a check, so it runs only when
// asked:
//
//	ARCHIVE_ENCRYPT_BENCH=1 go test -run TestArchiveEncryptionCost -v .
//	ARCHIVE_ENCRYPT_BENCH=1 ARCHIVE_ENCRYPT_BENCH_N=50000 ARCHIVE_ENCRYPT_BENCH_ROUNDS=9 go test ...
func TestArchiveEncryptionCost(t *testing.T) {
	if os.Getenv("ARCHIVE_ENCRYPT_BENCH") == "" {
		t.Skip("set ARCHIVE_ENCRYPT_BENCH=1 to measure")
	}
	n := envInt("ARCHIVE_ENCRYPT_BENCH_N", 20000)
	rounds := envInt("ARCHIVE_ENCRYPT_BENCH_ROUNDS", 7)
	ads := historyBenchAds(t, n)
	type arm struct {
		name    string
		key     bool
		encAttr []string
	}
	arms := []arm{{name: "nokey"}, {name: "private", key: true}, {name: "sealed", key: true, encAttr: []string{"Cmd", "Args"}}}
	opts := func(dir string, a arm) ArchiveOptions {
		o := ArchiveOptions{Dir: dir, ZoneAttrs: []string{"CompletionDate"}, SegmentSize: 1 << 22}
		if a.key {
			o.DataKey = sealedRefKey()
			o.EncryptedAttrs = a.encAttr
		}
		return o
	}
	samples := map[string][]time.Duration{}
	record := func(k string, d time.Duration) { samples[k] = append(samples[k], d) }

	// Appends: a fresh archive per arm per round, AppendDeferred with a Flush every 1000, as a
	// history tailer feeds one.
	for r := 0; r < rounds; r++ {
		order := append([]arm(nil), arms...)
		if r%2 == 1 {
			for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
				order[i], order[j] = order[j], order[i]
			}
		}
		for _, a := range order {
			arch, err := OpenArchive(opts(t.TempDir(), a))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			for i, ad := range ads {
				if err := arch.AppendDeferred(ad); err != nil {
					t.Fatal(err)
				}
				if (i+1)%1000 == 0 {
					if err := arch.Flush(); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := arch.Flush(); err != nil {
				t.Fatal(err)
			}
			record("append/"+a.name, time.Since(start))
			arch.Close()
		}
	}

	// Reads: one archive per arm, its sealed segments columnarized.
	built := map[string]*Archive{}
	defer func() {
		for _, arch := range built {
			arch.Close()
		}
	}()
	for _, a := range arms {
		arch, err := OpenArchive(opts(t.TempDir(), a))
		if err != nil {
			t.Fatal(err)
		}
		built[a.name] = arch
		for _, ad := range ads {
			if err := arch.AppendDeferred(ad); err != nil {
				t.Fatal(err)
			}
		}
		if err := arch.Flush(); err != nil {
			t.Fatal(err)
		}
		if !arch.BuildAndEnableSchemaScan(4096, 16) {
			t.Fatal("no schema")
		}
		arch.c.ColumnarizeSealed()
	}
	cut := 1766900000 + n*7/2
	window := mustQuery(t, "CompletionDate > "+strconv.Itoa(cut))
	scan := mustQuery(t, `ExitCode == 0 && Owner == "user3"`)
	type read struct {
		name string
		run  func(a *Archive, redact bool) int
	}
	reads := []read{
		{"columnar-agg", func(a *Archive, redact bool) int {
			f := a.NumStatsQuery
			if redact {
				f = a.NumStatsQueryRedacted
			}
			st, ok := f(window, "RemoteUserCpu")
			if !ok {
				t.Fatal("columnar aggregate declined")
			}
			return st.N
		}},
		{"topk", func(a *Archive, redact bool) int {
			th, proj := a.TopKOrderThreshold, a.QueryProjectStats
			if redact {
				th, proj = a.TopKOrderThresholdRedacted, a.QueryProjectStatsRedacted
			}
			cutoff, _, ok := th(window, "CompletionDate", true, 10, nil)
			q := window
			if ok {
				q = mustQuery(t, "CompletionDate > "+strconv.Itoa(cut)+" && CompletionDate >= "+strconv.FormatFloat(cutoff, 'f', -1, 64))
			}
			return countSeq(proj(q, []string{"ClusterId", "Owner", "Cmd", "CompletionDate"}, nil))
		}},
		{"projection", func(a *Archive, redact bool) int {
			proj := a.QueryProjectStats
			if redact {
				proj = a.QueryProjectStatsRedacted
			}
			return countSeq(proj(window, []string{"Owner", "RemoteWallClockTime", "Cmd"}, nil))
		}},
		{"full-ad", func(a *Archive, redact bool) int {
			if redact {
				return countSeq(a.QueryLimitRedacted(scan, 0))
			}
			return countSeq(a.QueryLimit(scan, 0))
		}},
	}
	for r := 0; r < rounds; r++ {
		for _, rd := range reads {
			order := append([]arm(nil), arms...)
			if r%2 == 1 {
				for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
					order[i], order[j] = order[j], order[i]
				}
			}
			for _, a := range order {
				modes := []bool{false}
				if a.key {
					modes = []bool{false, true}
					if r%2 == 1 {
						modes = []bool{true, false}
					}
				}
				for _, redact := range modes {
					start := time.Now()
					rd.run(built[a.name], redact)
					k := rd.name + "/" + a.name
					if redact {
						k += "/redacted"
					}
					record(k, time.Since(start))
				}
			}
		}
	}
	keys := make([]string, 0, len(samples))
	for k := range samples {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := samples[k]
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		t.Logf("%-32s median %10s  (min %s, max %s, n=%d)", k, s[len(s)/2].Round(time.Microsecond),
			s[0].Round(time.Microsecond), s[len(s)-1].Round(time.Microsecond), len(s))
	}
	fmt.Fprintf(os.Stderr, "ads=%d rounds=%d\n", n, rounds)
}
