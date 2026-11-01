package wire

import (
	"bytes"
	"strings"
	"testing"
)

// TestPatchedInlineRemovesAndKeepsSealedBytes: AppendAdPatchedInline drops every removed name,
// case-insensitively, from the base and from the overlays alike, applies the overlays as
// AppendAdMergedInline does, and copies a sealed value it is not told about without opening it.
func TestPatchedInlineRemovesAndKeepsSealedBytes(t *testing.T) {
	sealSecret := func(name string) bool { return strings.EqualFold(name, "Secret") }
	base := EncodeInlineWithHotEnc(nil, adOf(
		[2]string{"Keep", "k"}, [2]string{"Secret", "s3cr3t"}, [2]string{"Gone", "g"}, [2]string{"Over", "old"},
	), nil, sealSecret, xorSealer{})
	overlay := EncodeInlineDelta(nil, adOf(
		[2]string{"over", "new"}, [2]string{"Added", "a"}, [2]string{"AlsoGone", "x"},
	), nil, nil, nil)

	var sc MergeScratch
	out, ok := AppendAdPatchedInline(nil, Ad(base), []Ad{Ad(overlay)}, []string{"gone", "ALSOGONE"}, &sc)
	if !ok {
		t.Fatal("patched merge refused well-formed inline ads")
	}
	if IsDelta(out) {
		t.Fatal("the merged record carries the delta flag")
	}
	// No key: the sealed entry must come through as the very bytes the base stored.
	baseSecret, _ := Ad(base).LookupByName("Secret")
	gotSecret, found := Ad(out).LookupByName("Secret")
	if !found || !bytes.Equal(gotSecret, baseSecret) {
		t.Fatalf("sealed entry not copied verbatim: got %x, want %x", gotSecret, baseSecret)
	}
	dec, err := DecodeInlineEnc(out, xorSealer{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, a := range dec.Attributes {
		got[a.Name] = a.Value.String()
	}
	want := map[string]string{"Keep": `"k"`, "Secret": `"s3cr3t"`, "Over": `"new"`, "Added": `"a"`}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (all: %v)", k, got[k], v, got)
		}
	}

	// With nothing removed it is AppendAdMergedInline, byte for byte.
	merged, _ := AppendAdMergedInline(nil, Ad(base), []Ad{Ad(overlay)}, &sc)
	patched, _ := AppendAdPatchedInline(nil, Ad(base), []Ad{Ad(overlay)}, nil, &sc)
	if !bytes.Equal(merged, patched) {
		t.Fatal("AppendAdPatchedInline with no removals differs from AppendAdMergedInline")
	}
}
