package collections

import (
	"fmt"
	"os"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Does matchmaking against a COLUMNARIZED resource table pay a column-to-row reassembly for
// every candidate it visits, including the ones it immediately rejects?
//
// The match path never touches the columnar accelerator -- there is no reference to a block,
// a zone or ScanStats anywhere in match*.go -- so a candidate it visits is read back as a
// whole record, which on a columnarized segment means rebuilding the row from its columns.
// If that is what happens, the cost is paid per candidate rather than per match, and a
// columnar pre-filter (deciding from the columns and never rebuilding a rejected row) is
// worth building. If it is not, the pre-filter would be optimizing nothing.
//
// The benchmark holds the slot population fixed and varies only whether its sealed segments
// have been columnarized, across three job shapes that reject different fractions of the
// pool. Reassembly-per-candidate predicts that columnarization costs MORE for the selective
// jobs -- the ones that reject nearly everything -- because every rejected candidate is
// rebuilt before being thrown away.
var benchReverseArms = os.Getenv("BENCH_REVERSE_ARMS") != ""

func BenchmarkMatchColumnarReassembly(b *testing.B) {
	const n = 20_000

	// THREE arms, not two. Columnarizing requires a derived schema installed first, so
	// comparing "plain" against "columnarized" varies two things at once and would credit
	// the schema scan's own speedup to columnarization. The middle arm isolates it.
	const (
		armPlain   = 0 // no schema scan at all
		armSchema  = 1 // schema scan installed, segments NOT columnarized
		armColumns = 2 // schema scan installed and segments columnarized
	)
	armName := [...]string{"plain", "schema", "columnar"}

	build := func(b *testing.B, arm int) *Collection {
		b.Helper()
		dir := b.TempDir()
		c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 1 << 16, ColumnarSegmentBudget: 64})
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < n; i++ {
			// A heterogeneous pool: memory, cpus and disk vary, as does state.
			state := "Unclaimed"
			if i%3 == 0 {
				state = "Claimed"
			}
			ad, perr := classad.Parse(fmt.Sprintf(
				`[ Name="slot%d"; Memory=%d; Cpus=%d; Disk=%d; Arch="X86_64"; OpSys="LINUX"; `+
					`State=%q; HasFileTransfer=true; Rank=0; Requirements=true ]`,
				i, 1024*(1+i%64), 1+i%16, 10240*(1+i%100), state))
			if perr != nil {
				b.Fatal(perr)
			}
			if perr := c.Put([]byte(fmt.Sprintf("slot%d", i)), ad); perr != nil {
				b.Fatal(perr)
			}
		}
		if arm == armPlain {
			return c
		}
		sch, hot, ok := c.deriveSchema(4096, 8)
		if !ok || !c.installSchemaScan(sch, hot) {
			b.Skip("schema scan did not enable; the comparison would be vacuous")
		}
		c.EnableSchemaScan(sch, hot)
		if arm == armSchema {
			if c.anySegmentColumnarized() {
				b.Skip("segments columnarized without being asked; the arms would not differ")
			}
			return c
		}
		rounds := 0
		for c.ColumnarizeSealed() > 0 && rounds < 100 {
			rounds++
		}
		if !c.anySegmentColumnarized() {
			b.Skip("no segment columnarized; the comparison would be vacuous")
		}
		return c
	}

	jobs := []struct {
		name string
		text string
	}{
		// Rejects almost every slot: the shape where per-candidate reassembly would show up
		// most clearly, since nearly every visited candidate is thrown away.
		{"selective", `[ RequestMemory=64512; RequestCpus=16;
			Requirements = TARGET.Memory >= RequestMemory && TARGET.Cpus >= RequestCpus ]`},
		// Rejects about two thirds, on a string attribute the columnar form keeps as a
		// dictionary rather than a numeric column.
		{"state", `[ Requirements = TARGET.State == "Unclaimed" ]`},
		// Accepts everything: the control. If columnarization costs the same here and more
		// for "selective", the extra is being spent on rejected candidates.
		{"accept-all", `[ Requirements = true ]`},
	}

	for _, j := range jobs {
		job, err := classad.Parse(j.text)
		if err != nil {
			b.Fatal(err)
		}
		// The arms must agree on the ANSWER, or a timing difference is just a different
		// amount of work. Checked once per job shape, before any timing.
		var want int
		for arm := armPlain; arm <= armColumns; arm++ {
			c := build(b, arm)
			got := len(c.MatchSortedRanked(job, 0))
			if arm == armPlain {
				want = got
			} else if got != want {
				b.Fatalf("%s/%s returned %d matches, %s returned %d",
					j.name, armName[arm], got, armName[armPlain], want)
			}
			c.Close()
		}
		b.Logf("%s: %d of %d slots match", j.name, want, n)

		// Run the arms in REVERSE on odd rounds: a one-directional sweep lets machine
		// warm-up or thermal drift masquerade as an arm difference.
		order := []int{armPlain, armSchema, armColumns}
		if benchReverseArms {
			order = []int{armColumns, armSchema, armPlain}
		}
		for _, arm := range order {
			b.Run(fmt.Sprintf("%s/%s", j.name, armName[arm]), func(b *testing.B) {
				c := build(b, arm)
				defer c.Close()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_ = c.MatchSortedRanked(job, 0)
				}
			})
		}
	}
}

// anySegmentColumnarized reports whether the build actually produced columnar segments, so a
// benchmark that silently compared two row-form collections is caught rather than reported as
// "columnarization makes no difference".
func (c *Collection) anySegmentColumnarized() bool {
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg.columnarized() {
				sh.mu.RUnlock()
				return true
			}
		}
		sh.mu.RUnlock()
	}
	return false
}
