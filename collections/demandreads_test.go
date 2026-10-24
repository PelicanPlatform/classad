package collections

import (
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// demandReads is the read tally the hot set and the columnar hot tier rank by.
func demandReads(c *Collection, name string) int64 {
	if v, ok := c.demand.m.Load(strings.ToLower(name)); ok {
		return v.(*demandCounts).reads.Load()
	}
	return 0
}

// TestFilterReadDemandIsUniformAcrossPaths pins the property the self-tuning depends
// on: the SAME constrained query reports the SAME read demand for the constraint's
// attributes whichever scan path serves it.
//
// It is not a style point. RefreshHotSet and the columnar schema's hot tier both rank
// by this counter, so a path that evaluates a constraint without reporting it makes
// the attributes it reads look colder than attributes read far less often through the
// paths that do report -- and the self-tuning then accelerates the wrong columns,
// confidently, with nothing about the ranking looking wrong.
//
// Each path gets a fresh collection, so the tallies cannot leak between them.
func TestFilterReadDemandIsUniformAcrossPaths(t *testing.T) {
	t.Parallel()

	const constraint = `Cpus == 8`
	parse := func(t *testing.T) *vm.Query {
		t.Helper()
		q, err := vm.Parse(constraint)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}

	cases := []struct {
		name string
		// run drains the path and returns the collection it ran against.
		run func(t *testing.T) *Collection
		// projected names the attributes the path also SELECTS, which must gain read
		// demand of their own. Empty means a whole-ad path.
		projected []string
	}{{
		name: "Query",
		run: func(t *testing.T) *Collection {
			c := projTestCollection(t)
			for range c.Query(parse(t)) {
			}
			return c
		},
	}, {
		name: "QueryRaw",
		run: func(t *testing.T) *Collection {
			c := projTestCollection(t)
			for range c.QueryRaw(parse(t)) {
			}
			return c
		},
	}, {
		name: "QueryRawProjected",
		run: func(t *testing.T) *Collection {
			c := projTestCollection(t)
			for range c.QueryRawProjected(parse(t), []string{"Name"}, false, false) {
			}
			return c
		},
		projected: []string{"Name"},
	}, {
		name: "QueryProject",
		run: func(t *testing.T) *Collection {
			c := projTestCollection(t)
			for range c.QueryProject(parse(t), []string{"Name"}) {
			}
			return c
		},
		projected: []string{"Name"},
	}, {
		name: "QueryRawWire",
		run: func(t *testing.T) *Collection {
			c := projTestCollectionInline(t) // the wire path needs inline names
			for range c.QueryRawWire(parse(t), nil, false) {
			}
			return c
		},
	}, {
		name: "QueryRawFromSeq",
		run: func(t *testing.T) *Collection {
			c := projTestCollection(t)
			seq, _ := c.QueryRawFromSeq(parse(t), nil, SeqCursor{}, 0)
			for range seq {
			}
			return c
		},
	}, {
		name: "QueryAsOf",
		run: func(t *testing.T) *Collection {
			c := timeTravelProjCollection(t)
			seq, err := c.QueryAsOf(parse(t), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			for range seq {
			}
			return c
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := tc.run(t)
			if n := demandReads(c, "Cpus"); n == 0 {
				t.Errorf("%s evaluated %q but recorded no read demand for Cpus; "+
					"the hot set and the columnar hot tier rank by this counter", tc.name, constraint)
			}
			for _, p := range tc.projected {
				if n := demandReads(c, p); n == 0 {
					t.Errorf("%s projected %s but recorded no read demand for it", tc.name, p)
				}
			}
			// An attribute the query neither filters on nor selects is not demand,
			// whatever the path returns -- a whole-ad path must not report the
			// attributes it happens to render.
			if n := demandReads(c, "OpSys"); n != 0 {
				t.Errorf("%s recorded read demand (%d) for OpSys, which it neither filters on nor projects", tc.name, n)
			}
		})
	}
}

// timeTravelProjCollection is projTestCollection with time travel on, so QueryAsOf
// resolves rather than refusing.
func timeTravelProjCollection(t *testing.T) *Collection {
	t.Helper()
	c := New(Options{TimeTravel: &TimeTravelOptions{
		MaxDistance: time.Hour, CheckpointInterval: time.Millisecond,
	}})
	t.Cleanup(func() { c.Close() })
	for i, text := range projTestAdTexts {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte{byte('a' + i)}, ad); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// TestProjectedScanRecordsBothRoles pins that a projected query reports BOTH halves of
// what it reads out of each record -- the constraint's attributes and the projection's
// -- rather than only whichever half its path happens to know about. An attribute in
// both roles is counted in each, because each is a separate access per record.
func TestProjectedScanRecordsBothRoles(t *testing.T) {
	t.Parallel()
	c := projTestCollection(t)
	q, err := vm.Parse(`Cpus == 8`)
	if err != nil {
		t.Fatal(err)
	}
	// Cpus is both filtered on and selected; Name is only selected.
	for range c.QueryRawProjected(q, []string{"Cpus", "Name"}, false, false) {
	}
	both, only := demandReads(c, "Cpus"), demandReads(c, "Name")
	if only == 0 {
		t.Fatal("projected attribute recorded no read demand")
	}
	if both <= only {
		t.Errorf("Cpus is filtered AND projected but scored %d, no more than the projected-only Name (%d); "+
			"each role is a separate access per record", both, only)
	}
}
