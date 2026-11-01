package db

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

var keylessBenchSink int

// BenchmarkSealedEval measures the evaluating reads over a table that seals Salary and Note, for the
// privileged reader (which must not slow down when the unprivileged one stops using the key) and the
// unprivileged one. KEYLESS_BENCH_DIR, when set, reuses a store built there.
func BenchmarkSealedEval(b *testing.B) {
	dir := os.Getenv("KEYLESS_BENCH_DIR")
	if dir == "" {
		dir = b.TempDir()
	}
	d, err := OpenConfig(Config{Dir: dir, PoolKeys: []KEK{{ID: "POOL", Material: []byte("test-pool-key-material-32-bytes!!")}},
		EncryptedAttrs: []string{"Salary", "Note"}})
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	const n = 20000
	if d.Len() < n {
		tx := d.Begin()
		for i := 0; i < n; i++ {
			ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = 0\nOwner = \"u%d\"\nCpus = %d\nJobStatus = %d\n"+
				"Salary = %d\nNote = \"note %d\"\nRequestMemory = %d\nMemoryUsage = RequestMemory / 2\nCmd = \"/bin/sleep\"\nRequirements = true\n",
				i, i%50, 1+i%4, 1+i%5, 1000*(i%97), i%13, 512*(1+i%8)))
			if err != nil {
				b.Fatal(err)
			}
			tx.NewClassAd(strconv.Itoa(i)+".0", ad)
			if i%2000 == 1999 {
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
				tx = d.Begin()
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	job, _ := classad.ParseOld("RequestMemory = 1024\nRequirements = TARGET.Salary > 50000 && TARGET.Cpus >= 2\nRank = TARGET.Salary\n")
	type op struct {
		name string
		run  func() int
	}
	count := func(seq func(func(*classad.ClassAd) bool)) int {
		k := 0
		for range seq {
			k++
		}
		return k
	}
	ops := []op{
		{"Query", func() int {
			s, _ := d.Query("Salary > 50000 && JobStatus == 2")
			return count(s)
		}},
		{"QueryRedacted", func() int {
			s, _ := d.QueryRedacted("Salary > 50000 && JobStatus == 2")
			return count(s)
		}},
		{"Aggregate", func() int {
			rows, _ := d.AggregateColsStats("Salary > 50000", []GroupCol{{Attr: "Owner"}},
				[]AggSpec{{Func: AggCount, Arg: "*"}, {Func: AggSum, Arg: "MemoryUsage"}}, nil, nil)
			return len(rows)
		}},
		{"TopK", func() int {
			rows, _ := d.TopK("JobStatus == 2", []string{"ClusterId"}, "Salary", true, 10)
			return len(rows)
		}},
		{"MatchSorted", func() int { return len(d.MatchSorted(job, 10)) }},
	}
	for _, o := range ops {
		b.Run(o.name, func(b *testing.B) {
			if o.name != "QueryRedacted" && o.run() == 0 {
				b.Fatal("empty result")
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				keylessBenchSink += o.run()
			}
		})
	}
}
