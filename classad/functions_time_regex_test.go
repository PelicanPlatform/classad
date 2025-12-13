package classad

import (
	"strings"
	"testing"
)

func TestFormatTimeConversion(t *testing.T) {
	val := evalBuiltin(t, `formatTime(0, "%a %A %b %B %c %d %H %I %j %m %M %p %S %U %w %x %X %y %Y %Z %q")`)
	if val.IsError() {
		t.Fatalf("formatTime returned error")
	}

	s, _ := val.StringValue()
	for _, piece := range []string{"Thu", "Thursday", "Jan", "1970", "UTC", "%q"} {
		if !strings.Contains(s, piece) {
			t.Fatalf("expected formatted time to include %q, got %q", piece, s)
		}
	}
}

func TestRegexpsOptionsAndErrors(t *testing.T) {
	repl := evalBuiltin(t, `regexps("foo", "FOO foo", "bar", "i")`)
	if s, _ := repl.StringValue(); s != "bar bar" {
		t.Fatalf("unexpected case-insensitive regexps result: %q", s)
	}

	if errVal := evalBuiltin(t, `regexps("(", "a", "b")`); !errVal.IsError() {
		t.Fatalf("expected error for invalid regexps pattern")
	}

	member := evalBuiltin(t, `stringListRegexpMember("^foo$", "foo|bar", "|", "i")`)
	if b, _ := member.BoolValue(); !b {
		t.Fatalf("expected stringListRegexpMember to match with delimiter and options")
	}

	undef := evalBuiltin(t, `stringListRegexpMember("foo", undefined)`)
	if !undef.IsUndefined() {
		t.Fatalf("expected undefined when list argument is undefined")
	}
}

func TestStringListSetOperations(t *testing.T) {
	inter := evalBuiltin(t, `stringListsIntersect("", "", ",")`)
	if b, _ := inter.BoolValue(); b {
		t.Fatalf("expected no intersection for empty string lists")
	}

	subset := evalBuiltin(t, `stringListSubsetMatch("a|b", "a|b|c", "|")`)
	if b, _ := subset.BoolValue(); !b {
		t.Fatalf("expected subset match with custom delimiter")
	}

	undef := evalBuiltin(t, `stringListsIntersect(undefined, "a")`)
	if !undef.IsUndefined() {
		t.Fatalf("expected undefined when first list is undefined")
	}
}

func TestStricmpErrorAndBoolCompare(t *testing.T) {
	if val := evalBuiltin(t, `stricmp(true, "x")`); !val.IsError() {
		t.Fatalf("expected error for invalid stricmp argument types")
	}

	boolCompare := evalBuiltin(t, `anyCompare("==", {true, false}, true)`)
	if b, _ := boolCompare.BoolValue(); !b {
		t.Fatalf("expected anyCompare to match boolean value")
	}
}
