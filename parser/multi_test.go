package parser

import (
	"testing"
)

func TestParseMultipleClassAds(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{
			name:     "single ClassAd",
			input:    `[Foo = 1; Bar = 2]`,
			expected: 1,
		},
		{
			name:     "concatenated ClassAds",
			input:    `[Foo = 1][Bar = 2][Baz = 3]`,
			expected: 3,
		},
		{
			name:     "concatenated with attributes",
			input:    `[Url = "file1"; LocalFileName = "/tmp/file1"][Url = "file2"; LocalFileName = "/tmp/file2"]`,
			expected: 2,
		},
		{
			name: "concatenated with whitespace",
			input: `[Foo = 1]
			[Bar = 2]
			[Baz = 3]`,
			expected: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			classads, err := ParseMultipleClassAds(tt.input)
			if err != nil {
				t.Fatalf("ParseMultipleClassAds() error = %v", err)
			}
			if len(classads) != tt.expected {
				t.Errorf("ParseMultipleClassAds() returned %d ClassAds, expected %d", len(classads), tt.expected)
			}
		})
	}
}
