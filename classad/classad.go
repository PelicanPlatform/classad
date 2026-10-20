// Package classad provides a public API for working with HTCondor ClassAds.
// It mimics the C++ ClassAd library API from HTCondor.
//
// # Concurrency
//
// A *ClassAd may be read by any number of goroutines at once. Reading covers every
// method that does not change the ad: serialization (String, MarshalOld,
// MarshalJSON and their WithPrivate forms, json.Marshal), Equal, Lookup, Size,
// GetAttributes, AST, Redacted, Flatten, ExternalRefs/InternalRefs, and evaluation
// (EvaluateAttr*, EvaluateExpr*, Expr.Eval, Expr.EvalWithContext,
// EvaluateExprWithTarget, EvaluateExprAgainst). Reads do not modify the ad, so
// they need no locking and see consistent results.
//
// Writes need exclusive access: while one runs, no other goroutine may read or
// write the same ad. The writes are Insert and the methods built on it
// (InsertAttr*, InsertExpr, InsertListElement, InsertAttrList, Set), Delete, Clear,
// UnmarshalJSON, SetParent and SetTarget. NewMatchClassAd, ReplaceLeftAd and
// ReplaceRightAd call SetTarget on the ads they pair, so they are writes to both;
// to evaluate against a target that other goroutines share, use
// EvaluateExprAgainst, which binds MY and TARGET for one evaluation without
// changing either ad. An Evaluator is not safe for concurrent use (create one per
// goroutine); the ads it evaluates are only read.
//
// A Value is immutable and may be shared. A ClassAd obtained from a Value (a nested
// record) is a read-only view of the ad it came from; writing it detaches it onto a
// private copy and never changes that ad.
package classad

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/parser"
)

// Expr represents an unevaluated ClassAd expression.
// It provides methods to evaluate the expression in different contexts
// and to inspect its structure.
type Expr struct {
	expr ast.Expr
}

// Equal reports structural equality between two expressions.
func (e *Expr) Equal(other *Expr) bool {
	if e == nil && other == nil {
		return true
	}
	if e == nil || other == nil {
		return false
	}
	return exprEqual(e.internal(), other.internal())
}

// ParseExpr parses a ClassAd expression string and returns an Expr object.
// This allows you to work with expressions without evaluating them immediately.
//
// Example:
//
//	expr, err := classad.ParseExpr("Cpus * 2 + Memory / 1024")
//	if err != nil {
//	    log.Fatal(err)
//	}
func ParseExpr(input string) (*Expr, error) {
	expr, err := parser.ParseExpr(input)
	if err != nil {
		return nil, err
	}
	return &Expr{expr: expr}, nil
}

// ParseExprOld parses a standalone expression as HTCondor's ParseClassAdRvalExpr does,
// which reads config macro functions ($INT, $EVAL, ...), submit expressions and
// job_queue.log values. String literals have OLD-ClassAd semantics: an unrecognized escape
// sequence is kept literally rather than rejected (a filename in a TransferInput list
// carries an escaped comma or space, `"...\,\ ..."`, which ParseExpr would reject), and
// adjacent literals are not concatenated. Like libclassad, it ignores whatever follows a
// top-level conditional (a ? b : c). See parser.ParseExprOld.
func ParseExprOld(input string) (*Expr, error) {
	expr, err := parser.ParseExprOld(input)
	if err != nil {
		return nil, err
	}
	return &Expr{expr: expr}, nil
}

// Quote escapes a string for safe use in ClassAd expressions.
// It adds surrounding quotes and escapes special characters according to ClassAd syntax.
//
// Example:
//
//	quoted := classad.Quote(`value with "quotes"`)
//	// Returns: "value with \"quotes\""
func Quote(s string) string {
	return fmt.Sprintf("%q", s)
}

// Unquote removes ClassAd string quoting and unescapes special characters.
// It expects the input to be a quoted string (with surrounding quotes).
//
// Example:
//
//	original, err := classad.Unquote(`"value with \"quotes\""`)
//	// Returns: value with "quotes"
func Unquote(s string) (string, error) {
	// Use Go's strconv unquoting which handles the same escape sequences
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("string must be quoted")
	}

	// Parse using Go's string literal rules which match ClassAd escaping
	var result string
	_, err := fmt.Sscanf(s, "%q", &result)
	if err != nil {
		return "", fmt.Errorf("failed to unquote string: %w", err)
	}
	return result, nil
}

// String returns the string representation of the expression.
func (e *Expr) String() string {
	if e.expr == nil {
		return "undefined"
	}
	return e.expr.String()
}

// internal returns the internal ast.Expr representation.
// This is used internally for operations that require the AST node.
func (e *Expr) internal() ast.Expr {
	if e == nil {
		return nil
	}
	return e.expr
}

// FoldConstants returns the expression with its constant sub-expressions pre-computed
// (see the package-level FoldConstants). The result evaluates exactly as e does, in any
// scope.
func (e *Expr) FoldConstants() *Expr {
	if e == nil {
		return nil
	}
	return &Expr{expr: FoldConstants(e.expr)}
}

// Eval evaluates the expression in the context of the given ClassAd.
// This is equivalent to calling classad.EvaluateExpr(expr).
func (e *Expr) Eval(scope *ClassAd) (result Value) {
	if e.expr == nil {
		return NewUndefinedValue()
	}
	defer recoverCyclic(&result)
	evaluator := NewEvaluator(scope)
	return evaluator.Evaluate(e.expr)
}

// EvalWithContext evaluates the expression with explicit MY (scope) and TARGET contexts.
// The scope parameter provides the context for MY.attr references.
// The target parameter provides the context for TARGET.attr references.
// Either parameter may be nil if not needed.
//
// Example:
//
//	expr, _ := classad.ParseExpr("MY.Cpus > TARGET.Cpus")
//	result := expr.EvalWithContext(jobAd, machineAd)
func (e *Expr) EvalWithContext(scope, target *ClassAd) (result Value) {
	if e.expr == nil {
		return NewUndefinedValue()
	}

	defer recoverCyclic(&result)
	evaluator := NewEvaluator(scope)
	// TARGET is bound for this evaluation only, not written onto scope: the scope ad
	// may be shared by goroutines evaluating it against other targets.
	if scope != nil && target != nil {
		evaluator.bind = &targetBinding{a: scope, b: target}
	}
	return evaluator.Evaluate(e.expr)
}

// ClassAd represents a ClassAd with attributes that can be evaluated.
// This is the main type for working with ClassAds.
//
// A ClassAd is safe for concurrent reads: any number of goroutines may serialize,
// compare, look up, iterate and evaluate one ad at once, provided none of them writes
// it. The writes are Insert and its InsertAttr*/InsertExpr/InsertListElement/Set
// forms, Delete, Clear, UnmarshalJSON, SetParent and SetTarget -- and therefore
// NewMatchClassAd, ReplaceLeftAd and ReplaceRightAd, which call SetTarget on the ads
// they are given. A write needs exclusive access: no other goroutine may read or write
// the ad until it returns. See the package documentation.
type ClassAd struct {
	ad     *ast.ClassAd
	parent *ClassAd
	target *ClassAd
	// index maps each normalized attribute name to its value slot.
	//
	// A non-nil index marks an OWNED ad (every constructor: New, Parse, ParseOld,
	// FromAST, UnmarshalJSON, Reader). Its attributes are kept duplicate-free and
	// indexed AT ALL TIMES -- each write maintains both -- so no read has to
	// deduplicate or index. (A lazily built index used to be filled in by the first
	// read, a write that raced with every other reader.)
	//
	// A nil index with a non-nil ad marks a VIEW of an AST owned by something else,
	// e.g. the value of a nested record literal, which aliases the enclosing ad's
	// AST. A view is never modified: lookups scan it. Its first write detaches it
	// onto its own copy (see own), so writing a record value cannot reach the ad it
	// came from.
	index map[string]*ast.Expr
	// ordered holds the attributes in canonical order (sortAttributeAssignments:
	// case-insensitive by name), for the readers that need it -- the serializers,
	// Equal, AST. It is ad itself when ad is already in that order (the usual case
	// for parsed `condor_status -l` output and decoded wire ads), otherwise a sorted
	// copy built by the first reader that asks. Readers never reorder ad itself: the
	// slice used to be sorted in place on first read, so one reader could be walking
	// it while another reordered it, producing missing and duplicated attributes.
	// Racing first readers each build the same order and the atomic publish makes
	// either result safe to use. A write that can break the order clears it.
	ordered atomic.Pointer[ast.ClassAd]
}

// Equal reports whether two ClassAds have the same attributes and values, ignoring
// attribute order and casing of attribute names.
func (c *ClassAd) Equal(other *ClassAd) bool {
	if c == nil && other == nil {
		return true
	}
	if c == nil || other == nil {
		return false
	}

	la, ra := c.canonicalAttrs(), other.canonicalAttrs()
	if len(la) != len(ra) {
		return false
	}

	for i := range la {
		left := la[i]
		right := ra[i]
		if normalizeName(left.Name) != normalizeName(right.Name) {
			return false
		}
		if !exprEqual(left.Value, right.Value) {
			return false
		}
	}

	return true
}

