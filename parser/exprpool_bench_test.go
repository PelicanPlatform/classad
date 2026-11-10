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

// oldExprBenchCases are the values ParseExprOld sees per attribute on the
// job_queue.log and CEDAR ingest paths: mostly lone strings, some of them with
// backslashes, mixed with the numbers and expressions that are not strings.
var oldExprBenchCases = map[string][]string{
	"PlainString":     {`"vanilla"`, `"froth.cs.wisc.edu"`, `"user@example.com"`},
	"BackslashString": {`"C:\Users\x"`, `"warlock21\.beocat\.ksu\.edu"`, `"testing\,\ more"`},
	"EscapedQuote":    {`"a\"b"`, `"say \"hi\" now"`},
	"LongString":      {`"osdf:///chtc/staging/c/ckoch5/tests/space-comma/some-rather-long-file-name.dat"`},
	"Int":             {`0`, `1234567`, `-42`},
	"Expr": {
		`(TARGET.RequestCpus <= Cpus) && (TARGET.RequestMemory <= Memory)`,
		`ifThenElse(State =?= "Claimed", RemoteOwner, "none")`,
	},
}

func BenchmarkParseExprOld(b *testing.B) {
	for _, name := range []string{"PlainString", "BackslashString", "EscapedQuote", "LongString", "Int", "Expr"} {
		cases := oldExprBenchCases[name]
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, s := range cases {
					if _, err := ParseExprOld(s); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
