package db

import "testing"

func TestIsMatchAll(t *testing.T) {
	matchAll := []string{
		"",
		"true",
		"TRUE",
		"  true  ",
		"1 == 1",
		"  1 == 1  ",
		"true && true",
		"1 == 1 && 2 == 2",
		"1 < 2",
		"true || false",
	}
	for _, c := range matchAll {
		if !IsMatchAll(c) {
			t.Errorf("IsMatchAll(%q) = false, want true (constant tautology matches every record)", c)
		}
	}

	notMatchAll := []string{
		"false",
		"1 == 2",
		"JobStatus == 2",         // record-dependent
		"JobStatus =!= 2",        // TRUE against an empty ad, but NOT match-all -- must not fold
		"true || JobStatus == 2", // always true, but references an attribute: left to the scan
		"Owner == \"alice\"",
		"(&*^ not parseable",
	}
	for _, c := range notMatchAll {
		if IsMatchAll(c) {
			t.Errorf("IsMatchAll(%q) = true, want false", c)
		}
	}
}