// normalizeName returns a case-insensitive key for attribute lookups.
func normalizeName(name string) string {
	return strings.ToLower(name)
}

// foldCompare orders two attribute names exactly as their normalizeName forms compare,
// without allocating. The sort comparators below ran normalizeName on both operands of
// every comparison -- and strings.ToLower allocates whenever a name contains an uppercase
// letter, which for CamelCase ClassAd attributes is essentially always -- so sorting one
// ad's attributes allocated O(n log n) throwaway strings. Ordering does not need the
// lowered string, only the comparison, so fold a byte at a time instead.
//
// The ASCII path is exact; a non-ASCII byte anywhere that could affect the result defers
// to the reference normalization, because Unicode lowercasing is not length-preserving
// (U+0130 lowers to two runes) and a byte-wise fold would order such names differently.
func foldCompare(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := a[i], b[i]
		if ca >= utf8.RuneSelf || cb >= utf8.RuneSelf {
			return strings.Compare(normalizeName(a), normalizeName(b))
		}
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	// The shared prefix folds equal, so only the longer name's tail can decide. Pure ASCII
	// there means the shorter name sorts first; anything else goes to the reference path.
	tail := a[n:]
	if len(b) > n {
		tail = b[n:]
	}
	for i := 0; i < len(tail); i++ {
		if tail[i] >= utf8.RuneSelf {
			return strings.Compare(normalizeName(a), normalizeName(b))
		}
	}
	return len(a) - len(b)
}

// dedupAssignments collapses duplicate attribute names, keeping each name's last
// assignment, matching the reference engine -- a ClassAd is a map there, so a later
// "x = ..." overwrites an earlier one ([B=1;B=2] has B==2). Attribute names are
// case-insensitive: the reference engine keeps the first occurrence's name (its casing
// and position) but the last occurrence's value, so [A=1; a=2] is A==2. It returns
// attrs itself when there is no duplicate, and otherwise a new slice of new
// assignments -- attrs and the assignments it holds are never modified.
func dedupAssignments(attrs []*ast.AttributeAssignment) []*ast.AttributeAssignment {
	if len(attrs) < 2 {
		return attrs
	}
	firstIdx := make(map[string]int, len(attrs))
	lastValue := make(map[string]ast.Expr, len(attrs))
	order := make([]string, 0, len(attrs))
	for i, attr := range attrs {
		n := normalizeName(attr.Name)
		if _, seen := firstIdx[n]; !seen {
			firstIdx[n] = i
			order = append(order, n)
		}
		lastValue[n] = attr.Value
	}
	if len(firstIdx) == len(attrs) {
		return attrs // no duplicates
	}
	kept := make([]*ast.AttributeAssignment, 0, len(firstIdx))
	for _, n := range order {
		first := attrs[firstIdx[n]]
		kept = append(kept, &ast.AttributeAssignment{Name: first.Name, Value: lastValue[n]})
	}
	return kept
}

// hasDuplicateNames reports whether two of attrs' names are equal case-insensitively.
func hasDuplicateNames(attrs []*ast.AttributeAssignment) bool {
	if len(attrs) <= 16 {
		for i := 1; i < len(attrs); i++ {
			for j := 0; j < i; j++ {
				if foldCompare(attrs[i].Name, attrs[j].Name) == 0 {
					return true
				}
			}
		}
		return false
	}
	seen := make(map[string]struct{}, len(attrs))
	for _, attr := range attrs {
		n := normalizeName(attr.Name)
		if _, dup := seen[n]; dup {
			return true
		}
		seen[n] = struct{}{}
	}
	return false
}

// establish puts c.ad's attributes into the owned-ad invariant -- duplicate-free and
// indexed -- in place, and records whether they are already in canonical order. It
// runs only while the ad is being constructed (or detached), before any other
// goroutine can see it.
//
// The common input -- a parse of `condor_status -l` output or a wire decode, both
// already sorted and duplicate-free -- costs one fold of each name (which the index
// needs anyway) plus one comparison of adjacent folded names; it allocates only the
// index map. A duplicate (rare: hand-built or malformed input) falls back to the full
// dedup.
func (c *ClassAd) establish() {
	attrs := c.ad.Attributes
	if c.index == nil {
		c.index = make(map[string]*ast.Expr, len(attrs))
	} else {
		clear(c.index)
	}
	sorted, dup := true, false
	prev := ""
	for i, attr := range attrs {
		n := normalizeName(attr.Name)
		if _, exists := c.index[n]; exists {
			dup = true
			break
		}
		// foldCompare orders names exactly as their normalized forms compare, so
		// comparing the folded strings checks the order sortAttributeAssignments
		// produces (names are distinct here, so its case-sensitive tie-break never
		// applies).
		if i > 0 && n < prev {
			sorted = false
		}
		prev = n
		c.index[n] = &attr.Value
	}
	if dup {
		c.ad.Attributes = dedupAssignments(attrs)
		clear(c.index)
		for _, attr := range c.ad.Attributes {
			c.index[normalizeName(attr.Name)] = &attr.Value
		}
		sorted = false
	}
	if sorted {
		c.ordered.Store(c.ad)
	} else {
		c.ordered.Store(nil)
	}
}

// adopt wraps a in an owned ClassAd, taking ownership of it: its attribute slice may
// be deduplicated (see establish).
func adopt(a *ast.ClassAd) *ClassAd {
	if a == nil {
		a = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
	}
	c := &ClassAd{ad: a}
	c.establish()
	return c
}

// own prepares c for a write. A zero ClassAd gets an empty attribute list, and a view
// is detached onto a private copy of its attributes -- new assignments as well as a
// new slice, since Insert writes through an assignment's Value slot -- so the write
// cannot reach the AST the view was reading.
func (c *ClassAd) own() {
	if c.index != nil {
		return
	}
	if c.ad == nil {
		c.ad = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
		c.index = map[string]*ast.Expr{}
		c.ordered.Store(c.ad)
		return
	}
	attrs := make([]*ast.AttributeAssignment, len(c.ad.Attributes))
	for i, a := range c.ad.Attributes {
		attrs[i] = &ast.AttributeAssignment{Name: a.Name, Value: a.Value}
	}
	c.ad = &ast.ClassAd{Attributes: attrs}
	c.establish()
}

// canonical returns the attributes duplicate-free and in canonical order without
// modifying c.ad: c.ad itself if it is already in order, otherwise a sorted copy. The
// result is cached in c.ordered; it must not be modified. c.ad must be non-nil.
func (c *ClassAd) canonical() *ast.ClassAd {
	if p := c.ordered.Load(); p != nil {
		return p
	}
	attrs := c.ad.Attributes
	p := c.ad
	if !strictlyOrdered(attrs) {
		sorted := attrs
		if c.index == nil {
			// A view may repeat a name (an owned ad cannot); dedup copies if so.
			sorted = dedupAssignments(attrs)
		}
		if len(sorted) == len(attrs) {
			sorted = slices.Clone(attrs) // never sort attrs itself
		}
		sortAttributeAssignments(sorted)
		p = &ast.ClassAd{Attributes: sorted}
	}
	c.ordered.Store(p)
	return p
}

// strictlyOrdered reports whether attrs is in canonical order with no repeated name.
func strictlyOrdered(attrs []*ast.AttributeAssignment) bool {
	for i := 1; i < len(attrs); i++ {
		if foldCompare(attrs[i-1].Name, attrs[i].Name) >= 0 { // equal names are duplicates
			return false
		}
	}
	return true
}

// attrs returns the attributes to iterate for output, without modifying c.ad: an owned
// ad's attributes in canonical order, or a view's in source order (as the record was
// written) with duplicates collapsed.
func (c *ClassAd) attrs() []*ast.AttributeAssignment {
	if c.ad == nil {
		return nil
	}
	if c.index != nil {
		return c.canonical().Attributes
	}
	if !hasDuplicateNames(c.ad.Attributes) {
		return c.ad.Attributes
	}
	return dedupAssignments(c.ad.Attributes)
}

// canonicalAttrs returns the attributes duplicate-free and in canonical order, for an
// owned ad and a view alike.
func (c *ClassAd) canonicalAttrs() []*ast.AttributeAssignment {
	if c.ad == nil {
		return nil
	}
	return c.canonical().Attributes
}

// scan finds the value of name in a view by a linear search, honoring the
// last-assignment-wins rule for a duplicated name. Views are nested records -- a few
// attributes, looked up a few times per evaluation of the record -- for which a scan is
// cheaper than building an index.
func (c *ClassAd) scan(name string) ast.Expr {
	attrs := c.ad.Attributes
	for i := len(attrs) - 1; i >= 0; i-- {
		if foldCompare(attrs[i].Name, name) == 0 {
			return attrs[i].Value
		}
	}
	return nil
}

