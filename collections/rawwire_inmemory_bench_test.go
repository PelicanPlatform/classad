package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// These bound the claim the RAM-table wire path is made for: shipping assembled wire rows and
// rendering them at the consumer's edge, versus rendering old-ClassAd text at the store. Both
// measure the store side only (the relay's far edge renders what the wire path ships).
func benchInMemColl(b *testing.B, n int) *Collection {
	c := New(Options{Shards: 4})
	for i := 0; i < n; i++ {
		text := fmt.Sprintf(`[MyType="Machine"; TargetType="Job"; Name="slot%d@host"; State="Unclaimed";
		  Cpus=8; Memory=16384; Disk=1000000; OpSys="LINUX"; Arch="X86_64"; LoadAvg=0.25;
		  KeyboardIdle=1200; Activity="Idle"; EnteredCurrentActivity=%d; TotalSlots=16;
		  Start=(LoadAvg<0.5)&&(KeyboardIdle>900);
		  Requirements=MY.Start && (TARGET.RequestCpus<=Cpus) && (TARGET.RequestMemory<=Memory);
		  Rank=strcat(Name,"/",State); ClaimId="<secret%d>"]`, i, i, i)
		ad, err := classad.Parse(text)
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%06d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	return c
}

var proj12 = []string{"Name", "State", "Cpus", "Memory", "LoadAvg", "Activity", "Requirements", "Rank"}

func BenchmarkInMemWireProjected(b *testing.B) {
	c := benchInMemColl(b, 5000)
	defer c.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for range c.ScanRawWire(proj12, true) {
			n++
		}
		if n != 5000 {
			b.Fatalf("rows=%d", n)
		}
	}
	b.ReportMetric(float64(5000)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
}

func BenchmarkInMemTextProjected(b *testing.B) {
	c := benchInMemColl(b, 5000)
	defer c.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for range c.ScanRawProjected(proj12, false, true) {
			n++
		}
		if n != 5000 {
			b.Fatalf("rows=%d", n)
		}
	}
	b.ReportMetric(float64(5000)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
}

func BenchmarkInMemWireWhole(b *testing.B) {
	c := benchInMemColl(b, 5000)
	defer c.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for range c.ScanRawWire(nil, true) {
			n++
		}
		if n != 5000 {
			b.Fatalf("rows=%d", n)
		}
	}
	b.ReportMetric(float64(5000)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
}

func BenchmarkInMemTextWhole(b *testing.B) {
	c := benchInMemColl(b, 5000)
	defer c.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		for range c.ScanRawProjected(nil, false, true) {
			n++
		}
		if n != 5000 {
			b.Fatalf("rows=%d", n)
		}
	}
	b.ReportMetric(float64(5000)*float64(b.N)/b.Elapsed().Seconds(), "ads/sec")
}
