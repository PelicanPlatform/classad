package vm_test

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A function names a rule the script applies more than once.
//
// The body is function-scoped: a bare name in it is a parameter, a name the body binds, or an
// attribute of the ad. `requested` and `cap` are parameters, while `Optimistic` is neither -- so it
// is read from the ad, and a function with no parameters at all would still be a function of the ad.
func Example_scriptFunction() {
	sp, err := vm.ParseScript(`
def fits(requested, cap):
    if requested is undefined:
        return Optimistic
    return requested <= cap

result = fits(MY.RequestMemory, 8192) && fits(MY.RequestCpus, 16)
`)
	if err != nil {
		panic(err)
	}

	for _, src := range []string{
		`[ RequestMemory = 4096; RequestCpus = 4 ]`,
		`[ RequestMemory = 99999; RequestCpus = 4 ]`,
		`[ RequestCpus = 4; Optimistic = true ]`,
		`[ RequestCpus = 4 ]`,
	} {
		ad, perr := classad.Parse(src)
		if perr != nil {
			panic(perr)
		}
		fmt.Printf("%-44s %s\n", src, sp.Run(ad).String())
	}

	// Output:
	// [ RequestMemory = 4096; RequestCpus = 4 ]    true
	// [ RequestMemory = 99999; RequestCpus = 4 ]   false
	// [ RequestCpus = 4; Optimistic = true ]       true
	// [ RequestCpus = 4 ]                          undefined
}
