package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// BenchmarkColScopeGroupResolve is the per-record cost of resolving a group attribute through the
// column scope: the cached binding, the membership rank and the group block's slot read.
func BenchmarkColScopeGroupResolve(b *testing.B) {
	c := New(Options{Shards: 1})
	defer c.Close()
	const n = 4096
	cs, _, _ := groupSchemaReadFixture(b, c, n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for k := 0; k < n; k++ {
			cs.k, cs.fellBack = k, false
			_ = cs.resolve("GA", ast.NoScope)
		}
	}
}
