package vm_test

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A script written down rather than built: three statements that give an absent request a default
// and clamp it to a floor. The expressions are ordinary ClassAd expressions -- `is` is `=?=` -- and
// the statements around them are the part a single expression cannot say.
func ExampleParseScript() {
	sp, err := vm.ParseScript(`
# Effective memory request, with a floor and an explicit absence case.
want = MY.RequestMemory
if want is undefined:
    want = 2048
if want < 512:
    want = 512
result = want
`)
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, src := range []string{
		`[RequestMemory = 4096]`,
		`[RequestMemory = 64]`,
		`[Owner = "alice"]`,
	} {
		ad, err := classad.Parse(src)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("%-24s -> %s\n", src, sp.Run(ad))
	}

	// Output:
	// [RequestMemory = 4096]   -> 4096
	// [RequestMemory = 64]     -> 512
	// [Owner = "alice"]        -> 2048
}

// A loop, which is the shape no single ClassAd expression has: the number of trips depends on the
// value being computed.
func ExampleParseScript_loop() {
	sp, err := vm.ParseScript(`
# How many halvings fit the request under the cap.
size = MY.RequestMemory
steps = 0
while size > MY.Cap:
    size = size / 2
    steps += 1
result = steps
`)
	if err != nil {
		fmt.Println(err)
		return
	}
	ad, err := classad.Parse(`[RequestMemory = 32768; Cap = 1024]`)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(sp.Run(ad))
	// Output: 5
}

// A syntax error names the line, the column and what was expected, and renders with the offending
// line under it.
func ExampleParseScript_syntaxError() {
	_, err := vm.ParseScript("want = MY.RequestMemory\nif want < 512\n    want = 512\nresult = want\n")
	fmt.Println(err)
	// Output:
	// line 2, column 14: expected ':' at the end of the 'if' header
	// if want < 512
	//              ^
}
