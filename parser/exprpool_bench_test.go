package parser

import "testing"

// realistic computed values from startd ads (a requirements expr and an AddressV1
// list), the kind ingest parses per attribute.
var exprBenchCases = []string{
	`(TARGET.RequestCpus <= Cpus) && (TARGET.RequestMemory <= Memory) && (TARGET.RequestDisk <= Disk)`,
	`{ "alias=host.example.com,p=9618,n=abc", "alias=host.example.com,p=9618,n=def", 1, 2, 3 }`,
	`ifThenElse(State =?= "Claimed", RemoteOwner, "none")`,
}

func BenchmarkParseExpr(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, s := range exprBenchCases {
			if _, err := ParseExpr(s); err != nil {
				b.Fatal(err)
			}
		}
	}
}