// New creates a new empty ClassAd.
func New() *ClassAd {
	return &ClassAd{
		ad: &ast.ClassAd{
			Attributes: []*ast.AttributeAssignment{},
		},
		index: map[string]*ast.Expr{},
	}
}

// Parse parses a ClassAd string and returns a ClassAd object.
func Parse(input string) (*ClassAd, error) {
	ad, err := parser.ParseClassAd(input)
	if err != nil {
		return nil, err
	}
	if ad == nil {
		return nil, fmt.Errorf("failed to parse ClassAd")
	}
	return adopt(ad), nil
}

// ParseOld parses a ClassAd in the "old" HTCondor format and returns a ClassAd object.
// Old ClassAds have attributes separated by newlines without surrounding brackets.
// Example:
//
//	Foo = 3
//	Bar = "hello"
//	Moo = Foo =!= Undefined
func ParseOld(input string) (*ClassAd, error) {
	ad, err := parser.ParseOldClassAd(input)
	if err != nil {
		return nil, err
	}
	if ad == nil {
		return nil, fmt.Errorf("failed to parse old ClassAd")
	}
	return adopt(ad), nil
}

// String returns the string representation of the ClassAd.
// unparseAttrName renders an attribute name so it re-parses (see
// ast.QuoteAttributeName).
func unparseAttrName(name string) string {
	return ast.QuoteAttributeName(name)
}

// String renders the ClassAd in new (bracketed) format, excluding private
// (secret) attributes -- the default-safe form for any output that may reach a
// client. Use StringWithPrivate where the full ad, including secrets, is required
// (internal diagnostics, an authorized private channel).
func (c *ClassAd) String() string { return c.unparse(false) }

// StringWithPrivate is String including private attributes. Prefer String for
// anything client-facing; see IsPrivateAttribute.
func (c *ClassAd) StringWithPrivate() string { return c.unparse(true) }

func (c *ClassAd) unparse(includePrivate bool) string {
	if c.ad == nil {
		return "[]"
	}

	var b strings.Builder
	b.WriteByte('[')
	first := true
	for _, attr := range c.attrs() {
		if !includePrivate && IsPrivateAttribute(attr.Name) {
			continue
		}
		if !first {
			b.WriteString("; ")
		}
		first = false
		b.WriteString(unparseAttrName(attr.Name))
		b.WriteString(" = ")
		b.WriteString(attr.Value.String())
	}
	b.WriteByte(']')
	return b.String()
}

// ToOldFormat serializes the ClassAd to old HTCondor format (newline-delimited).
// Old format has one attribute per line without surrounding brackets.
//
// Example:
//
//	ad, _ := classad.Parse("[Cpus = 4; Memory = 8192]")
//	oldFmt := ad.MarshalOld()
//	// Returns: "Cpus = 4\nMemory = 8192"
//
// MarshalOld excludes private (secret) attributes; use MarshalOldWithPrivate for
// the full ad. See IsPrivateAttribute.
func (c *ClassAd) MarshalOld() string { return c.marshalOld(false) }

// MarshalOldWithPrivate is MarshalOld including private attributes. Prefer
// MarshalOld for anything client-facing.
func (c *ClassAd) MarshalOldWithPrivate() string { return c.marshalOld(true) }

func (c *ClassAd) marshalOld(includePrivate bool) string {
	if c.ad == nil || len(c.ad.Attributes) == 0 {
		return ""
	}

	// Build in a strings.Builder: `result += ...` in a loop is O(n^2) (each += copies
	// the whole accumulated string), which allocated tens of MB to render a single
	// large (~21 KB) ad.
	var b strings.Builder
	first := true
	for _, attr := range c.attrs() {
		if !includePrivate && IsPrivateAttribute(attr.Name) {
			continue
		}
		if !first {
			b.WriteByte('\n')
		}
		first = false
		// Old-ClassAd wire format takes the attribute name VERBATIM (name before '='), as
		// the C++ serializer does -- do NOT quote non-bare names here. A name like
		// "user:condor_tail_X" must round-trip as itself so C++ clients (condor_status, the
		// negotiator) read the same attribute; the new-format String() path still quotes.
		b.WriteString(attr.Name)
		b.WriteString(" = ")
		// Render the value in OLD-ClassAd form: string literals escape only the delimiter
		// (backslashes/control chars verbatim), matching the C++ old-format unparser and the
		// lenient old-format lexer -- so a value like a path or regex round-trips instead of
		// gaining a backslash each hop. attr.Value.String() would use new-format escaping.
		b.WriteString(unparseExprStringOld(attr.Value))
	}
	return b.String()
}

// Insert inserts an attribute with an expression into the ClassAd.
func (c *ClassAd) Insert(name string, expr ast.Expr) {
	c.own()
	normalized := normalizeName(name)
	if ptr, ok := c.index[normalized]; ok {
		// Replacing an existing attribute's VALUE, in place. The attribute set is
		// unchanged, so the ad stays sorted. This is the overwhelmingly common write on a
		// live job: an update to LastJobLeaseRenewal or MemoryUsage names an attribute the
		// ad already has.
		*ptr = expr
		return
	}
	c.insertNew(normalized, &ast.AttributeAssignment{Name: name, Value: expr})
}

// insertNew appends an attribute the owned ad c does not have. Appending is what
// keeps building an ad cheap (a wire decode inserts every attribute of every ad it
// receives, in the sender's order); the canonical order is rebuilt by the next reader
// that needs it, unless the new name sorts after every other, which keeps it.
func (c *ClassAd) insertNew(normalized string, a *ast.AttributeAssignment) {
	n := len(c.ad.Attributes)
	inOrder := c.ordered.Load() == c.ad &&
		(n == 0 || foldCompare(c.ad.Attributes[n-1].Name, a.Name) < 0)
	c.ad.Attributes = append(c.ad.Attributes, a)
	c.index[normalized] = &a.Value
	if !inOrder {
		c.ordered.Store(nil)
	}
}

// InsertExpr inserts an attribute with an Expr value into the ClassAd.
// This allows you to copy expressions between ClassAds without evaluating them.
//
// Example:
//
//	expr, _ := sourceAd.Lookup("Formula")
//	targetAd.InsertExpr("Formula", expr)
func (c *ClassAd) InsertExpr(name string, expr *Expr) {
	if expr == nil {
		return
	}
	c.Insert(name, expr.internal())
}

// InsertAttr inserts an attribute with an integer value.
func (c *ClassAd) InsertAttr(name string, value int64) {
	c.Insert(name, &ast.IntegerLiteral{Value: value})
}

// InsertAttrFloat inserts an attribute with a float value.
func (c *ClassAd) InsertAttrFloat(name string, value float64) {
	c.Insert(name, &ast.RealLiteral{Value: value})
}

// InsertAttrString inserts an attribute with a string value.
func (c *ClassAd) InsertAttrString(name, value string) {
	c.Insert(name, &ast.StringLiteral{Value: value})
}

// InsertAttrBool inserts an attribute with a boolean value.
func (c *ClassAd) InsertAttrBool(name string, value bool) {
	c.Insert(name, &ast.BooleanLiteral{Value: value})
}

// InsertAttrClassAd inserts an attribute with a nested ClassAd value.
// The provided ClassAd will be embedded as a record literal.
func (c *ClassAd) InsertAttrClassAd(name string, value *ClassAd) {
	var inner *ast.ClassAd
	if value != nil {
		inner = value.ad
	}
	if inner == nil {
		inner = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
	}
	c.Insert(name, &ast.RecordLiteral{ClassAd: inner})
}

// InsertAttrList inserts an attribute with a list of values using generics.
// Supported types are: int64, float64, string, bool, *ClassAd, and *Expr.
//
// Example:
//
//	ad := classad.New()
//	InsertAttrList(ad, "numbers", []int64{1, 2, 3, 4, 5})
//	InsertAttrList(ad, "names", []string{"Alice", "Bob", "Charlie"})
//	InsertAttrList(ad, "flags", []bool{true, false, true})
//	InsertAttrList(ad, "values", []float64{1.5, 2.7, 3.14})
//
//	// Also works with ClassAds
//	ad1, ad2 := classad.New(), classad.New()
//	ad1.InsertAttr("x", 1)
//	ad2.InsertAttr("y", 2)
//	InsertAttrList(ad, "items", []*classad.ClassAd{ad1, ad2})
//
//	// And with expressions
//	expr1, _ := classad.ParseExpr("\"hello\"")
//	expr2, _ := classad.ParseExpr("42")
//	InsertAttrList(ad, "mixed", []*classad.Expr{expr1, expr2})
func InsertAttrList[T int64 | float64 | string | bool | *ClassAd | *Expr](c *ClassAd, name string, values []T) {
	elements := make([]ast.Expr, len(values))
	for i, v := range values {
		elements[i] = valueToAstExpr(v)
	}
	c.Insert(name, &ast.ListLiteral{Elements: elements})
}

