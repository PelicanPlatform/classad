package classad

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// These tests share ONE ClassAd between goroutines that only read it. Every read
// method must leave the ad untouched, so under -race they report nothing, and the
// output of each goroutine must equal what a single goroutine produces.

const (
	readers   = 8
	readIters = 200
)

// hammer runs fn from readers goroutines, readIters times each.
func hammer(t *testing.T, fn func()) {
	t.Helper()
	var wg sync.WaitGroup
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < readIters; i++ {
				fn()
			}
		}()
	}
	wg.Wait()
}

const concurrentAdSrc = `[Zeta = 1; Alpha = 2; Mid = "x"; Beta = Alpha + 1; ClaimId = "secret";
	Nested = [Zz = 1; Aa = Zz + 1]; List = {Beta, Alpha, 3}; Gamma = Beta * Alpha]`

// adBuilders produce the same ad through every construction path, each leaving
// its attributes in source (unsorted) order where the path allows it.
var adBuilders = map[string]func(t *testing.T) *ClassAd{
	"Parse": func(t *testing.T) *ClassAd {
		ad, err := Parse(concurrentAdSrc)
		if err != nil {
			t.Fatal(err)
		}
		return ad
	},
	"ParseOld": func(t *testing.T) *ClassAd {
		ad, err := ParseOld("Zeta = 1\nAlpha = 2\nMid = \"x\"\nBeta = Alpha + 1\nClaimId = \"secret\"\n" +
			"Nested = [Zz = 1; Aa = Zz + 1]\nList = {Beta, Alpha, 3}\nGamma = Beta * Alpha")
		if err != nil {
			t.Fatal(err)
		}
		return ad
	},
	"UnmarshalJSON": func(t *testing.T) *ClassAd {
		var ad ClassAd
		src := `{"Zeta":1,"Alpha":2,"Mid":"x","Beta":"\/Expr(Alpha + 1)\/","ClaimId":"secret",` +
			`"Nested":{"Zz":1,"Aa":"\/Expr(Zz + 1)\/"},"List":["\/Expr(Beta)\/","\/Expr(Alpha)\/",3],` +
			`"Gamma":"\/Expr(Beta * Alpha)\/"}`
		if err := json.Unmarshal([]byte(src), &ad); err != nil {
			t.Fatal(err)
		}
		return &ad
	},
	"Reader": func(t *testing.T) *ClassAd {
		r := NewReader(strings.NewReader(concurrentAdSrc))
		if !r.Next() {
			t.Fatalf("reader: %v", r.Err())
		}
		return r.ClassAd()
	},
	"FromAST": func(t *testing.T) *ClassAd {
		parsed, err := Parse(concurrentAdSrc)
		if err != nil {
			t.Fatal(err)
		}
		// Rebuild the AST in source order, as a decoder would hand it over.
		src := parsed.AST()
		attrs := make([]*ast.AttributeAssignment, len(src.Attributes))
		copy(attrs, src.Attributes)
		for i, j := 0, len(attrs)-1; i < j; i, j = i+1, j-1 {
			attrs[i], attrs[j] = attrs[j], attrs[i]
		}
		return FromAST(&ast.ClassAd{Attributes: attrs})
	},
	"Insert": func(t *testing.T) *ClassAd {
		ad := New()
		for _, kv := range [][2]string{
			{"Zeta", "1"}, {"Alpha", "2"}, {"Mid", `"x"`}, {"Beta", "Alpha + 1"}, {"ClaimId", `"secret"`},
			{"Nested", "[Zz = 1; Aa = Zz + 1]"}, {"List", "{Beta, Alpha, 3}"}, {"Gamma", "Beta * Alpha"},
		} {
			e, err := ParseExpr(kv[1])
			if err != nil {
				t.Fatal(err)
			}
			ad.InsertExpr(kv[0], e)
		}
		return ad
	},
}

// readOps are the read-only operations exercised concurrently. Each returns a
// string that must be identical across goroutines and equal to the reference.
var readOps = map[string]func(ad *ClassAd) string{
	"MarshalJSON": func(ad *ClassAd) string {
		b, err := json.Marshal(ad)
		if err != nil {
			return "error: " + err.Error()
		}
		return string(b)
	},
	"MarshalJSONWithPrivate": func(ad *ClassAd) string {
		b, err := ad.MarshalJSONWithPrivate()
		if err != nil {
			return "error: " + err.Error()
		}
		return string(b)
	},
	"String":            func(ad *ClassAd) string { return ad.String() },
	"StringWithPrivate": func(ad *ClassAd) string { return ad.StringWithPrivate() },
	"MarshalOld":        func(ad *ClassAd) string { return ad.MarshalOld() },
	"GetAttributes":     func(ad *ClassAd) string { return strings.Join(ad.GetAttributes(), ",") },
	"AST":               func(ad *ClassAd) string { return (&ClassAd{ad: ad.AST()}).StringWithPrivate() },
	"Lookup": func(ad *ClassAd) string {
		var b strings.Builder
		for _, n := range []string{"zeta", "ALPHA", "Beta", "nested", "missing"} {
			if e, ok := ad.Lookup(n); ok {
				b.WriteString(e.String())
			}
			b.WriteByte('|')
		}
		return b.String()
	},
	"Evaluate": func(ad *ClassAd) string {
		var b strings.Builder
		for _, n := range []string{"Gamma", "Beta", "Mid", "List", "Nested", "missing"} {
			b.WriteString(ad.EvaluateAttr(n).String())
			b.WriteByte('|')
		}
		g, _ := ad.EvaluateAttrInt("Gamma")
		b.WriteString(Quote(string(rune('0' + g))))
		v, _ := ad.EvaluateExprString("Nested.Aa + size(List)")
		b.WriteString(v.String())
		return b.String()
	},
	"Flatten": func(ad *ClassAd) string {
		e, _ := ParseExpr("Gamma + Unknown")
		return ad.Flatten(e).String()
	},
	"Redacted": func(ad *ClassAd) string { return ad.Redacted().StringWithPrivate() },
}

