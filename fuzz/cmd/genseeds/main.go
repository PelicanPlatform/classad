// Command genseeds writes a deterministic seed corpus of ClassAds (one per
// line) to stdout, for use with `cafuzz -corpus` and as FuzzDifferential seeds.
//
//	go run ./fuzz/cmd/genseeds > fuzz/corpus/seeds.txt
package main

import (
	"bufio"
	"fmt"
	"os"

	"github.com/PelicanPlatform/classad/fuzz/gen"
)

func main() {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()

	fmt.Fprintln(w, "# Auto-generated seed corpus for FuzzDifferential / cafuzz -corpus.")
	fmt.Fprintln(w, "# One ClassAd per line. Regenerate with: go run ./fuzz/cmd/genseeds > fuzz/corpus/seeds.txt")

	cfgs := []gen.Config{
		{MaxAttrs: 3, MaxDepth: 2, MaxListLen: 3},
		{MaxAttrs: 5, MaxDepth: 3, MaxListLen: 3},
		{MaxAttrs: 6, MaxDepth: 4, MaxListLen: 4},
	}
	for ci, c := range cfgs {
		for s := int64(0); s < 250; s++ {
			g := gen.New(int64(ci)*1000+s, c)
			fmt.Fprintln(w, g.ClassAd())
		}
	}
}
