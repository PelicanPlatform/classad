package classad

import "testing"

// These tests cover the old-ClassAd matchmaking fallthrough: during a match, an
// unqualified attribute reference that is not found in the evaluating (MY) ad
// resolves against the TARGET ad, mirroring the C++ classad library's
// ClassAd::alternateScope (src/classad/attrrefs.cpp FindExpr) as wired up by
// MatchClassAd under old-ClassAd semantics (src/classad/matchClassad.cpp).

func mustParse(t *testing.T, s string) *ClassAd {
	t.Helper()
	ad, err := Parse(s)
	if err != nil {
		t.Fatalf("parse %q failed: %v", s, err)
	}
	return ad
}

// A job Requirements expression naming a machine attribute (Memory) without
// TARGET qualification must match against the machine ad, as in HTCondor.
func TestTargetFallthroughJobRequirements(t *testing.T) {
	job := mustParse(t, `[Requirements = Memory >= 1024]`)
	machine := mustParse(t, `[Requirements = true; Memory = 2048]`)
	match := NewMatchClassAd(job, machine)

	if !match.Match() {
		t.Fatalf("expected job [Memory >= 1024] to match machine with Memory=2048")
	}

	smallMachine := mustParse(t, `[Requirements = true; Memory = 512]`)
	match.ReplaceRightAd(smallMachine)
	if match.Match() {
		t.Fatalf("expected job [Memory >= 1024] NOT to match machine with Memory=512")
	}
}

// A startd START-style expression naming a job attribute (Owner) without
// TARGET qualification must match against the job ad.
func TestTargetFallthroughMachineStart(t *testing.T) {
	machine := mustParse(t, `[Requirements = Owner != "smith"]`)
	job := mustParse(t, `[Requirements = true; Owner = "jones"]`)
	match := NewMatchClassAd(job, machine)

	if !match.Match() {
		t.Fatalf("expected machine [Owner != \"smith\"] to match job with Owner=\"jones\"")
	}

	smithJob := mustParse(t, `[Requirements = true; Owner = "smith"]`)
	match.ReplaceLeftAd(smithJob)
	if match.Match() {
		t.Fatalf("expected machine [Owner != \"smith\"] NOT to match job with Owner=\"smith\"")
	}
}

// An explicit MY.attr reference must NOT fall through to the target: MY means
// this ad only. In C++ terms, MY.attr is an expr.attr reference, which the
// !expr guard in FindExpr excludes from the alternateScope lookup.
func TestTargetNoFallthroughForMyScope(t *testing.T) {
	job := mustParse(t, `[CheckMy = MY.Memory; CheckBare = Memory]`)
	machine := mustParse(t, `[Memory = 2048]`)
	NewMatchClassAd(job, machine)

	if val := job.EvaluateAttr("CheckMy"); !val.IsUndefined() {
		t.Fatalf("MY.Memory on a job without Memory must stay undefined, got %v", val)
	}
	if val := job.EvaluateAttr("CheckBare"); !val.IsInteger() {
		t.Fatalf("bare Memory should fall through to the machine ad, got %v", val)
	} else if n, _ := val.IntValue(); n != 2048 {
		t.Fatalf("bare Memory fallthrough returned %d, want 2048", n)
	}
}

// Explicit TARGET.attr keeps working alongside the unqualified fallthrough,
// and both sides of a Symmetry call use fallthrough in the same evaluation.
func TestTargetFallthroughBothSidesSymmetry(t *testing.T) {
	job := mustParse(t, `[Requirements = Memory >= 1024 && TARGET.Cpus > 1; Owner = "jones"]`)
	machine := mustParse(t, `[Requirements = Owner != "smith"; Memory = 4096; Cpus = 8]`)
	match := NewMatchClassAd(job, machine)

	if !match.Symmetry("Requirements", "Requirements") {
		t.Fatalf("expected bilateral match with fallthrough on both sides")
	}

	badJob := mustParse(t, `[Requirements = Memory >= 1024 && TARGET.Cpus > 1; Owner = "smith"]`)
	match.ReplaceLeftAd(badJob)
	if match.Symmetry("Requirements", "Requirements") {
		t.Fatalf("expected symmetry to fail when the right side's fallthrough rejects Owner")
	}
}