// valueToAstExpr converts a value to an ast.Expr based on its type.
func valueToAstExpr[T int64 | float64 | string | bool | *ClassAd | *Expr](v T) ast.Expr {
	switch val := any(v).(type) {
	case int64:
		return &ast.IntegerLiteral{Value: val}
	case float64:
		return &ast.RealLiteral{Value: val}
	case string:
		return &ast.StringLiteral{Value: val}
	case bool:
		return &ast.BooleanLiteral{Value: val}
	case *ClassAd:
		var inner *ast.ClassAd
		if val != nil {
			inner = val.ad
		}
		if inner == nil {
			inner = &ast.ClassAd{Attributes: []*ast.AttributeAssignment{}}
		}
		return &ast.RecordLiteral{ClassAd: inner}
	case *Expr:
		if val == nil {
			return &ast.UndefinedLiteral{}
		}
		return val.internal()
	default:
		return &ast.UndefinedLiteral{}
	}
}

// InsertListElement inserts an element into a list attribute.
// If the attribute doesn't exist, it creates a new list with the element.
// If the attribute exists and is a list, it appends the element to the existing list.
// If the attribute exists but is not a list, it replaces it with a new list containing the element.
//
// Example:
//
//	ad := classad.New()
//	expr1, _ := classad.ParseExpr("\"first\"")
//	expr2, _ := classad.ParseExpr("\"second\"")
//	ad.InsertListElement("items", expr1)
//	ad.InsertListElement("items", expr2)
//	// Result: items = {"first", "second"}
func (c *ClassAd) InsertListElement(name string, element *Expr) {
	c.own()

	var astExpr ast.Expr
	if element == nil {
		astExpr = &ast.UndefinedLiteral{}
	} else {
		astExpr = element.internal()
	}

	if ptr, ok := c.index[normalizeName(name)]; ok {
		if list, ok := (*ptr).(*ast.ListLiteral); ok {
			// Replace the list node rather than append to it: the node may be shared
			// with another ad (InsertExpr of a Lookup result) or a caller's Expr, and
			// appending in place grew their list too. Clip so the append copies.
			elems := append(slices.Clip(list.Elements), astExpr)
			*ptr = &ast.ListLiteral{Elements: elems}
			return
		}
		*ptr = &ast.ListLiteral{Elements: []ast.Expr{astExpr}}
		return
	}

	// Add new list attribute
	c.insertNew(normalizeName(name), &ast.AttributeAssignment{
		Name:  name,
		Value: &ast.ListLiteral{Elements: []ast.Expr{astExpr}},
	})
}

// Lookup returns the unevaluated expression for an attribute.
// Returns nil if the attribute doesn't exist.
// This is useful for inspecting or copying expressions without evaluating them.
//
// Example:
//
//	ad, _ := classad.Parse("[x = 10; y = x * 2]")
//	expr, ok := ad.Lookup("y")  // Returns expression for "x * 2"
//	if ok {
//	    fmt.Println(expr.String())  // Prints: x * 2
//	}
func (c *ClassAd) Lookup(name string) (*Expr, bool) {
	if expr := c.lookupInternal(name); expr != nil {
		return &Expr{expr: expr}, true
	}
	return nil, false
}

// lookupInternal finds an expression bound to an attribute name.
// Returns nil if the attribute doesn't exist.
// This is the internal version that returns ast.Expr for backward compatibility.
func (c *ClassAd) lookupInternal(name string) ast.Expr {
	if c.ad == nil {
		return nil
	}
	if c.index == nil {
		return c.scan(name)
	}
	// Attribute names are ASCII identifiers, so case-fold into a stack buffer and
	// index the map with string(buf): the compiler special-cases a string([]byte)
	// map key to avoid a heap allocation, eliminating the strings.ToLower that
	// normalizeName would make on every lookup. This is the matchmaking read hot
	// path (EvaluateAttr per Symmetry / rank, once per candidate). A non-ASCII or
	// over-long name falls back to the allocating normalize.
	const maxStack = 128
	if n := len(name); n > 0 && n <= maxStack {
		var buf [maxStack]byte
		b := buf[:n]
		ascii := true
		for i := 0; i < n; i++ {
			ch := name[i]
			if ch >= 0x80 {
				ascii = false
				break
			}
			if 'A' <= ch && ch <= 'Z' {
				ch += 'a' - 'A'
			}
			b[i] = ch
		}
		if ascii {
			if ptr, ok := c.index[string(b)]; ok {
				return *ptr
			}
			return nil
		}
	}
	return c.lookupNorm(normalizeName(name))
}

// lookupNorm is lookupInternal for an already-normalized (lower-cased) name. Hot
// callers that resolve the same reference repeatedly (the evaluator's attribute
// resolution) normalize once and call this to avoid a strings.ToLower allocation
// per lookup.
func (c *ClassAd) lookupNorm(norm string) ast.Expr {
	if c.ad == nil {
		return nil
	}
	if c.index == nil {
		return c.scan(norm)
	}
	if ptr, ok := c.index[norm]; ok {
		return *ptr
	}
	return nil
}

// Set is a generic method that accepts any Go value and inserts it as an attribute.
// This provides a more idiomatic alternative to the type-specific Insert* methods.
// Supported types: int/int64/etc., float64, string, bool, []T, *ClassAd, *Expr, and structs.
//
// Example:
//
//	ad := classad.New()
//	ad.Set("cpus", 4)                    // int
//	ad.Set("name", "job-1")              // string
//	ad.Set("price", 3.14)                // float64
//	ad.Set("enabled", true)              // bool
//	ad.Set("tags", []string{"a", "b"})   // slice
//	ad.Set("config", nestedClassAd)      // *ClassAd
func (c *ClassAd) Set(name string, value any) error {
	if value == nil {
		c.Insert(name, &ast.UndefinedLiteral{})
		return nil
	}

	val := reflect.ValueOf(value)

	// Handle special types first
	switch v := value.(type) {
	case *ClassAd:
		c.InsertAttrClassAd(name, v)
		return nil
	case *Expr:
		c.InsertExpr(name, v)
		return nil
	case int64:
		c.InsertAttr(name, v)
		return nil
	case float64:
		c.InsertAttrFloat(name, v)
		return nil
	case string:
		c.InsertAttrString(name, v)
		return nil
	case bool:
		c.InsertAttrBool(name, v)
		return nil
	}

	// Handle other integer types
	switch val.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		c.InsertAttr(name, val.Int())
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		c.InsertAttr(name, int64(val.Uint()))
		return nil
	case reflect.Float32:
		c.InsertAttrFloat(name, val.Float())
		return nil
	}

	// Try to marshal the value using the struct marshaling logic
	expr, err := marshalValue(val)
	if err != nil {
		return fmt.Errorf("failed to marshal value of type %T: %w", value, err)
	}
	c.Insert(name, expr)
	return nil
}

// GetAs retrieves and evaluates an attribute, converting it to the specified type.
// This is a type-safe generic getter that handles type conversions automatically.
// Returns the zero value and false if the attribute doesn't exist or conversion fails.
//
// Example:
//
//	cpus, ok := classad.GetAs[int](ad, "cpus")
//	name, ok := classad.GetAs[string](ad, "name")
//	price, ok := classad.GetAs[float64](ad, "price")
//	tags, ok := classad.GetAs[[]string](ad, "tags")
//	config, ok := classad.GetAs[*classad.ClassAd](ad, "config")
func GetAs[T any](c *ClassAd, name string) (T, bool) {
	var zero T

	// Special case for *Expr - return unevaluated expression
	if _, isExpr := any(zero).(*Expr); isExpr {
		expr, ok := c.Lookup(name)
		if !ok {
			return zero, false
		}
		// Safe type assertion since we checked isExpr above
		result, ok := any(expr).(T)
		if !ok {
			return zero, false
		}
		return result, true
	}

	// For other types, evaluate the attribute
	val := c.EvaluateAttr(name)
	if val.IsUndefined() {
		return zero, false
	}

	// Try to unmarshal into the target type
	target := reflect.New(reflect.TypeOf(zero)).Elem()
	if err := unmarshalValueInto(val, target); err != nil {
		return zero, false
	}

	// Safe type assertion since unmarshalValueInto ensures type compatibility
	result, ok := target.Interface().(T)
	if !ok {
		return zero, false
	}
	return result, true
}

// GetOr retrieves and evaluates an attribute with a default value.
// If the attribute doesn't exist or conversion fails, returns the default value.
// This is a type-safe generic getter with fallback.
//
// Example:
//
//	cpus := classad.GetOr(ad, "cpus", 1)           // Defaults to 1
//	name := classad.GetOr(ad, "name", "unknown")   // Defaults to "unknown"
//	timeout := classad.GetOr(ad, "timeout", 300)   // Defaults to 300
func GetOr[T any](c *ClassAd, name string, defaultValue T) T {
	if value, ok := GetAs[T](c, name); ok {
		return value
	}
	return defaultValue
}

