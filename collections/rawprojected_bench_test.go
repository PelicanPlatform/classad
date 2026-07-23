package collections

import (
	"testing"
)

// BenchmarkScanRawProjectedInline measures the projected raw scan on a
// PERSISTENT (inline-names) collection -- the mode htcondordb tables run in --
// with the projection pinned hot and the ads re-encoded (the converged state),
// against the unprojected raw scan of the same store.
func BenchmarkScanRawProjectedInline(b *testing.B) {
	sample := loadCorpus(b)
	const n = 2000
	c := populatePersistent(b, sample, n, b.TempDir())
	defer c.Close()
	proj := []string{
		"Name", "Machine", "OpSys", "Arch", "State", "Activity",
		"LoadAvg", "Memory", "Cpus", "EnteredCurrentActivity", "MyCurrentTime", "TotalSlots",
	}
	b.Run("unprojected", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cnt := 0
			for range c.ScanRaw() {
				cnt++
			}
		}
		b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
	})
	b.Run("projected-cold", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cnt := 0
			for range c.ScanRawProjected(proj, false, true) {
				cnt++
			}
		}
		b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
	})
	// Converge: pin the projection + type fields hot, re-encode every ad.
	c.AddHotAttrs(append(append([]string{}, proj...), "MyType", "TargetType")...)
	keys := c.Keys()
	for _, k := range keys {
		if ad, ok := c.Get([]byte(k)); ok {
			if err := c.Put([]byte(k), ad); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("projected-hot", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			cnt := 0
			for range c.ScanRawProjected(proj, false, true) {
				cnt++
			}
		}
		b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
	})
}
