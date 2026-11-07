package vm_test

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A loop over a list, and a local indexed by the loop's own variable.
//
// The sequence is evaluated once, before the first trip. A sequence that is not a list is a fault:
// the script ends with ERROR rather than running the body zero times, so a misspelled attribute
// says so instead of quietly computing the identity.
func ExampleParseScript_for() {
	sp, err := vm.ParseScript(`
# The largest request in a list, and how many of them exceed the cap.
largest = 0
over = 0
for request in MY.Requests:
    if request > largest:
        largest = request
    if request > MY.Cap:
        over += 1
result = largest * 1000 + over
`)
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, src := range []string{
		`[Cap = 4096; Requests = {1024, 8192, 2048}]`,
		`[Cap = 4096; Requests = {}]`,
		`[Cap = 4096]`,
	} {
		ad, err := classad.Parse(src)
		if err != nil {
			fmt.Println(err)
			return
		}
		value, _, fault := sp.RunWithFault(ad)
		if fault != nil {
			fmt.Printf("%-42s -> %s\n", src, fault)
			continue
		}
		fmt.Printf("%-42s -> %s\n", src, value)
	}
	// Output:
	// [Cap = 4096; Requests = {1024, 8192, 2048}] -> 8192001
	// [Cap = 4096; Requests = {}]                -> 0
	// [Cap = 4096]                               -> condition fault at for #1 condition: the condition was an error value
}

// A script local can be indexed, which is what the element load is for: `seq[0]` reads the local's
// own value rather than an attribute named seq.
//
// An index past the end of a list is ERROR, not UNDEFINED -- that is the reference engine's answer
// and the compiled one is the same -- so `?:`, which defaults only an UNDEFINED, does not catch it.
func ExampleParseScript_subscript() {
	sp, err := vm.ParseScript(`
# The first two elements of a list the ad supplies, defaulted elementwise.
seq = MY.Sizes
result = (seq[0] ?: 0) + (seq[1] ?: 0)
`)
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, src := range []string{
		`[Sizes = {10, 20, 30}]`,
		`[Sizes = {10}]`,
		`[]`,
	} {
		ad, err := classad.Parse(src)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("%-24s -> %s\n", src, sp.Run(ad))
	}
	// Output:
	// [Sizes = {10, 20, 30}]   -> 30
	// [Sizes = {10}]           -> error
	// []                       -> 0
}