// Delete removes an attribute from the ClassAd.
// Returns true if the attribute was found and deleted.
func (c *ClassAd) Delete(name string) bool {
	if c.ad == nil {
		return false
	}
	c.own()

	normalized := normalizeName(name)
	ptr, ok := c.index[normalized]
	if !ok {
		return false
	}

	// Find the matching attribute by pointer equality on Value. Removing an element
	// leaves the rest of c.ad in order, but a separate sorted copy is now stale.
	if c.ordered.Load() != c.ad {
		c.ordered.Store(nil)
	}
	for i := range c.ad.Attributes {
		if &c.ad.Attributes[i].Value == ptr {
			c.ad.Attributes = append(c.ad.Attributes[:i], c.ad.Attributes[i+1:]...)
			delete(c.index, normalized)
			return true
		}
	}
	return false
}

// Size returns the number of attributes in the ClassAd.
func (c *ClassAd) Size() int {
	if c.ad == nil {
		return 0
	}
	if c.index != nil {
		return len(c.ad.Attributes)
	}
	return len(c.attrs())
}

// Clear removes all attributes from the ClassAd.
func (c *ClassAd) Clear() {
	if c.ad == nil || c.index == nil {
		// A view must not clear the AST it is reading: start a new one.
		c.ad = &ast.ClassAd{}
	}
	c.ad.Attributes = []*ast.AttributeAssignment{}
	c.index = map[string]*ast.Expr{}
	c.ordered.Store(c.ad)
}

// GetAttributes returns a list of all attribute names, in the order the ad
// serializes them (sorted case-insensitively).
func (c *ClassAd) GetAttributes() []string {
	if c.ad == nil {
		return []string{}
	}

	attrs := c.attrs()
	names := make([]string, len(attrs))
	for i, attr := range attrs {
		names[i] = attr.Name
	}
	return names
}

// SetParent sets the parent ClassAd for this ClassAd.
// The parent is used for PARENT.attr references during evaluation.
// SetParent is a write: see the package documentation.
func (c *ClassAd) SetParent(parent *ClassAd) {
	c.parent = parent
}

// GetParent returns the parent ClassAd, if any.
func (c *ClassAd) GetParent() *ClassAd {
	return c.parent
}

// SetTarget sets the target ClassAd for this ClassAd.
// The target is used for TARGET.attr references during evaluation.
// SetTarget is a write: see the package documentation.
func (c *ClassAd) SetTarget(target *ClassAd) {
	c.target = target
}

// GetTarget returns the target ClassAd, if any.
func (c *ClassAd) GetTarget() *ClassAd {
	return c.target
}

// EvaluateAttr evaluates an attribute and returns its value.
func (c *ClassAd) EvaluateAttr(name string) (result Value) {
	expr := c.lookupInternal(name)
	if expr == nil {
		// CurrentTime is a magic attribute: when the ad does not define it, it resolves to
		// the current time (matching how an expression referencing it evaluates).
		if strings.EqualFold(name, currentTimeAttr) {
			return currentTimeValue()
		}
		return NewUndefinedValue()
	}

	defer recoverCyclic(&result)
	evaluator := NewEvaluator(c)
	return evaluator.Evaluate(expr)
}

// EvaluateAttrInt evaluates an attribute as an integer.
// Returns true if the attribute evaluated to an integer.
func (c *ClassAd) EvaluateAttrInt(name string) (int64, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsInteger() {
		return 0, false
	}
	intVal, err := val.IntValue()
	if err != nil {
		return 0, false
	}
	return intVal, true
}

// EvaluateAttrReal evaluates an attribute as a real number.
// Returns true if the attribute evaluated to a real.
func (c *ClassAd) EvaluateAttrReal(name string) (float64, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsReal() {
		return 0, false
	}
	realVal, err := val.RealValue()
	if err != nil {
		return 0, false
	}
	return realVal, true
}

// EvaluateAttrNumber evaluates an attribute as a number (int or real).
// Returns true if the attribute evaluated to a number.
// Integers are promoted to float64.
func (c *ClassAd) EvaluateAttrNumber(name string) (float64, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsNumber() {
		return 0, false
	}
	numVal, err := val.NumberValue()
	if err != nil {
		return 0, false
	}
	return numVal, true
}

// EvaluateAttrString evaluates an attribute as a string.
// Returns true if the attribute evaluated to a string.
func (c *ClassAd) EvaluateAttrString(name string) (string, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsString() {
		return "", false
	}
	strVal, err := val.StringValue()
	if err != nil {
		return "", false
	}
	return strVal, true
}

// EvaluateAttrBool evaluates an attribute as a boolean.
// Returns true if the attribute evaluated to a boolean.
func (c *ClassAd) EvaluateAttrBool(name string) (bool, bool) {
	val := c.EvaluateAttr(name)
	if !val.IsBool() {
		return false, false
	}
	boolVal, err := val.BoolValue()
	if err != nil {
		return false, false
	}
	return boolVal, true
}

// EvaluateExpr evaluates an arbitrary expression in the context of this ClassAd.
func (c *ClassAd) EvaluateExpr(expr ast.Expr) (result Value) {
	defer recoverCyclic(&result)
	evaluator := NewEvaluator(c)
	return evaluator.Evaluate(expr)
}

// EvaluateExprString parses and evaluates an expression string.
func (c *ClassAd) EvaluateExprString(exprStr string) (Value, error) {
	// Parse the expression
	node, err := parser.Parse(exprStr)
	if err != nil {
		return NewErrorValue(), err
	}

	// Extract expression from parsed result
	var expr ast.Expr
	if ad, ok := node.(*ast.ClassAd); ok && len(ad.Attributes) == 1 {
		// If it's a simple assignment, evaluate the RHS
		expr = ad.Attributes[0].Value
	} else if e, ok := node.(ast.Expr); ok {
		expr = e
	} else {
		return NewErrorValue(), fmt.Errorf("unable to extract expression from parsed result")
	}

	return c.EvaluateExpr(expr), nil
}

// EvaluateExprWithTarget evaluates an Expr in the context of this ClassAd with a target.
// This ClassAd serves as the MY scope, and the target parameter serves as the TARGET scope.
// This is useful for match-making operations where expressions reference both ClassAds.
//
// Example:
//
//	expr, _ := classad.ParseExpr("MY.Cpus > TARGET.Cpus")
//	result := jobAd.EvaluateExprWithTarget(expr, machineAd)
func (c *ClassAd) EvaluateExprWithTarget(expr *Expr, target *ClassAd) Value {
	if expr == nil {
		return NewUndefinedValue()
	}
	return expr.EvalWithContext(c, target)
}

// EvaluateExprAgainst evaluates expr with this ClassAd as MY and target as TARGET,
// pairing them as NewMatchClassAd does -- within target's attributes, TARGET is this
// ad -- but for this evaluation only: neither ad is modified, so many goroutines may
// evaluate against one shared target (a cached slot ad, say) at once. NewMatchClassAd
// instead records each ad as the other's TARGET on the ads themselves, which is a
// write to both.
//
// Example:
//
//	expr, _ := parser.ParseExpr("TARGET.Memory >= RequestMemory")
//	v := jobAd.EvaluateExprAgainst(expr, slotAd)
func (c *ClassAd) EvaluateExprAgainst(expr ast.Expr, target *ClassAd) (result Value) {
	defer recoverCyclic(&result)
	evaluator := NewEvaluator(c)
	evaluator.bind = &targetBinding{a: c, b: target, symmetric: true}
	return evaluator.Evaluate(expr)
}

// ExternalRefs returns a list of attribute names referenced in the expression
// but not defined in this ClassAd. This is useful for validating that a ClassAd
// has all required attributes before evaluation.
//
// Example:
//
//	ad, _ := classad.Parse("[Cpus = 4; Memory = 8192]")
//	expr, _ := classad.ParseExpr("Cpus * 2 + ExternalAttr")
//	external := ad.ExternalRefs(expr)  // Returns: ["ExternalAttr"]
func (c *ClassAd) ExternalRefs(expr *Expr) []string {
	if expr == nil {
		return []string{}
	}

	allRefs := c.collectRefs(expr.expr)
	external := []string{}

	for _, ref := range allRefs {
		if _, ok := c.Lookup(ref); !ok {
			external = append(external, ref)
		}
	}

	return external
}

// InternalRefs returns a list of attribute names referenced in the expression
// that are defined in this ClassAd.
//
// Example:
//
//	ad, _ := classad.Parse("[Cpus = 4; Memory = 8192]")
//	expr, _ := classad.ParseExpr("Cpus * 2 + ExternalAttr")
//	internal := ad.InternalRefs(expr)  // Returns: ["Cpus"]
func (c *ClassAd) InternalRefs(expr *Expr) []string {
	if expr == nil {
		return []string{}
	}

	allRefs := c.collectRefs(expr.expr)
	internal := []string{}

	for _, ref := range allRefs {
		if _, ok := c.Lookup(ref); ok {
			internal = append(internal, ref)
		}
	}

	return internal
}

// collectRefs recursively collects all attribute references from an expression
func (c *ClassAd) collectRefs(expr ast.Expr) []string {
	if expr == nil {
		return []string{}
	}

	refs := make(map[string]bool)
	c.collectRefsHelper(expr, refs)

	// Convert map to slice
	result := make([]string, 0, len(refs))
	for ref := range refs {
		result = append(result, ref)
	}
	return result
}

