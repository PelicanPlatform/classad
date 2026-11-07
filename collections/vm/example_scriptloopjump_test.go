package vm_test

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// `break` leaves the innermost enclosing `while`, and `continue` starts its next iteration. Here
// the loop walks the powers of two looking for the first one over the request; `continue` skips the
// sizes the machine cannot hold, and `break` stops the walk as soon as one fits.
func ExampleParseScript_breakAndContinue() {
	sp, err := vm.ParseScript(`
# The smallest power of two that holds the request and fits the machine.
size = 1
chosen = 0
while size <= 65536:
    size = size * 2
    if size > MY.MachineMemory:
        continue
    if size < MY.RequestMemory:
        continue
    chosen = size
    break
result = chosen
`)
	if err != nil {
		fmt.Println(err)
		return
	}

	for _, src := range []string{
		`[RequestMemory = 3000; MachineMemory = 65536]`,
		`[RequestMemory = 100; MachineMemory = 65536]`,
		`[RequestMemory = 3000; MachineMemory = 2048]`,
	} {
		ad, err := classad.Parse(src)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Printf("%-45s -> %s\n", src, sp.Run(ad))
	}

	// Output:
	// [RequestMemory = 3000; MachineMemory = 65536] -> 4096
	// [RequestMemory = 100; MachineMemory = 65536]  -> 128
	// [RequestMemory = 3000; MachineMemory = 2048]  -> 0
}
