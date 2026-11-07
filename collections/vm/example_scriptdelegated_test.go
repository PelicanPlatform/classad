package vm

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

// Example_scriptDelegatedLocals shows what a delegated construct sees of a script's locals.
//
// A select and a lazy built-in are evaluated by the tree-walking evaluator rather than compiled, and
// they run with the script's locals overlaid on the ad. So a bare bound name there is the local,
// exactly as it is in a compiled position such as a ternary -- and MY.x is still the attribute,
// which is how a script says it meant the ad, in either kind of position.
//
// The ad below defines x as well, with a different value, so each line's answer says which one was
// read.
func Example_scriptDelegatedLocals() {
	const ad = `[ x = 0; Cpus = 4; Nested = [ tier = "gold" ] ]`

	scripts := map[string]string{
		"ternary over the local": "x = Cpus + 1\nresult = x ? \"local\" : \"attribute\"\n",
		"ternary over MY.x":      "x = Cpus + 1\nresult = MY.x ? \"local\" : \"attribute\"\n",
		"rebinding":              "x = x + 1\nresult = x ? x : -1\n",
		"select over a local":    "n = Nested\nresult = n.tier\n",
		"lazy built-in":          "tag = \"job-\"\nresult = strcat(tag, Cpus)\n",
	}
	for _, name := range []string{
		"ternary over the local", "ternary over MY.x", "rebinding",
		"select over a local", "lazy built-in",
	} {
		sp, err := ParseScript(scripts[name])
		if err != nil {
			panic(err)
		}
		scope, perr := classad.Parse(ad)
		if perr != nil {
			panic(perr)
		}
		fmt.Printf("%-22s %s\n", name, sp.Run(scope))
	}

	// A list literal is the one shape that cannot read a local: its elements are evaluated on
	// demand, in a scope captured when the list was built, which may be long after the script has
	// finished. That is refused rather than quietly read from the ad.
	_, err := ParseScript("x = Cpus\nresult = {x, 1}\n")
	fmt.Println(err != nil)

	// Output:
	// ternary over the local "local"
	// ternary over MY.x      "attribute"
	// rebinding              1
	// select over a local    "gold"
	// lazy built-in          "job-4"
	// true
}