// collectRefsHelper is a recursive helper for collectRefs
func (c *ClassAd) collectRefsHelper(expr ast.Expr, refs map[string]bool) {
	switch v := expr.(type) {
	case *ast.ParenExpr:
		c.collectRefsHelper(v.Inner, refs)

	case *ast.AttributeReference:
		// Only collect non-scoped references (no MY., TARGET., PARENT.)
		if v.Scope == ast.NoScope {
			refs[v.Name] = true
		}

	case *ast.BinaryOp:
		c.collectRefsHelper(v.Left, refs)
		c.collectRefsHelper(v.Right, refs)

	case *ast.UnaryOp:
		c.collectRefsHelper(v.Expr, refs)

	case *ast.ConditionalExpr:
		c.collectRefsHelper(v.Condition, refs)
		c.collectRefsHelper(v.TrueExpr, refs)
		c.collectRefsHelper(v.FalseExpr, refs)

	case *ast.ElvisExpr:
		c.collectRefsHelper(v.Left, refs)
		c.collectRefsHelper(v.Right, refs)

	case *ast.FunctionCall:
		for _, arg := range v.Args {
			c.collectRefsHelper(arg, refs)
		}

	case *ast.ListLiteral:
		for _, elem := range v.Elements {
			c.collectRefsHelper(elem, refs)
		}

	case *ast.ClassAd:
		for _, attr := range v.Attributes {
			c.collectRefsHelper(attr.Value, refs)
		}

	case *ast.SelectExpr:
		c.collectRefsHelper(v.Record, refs)

	case *ast.SubscriptExpr:
		c.collectRefsHelper(v.Container, refs)
		c.collectRefsHelper(v.Index, refs)

	// Literals don't contain references
	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral,
		*ast.BooleanLiteral, *ast.UndefinedLiteral, *ast.ErrorLiteral:
		// Nothing to do
	}
}

// Flatten partially evaluates an expression in the context of this ClassAd.
// Attributes that are defined in the ClassAd are evaluated and replaced with their values.
// Undefined attributes are left as references. This is useful for optimizing expressions
// by pre-computing constant sub-expressions.
//
// Example:
//
//	ad, _ := classad.Parse("[RequestMemory = 2048]")
//	expr, _ := classad.ParseExpr("RequestMemory * 1024 * 1024")
//	flattened := ad.Flatten(expr)  // Returns expression equivalent to: 2147483648
func (c *ClassAd) Flatten(expr *Expr) *Expr {
	if expr == nil {
		return nil
	}

	flattened := c.flattenExpr(expr.expr)
	return &Expr{expr: flattened}
}

// flattenExpr recursively flattens an AST expression in the context of c. A CurrentTime
// reference c does not define folds to the current time, since c is the scope.
func (c *ClassAd) flattenExpr(expr ast.Expr) ast.Expr {
	return (&flattener{c: c, time: timeClock}).flatten(expr)
}

// curTimeFold says what flattening does with a CurrentTime reference the ad does not define.
type curTimeFold int

const (
	timeClock curTimeFold = iota // fold it to the current time: the ad is the evaluation scope
	timeKeep                     // keep the reference: the scope is not known (FoldConstants)
	timeFixed                    // fold it to flattener.now (FoldConstantsAt)
)

// flattener carries the options of one flattening pass.
type flattener struct {
	c    *ClassAd
	time curTimeFold
	now  int64
	// keepLists leaves list literals as written. A list's elements are expressions until
	// something reads them, and string() / strcat() print those expressions, so folding
	// `{-(-1.0)}` to `{1.0}` changes what string({-(-1.0)}) returns. FoldConstants, whose
	// result must evaluate exactly like its input, sets it.
	keepLists bool
}

func (f *flattener) flatten(expr ast.Expr) ast.Expr {
	c := f.c
	if expr == nil {
		return nil
	}

	switch v := expr.(type) {
	case *ast.ParenExpr:
		inner := f.flatten(v.Inner)
		// Keep the parentheses only around an operator, where they carry
		// precedence; drop them once the inner expression has collapsed to a
		// literal/primary so constant folding can see it (and so the flattened
		// form is not littered with cosmetic parens).
		switch inner.(type) {
		case *ast.BinaryOp, *ast.UnaryOp, *ast.ConditionalExpr, *ast.ElvisExpr:
			return &ast.ParenExpr{Inner: inner}
		default:
			return inner
		}

	case *ast.AttributeReference:
		// Try to evaluate the reference
		if v.Scope == ast.NoScope {
			if _, ok := c.Lookup(v.Name); ok {
				// Evaluate the attribute
				val := c.EvaluateAttr(v.Name)
				return c.valueToExpr(val)
			}
		}
		// The magic CurrentTime attribute, when the ad does not define it, is the current
		// time (the evaluation-time magic in evaluateAttributeReference). Flatten folds it
		// to the clock, since the ad is the scope; FoldConstantsAt to a given time, so a
		// query optimizer sees an indexable literal (`CompletionDate > CurrentTime - 86400`
		// becomes a range probe); FoldConstants keeps it, since the scope it will be
		// evaluated in may define CurrentTime.
		if isCurrentTimeRef(v) {
			if _, ok := c.Lookup(v.Name); !ok {
				switch f.time {
				case timeClock:
					return c.valueToExpr(currentTimeValue())
				case timeFixed:
					return &ast.IntegerLiteral{Value: f.now}
				}
			}
		}
		// Keep the reference if undefined or scoped
		return expr

	case *ast.BinaryOp:
		// Flatten both sides
		left := f.flatten(v.Left)
		right := f.flatten(v.Right)

		if v.Op == "&&" || v.Op == "||" {
			if folded, ok := c.foldLogical(v.Op, left, right); ok {
				return folded
			}
			return &ast.BinaryOp{Op: v.Op, Left: left, Right: right}
		}

		// Try to evaluate if both sides are literals
		leftVal := c.exprToValue(left)
		rightVal := c.exprToValue(right)

		// `is`/`isnt` (=?=/=!=) are total: they compare undefined/error as values, so
		// fold them whenever both operands are literals (including undefined/error) --
		// the general path below bails when an operand is undefined. This lets
		// `undefined is undefined` -> true, so a guard like
		// `ifThenElse((X is undefined) || ..., 0, Y)` with X baked to undefined
		// collapses to its taken branch.
		if (v.Op == "is" || v.Op == "isnt") && isLiteralExpr(left) && isLiteralExpr(right) {
			if result := c.evaluateBinaryOp(v.Op, leftVal, rightVal); result.IsBool() {
				if b, err := result.BoolValue(); err == nil {
					return &ast.BooleanLiteral{Value: b}
				}
			}
		}

		if !leftVal.IsUndefined() && !rightVal.IsUndefined() {
			// Try to compute the operation
			result := c.evaluateBinaryOp(v.Op, leftVal, rightVal)
			if !result.IsUndefined() && !result.IsError() {
				return c.valueToExpr(result)
			}
		}

		// Return with flattened operands
		return &ast.BinaryOp{
			Op:    v.Op,
			Left:  left,
			Right: right,
		}

	case *ast.UnaryOp:
		operand := f.flatten(v.Expr)
		operandVal := c.exprToValue(operand)

		if !operandVal.IsUndefined() {
			result := c.evaluateUnaryOp(v.Op, operandVal)
			if !result.IsUndefined() && !result.IsError() {
				return c.valueToExpr(result)
			}
		}

		return &ast.UnaryOp{
			Op:   v.Op,
			Expr: operand,
		}

	case *ast.ConditionalExpr:
		condition := f.flatten(v.Condition)
		trueExpr := f.flatten(v.TrueExpr)
		falseExpr := f.flatten(v.FalseExpr)

		// If condition is a literal boolean, return the appropriate branch
		condVal := c.exprToValue(condition)
		if condVal.IsBool() {
			if boolVal, err := condVal.BoolValue(); err == nil && boolVal {
				return trueExpr
			}
			return falseExpr
		}

		return &ast.ConditionalExpr{
			Condition: condition,
			TrueExpr:  trueExpr,
			FalseExpr: falseExpr,
		}

	case *ast.ElvisExpr:
		left := f.flatten(v.Left)
		right := f.flatten(v.Right)

		// If left is a literal undefined, return right. exprToValue reads any
		// non-literal as undefined, so check that left really is a literal.
		if isLiteralExpr(left) && c.exprToValue(left).IsUndefined() {
			return right
		}

		return &ast.ElvisExpr{
			Left:  left,
			Right: right,
		}

	case *ast.FunctionCall:
		// Flatten arguments
		args := make([]ast.Expr, len(v.Args))
		for i, arg := range v.Args {
			args[i] = f.flatten(arg)
		}

		// Fold ifThenElse when the condition is a literal boolean after flattening.
		if strings.EqualFold(v.Name, "ifThenElse") && len(args) == 3 {
			condVal := c.exprToValue(args[0])
			if condVal.IsBool() {
				boolVal, err := condVal.BoolValue()
				if err != nil {
					return &ast.ErrorLiteral{}
				}
				if boolVal {
					return args[1]
				}
				return args[2]
			}
		}

		return &ast.FunctionCall{Name: v.Name, Args: args}

	case *ast.ListLiteral:
		if f.keepLists {
			return expr
		}
		elements := make([]ast.Expr, len(v.Elements))
		for i, elem := range v.Elements {
			elements[i] = f.flatten(elem)
		}
		return &ast.ListLiteral{
			Elements: elements,
		}

	case *ast.SelectExpr:
		record := f.flatten(v.Record)
		return &ast.SelectExpr{
			Record: record,
			Attr:   v.Attr,
		}

	case *ast.SubscriptExpr:
		container := f.flatten(v.Container)
		index := f.flatten(v.Index)
		return &ast.SubscriptExpr{
			Container: container,
			Index:     index,
		}

	// Literals are already fully evaluated
	default:
		return expr
	}
}

