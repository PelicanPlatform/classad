package parser

import (
	"strings"
	"testing"
)

// jobAdText is a job ad of the width a query actually returns, so the benchmark measures the
// mix of fixed per-parse setup and per-attribute work that decoding a result set pays.
var jobAdText = strings.Join([]string{
	`[ ClusterId = 1234; ProcId = 7; Owner = "alice"; JobStatus = 2; QDate = 1700000000;`,
	`RequestMemory = 2048; RequestCpus = 4; RemoteHost = "slot1@node42.example.edu";`,
	`Cmd = "/home/alice/run.sh"; Iwd = "/home/alice/work"; JobUniverse = 5;`,
	`RemoteWallClockTime = 12345.0; NumJobStarts = 1; ExitCode = 0 ]`,
}, "\n")

var oldJobAdText = strings.NewReplacer(";", "\n", "[", "", "]", "").Replace(jobAdText)

// BenchmarkParseClassAd is the whole-ad parse every client pays once per returned row.
func BenchmarkParseClassAd(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseClassAd(jobAdText); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseOldClassAd covers the old-ClassAd form, which is what the server's projected
// query op streams.
func BenchmarkParseOldClassAd(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseOldClassAd(oldJobAdText); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkParseClassAdNarrow is the same parse over a two-attribute ad: with the fixed setup
// pooled away, the cost should track the ad's width rather than being dominated by a constant.
func BenchmarkParseClassAdNarrow(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseClassAd(`[ Owner = "alice"; JobStatus = 2 ]`); err != nil {
			b.Fatal(err)
		}
	}
}
