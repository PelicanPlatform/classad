package vm

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

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
// A FAULT IS PER RECORD IN THE VECTOR EXECUTOR. A batch runs a script's statements over many records
// at once (see vecpred.go), and a condition that is neither true nor false for SOME of them faults
// only those: they are cleared from the live mask, which is monotone, and their results are forced
// to ERROR when the batch ends. The scalar case is the degenerate one, where n = 1 and "fault the
// record" and "fault the script" coincide.
//
// WHAT A VECTOR RUN DOES NOT PRODUCE IS A REPORT. The fault plane says which records faulted and
// nothing says where or why, because a per-record diagnostic is not affordable at 8192 records and
// the useful form is an aggregate -- "the 2nd if's condition: 8192 records, undefined". The site
// table is laid out so that aggregate adds counts to these entries rather than replacing them. A
// scalar run reports fully: one site, one class and one cause.
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

// FaultCause says what an unreadable condition actually held.
//
// The class says a condition could not be read; the cause says what it held, and the three are
// different mistakes with different fixes. UNDEFINED is almost always a misspelled attribute or one
// the ad does not carry. ERROR is an expression that went wrong further down -- a string compared to
// a number, a division by zero. A value with no truthiness at all is a condition that was never
// boolean to begin with, like naming a string attribute where a comparison was meant. Reporting all
// three as "neither true nor false" is accurate and tells the author nothing they did not know.
type FaultCause uint8

const (
	// CauseUnknown is the zero value: no fault, or a fault whose condition was not recorded.
	CauseUnknown FaultCause = iota
	// CauseUndefined is an UNDEFINED condition.
	CauseUndefined
	// CauseError is an ERROR condition.
	CauseError
	// CauseNotBoolean is a condition with no truthiness: a string, a list or an ad. Numbers are not
	// here -- ClassAd reads a number's truthiness, so `if 0` is false and faults nothing.
	CauseNotBoolean
)

// describe renders the cause as the second half of the fault sentence.
func (c FaultCause) describe() string {
	switch c {
	case CauseUndefined:
		return "the condition was undefined"
	case CauseError:
		return "the condition was an error value"
	case CauseNotBoolean:
		return "the condition was a string, a list or an ad, which has no truthiness"
	}
	return "the condition was neither true nor false"
}

// conditionCause classifies the value a faulting condition held.
//
// It asks the value rather than classad.ConditionState, which cannot answer: that function folds an
// ERROR value and a string into one CondError, because the two take the same branch. They are the
// same CONTROL-FLOW answer and different mistakes, and this is the only place that difference is
// wanted.
func conditionCause(v classad.Value) FaultCause {
	switch {
	case v.IsUndefined():
		return CauseUndefined
	case v.IsError():
		return CauseError
	}
	return CauseNotBoolean
}

// faultSite is one place in a program that can fault.
type faultSite struct {
	class FaultClass
	kind  string // the construct: "if", "while" or "for"
	ord   int    // which one of that kind, counted from 1 in the order they were built
}

// Fault is what a faulted run reports.
type Fault struct {
	Class FaultClass
	// Cause is what the condition held, which the class alone does not say.
	Cause FaultCause
	// Where names the construct, e.g. `if #2 condition`. It is a diagnostic string and not a
	// structured position, because there are no positions until there is a parser.
	Where string
}

// Error renders the fault as a sentence, so a caller can hand it to a user unchanged.
func (f *Fault) Error() string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%s fault at %s: %s", f.Class, f.Where, f.Cause.describe())
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

// renameLastFaultSite relabels the site added most recently, for a construct that is LOWERED ONTO
// another one and must still name itself in a diagnostic: `for` compiles to the counted `while`
// that records the site, and "while #1 condition" is not a line the author can find.
//
// The ordinal is recomputed, because it counts the sites of one kind and the site has just changed
// kind. Renaming rather than parameterizing the construct that records the site is what keeps the
// loop lowering in one place.
func (p *Program) renameLastFaultSite(kind string) {
	if len(p.faultSites) == 0 {
		return
	}
	last := len(p.faultSites) - 1
	ord := 1
	for _, s := range p.faultSites[:last] {
		if s.kind == kind {
			ord++
		}
	}
	p.faultSites[last].kind = kind
	p.faultSites[last].ord = ord
}

// faultAt builds the report for a site index, or nil if there is no such site.
//
// An out-of-range index is reported as an unknown site rather than ignored: a fault that happened
// must not be reported as a clean result because its site table entry is missing.
func (p *Program) faultAt(site int32, cond classad.Value) *Fault {
	cause := conditionCause(cond)
	if site < 0 || int(site) >= len(p.faultSites) {
		return &Fault{Class: FaultCondition, Cause: cause, Where: "an unrecorded site"}
	}
	s := p.faultSites[site]
	return &Fault{Class: s.class, Cause: cause, Where: fmt.Sprintf("%s #%d condition", s.kind, s.ord)}
}
