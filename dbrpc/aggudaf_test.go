package dbrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/PelicanPlatform/classad/db"
)

// A user-defined aggregate reduces fine in the db package and cannot be NAMED in any frame here,
// so both ends refuse it. The pair of tests below is the point: a client that refused but a server
// that accepted would leave the unnameable request reachable from any other client, and a server
// that refused but a client that sent would turn a local mistake into a remote error.

func TestClientRefusesAUserAggregate(t *testing.T) {
	c, cleanup := testPair(t)
	defer cleanup()

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"Aggregate", func() error {
			_, err := c.Aggregate(context.Background(), "true", []string{"Owner"},
				[]AggSpec{{Func: db.AggUser, UserFunc: "p95", Arg: "Cpus"}})
			return err
		}},
		{"AggregateBucketed", func() error {
			_, err := c.AggregateBucketed(context.Background(), "true", []GroupCol{{Attr: "Owner"}},
				[]AggSpec{{Func: db.AggUser, UserFunc: "p95", Arg: "Cpus"}})
			return err
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			err := call.run()
			if !errors.Is(err, ErrUserAggregateUnsupported) {
				t.Fatalf("want ErrUserAggregateUnsupported, got %v", err)
			}
		})
	}
}

// TestServerRefusesAUserAggregateFrame goes under the client, because the client's own refusal is
// what stops this frame ever being built. A server trusting the selector would reach the engine
// asking for the aggregate registered under the EMPTY NAME.
func TestServerRefusesAUserAggregateFrame(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		var b []byte
		b = putI32(b, 1)
		b = putStr(putU8(b, byte(db.AggUser)), "Cpus")
		if filtered {
			b = putStr(b, "")
		}
		var wrote [][]byte
		aggs, ok := readAggSpecs(&reader{b: b}, 7, func(f []byte) { wrote = append(wrote, f) }, filtered)
		if ok {
			t.Fatalf("filtered=%v: the server accepted a frame naming a user aggregate: %+v", filtered, aggs)
		}
		if len(wrote) != 1 {
			t.Fatalf("filtered=%v: want one refusal frame, got %d", filtered, len(wrote))
		}
	}
}

// TestServerStillReadsTheAggregatesItCanServe guards the refusal above from being a refusal of
// everything, which would pass the test it was written for and break every aggregate there is.
func TestServerStillReadsTheAggregatesItCanServe(t *testing.T) {
	var b []byte
	b = putI32(b, 2)
	b = putStr(putU8(b, byte(db.AggCount)), "*")
	b = putStr(putU8(b, byte(db.AggCountDistinct)), "Owner")
	aggs, ok := readAggSpecs(&reader{b: b}, 7, func([]byte) { t.Fatal("refused an ordinary request") }, false)
	if !ok || len(aggs) != 2 {
		t.Fatalf("ok=%v aggs=%+v", ok, aggs)
	}
	if aggs[0].Func != db.AggCount || aggs[1].Func != db.AggCountDistinct {
		t.Fatalf("decoded the wrong functions: %+v", aggs)
	}
}