// foldLogical folds `left op right` for op "&&" or "||", given the already-flattened operands,
// and reports whether it did.
//
// && and || evaluate left to right and are not commutative. The left operand alone can decide the
// result -- FALSE && x is FALSE and TRUE || x is TRUE whatever x is, and an ERROR or non-boolean
// left operand makes either one ERROR -- but a literal on the right decides nothing about a left
// operand that may turn out ERROR or non-boolean: ERROR && FALSE is ERROR, and "a" || TRUE is
// ERROR. The identity cases (TRUE && x, FALSE || x, x && TRUE, x || FALSE) reduce to x only when x
// can only be boolean, undefined or error, because the operator maps any other value: a number
// to a boolean (5 && TRUE is TRUE, not 5) and anything else to ERROR.
func (c *ClassAd) foldLogical(op string, left, right ast.Expr) (ast.Expr, bool) {
	leftLit, rightLit := isLiteralExpr(left), isLiteralExpr(right)
	switch {
	case leftLit && rightLit:
		return c.valueToExpr(c.evaluateBinaryOp(op, c.exprToValue(left), c.exprToValue(right))), true
	case leftLit:
		switch ls := logicalView(c.exprToValue(left)); {
		case ls == lsErr:
			return &ast.ErrorLiteral{}, true
		case op == "&&" && ls == lsFalse:
			return &ast.BooleanLiteral{Value: false}, true
		case op == "||" && ls == lsTrue:
			return &ast.BooleanLiteral{Value: true}, true
		case (op == "&&" && ls == lsTrue || op == "||" && ls == lsFalse) && isLogicalTyped(right):
			return right, true
		}
	case rightLit:
		rs := logicalView(c.exprToValue(right))
		if (op == "&&" && rs == lsTrue || op == "||" && rs == lsFalse) && isLogicalTyped(left) {
			return left, true
		}
	}
	return nil, false
}

// isLogicalTyped reports whether e can only evaluate to a boolean, undefined or error: a
// comparison, a logical operator, or a literal of one of those values. && and || pass such an
// operand through unchanged where they would map any other value.
func isLogicalTyped(e ast.Expr) bool {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.Inner
	}
	switch v := e.(type) {
	case *ast.BooleanLiteral, *ast.UndefinedLiteral, *ast.ErrorLiteral:
		return true
	case *ast.UnaryOp:
		return v.Op == "!"
	case *ast.BinaryOp:
		switch v.Op {
		case "==", "!=", "<", "<=", ">", ">=", "is", "isnt", "&&", "||":
			return true
		}
	}
	return false
}

// exprToValue converts a literal expression to a Value, returns undefined for non-literals
func (c *ClassAd) exprToValue(expr ast.Expr) Value {
	switch v := expr.(type) {
	case *ast.IntegerLiteral:
		return NewIntValue(v.Value)
	case *ast.RealLiteral:
		return NewRealValue(v.Value)
	case *ast.StringLiteral:
		return NewStringValue(v.Value)
	case *ast.BooleanLiteral:
		return NewBoolValue(v.Value)
	case *ast.UndefinedLiteral:
		return NewUndefinedValue()
	case *ast.ErrorLiteral:
		return NewErrorValue()
	default:
		return NewUndefinedValue()
	}
}

// valueToExpr converts a Value to an AST expression
func (c *ClassAd) valueToExpr(val Value) ast.Expr {
	switch val.Type() {
	case IntegerValue:
		if intVal, err := val.IntValue(); err == nil {
			return &ast.IntegerLiteral{Value: intVal}
		}
	case RealValue:
		if realVal, err := val.RealValue(); err == nil {
			return &ast.RealLiteral{Value: realVal}
		}
	case StringValue:
		if strVal, err := val.StringValue(); err == nil {
			return &ast.StringLiteral{Value: strVal}
		}
	case BooleanValue:
		if boolVal, err := val.BoolValue(); err == nil {
			return &ast.BooleanLiteral{Value: boolVal}
		}
	case ListValue:
		list, err := val.ListValue()
		if err == nil {
			elements := make([]ast.Expr, 0, len(list))
			for _, item := range list {
				elements = append(elements, c.valueToExpr(item))
			}
			return &ast.ListLiteral{Elements: elements}
		}
	case ClassAdValue:
		adVal, err := val.ClassAdValue()
		if err == nil && adVal != nil {
			return &ast.RecordLiteral{ClassAd: adVal.ad}
		}
	case UndefinedValue:
		return &ast.UndefinedLiteral{}
	case ErrorValue:
		return &ast.ErrorLiteral{}
	}
	return &ast.UndefinedLiteral{}
}

// exprEqual compares two ast expressions for structural equality.
func exprEqual(a, b ast.Expr) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// Parentheses are transparent to structural equality.
	for {
		if p, ok := a.(*ast.ParenExpr); ok {
			a = p.Inner
		} else {
			break
		}
	}
	for {
		if p, ok := b.(*ast.ParenExpr); ok {
			b = p.Inner
		} else {
			break
		}
	}

	switch av := a.(type) {
	case *ast.IntegerLiteral:
		bv, ok := b.(*ast.IntegerLiteral)
		return ok && av.Value == bv.Value
	case *ast.RealLiteral:
		bv, ok := b.(*ast.RealLiteral)
		return ok && floatEqual(av.Value, bv.Value)
	case *ast.StringLiteral:
		bv, ok := b.(*ast.StringLiteral)
		return ok && av.Value == bv.Value
	case *ast.BooleanLiteral:
		bv, ok := b.(*ast.BooleanLiteral)
		return ok && av.Value == bv.Value
	case *ast.UndefinedLiteral:
		_, ok := b.(*ast.UndefinedLiteral)
		return ok
	case *ast.ErrorLiteral:
		_, ok := b.(*ast.ErrorLiteral)
		return ok
	case *ast.AttributeReference:
		bv, ok := b.(*ast.AttributeReference)
		return ok && av.Scope == bv.Scope && strings.EqualFold(av.Name, bv.Name)
	case *ast.BinaryOp:
		bv, ok := b.(*ast.BinaryOp)
		return ok && av.Op == bv.Op && exprEqual(av.Left, bv.Left) && exprEqual(av.Right, bv.Right)
	case *ast.UnaryOp:
		bv, ok := b.(*ast.UnaryOp)
		return ok && av.Op == bv.Op && exprEqual(av.Expr, bv.Expr)
	case *ast.ConditionalExpr:
		bv, ok := b.(*ast.ConditionalExpr)
		return ok && exprEqual(av.Condition, bv.Condition) && exprEqual(av.TrueExpr, bv.TrueExpr) && exprEqual(av.FalseExpr, bv.FalseExpr)
	case *ast.ElvisExpr:
		bv, ok := b.(*ast.ElvisExpr)
		return ok && exprEqual(av.Left, bv.Left) && exprEqual(av.Right, bv.Right)
	case *ast.FunctionCall:
		bv, ok := b.(*ast.FunctionCall)
		if !ok || !strings.EqualFold(av.Name, bv.Name) || len(av.Args) != len(bv.Args) {
			return false
		}
		for i := range av.Args {
			if !exprEqual(av.Args[i], bv.Args[i]) {
				return false
			}
		}
		return true
	case *ast.ListLiteral:
		bv, ok := b.(*ast.ListLiteral)
		if !ok || len(av.Elements) != len(bv.Elements) {
			return false
		}
		for i := range av.Elements {
			if !exprEqual(av.Elements[i], bv.Elements[i]) {
				return false
			}
		}
		return true
	case *ast.RecordLiteral:
		bv, ok := b.(*ast.RecordLiteral)
		if !ok {
			return false
		}
		// Views: Equal compares canonical copies and never touches the shared ASTs.
		return (&ClassAd{ad: av.ClassAd}).Equal(&ClassAd{ad: bv.ClassAd})
	case *ast.SelectExpr:
		bv, ok := b.(*ast.SelectExpr)
		return ok && strings.EqualFold(av.Attr, bv.Attr) && exprEqual(av.Record, bv.Record)
	case *ast.SubscriptExpr:
		bv, ok := b.(*ast.SubscriptExpr)
		return ok && exprEqual(av.Container, bv.Container) && exprEqual(av.Index, bv.Index)
	default:
		return false
	}
}

