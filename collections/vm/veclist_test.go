package vm

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// listSource serves scalar attributes through testSource and list attributes as ListColumns, built from
// the same per-record list texts the reference side parses into its ads. A record whose list text is ""
// lacks the attribute.
type listSource struct {
	*testSource
	lists map[string][]string // folded name -> per-record list literal text, "" for absent
	cols  map[string]*ListColumn
	loads int
}

func newListSource(t *testing.T, scalars []map[string]string, lists map[string][]string) *listSource {
	t.Helper()
	ls := &listSource{testSource: newTestSource(scalars), lists: map[string][]string{}, cols: map[string]*ListColumn{}}
	for name, recs := range lists {
		lc, err := buildTestListColumn(recs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		ls.lists[strings.ToLower(name)] = recs
		ls.cols[strings.ToLower(name)] = lc
	}
	return ls
}

// buildTestListColumn lays out list literal texts as a ListColumn the way a block would: one kind per
// column, decided by the first defined element.
func buildTestListColumn(recs []string) (*ListColumn, error) {
	n := len(recs)
	lc := &ListColumn{N: n, Present: make([]byte, (n+7)/8), Kind: VsInt}
	var elems []classad.Value
	offs := binary.LittleEndian.AppendUint32(nil, 0)
	kindSet := false
	for k, src := range recs {
		if strings.HasPrefix(src, "!") {
			// An exception: the record holds the attribute as something no list column carries.
			q, err := Parse(src[1:])
			if err != nil {
				return nil, err
			}
			if lc.Except == nil {
				lc.Except = map[int]classad.Value{}
			}
			lc.Except[k] = q.Eval(classad.New())
			offs = binary.LittleEndian.AppendUint32(offs, uint32(len(elems)))
			continue
		}
		if src != "" {
			q, err := Parse(src)
			if err != nil {
				return nil, err
			}
			v := q.Eval(classad.New())
			vals, err := v.ListValue()
			if err != nil {
				return nil, fmt.Errorf("record %d: %q is not a list", k, src)
			}
			lc.Present[k>>3] |= 1 << uint(k&7)
			for _, e := range vals {
				if !kindSet && !e.IsUndefined() {
					kindSet = true
					switch {
					case e.IsInteger():
						lc.Kind = VsInt
					case e.IsReal():
						lc.Kind = VsReal
					case e.IsBool():
						lc.Kind = VsBool
					case e.IsString():
						lc.Kind = VsString
					}
				}
				elems = append(elems, e)
			}
		}
		offs = binary.LittleEndian.AppendUint32(offs, uint32(len(elems)))
	}
	lc.Offs = offs
	m := len(elems)
	dict := map[string]int{}
	codes := []uint32{}
	for i, e := range elems {
		if e.IsUndefined() {
			if lc.Valid == nil {
				lc.Valid = make([]byte, (m+7)/8)
				for j := 0; j < i; j++ {
					lc.Valid[j>>3] |= 1 << uint(j&7)
				}
			}
		} else if lc.Valid != nil {
			lc.Valid[i>>3] |= 1 << uint(i&7)
		}
		switch lc.Kind {
		case VsInt:
			x, _ := e.IntValue()
			if !e.IsUndefined() && !e.IsInteger() {
				return nil, fmt.Errorf("mixed kinds")
			}
			lc.Fixed = binary.LittleEndian.AppendUint64(lc.Fixed, uint64(x))
		case VsReal:
			f, _ := e.RealValue()
			if !e.IsUndefined() && !e.IsReal() {
				return nil, fmt.Errorf("mixed kinds")
			}
			lc.Fixed = binary.LittleEndian.AppendUint64(lc.Fixed, math.Float64bits(f))
		case VsBool:
			if !e.IsUndefined() && !e.IsBool() {
				return nil, fmt.Errorf("mixed kinds")
			}
			if lc.Bits == nil || len(lc.Bits) < (m+7)/8 {
				lc.Bits = make([]byte, (m+7)/8)
			}
			if b, _ := e.BoolValue(); b {
				lc.Bits[i>>3] |= 1 << uint(i&7)
			}
		case VsString:
			if !e.IsUndefined() && !e.IsString() {
				return nil, fmt.Errorf("mixed kinds")
			}
			str, _ := e.StringValue()
			c, ok := dict[str]
			if !ok {
				c = len(lc.Dict)
				dict[str] = c
				lc.Dict = append(lc.Dict, str)
			}
			codes = append(codes, uint32(c))
		}
	}
	if lc.Kind == VsString {
		lc.CodeWidth = 4
		for _, c := range codes {
			lc.Codes = binary.LittleEndian.AppendUint32(lc.Codes, c)
		}
		if len(lc.Dict) == 0 {
			lc.Dict = []string{""}
		}
	}
	return lc, nil
}

func (s *listSource) LoadListColumn(name string, scope ast.AttributeScope) (*ListColumn, bool) {
	if scope != ast.NoScope && scope != ast.MyScope {
		return nil, false
	}
	lc, ok := s.cols[strings.ToLower(name)]
	if ok {
		s.loads++
	}
	return lc, ok
}

func (s *listSource) LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool {
	if _, isList := s.cols[strings.ToLower(name)]; isList {
		return false // a list attribute is never a vector, as in a real source
	}
	return s.testSource.LoadColumn(name, scope, dst)
}

func (s *listSource) ad(i int) *classad.ClassAd {
	ad := s.testSource.ad(i)
	for name, recs := range s.lists {
		if recs[i] == "" {
			continue
		}
		q, err := Parse(strings.TrimPrefix(recs[i], "!"))
		if err != nil {
			panic(err)
		}
		ad.Insert(name, q.Expr())
	}
	return ad
}

// checkListParity evaluates expr with the vector executor over src and compares every record's value
// with the tree-walker's on that record's ad. It reports whether the list path served the expression.
func checkListParity(t *testing.T, src *listSource, expr string) bool {
	t.Helper()
	q, err := Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	n := len(src.recs)
	before := ListFusedCalls.Load()
	vec, ok := q.VecEval(src, n, nil)
	if !ok {
		return false
	}
	served := ListFusedCalls.Load() > before
	for i := 0; i < n; i++ {
		want := refEval(src.ad(i), q.Expr())
		got := vec.value(i)
		if !sameValue(got, want) {
			t.Errorf("%s, record %d: vector %v, reference %v", expr, i, got, want)
		}
	}
	return served
}

func listParityCorpus(t *testing.T) *listSource {
	scal := []map[string]string{
		{"S": `"busy"`, "X": "1"},
		{"S": `"BUSY"`, "X": "2"},
		{"S": `"idle"`, "X": "2.0"},
		{"X": "undefined"},
		{"S": `"claimed"`, "X": `"1"`},
		{"S": `""`, "X": "true"},
		{"X": "error"},
		{"S": `"busy"`, "X": "64"},
	}
	return newListSource(t, scal, map[string][]string{
		"LI": {"{1, 2, 3}", "{}", "", "{1, undefined, 64}", "{-5}", "{0, 0, 0, 0}", "{9223372036854775807, 1}", "{64}"},
		"LR": {"{1.0, 2.5}", "{}", "{0.0}", "", "{-0.0, 1.0}", "{2.0}", "{undefined}", "{1.5, 2.5, 3.5}"},
		"LS": {`{"Busy", "Idle"}`, `{"busy"}`, "", `{undefined, "claimed"}`, `{}`, `{"", "x"}`, `{"BUSY"}`, `{"Claimed", "Busy", "busy"}`},
		"LB": {"{true, false}", "{}", "{true}", "", "{false}", "{undefined, true}", "{false, false}", "{true}"},
		"LU": {"{undefined}", "", "{undefined, undefined}", "{}", "", "{undefined}", "", "{}"},
		"LX": {"{1, 2}", "!5", "!{1, 2.5}", "", `!{1, "a"}`, "{3}", "!undefined", `!"abc"`},
	})
}

func TestVecListParity(t *testing.T) {
	src := listParityCorpus(t)
	lists := []string{"LI", "LR", "LS", "LB", "LU", "LX", "Missing"}
	args := []string{"1", "64", "2.5", "-0.0", `"busy"`, `"BUSY"`, `""`, "true", "false", "undefined", "error", "S", "X"}
	var exprs []string
	for _, l := range lists {
		for _, f := range []string{"size", "isList", "isUndefined", "sum", "avg", "min", "max"} {
			exprs = append(exprs, fmt.Sprintf("%s(%s)", f, l))
		}
		exprs = append(exprs, l+" is undefined", l+" isnt undefined", l+" =?= undefined", l+" =!= undefined")
		for _, a := range args {
			exprs = append(exprs, fmt.Sprintf("member(%s, %s)", a, l), fmt.Sprintf("identicalMember(%s, %s)", a, l))
		}
		exprs = append(exprs,
			fmt.Sprintf("size(%s) > 1 && member(\"busy\", %s)", l, l),
			fmt.Sprintf("size(%s) + 1", l),
			fmt.Sprintf("!member(1, %s) || isList(%s)", l, l),
			fmt.Sprintf("size(%s ?: LS)", l), // the call consumes the join, not LS: never fused
			fmt.Sprintf("member(\"busy\", %s ?: LS)", l),
			fmt.Sprintf("ifThenElse(X == 1, size(%s), size(LS))", l),
		)
	}
	served := 0
	for _, e := range exprs {
		if checkListParity(t, src, e) {
			served++
		}
	}
	if served < len(exprs)/2 {
		t.Fatalf("the list path served %d of %d expressions", served, len(exprs))
	}
	t.Logf("list path served %d of %d expressions", served, len(exprs))
}

// TestVecListJoinNotFused pins the jump-target guard: a call whose argument is the join of an Elvis must
// see the join's value, so it must not be fused with the load on the fallback path.
func TestVecListJoinNotFused(t *testing.T) {
	src := listParityCorpus(t)
	for _, e := range []string{"size(S ?: LS)", `member("busy", S ?: LS)`, "size(X ?: LI)"} {
		q, err := Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		// The list's load is followed by the call, which is the Elvis's join: the call's argument is S
		// where S is defined, and the list only where it is not.
		before := ListFusedCalls.Load()
		vec, ok := q.VecEval(src, len(src.recs), nil)
		if ListFusedCalls.Load() != before {
			t.Errorf("%s: a load whose successor is a join was fused", e)
		}
		if ok {
			for i := range src.recs {
				if want := refEval(src.ad(i), q.Expr()); !sameValue(vec.value(i), want) {
					t.Errorf("%s, record %d: vector %v, reference %v", e, i, vec.value(i), want)
				}
			}
		}
	}
}

// TestVecListRandomParity is a randomized differential over list shapes and call shapes.
func TestVecListRandomParity(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	elem := map[string]func() string{
		"int":    func() string { return fmt.Sprint(rng.Intn(5) - 2) },
		"real":   func() string { return fmt.Sprintf("%.1f", float64(rng.Intn(5))-2) },
		"string": func() string { return []string{`"a"`, `"A"`, `"b"`, `""`}[rng.Intn(4)] },
		"bool":   func() string { return []string{"true", "false"}[rng.Intn(2)] },
	}
	for round := 0; round < 40; round++ {
		n := 1 + rng.Intn(70)
		kinds := []string{"int", "real", "string", "bool"}
		lists := map[string][]string{}
		for _, k := range kinds {
			recs := make([]string, n)
			for i := range recs {
				if rng.Intn(5) == 0 {
					continue // absent
				}
				if rng.Intn(15) == 0 {
					recs[i] = []string{"!7", `!{1, "x"}`, "!undefined", `!"s"`, "!{1.5, 2}"}[rng.Intn(5)]
					continue
				}
				m := rng.Intn(5)
				parts := make([]string, m)
				for j := range parts {
					if rng.Intn(8) == 0 {
						parts[j] = "undefined"
					} else {
						parts[j] = elem[k]()
					}
				}
				recs[i] = "{" + strings.Join(parts, ", ") + "}"
			}
			lists["L"+k] = recs
		}
		scal := make([]map[string]string, n)
		for i := range scal {
			scal[i] = map[string]string{"Xi": elem["int"](), "Xs": elem["string"]()}
		}
		src := newListSource(t, scal, lists)
		for _, k := range kinds {
			l := "L" + k
			for _, e := range []string{
				"size(" + l + ")", "sum(" + l + ")", "avg(" + l + ")", "min(" + l + ")", "max(" + l + ")",
				"member(" + elem[k]() + ", " + l + ")", "identicalMember(" + elem[k]() + ", " + l + ")",
				"member(Xi, " + l + ")", "member(Xs, " + l + ")", "identicalMember(Xs, " + l + ")",
				l + " is undefined",
			} {
				checkListParity(t, src, e)
			}
		}
	}
}