// The expression found in the target evaluates in the TARGET's own context, so
// its own unqualified references resolve target-ad-first and then fall back to
// the original ad (one level of ping-pong). This mirrors C++: LookupInScope on
// alternateScope sets the eval scope to that ad, whose own alternateScope
// points back.
func TestTargetFallthroughChainedPingPong(t *testing.T) {
	// left.Answer -> X (not in left) -> right.X = Y * 2; Y not in right ->
	// falls back to left.Y = 5 => 10.
	left := mustParse(t, `[Answer = X; Y = 5]`)
	right := mustParse(t, `[X = Y * 2]`)
	NewMatchClassAd(left, right)

	val := left.EvaluateAttr("Answer")
	if !val.IsInteger() {
		t.Fatalf("expected integer from chained fallthrough, got %v", val)
	}
	if n, _ := val.IntValue(); n != 10 {
		t.Fatalf("chained fallthrough returned %d, want 10", n)
	}
}

// Mutually-recursive fallthrough references (left.a -> right.b -> left.a ...)
// must terminate via cycle protection instead of hanging or overflowing the
// stack. The C++ eval_stack guard reports the cycling reference as UNDEFINED;
// this library's pre-existing cyclic guard reports cycles (e.g. [a=b; b=a]) as
// ERROR, and the match-fallthrough cycle inherits that. Either is a failed
// (non-true) Requirements; the essential property is termination.
func TestTargetFallthroughCycleTerminates(t *testing.T) {
	left := mustParse(t, `[a = b; Requirements = a]`)
	right := mustParse(t, `[b = a]`)
	match := NewMatchClassAd(left, right)

	val := left.EvaluateAttr("a")
	if !val.IsError() && !val.IsUndefined() {
		t.Fatalf("cyclic cross-ad reference should be error/undefined, got %v", val)
	}
	if match.Match() {
		t.Fatalf("cyclic Requirements must not match")
	}
}

// Rank expressions get the same fallthrough: a job Rank naming an unqualified
// machine attribute evaluates against the machine ad.
func TestTargetFallthroughRank(t *testing.T) {
	job := mustParse(t, `[Requirements = true; Rank = KFlops / 1000.0]`)
	machine := mustParse(t, `[Requirements = true; KFlops = 4000]`)
	match := NewMatchClassAd(job, machine)

	rank, ok := match.EvaluateRankLeft()
	if !ok {
		t.Fatalf("expected rank to evaluate via fallthrough")
	}
	if rank != 4.0 {
		t.Fatalf("rank = %g, want 4.0", rank)
	}
}

// ReplaceRightAd (the negotiator's hot loop: one MatchClassAd reused across
// many candidate ads) must keep fallthrough working against the new right ad
// and must not leak attributes from the replaced ad.
func TestTargetFallthroughReplaceRightAdReuse(t *testing.T) {
	job := mustParse(t, `[Requirements = Memory >= 1024]`)
	match := NewMatchClassAd(job, nil)

	machines := []struct {
		ad        string
		wantMatch bool
	}{
		{`[Requirements = true; Memory = 2048]`, true},
		{`[Requirements = true; Memory = 512]`, false},
		{`[Requirements = true]`, false}, // no Memory anywhere: no stale value from the 2048 ad
		{`[Requirements = true; Memory = 8192]`, true},
	}
	for i, m := range machines {
		match.ReplaceRightAd(mustParse(t, m.ad))
		if got := match.Match(); got != m.wantMatch {
			t.Fatalf("candidate %d (%s): match = %v, want %v", i, m.ad, got, m.wantMatch)
		}
	}

	// Detach the right ad entirely: fallthrough must stop, not reuse a stale
	// target.
	match.ReplaceRightAd(nil)
	if val := job.EvaluateAttr("Requirements"); !val.IsUndefined() {
		t.Fatalf("with no right ad, unqualified Memory must be undefined, got %v", val)
	}
}

// Without a target set (plain, non-match evaluation) unqualified references
// that miss stay undefined -- the fallthrough only applies when SetTarget has
// wired up a match context.
func TestNoTargetNoFallthrough(t *testing.T) {
	ad := mustParse(t, `[Requirements = Memory >= 1024]`)
	if val := ad.EvaluateAttr("Requirements"); !val.IsUndefined() {
		t.Fatalf("without a target, missing Memory must yield undefined, got %v", val)
	}
}
