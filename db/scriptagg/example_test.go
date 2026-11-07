package scriptagg_test

import (
	"fmt"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/db"
	"github.com/PelicanPlatform/classad/db/scriptagg"
)

// Example registers an aggregate written in ClassAd Script and runs a GROUP BY with it, exactly as
// a query naming SUM would run.
//
// The state is a list because the answer needs two numbers at once -- a sum and a count -- and a
// mean cannot be carried in one. The four functions are pure and the state is threaded through
// them; nothing is mutated and nothing is held between calls.
func Example() {
	mod, err := vm.ParseScriptModule(`
def init():
    return {0, 0}

def add(s, v):
    return isReal(v) || isInteger(v) ? {s[0] + v, s[1] + 1} : s

def merge(a, b):
    return {a[0] + b[0], a[1] + b[1]}

def result(s):
    return s[1] > 0 ? real(s[0]) / s[1] : undefined
`)
	if err != nil {
		panic(err)
	}
	agg, err := scriptagg.Register("exampleMean", mod, scriptagg.Spec{})
	if err != nil {
		panic(err)
	}

	// The laws the reduction relies on are the script's to keep, so they are tested rather than
	// assumed. The sample holds what a row can really contain, not only what the aggregate was
	// written for.
	fmt.Println("laws:", db.CheckAggregate(agg, []classad.Value{
		classad.NewIntValue(1024),
		classad.NewIntValue(2048),
		classad.NewRealValue(4096.5),
		classad.NewStringValue("lots"),
		classad.NewUndefinedValue(),
	}))

	d, err := db.OpenConfig(db.Config{})
	if err != nil {
		panic(err)
	}
	defer d.Close()

	tx := d.Begin()
	for i, src := range []string{
		`[ Pool = "cm1"; Memory = 1024 ]`,
		`[ Pool = "cm1"; Memory = 2048 ]`,
		`[ Pool = "cm1"; Memory = 4096 ]`,
		`[ Pool = "cm2"; Memory = 8192 ]`,
		`[ Pool = "cm2"; Memory = "lots" ]`, // not a number: `add` skips it
	} {
		ad, perr := classad.Parse(src)
		if perr != nil {
			panic(perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if cerr := tx.Commit(); cerr != nil {
		panic(cerr)
	}

	rows, err := d.AggregateColsStats("true",
		[]db.GroupCol{{Attr: "Pool"}},
		[]db.AggSpec{
			{Func: db.AggCount, Arg: "*"},
			{Func: db.AggUser, UserFunc: "exampleMean", Arg: "Memory"},
		}, nil, nil)
	if err != nil {
		panic(err)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Group[0] < rows[j].Group[0] })
	for _, r := range rows {
		fmt.Printf("%s count=%s mean=%s\n", r.Group[0], r.Values[0], r.Values[1])
	}

	// Output:
	// laws: <nil>
	// "cm1" count=3 mean=2389.3333333333335
	// "cm2" count=2 mean=8192
}
