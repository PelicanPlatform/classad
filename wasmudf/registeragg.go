package wasmudf

import (
	"context"
	"fmt"

	"github.com/PelicanPlatform/classad/db"
)

// AggregateRegistry is the part of db's aggregate registry this package needs.
//
// It is an interface for the reason the scalar Registry is one: a test can hand
// RegisterAggregate a registry it can inspect, which is the only way to assert
// that loading registers the right name against the right aggregate without
// writing to the process-wide table every other test in this package shares.
//
// Unlike the scalar case there is no adapter waiting on a function that does not
// exist yet: db.RegisterAggregate is here today, and DBRegistry calls it.
type AggregateRegistry interface {
	RegisterAggregate(name string, agg db.Aggregate) error
}

// DBRegistry returns the process-wide aggregate registry in package db.
func DBRegistry() AggregateRegistry { return dbRegistry{} }

type dbRegistry struct{}

func (dbRegistry) RegisterAggregate(name string, agg db.Aggregate) error {
	return db.RegisterAggregate(name, agg)
}

// RegisterAggregate loads wasm as name and registers it, so a query naming name
// as a user-defined aggregate reduces with the guest.
//
// Loading comes first and registration second, which is the order that matters:
// every static problem with the module is a returned error here, and a name is
// only ever registered against a module that compiled, exports what it must,
// speaks this ABI, instantiated, and produced an identity state that fits. db's
// registry does take a withdrawal, but a name registered against an unproven
// module is one queries reduce with until somebody withdraws it, which is a
// window this order does not open.
//
// On a registration error the aggregate stays loaded in the Loader, reachable
// through LookupAggregate, for the same reason a scalar UDF does: the usual cause
// is that the name is taken, and discarding a compiled module on the way out
// makes a retry under another name recompile for no reason.
func RegisterAggregate(ctx context.Context, l *Loader, reg AggregateRegistry, name string, wasm []byte) (*UDAF, error) {
	return RegisterAggregateWith(ctx, l, reg, name, wasm, LoadOptions{})
}

// RegisterAggregateWith is RegisterAggregate with the per-module options an admin
// sets. The trust label arrives HERE, from configuration, and reaches the Loader
// as a Go value; there is no path by which the module could influence it. An
// aggregate is at least as untrusted as a function and runs under the same step
// budget, memory cap and wall-clock backstop.
func RegisterAggregateWith(ctx context.Context, l *Loader, reg AggregateRegistry, name string, wasm []byte, lo LoadOptions) (*UDAF, error) {
	a, err := l.LoadAggregateWith(ctx, name, wasm, lo)
	if err != nil {
		return nil, err
	}
	if err := reg.RegisterAggregate(name, a.Aggregate()); err != nil {
		return a, fmt.Errorf("wasmudf: registering aggregate %s: %w", name, err)
	}
	return a, nil
}