func TestConcurrentReadsOfOneAd(t *testing.T) {
	for bname, build := range adBuilders {
		for oname, op := range readOps {
			t.Run(bname+"/"+oname, func(t *testing.T) {
				want := op(build(t)) // reference: a fresh ad, one goroutine
				ad := build(t)       // the shared ad, never read before the race
				var mu sync.Mutex
				var bad []string
				hammer(t, func() {
					if got := op(ad); got != want {
						mu.Lock()
						if len(bad) < 3 {
							bad = append(bad, got)
						}
						mu.Unlock()
					}
				})
				for _, got := range bad {
					t.Errorf("concurrent %s:\n got %s\nwant %s", oname, got, want)
				}
			})
		}
	}
}

// TestConcurrentMixedReadsOfOneAd runs every read operation at once on one ad.
func TestConcurrentMixedReadsOfOneAd(t *testing.T) {
	for bname, build := range adBuilders {
		t.Run(bname, func(t *testing.T) {
			ref := build(t)
			want := map[string]string{}
			for name, op := range readOps {
				want[name] = op(ref)
			}
			ad := build(t)
			var wg sync.WaitGroup
			errs := make(chan string, len(readOps)*readers)
			for name, op := range readOps {
				for g := 0; g < readers/2; g++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for i := 0; i < readIters/4; i++ {
							if got := op(ad); got != want[name] {
								errs <- name + ": got " + got + " want " + want[name]
								return
							}
						}
					}()
				}
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				t.Error(e)
			}
		})
	}
}

// TestConcurrentMarshalOfOneAd is the reduced production race: two goroutines
// marshaling one freshly parsed ad.
func TestConcurrentMarshalOfOneAd(t *testing.T) {
	ad, err := Parse(`[Zeta = 1; Alpha = 2; Mid = "x"; Beta = Alpha + 1]`)
	if err != nil {
		t.Fatal(err)
	}
	hammer(t, func() { _, _ = json.Marshal(ad) })
}

// TestConcurrentEvalOfOneAd: concurrent evaluation of a non-literal attribute
// used to write the ad's cycle-detection map, a fatal concurrent map write.
func TestConcurrentEvalOfOneAd(t *testing.T) {
	ad, err := Parse(`[A = 1; B = A + 1; C = B * 2]`)
	if err != nil {
		t.Fatal(err)
	}
	_ = ad.String()
	hammer(t, func() {
		if v, ok := ad.EvaluateAttrInt("C"); !ok || v != 4 {
			t.Errorf("C = %v, %v; want 4", v, ok)
		}
	})
}

// TestConcurrentEqual compares two shared ads from many goroutines.
func TestConcurrentEqual(t *testing.T) {
	a := adBuilders["Parse"](t)
	b := adBuilders["Insert"](t)
	hammer(t, func() {
		if !a.Equal(b) || !b.Equal(a) {
			t.Error("ads built from the same attributes compare unequal")
		}
	})
}

// TestConcurrentEvalWithTarget evaluates against one shared scope ad with a
// different target per goroutine. EvalWithContext used to install the target on
// the scope ad for the duration of the call, so goroutines saw each other's
// TARGET (and the last restore could leave a stale one behind).
func TestConcurrentEvalWithTarget(t *testing.T) {
	scope, err := Parse(`[Want = TARGET.Have * 10]`)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := ParseExpr("Want + TARGET.Have")
	if err != nil {
		t.Fatal(err)
	}
	targets := make([]*ClassAd, readers)
	for i := range targets {
		targets[i] = New()
		targets[i].InsertAttr("Have", int64(i))
	}
	var wg sync.WaitGroup
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < readIters; i++ {
				v := scope.EvaluateExprWithTarget(expr, targets[g])
				if got, _ := v.IntValue(); got != int64(11*g) {
					t.Errorf("goroutine %d: got %v, want %d", g, v, 11*g)
					return
				}
			}
		}()
	}
	wg.Wait()
	if scope.GetTarget() != nil {
		t.Errorf("EvaluateExprWithTarget left a TARGET on the scope ad")
	}
}

