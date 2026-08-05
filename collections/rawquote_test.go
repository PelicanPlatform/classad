package collections

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func newInlineQuoteColl(t *testing.T) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir() + "/store", Shards: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if !c.inline {
		t.Skip("this store is not inline-name; the test targets that representation")
	}
	return c
}

// bothStores runs fn against each storage representation. The two render raw text through
// different paths, and the escaping bug this covers appeared in both.
func bothStores(t *testing.T, fn func(*testing.T, *Collection)) {
	t.Helper()
	for _, store := range []struct {
		name string
		open func(*testing.T) *Collection
	}{
		{"interned", func(t *testing.T) *Collection { return New(Options{}) }},
		{"inline", newInlineQuoteColl},
	} {
		t.Run(store.name, func(t *testing.T) { fn(t, store.open(t)) })
	}
}

// rawReparse reads the collection back through the raw (old-ClassAd text) projection and
// re-parses that text the way every consumer of it does.
func rawReparse(t *testing.T, c *Collection) *classad.ClassAd {
	t.Helper()
	n := 0
	var out *classad.ClassAd
	for ra := range c.ScanRawProjected(nil, false, false) {
		n++
		lines := make([]string, 0, len(ra.Exprs))
		for _, e := range ra.Exprs {
			lines = append(lines, string(e))
		}
		back, err := classad.ParseOld(strings.Join(lines, "\n") + "\n")
		if err != nil {
			t.Fatalf("re-parsing raw text %q: %v", lines, err)
		}
		out = back
	}
	if n != 1 {
		t.Fatalf("got %d ads, want 1", n)
	}
	return out
}

// A raw projection renders old-ClassAd text, read back by a tokenizer that does no escape
// processing. Quoting it for new-ClassAd rules escaped backslashes and tabs the reader then
// took literally, doubling them on every round trip.
//
// Values are inserted through the ClassAd API rather than parsed from text, so what is
// being round-tripped is unambiguous -- `"a\\b"` means different things to the two
// dialects, which is the whole point.
func TestRawTextRoundTripsStringValues(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"backslash", `C:\Users`},
		{"two backslashes", `a\\b`},
		{"tab", "a\tb"},
		{"quote", `say "hi"`},
		{"regex", `^\d+$`},
		{"plain", "alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bothStores(t, func(t *testing.T, c *Collection) {
				ad := classad.New()
				ad.InsertAttrString("Path", tc.value)
				if err := c.Put([]byte("k"), ad); err != nil {
					t.Fatal(err)
				}
				got, err := rawReparse(t, c).EvaluateAttr("Path").StringValue()
				if err != nil {
					t.Fatalf("Path did not come back a string: %v", err)
				}
				if got != tc.value {
					t.Errorf("round trip gave %q, want %q", got, tc.value)
				}
			})
		})
	}
}

// A string nested in a list goes through the general node renderer rather than the
// top-level string fast path, so it needs the same dialect. On the interned representation
// that nesting was the only way to reach the bug, since a top-level string already took a
// correctly-quoting fast path.
func TestRawTextRoundTripsNestedStrings(t *testing.T) {
	bothStores(t, func(t *testing.T, c *Collection) {
		// Built through the new-ClassAd parser, where \\ is one backslash, so the list's
		// element is exactly `C:\Users`.
		src, err := classad.Parse(`[Args = {"C:\\Users", "b"}]`)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte("k"), src); err != nil {
			t.Fatal(err)
		}

		list, err := rawReparse(t, c).EvaluateAttr("Args").ListValue()
		if err != nil {
			t.Fatalf("Args did not come back a list: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("got %d elements, want 2", len(list))
		}
		got, err := list[0].StringValue()
		if err != nil {
			t.Fatalf("element 0 is not a string: %v", err)
		}
		if want := `C:\Users`; got != want {
			t.Errorf("nested string round-tripped to %q, want %q", got, want)
		}
	})
}
