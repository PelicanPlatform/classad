package vm

import "fmt"

// FAULTS: why a script stopped, and where.
//
// A fault is not a value. Expressions are total -- UNDEFINED and ERROR are values every operator
// accepts and returns -- so nothing an expression does can fault. Statements are different: an `if`
// has to choose, and a condition that is neither true nor false leaves it nothing to choose from.
// That is the only fault class reachable today.
//
// WHY A REPORT AND NOT JUST A VALUE. Without one, a faulted script returns ERROR and the caller
// cannot tell it from an expression that evaluated to ERROR on purpose, nor find which of five
// conditions was the undefined one. That is the failure mode the design calls "a transform that
// quietly did nothing", and the whole point of faulting rather than treating absence as false is to
// make it visible. A value alone cannot carry that.
//
// WHAT THIS IS NOT, YET. In the vector context a fault belongs to ONE RECORD of a batch: the record
// is cleared from the live mask, its result is ERROR, and the report aggregates per site -- "F1 at
// this site: 8192 records, undefined condition" -- rather than per record, because per-record
// diagnostics are not affordable at 8192. None of that exists; there is no vector executor for
// scripts. What exists is the scalar case, where n = 1 and "fault the record" and "fault the script"
// coincide, so one site and one class is the whole report. The site table is laid out so the vector
// report adds counts to these entries rather than replacing them.
//
// SITES ARE NOT SOURCE POSITIONS, because there is no parser yet. A site records the construct that
// could fault and which one it is -- "the 2nd if's condition" -- which is enough to find it in a
// script built through the builder API. A line and column replace the ordinal when a parser exists,
// and nothing above this changes when they do.

// FaultClass is why a fault happened. The numbering follows the design's F1..F4.
type FaultClass uint8

const (
	// FaultNone is the zero value: no fault.
	FaultNone FaultClass = iota
	// FaultCondition (F1) is an if/while condition that is not boolean-equivalent: UNDEFINED, ERROR,
	// or a value no truthiness rule covers such as a string, list or ad.
	FaultCondition
)

// String names the class for a diagnostic.
func (c FaultClass) String() string {
	switch c {
	case FaultNone:
		return "none"
	case FaultCondition:
		return "condition"
	}
	return fmt.Sprintf("class(%d)", uint8(c))
}

// faultSite is one place in a program that can fault.
type faultSite struct {
	class FaultClass
	kind  string // the construct: "if" or "while"
	ord   int    // which one of that kind, counted from 1 in the order they were built
}

// Fault is what a faulted run reports.
type Fault struct {
	Class FaultClass
	// Where names the construct, e.g. `if #2 condition`. It is a diagnostic string and not a
	// structured position, because there are no positions until there is a parser.
	Where string
}

// Error renders the fault as a sentence, so a caller can hand it to a user unchanged.
func (f *Fault) Error() string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%s fault at %s: the condition was neither true nor false", f.Class, f.Where)
}

// addFaultSite records a site and returns its index, which is the operand OpFault carries.
func (p *Program) addFaultSite(class FaultClass, kind string) int32 {
	ord := 1
	for _, s := range p.faultSites {
		if s.kind == kind {
			ord++
		}
	}
	p.faultSites = append(p.faultSites, faultSite{class: class, kind: kind, ord: ord})
	return int32(len(p.faultSites) - 1)
}

// faultAt builds the report for a site index, or nil if there is no such site.
//
// An out-of-range index is reported as an unknown site rather than ignored: a fault that happened
// must not be reported as a clean result because its site table entry is missing.
func (p *Program) faultAt(site int32) *Fault {
	if site < 0 || int(site) >= len(p.faultSites) {
		return &Fault{Class: FaultCondition, Where: "an unrecorded site"}
	}
	s := p.faultSites[site]
	return &Fault{Class: s.class, Where: fmt.Sprintf("%s #%d condition", s.kind, s.ord)}
}