// floatEqual compares two float64 values with a relative tolerance to account for
// floating point rounding. NaN only equals NaN; +Inf/-Inf must match exactly.
func floatEqual(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return math.IsInf(a, 1) == math.IsInf(b, 1) && math.IsInf(a, -1) == math.IsInf(b, -1)
	}

	const relTol = 1e-9
	diff := math.Abs(a - b)
	if diff == 0 {
		return true
	}
	mag := math.Max(math.Abs(a), math.Abs(b))
	return diff <= relTol*mag
}

// Helper functions for evaluating operations during flattening
func (c *ClassAd) evaluateBinaryOp(op string, left, right Value) Value {
	// Dispatch directly on the operand values via the shared value-level core,
	// avoiding a temporary AST + evaluator round-trip.
	return NewEvaluator(c).applyBinaryValues(op, left, right)
}

func (c *ClassAd) evaluateUnaryOp(op string, operand Value) Value {
	return NewEvaluator(c).applyUnaryValue(op, operand)
}

// MarshalJSON implements the json.Marshaler interface for ClassAd.
// ClassAds are serialized as JSON objects where:
//   - Attribute names are JSON keys
//   - Simple values (strings, numbers, booleans) are JSON values
//   - Lists are JSON arrays
//   - Nested ClassAds are JSON objects
//   - Expressions are serialized as strings with the format "/Expr(<expression>)/"
//     (which appears as "\/Expr(<expression>)\/" in JSON due to escaping)
//
// Example:
//
//	ad, _ := classad.Parse(`[x = 5; y = x + 3; name = "test"]`)
//	jsonBytes, _ := json.Marshal(ad)
//	// {"name":"test","x":5,"y":"\/Expr(x + 3)\/"}
//
// MarshalJSON implements json.Marshaler, excluding private (secret) attributes so
// that any HTTP/JSON response that marshals a ClassAd is default-safe -- a job's
// ClaimId or a slot's Capability never reaches a client through json.Marshal. Use
// MarshalJSONWithPrivate where the full ad is required. See IsPrivateAttribute.
func (c *ClassAd) MarshalJSON() ([]byte, error) { return c.marshalJSON(false) }

// MarshalJSONWithPrivate is MarshalJSON including private attributes. Prefer
// MarshalJSON for anything client-facing.
func (c *ClassAd) MarshalJSONWithPrivate() ([]byte, error) { return c.marshalJSON(true) }

func (c *ClassAd) marshalJSON(includePrivate bool) ([]byte, error) {
	if c.ad == nil {
		return []byte("{}"), nil
	}

	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	for _, attr := range c.attrs() {
		if !includePrivate && IsPrivateAttribute(attr.Name) {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		keyBytes, err := json.Marshal(attr.Name)
		if err != nil {
			return nil, err
		}
		value, err := c.marshalValue(attr.Value, includePrivate)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal attribute %s: %w", attr.Name, err)
		}
		valBytes, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		buf.Write(keyBytes)
		buf.WriteByte(':')
		buf.Write(valBytes)
	}
	buf.WriteByte('}')

	jsonBytes := buf.Bytes()
	jsonBytes = []byte(strings.ReplaceAll(string(jsonBytes), "\"/Expr(", "\"\\/Expr("))
	jsonBytes = []byte(strings.ReplaceAll(string(jsonBytes), ")/\"", ")\\/\""))

	return jsonBytes, nil
}

// marshalValue converts an AST expression to a JSON-serializable value.
// Simple literals are converted directly, complex expressions are wrapped.
// includePrivate carries the caller's choice into nested records.
func (c *ClassAd) marshalValue(expr ast.Expr, includePrivate bool) (interface{}, error) {
	switch v := expr.(type) {
	case *ast.IntegerLiteral:
		return v.Value, nil
	case *ast.RealLiteral:
		return v.Value, nil
	case *ast.StringLiteral:
		return v.Value, nil
	case *ast.BooleanLiteral:
		return v.Value, nil
	case *ast.UndefinedLiteral:
		return nil, nil
	case *ast.ListLiteral:
		list := make([]interface{}, len(v.Elements))
		for i, elem := range v.Elements {
			val, err := c.marshalValue(elem, includePrivate)
			if err != nil {
				return nil, err
			}
			list[i] = val
		}
		return list, nil
	case *ast.RecordLiteral:
		// Nested ClassAd: serialize deterministically (in canonical order) and embed as
		// raw JSON. The record's AST belongs to the enclosing ad, so order a copy; never
		// the AST itself.
		canon := (&ClassAd{ad: v.ClassAd}).canonicalAttrs()
		nested := &ClassAd{ad: &ast.ClassAd{Attributes: canon}}
		nestedBytes, err := nested.marshalJSON(includePrivate)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(nestedBytes), nil
	default:
		// Complex expression - serialize as string with special markers
		// Format: /Expr(<expression>)/
		exprStr := expr.String()
		return fmt.Sprintf("/Expr(%s)/", exprStr), nil
	}
}

// UnmarshalJSON implements the json.Unmarshaler interface for ClassAd.
// Deserializes JSON into a ClassAd, handling the special expression format.
// Strings matching the pattern "/Expr(<expression>)/" are parsed as expressions.
// Note: When unmarshaling from JSON, the Go json package automatically unescapes
// "\/Expr(...)\/" to "/Expr(...)/" so only the unescaped format is checked.
//
// Example:
//
//	jsonStr := `{"name":"test","x":5,"y":"\/Expr(x + 3)\/"}`
//	var ad ClassAd
//	json.Unmarshal([]byte(jsonStr), &ad)
func (c *ClassAd) UnmarshalJSON(data []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	attributes := make([]*ast.AttributeAssignment, 0, len(raw))
	for name, value := range raw {
		expr, err := c.unmarshalValue(value)
		if err != nil {
			return fmt.Errorf("failed to unmarshal attribute %s: %w", name, err)
		}
		attributes = append(attributes, &ast.AttributeAssignment{
			Name:  name,
			Value: expr,
		})
	}
	sortAttributeAssignments(attributes)

	c.ad = &ast.ClassAd{Attributes: attributes}
	c.establish()
	return nil
}

// sortAttributeAssignments provides deterministic ordering by case-insensitive name with
// a secondary case-sensitive tie-breaker to preserve stable behavior.
func sortAttributeAssignments(attrs []*ast.AttributeAssignment) {
	// slices.SortStableFunc over the concrete slice, not sort.SliceStable: the latter
	// swaps through reflect, which on an already-near-sorted ad costs more than the
	// comparisons do.
	slices.SortStableFunc(attrs, func(x, y *ast.AttributeAssignment) int {
		if c := foldCompare(x.Name, y.Name); c != 0 {
			return c
		}
		return strings.Compare(x.Name, y.Name)
	})
}

// unmarshalValue converts a JSON value back into an AST expression.
func (c *ClassAd) unmarshalValue(value interface{}) (ast.Expr, error) {
	switch v := value.(type) {
	case nil:
		return &ast.UndefinedLiteral{}, nil
	case bool:
		return &ast.BooleanLiteral{Value: v}, nil
	case float64:
		// JSON numbers are always float64
		// Check if it's actually an integer
		if v == float64(int64(v)) {
			return &ast.IntegerLiteral{Value: int64(v)}, nil
		}
		return &ast.RealLiteral{Value: v}, nil
	case string:
		// Check if it's an expression string.
		if strings.HasPrefix(v, "/Expr(") && strings.HasSuffix(v, ")/") {
			exprStr := v[6 : len(v)-2] // Remove "/Expr(" and ")/"
			return c.parseExpression(exprStr)
		}
		// Regular string literal
		return &ast.StringLiteral{Value: v}, nil
	case []interface{}:
		// JSON array -> ListLiteral
		elements := make([]ast.Expr, len(v))
		for i, elem := range v {
			expr, err := c.unmarshalValue(elem)
			if err != nil {
				return nil, err
			}
			elements[i] = expr
		}
		return &ast.ListLiteral{Elements: elements}, nil
	case map[string]interface{}:
		// JSON object -> RecordLiteral (nested ClassAd)
		attributes := make([]*ast.AttributeAssignment, 0, len(v))
		for name, val := range v {
			expr, err := c.unmarshalValue(val)
			if err != nil {
				return nil, err
			}
			attributes = append(attributes, &ast.AttributeAssignment{
				Name:  name,
				Value: expr,
			})
		}
		sortAttributeAssignments(attributes)
		return &ast.RecordLiteral{
			ClassAd: &ast.ClassAd{Attributes: attributes},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value type: %T", value)
	}
}

// parseExpression parses an expression string into an AST expression.
func (c *ClassAd) parseExpression(exprStr string) (ast.Expr, error) {
	expr, err := parser.ParseExpr(exprStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse expression %q: %w", exprStr, err)
	}
	return expr, nil
}