// TestConcurrentMatchAgainstSharedSlot: many analyses evaluate different job ads
// against one cached slot ad with EvaluateExprAgainst, which binds MY/TARGET for
// the call without writing either ad.
func TestConcurrentMatchAgainstSharedSlot(t *testing.T) {
	slot, err := Parse(`[Memory = 4096; Cpus = 4; Requirements = TARGET.RequestMemory <= Memory]`)
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := ParseExpr("TARGET.Requirements && RequestCpus <= TARGET.Cpus")
	if err != nil {
		t.Fatal(err)
	}
	jobs := make([]*ClassAd, readers)
	for i := range jobs {
		jobs[i], err = Parse(`[RequestCpus = 1; RequestMemory = ` + []string{"1024", "8192"}[i%2] + `]`)
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := g%2 == 0
			for i := 0; i < readIters; i++ {
				v := jobs[g].EvaluateExprAgainst(reqs.internal(), slot)
				if got, ok := v.BoolValue(); ok != nil || got != want {
					t.Errorf("job %d: got %v, want %v", g, v, want)
					return
				}
			}
		}()
	}
	wg.Wait()
	if slot.GetTarget() != nil || jobs[0].GetTarget() != nil {
		t.Error("EvaluateExprAgainst set TARGET on an ad")
	}
}

// TestRecordValueIsAView: the ClassAd inside a nested-record value reads the
// enclosing ad's AST. Reading it must not reorder that AST, and writing it must
// detach rather than change the enclosing ad.
func TestRecordValueIsAView(t *testing.T) {
	src := `[R = [Zz = 1; Aa = 2; Mm = Aa + Zz]]`
	ad, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	before := ad.String()
	rec, err := ad.EvaluateAttr("R").ClassAdValue()
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.String(); got != "[Zz = 1; Aa = 2; Mm = (Aa + Zz)]" {
		t.Errorf("record view renders %s, want source order", got)
	}
	if v, _ := rec.EvaluateAttrInt("mm"); v != 3 {
		t.Errorf("Mm = %d, want 3", v)
	}
	if got := rec.AST().Attributes[0].Name; got != "Aa" {
		t.Errorf("AST of a view starts with %s, want Aa (sorted)", got)
	}
	_, _ = json.Marshal(ad)
	other, _ := Parse(src)
	_ = ad.Equal(other)
	if after := ad.String(); after != before {
		t.Fatalf("reading the record changed the ad:\nbefore %s\nafter  %s", before, after)
	}

	rec.InsertAttr("Zz", 100)
	rec.InsertAttr("New", 5)
	rec.Delete("Aa")
	if after := ad.String(); after != before {
		t.Fatalf("writing the record value changed the enclosing ad:\nbefore %s\nafter  %s", before, after)
	}
	if got := rec.String(); got != "[Mm = (Aa + Zz); New = 5; Zz = 100]" {
		t.Errorf("detached record = %s", got)
	}
	rec2, _ := ad.EvaluateAttr("R").ClassAdValue()
	rec2.Clear()
	if after := ad.String(); after != before {
		t.Fatalf("Clear on the record value cleared the enclosing ad's record:\nbefore %s\nafter  %s", before, after)
	}
}

// TestSelectDoesNotChainSharedAd: selecting from a record value links the record's
// scope to the selecting ad for that evaluation only.
func TestSelectDoesNotChainSharedAd(t *testing.T) {
	shared, _ := Parse(`[X = Y]`)
	ad, _ := Parse(`[Y = 7]`)
	ev := NewEvaluator(ad)
	ev.SetResolver(func(name string, _ ast.AttributeScope) Value {
		switch strings.ToLower(name) {
		case "rec":
			return NewClassAdValue(shared)
		case "y":
			return NewIntValue(7)
		}
		return NewUndefinedValue()
	})
	sel, _ := ParseExpr("Rec.X")
	if v := ev.Evaluate(sel.internal()); v.String() != "7" {
		t.Errorf("Rec.X = %v, want 7 (X = Y resolved in the selecting scope)", v)
	}
	if shared.GetParent() != nil {
		t.Error("select left the shared ad chained to the selecting scope")
	}
}

// TestViewWithDuplicateNames: a record literal may repeat a name; every read of the
// view sees the last assignment, and canonical forms collapse the duplicate.
func TestViewWithDuplicateNames(t *testing.T) {
	ad, err := Parse(`[R = [b = 1; A = 2; B = 3]]`)
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := ad.EvaluateAttr("R").ClassAdValue()
	if v, _ := rec.EvaluateAttrInt("B"); v != 3 {
		t.Errorf("B = %d, want 3 (last assignment wins)", v)
	}
	if got := rec.String(); got != "[b = 3; A = 2]" {
		t.Errorf("String = %s", got)
	}
	if got := len(rec.AST().Attributes); got != 2 {
		t.Errorf("AST has %d attributes, want 2", got)
	}
	want, _ := Parse(`[A = 2; b = 3]`)
	if !rec.Equal(want) {
		t.Errorf("%s != %s", rec, want)
	}
	if rec.Size() != 2 {
		t.Errorf("Size = %d, want 2", rec.Size())
	}
}
